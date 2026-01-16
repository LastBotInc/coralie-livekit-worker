package bridge

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"coralie-conference-service/pkg/workersdk"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/LastBotInc/coralie-livekit-worker/internal/logging"
	"github.com/LastBotInc/coralie-livekit-worker/internal/transcribe"
)

// Bridge handles bidirectional audio forwarding between LiveKit and Coralie conference.
type Bridge struct {
	room          *lksdk.Room
	confClient    *workersdk.Client
	roomName      string
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	egressID      string

	// Ingress: LiveKit → Conference
	ingressTracks map[string]*IngressTrack // participant identity -> track handler
	ingressMu      sync.RWMutex

	// Egress: Conference → LiveKit
	egress         *Egress
	egressMu       sync.Mutex

	// Transcription tap (optional)
	tap transcribe.Tap
}

// NewBridge creates a new audio bridge.
func NewBridge(
	room *lksdk.Room,
	confClient *workersdk.Client,
	roomName string,
	tap transcribe.Tap,
) *Bridge {
	ctx, cancel := context.WithCancel(context.Background())
		return &Bridge{
		room:          room,
		confClient:    confClient,
		roomName:      roomName,
		ctx:           ctx,
		cancel:        cancel,
		ingressTracks: make(map[string]*IngressTrack),
		tap:           tap,
	}
}

// Start starts the audio bridge.
func (b *Bridge) Start() error {
	// Create egress participant (receives mixed audio from all others, excluding bridge participants)
	egressParticipantID := fmt.Sprintf("lk-egress-%s", randomParticipantSuffix(8))
	egressParticipant, err := b.confClient.ConnectParticipant(b.roomName, egressParticipantID, "bridge-egress")
	if err != nil {
		return fmt.Errorf("connect egress participant: %w", err)
	}

	b.egressID = egressParticipantID

	// Start egress handler
	egress, err := NewEgress(b.room, egressParticipant)
	if err != nil {
		b.confClient.DisconnectParticipant(egressParticipantID)
		return fmt.Errorf("create egress: %w", err)
	}

	b.egressMu.Lock()
	b.egress = egress
	b.egressMu.Unlock()

	egress.Start()
	logging.Info(logging.CategoryBridge, "audio bridge started roomName=%s", b.roomName)

	return nil
}

// Stop stops the audio bridge.
func (b *Bridge) Stop() {
	logging.Info(logging.CategoryBridge, "stopping audio bridge roomName=%s", b.roomName)
	b.cancel()

	// Stop egress first
	b.egressMu.Lock()
	if b.egress != nil {
		b.egress.Stop()
		b.egress = nil
	}
	b.egressMu.Unlock()

	// Disconnect egress participant
	if b.egressID != "" {
		if err := b.confClient.DisconnectParticipant(b.egressID); err != nil {
			logging.Warning(logging.CategoryBridge, "failed to disconnect egress participant: %v", err)
		}
	} else if err := b.confClient.DisconnectParticipant("lk-egress"); err != nil {
		logging.Warning(logging.CategoryBridge, "failed to disconnect egress participant: %v", err)
	}

	// Stop all ingress tracks
	b.ingressMu.Lock()
	for _, track := range b.ingressTracks {
		track.Stop()
	}
	b.ingressTracks = make(map[string]*IngressTrack)
	b.ingressMu.Unlock()

	// Wait for goroutines to finish
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logging.Info(logging.CategoryBridge, "audio bridge stopped roomName=%s", b.roomName)
	case <-b.ctx.Done():
		logging.Warning(logging.CategoryBridge, "timeout waiting for audio bridge goroutines to stop")
	}
}

// HandleTrack handles a new audio track from a LiveKit participant.
func (b *Bridge) HandleTrack(participant *lksdk.RemoteParticipant, track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication) {
	// Only handle audio tracks
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}

	identity := participant.Identity()
	logging.Info(logging.CategoryBridge, "handling audio track participant=%s", identity)

	b.ingressMu.Lock()
	defer b.ingressMu.Unlock()

	// Check if track already exists
	if _, exists := b.ingressTracks[identity]; exists {
		logging.Warning(logging.CategoryBridge, "track already exists for participant participant=%s", identity)
		return
	}

	// Ensure conference participant exists (bridge-ingress: contributes audio but never receives egress)
	participantID := fmt.Sprintf("lk:%s-%s", identity, randomParticipantSuffix(8))
	confParticipant, err := b.confClient.ConnectParticipant(b.roomName, participantID, "bridge-ingress")
	if err != nil {
		logging.Error(logging.CategoryBridge, "failed to connect conference participant participant=%s: %v", participantID, err)
		return
	}

	// Small delay to ensure participant is fully registered before Sync arrives
	// This helps avoid the race condition where Sync arrives before participant is in the map
	time.Sleep(10 * time.Millisecond)

	// Create ingress track handler
	ingressTrack, err := NewIngressTrack(participantID, confParticipant, b.confClient, b.tap)
	if err != nil {
		logging.Error(logging.CategoryBridge, "failed to create ingress track participant=%s: %v", participantID, err)
		b.confClient.DisconnectParticipant(participantID)
		return
	}

	b.ingressTracks[identity] = ingressTrack
	ingressTrack.Start(track)
}

// RemoveTrack removes an audio track handler.
func (b *Bridge) RemoveTrack(participantIdentity string) {
	b.ingressMu.Lock()
	track, exists := b.ingressTracks[participantIdentity]
	if exists {
		delete(b.ingressTracks, participantIdentity)
	}
	b.ingressMu.Unlock()

	if exists {
		track.Stop()
		// Disconnect conference participant
		if err := b.confClient.DisconnectParticipant(track.participantID); err != nil {
			logging.Warning(logging.CategoryBridge, "failed to disconnect conference participant participant=%s: %v", track.participantID, err)
		}
		logging.Info(logging.CategoryBridge, "removed audio track participant=%s", participantIdentity)
	}
}

func randomParticipantSuffix(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	if length <= 0 {
		return ""
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		fallback := fmt.Sprintf("%x", time.Now().UnixNano())
		if len(fallback) >= length {
			return fallback[:length]
		}
		return fallback
	}
	for i := range buf {
		buf[i] = charset[int(buf[i])%len(charset)]
	}
	return string(buf)
}
