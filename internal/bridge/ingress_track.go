package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"coralie-conference-service/pkg/workersdk"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	soxr "github.com/zaf/resample"
	opus "gopkg.in/hraban/opus.v2"

	"github.com/LastBotInc/coralie-livekit-worker/internal/logging"
	"github.com/LastBotInc/coralie-livekit-worker/internal/transcribe"
)

// IngressTrack handles forwarding audio from a LiveKit participant to Coralie conference.
// It decodes Opus packets, resamples from 48kHz to 24kHz, and sends PCM frames to the conference.
type IngressTrack struct {
	participantID string
	participant   *workersdk.Participant
	client        *workersdk.Client
	decoder       *opus.Decoder
	resampler     *soxr.Resampler
	resamplerBuf  *bytes.Buffer
	resamplerMu   sync.Mutex
	// Preallocated buffers to avoid per-call allocations
	inputBytesBuf    []byte  // Reused for input byte conversion
	outputSamplesBuf []int16 // Reused for output samples (may grow)
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	// Remaining samples buffer (for partial frames after chunking)
	remainingSamples []int16
	remainingMu      sync.Mutex
	// Transcription tap (optional)
	tap transcribe.Tap
	// Logging flags to avoid spam
	firstRTPLogged  bool
	firstSendLogged bool
}

// NewIngressTrack creates a new ingress track handler.
func NewIngressTrack(
	participantID string,
	participant *workersdk.Participant,
	client *workersdk.Client,
	tap transcribe.Tap,
) (*IngressTrack, error) {
	// Create Opus decoder for 48kHz mono
	decoder, err := opus.NewDecoder(48000, 1)
	if err != nil {
		return nil, fmt.Errorf("create opus decoder: %w", err)
	}

	// Create resampler for 48kHz -> 24kHz
	// IMPORTANT: Store *bytes.Buffer so resampler writes to the same buffer we read from
	resamplerBuf := &bytes.Buffer{}
	resampler, err := soxr.New(resamplerBuf, 48000.0, 24000.0, 1, soxr.I16, soxr.HighQ)
	if err != nil {
		decoder = nil // Cleanup
		return nil, fmt.Errorf("create resampler: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &IngressTrack{
		participantID:    participantID,
		participant:      participant,
		client:           client,
		decoder:          decoder,
		resampler:        resampler,
		resamplerBuf:     resamplerBuf,
		ctx:              ctx,
		cancel:           cancel,
		remainingSamples: make([]int16, 0, 480), // Pre-allocate for one frame
		tap:              tap,
		inputBytesBuf:    make([]byte, 0, 1920), // Pre-allocate for 960 samples * 2 bytes
		outputSamplesBuf: make([]int16, 0, 480), // Pre-allocate for 480 samples
	}, nil
}

// Start starts processing the track by reading RTP packets and decoding Opus.
func (t *IngressTrack) Start(track *webrtc.TrackRemote) {
	t.wg.Add(1)
	go t.processTrack(track)
	logging.Info(logging.CategoryBridge, "started ingress track processing participantID=%s", t.participantID)
}

// Stop stops the track handler.
func (t *IngressTrack) Stop() {
	t.cancel()
	t.wg.Wait()
	if t.resampler != nil {
		t.resampler.Close()
	}
}

// processTrack processes RTP packets from the track.
func (t *IngressTrack) processTrack(track *webrtc.TrackRemote) {
	defer t.wg.Done()

	buf := make([]byte, 1500)
	rtpPacket := &rtp.Packet{}
	pcmFrame48k := make([]int16, 960) // 960 samples = 20ms @ 48kHz

	for {
		select {
		case <-t.ctx.Done():
			return
		default:
			// Read RTP packet
			n, _, err := track.Read(buf)
			if err != nil {
				if t.ctx.Err() == nil {
					logging.Warning(logging.CategoryBridge, "failed to read RTP packet participantID=%s: %v", t.participantID, err)
				}
				return
			}

			// Log first RTP packet only once
			if !t.firstRTPLogged {
				t.firstRTPLogged = true
				logging.Info(logging.CategoryBridge, "received first RTP packet participantID=%s size=%d", t.participantID, n)
			}

			// Unmarshal RTP packet
			if err := rtpPacket.Unmarshal(buf[:n]); err != nil {
				logging.Warning(logging.CategoryBridge, "failed to unmarshal RTP packet participantID=%s: %v", t.participantID, err)
				continue
			}

			// Extract Opus payload
			opusPayload := rtpPacket.Payload
			if len(opusPayload) == 0 {
				continue // DTX packet
			}

			// Decode Opus to PCM (48kHz)
			sampleCount, err := t.decoder.Decode(opusPayload, pcmFrame48k)
			if err != nil {
				if err.Error() == "opus: no data supplied" {
					continue // DTX packet
				}
				logging.Warning(logging.CategoryBridge, "failed to decode Opus participantID=%s: %v", t.participantID, err)
				continue
			}

			if sampleCount == 0 {
				continue
			}

			// Resample from 48kHz to 24kHz
			decodedSamples48k := pcmFrame48k[:sampleCount]
			resampled24k, err := t.resample48kTo24k(decodedSamples48k)
			if err != nil {
				logging.Warning(logging.CategoryBridge, "failed to resample participantID=%s: %v", t.participantID, err)
				continue
			}

			if len(resampled24k) == 0 {
				// Resampler might be buffering - this is normal, continue
				continue
			}

			// Combine with any remaining samples from previous chunk
			t.remainingMu.Lock()
			combinedSamples := append(t.remainingSamples, resampled24k...)
			t.remainingSamples = t.remainingSamples[:0] // Clear but keep capacity
			t.remainingMu.Unlock()

			// Break into 480-sample chunks and send immediately
			// This removes buffering delay and aligns frame IDs closer to arrival time
			for len(combinedSamples) >= 480 {
				chunk := combinedSamples[:480]
				combinedSamples = combinedSamples[480:]

				// Tap for transcription (before sending)
				if t.tap != nil {
					t.tap.OnFrame(t.participantID, chunk)
				}

				// Send immediately - no ticker delay
				// ErrNotSynced is expected until Sync arrives and should be ignored
				if err := t.client.SendPCMFrameWithTick(t.participantID, chunk, true); err != nil {
					if err == workersdk.ErrNotSynced {
						// Sync not ready yet - continue, frames will start succeeding once Sync arrives
						// Don't log to avoid spam
						continue
					}
					logging.Warning(logging.CategoryBridge, "failed to send PCM frame participantID=%s: %v", t.participantID, err)
				} else {
					// Frame sent successfully
					if !t.firstSendLogged {
						t.firstSendLogged = true
						logging.Info(logging.CategoryBridge, "sent first PCM frame to conference participantID=%s", t.participantID)
					}
				}
			}

			// Store remainder for next iteration
			if len(combinedSamples) > 0 {
				t.remainingMu.Lock()
				t.remainingSamples = append(t.remainingSamples, combinedSamples...)
				// Limit buffer size (max 1 frame = 480 samples) to avoid excessive buffering
				if len(t.remainingSamples) > 480 {
					// Drop oldest samples if we exceed one frame
					dropCount := len(t.remainingSamples) - 480
					copy(t.remainingSamples, t.remainingSamples[dropCount:])
					t.remainingSamples = t.remainingSamples[:480]
				}
				t.remainingMu.Unlock()
			}
		}
	}
}

// resample48kTo24k resamples PCM from 48kHz to 24kHz.
func (t *IngressTrack) resample48kTo24k(samples48k []int16) ([]int16, error) {
	if len(samples48k) == 0 {
		return nil, nil
	}

	t.resamplerMu.Lock()
	defer t.resamplerMu.Unlock()

	// Reuse preallocated buffer for input bytes (grow if needed)
	inputSize := len(samples48k) * 2
	if cap(t.inputBytesBuf) < inputSize {
		t.inputBytesBuf = make([]byte, inputSize)
	}
	inputBytes := t.inputBytesBuf[:inputSize]
	for i, sample := range samples48k {
		binary.LittleEndian.PutUint16(inputBytes[i*2:], uint16(sample))
	}

	// Reset buffer and write input
	t.resamplerBuf.Reset()
	if _, err := t.resampler.Write(inputBytes); err != nil {
		return nil, fmt.Errorf("resampler write: %w", err)
	}

	// Read resampled output from buffer
	outputBytes := t.resamplerBuf.Bytes()
	if len(outputBytes) == 0 {
		return nil, nil
	}

	// Reuse preallocated buffer for output samples (grow if needed)
	outputSize := len(outputBytes) / 2
	if cap(t.outputSamplesBuf) < outputSize {
		t.outputSamplesBuf = make([]int16, outputSize)
	}
	outputSamples := t.outputSamplesBuf[:outputSize]
	for i := 0; i < outputSize; i++ {
		outputSamples[i] = int16(binary.LittleEndian.Uint16(outputBytes[i*2:]))
	}

	// Return a copy since the buffer is reused
	result := make([]int16, outputSize)
	copy(result, outputSamples)
	return result, nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || strings.Contains(s, substr))
}
