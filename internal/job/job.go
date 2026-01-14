package job

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"coralie-conference-service/pkg/workersdk"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/LastBotInc/coralie-livekit-worker/internal/bridge"
	"github.com/LastBotInc/coralie-livekit-worker/internal/config"
	"github.com/LastBotInc/coralie-livekit-worker/internal/logging"
	"github.com/LastBotInc/coralie-livekit-worker/internal/transcribe"
)

// Job represents a single room job execution.
// It connects to a LiveKit room and bridges audio to/from a Coralie conference.
type Job struct {
	JobID       string
	RoomName    string
	Token       string
	URL         string
	Config      *config.Config
	ConfClient  *workersdk.Client
	RoomService *lksdk.RoomServiceClient
	Tap         transcribe.Tap
}

// Run executes the job - connects to room, handles participants, audio bridging, etc.
func (j *Job) Run(ctx context.Context) error {
	logging.Info(logging.CategoryJob, "starting job jobID=%s room=%s", j.JobID, j.RoomName)

	// Track LiveKit participants
	participantsMu := sync.RWMutex{}
	participantsMap := make(map[string]bool) // identity -> exists

	var room *lksdk.Room
	var audioBridge *bridge.Bridge

	// Set opcode handler BEFORE connecting to room to ensure it's ready when opcodes arrive
	j.ConfClient.SetParticipantControlHandler(func(conferenceID, participantID, opcode string, parameters map[string]string) {
		// Only log opcodes if AGGRESSIVE_LOGGING is enabled
		if strings.ToLower(opcode) != "sync_tick" || strings.ToLower(os.Getenv("AGGRESSIVE_LOGGING")) == "true" {
			logging.Info(logging.CategoryJob, "received opcode opcode=%s conferenceID=%s participantID=%s", opcode, conferenceID, participantID)
		}

		// Handle disconnect/leave opcodes
		if opcode == "disconnect" || opcode == "leave_conference" {
			// Check if this targets our job/worker
			targetWorkerID, hasTarget := parameters["target_worker_id"]
			if hasTarget && targetWorkerID != j.Config.ConferenceWorkerID {
				// Not for us, ignore
				return
			}

			// Check if this targets a specific participant
			_, hasTargetPart := parameters["target_participant_id"]
			if hasTargetPart {
				// Extract LiveKit identity from participantID (format: "lk:identity")
				if strings.HasPrefix(participantID, "lk:") {
					lkIdentity := strings.TrimPrefix(participantID, "lk:")
					logging.Info(logging.CategoryJob, "disconnecting LiveKit participant via opcode participantID=%s lkIdentity=%s opcode=%s", participantID, lkIdentity, opcode)

					// Stop audio bridge track for this participant
					if audioBridge != nil {
						audioBridge.RemoveTrack(lkIdentity)
					}

					// Disconnect from LiveKit using RoomServiceClient
					if j.RoomService != nil {
						req := &livekit.RoomParticipantIdentity{
							Room:     j.RoomName,
							Identity: lkIdentity,
						}
						_, err := j.RoomService.RemoveParticipant(context.Background(), req)
						if err != nil {
							logging.Error(logging.CategoryJob, "failed to remove participant from LiveKit participantID=%s: %v", participantID, err)
						} else {
							logging.Info(logging.CategoryJob, "removed participant from LiveKit participantID=%s", participantID)
						}
					}
				}
			} else {
				// No target participant - this might be a general disconnect for the job
				// If conferenceID matches our room name, disconnect the entire job
				if conferenceID == j.RoomName {
					logging.Info(logging.CategoryJob, "disconnecting job via opcode opcode=%s", opcode)
					// Cancel the job context to trigger shutdown
					// Note: We can't cancel the parent context directly, but we can signal via bridge
					if audioBridge != nil {
						audioBridge.Stop()
					}
					// Disconnect from LiveKit room
					if room != nil {
						room.Disconnect()
					}
				}
			}

			return
		}

		// For all other opcodes: return "not implemented" in control response
		// (handled by workersdk automatically)
	})

	// Create callback struct with participant handlers
	callbacks := &lksdk.RoomCallback{
		OnDisconnected: func() {
			logging.Info(logging.CategoryJob, "disconnected from room")
			// Stop audio bridge when room disconnects
			if audioBridge != nil {
				audioBridge.Stop()
			}
		},
		OnParticipantConnected: func(participant *lksdk.RemoteParticipant) {
			identity := participant.Identity()
			// Skip agent participants
			if strings.HasPrefix(identity, "agent-") {
				logging.Info(logging.CategoryJob, "skipping agent participant identity=%s", identity)
				return
			}
			logging.Info(logging.CategoryJob, "participant connected identity=%s", identity)
			participantsMu.Lock()
			participantsMap[identity] = true
			participantsMu.Unlock()
		},
		OnParticipantDisconnected: func(participant *lksdk.RemoteParticipant) {
			identity := participant.Identity()
			logging.Info(logging.CategoryJob, "participant disconnected identity=%s", identity)
			participantsMu.Lock()
			delete(participantsMap, identity)
			participantsMu.Unlock()
			// Remove from bridge
			if audioBridge != nil {
				audioBridge.RemoveTrack(identity)
			}
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				identity := rp.Identity()
				// Only handle audio tracks - ignore video tracks
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					logging.Info(logging.CategoryJob, "track subscribed participant=%s track=%s", identity, track.Kind().String())
					// Handle audio tracks for forwarding
					if audioBridge != nil {
						audioBridge.HandleTrack(rp, track, pub)
					}
				}
				// Video tracks are ignored - don't log or subscribe to them
			},
			OnTrackUnsubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				identity := rp.Identity()
				// Only handle audio tracks - ignore video tracks
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					logging.Info(logging.CategoryJob, "track unsubscribed participant=%s track=%s", identity, track.Kind().String())
					// Remove audio track handler
					if audioBridge != nil {
						audioBridge.RemoveTrack(identity)
					}
				}
				// Video tracks are ignored - don't log or unsubscribe from them
			},
		},
	}

	// Connect to room using token
	room, err := lksdk.ConnectToRoomWithToken(j.URL, j.Token, callbacks)
	if err != nil {
		return fmt.Errorf("connect to room: %w", err)
	}
	defer room.Disconnect()

	logging.Info(logging.CategoryJob, "connected to room room=%s identity=%s", room.Name(), room.LocalParticipant.Identity())

	// Initialize audio bridge
	audioBridge = bridge.NewBridge(room, j.ConfClient, j.RoomName, j.Tap)
	if err := audioBridge.Start(); err != nil {
		logging.Error(logging.CategoryJob, "failed to start audio bridge: %v", err)
		audioBridge = nil
	} else {
		defer audioBridge.Stop()
		logging.Info(logging.CategoryJob, "audio bridge started")
	}

	// Process existing participants
	existingParticipants := room.GetRemoteParticipants()
	for _, p := range existingParticipants {
		identity := p.Identity()
		// Skip agent participants
		if strings.HasPrefix(identity, "agent-") {
			logging.Info(logging.CategoryJob, "skipping agent participant", "identity", identity)
			continue
		}
		logging.Info(logging.CategoryJob, "existing participant identity=%s", identity)
		participantsMu.Lock()
		participantsMap[identity] = true
		participantsMu.Unlock()

		// Handle existing audio tracks
		if audioBridge != nil {
			for _, pub := range p.TrackPublications() {
				if pub.Kind() == lksdk.TrackKindAudio {
					if remotePub, ok := pub.(*lksdk.RemoteTrackPublication); ok {
						// Ensure track is subscribed
						if !remotePub.IsSubscribed() {
							remotePub.SetSubscribed(true)
						}
						// If track is already subscribed, handle it immediately
						if track := remotePub.Track(); track != nil {
							if remoteTrack, ok := track.(*webrtc.TrackRemote); ok {
								audioBridge.HandleTrack(p, remoteTrack, remotePub)
							}
						}
					}
				}
			}
		}
	}

	// Run until context cancel
	<-ctx.Done()
	logging.Info(logging.CategoryJob, "context cancelled, exiting jobID=%s", j.JobID)

	// Stop audio bridge first to stop all audio processing
	if audioBridge != nil {
		audioBridge.Stop()
	}

	// Disconnect from LiveKit room
	if room != nil {
		room.Disconnect()
	}

	// Clean up all conference participants
	participantsMu.RLock()
	for identity := range participantsMap {
		participantID := fmt.Sprintf("lk:%s", identity)
		if err := j.ConfClient.DisconnectParticipant(participantID); err != nil {
			logging.Warning(logging.CategoryJob, "failed to disconnect conference participant participant=%s: %v", participantID, err)
		}
	}
	participantsMu.RUnlock()

	logging.Info(logging.CategoryJob, "job completed jobID=%s", j.JobID)
	return nil
}
