package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"coralie-conference-service/pkg/workersdk"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	soxr "github.com/zaf/resample"

	"github.com/LastBotInc/coralie-livekit-worker/internal/logging"
)

// Egress handles forwarding audio from Coralie conference to LiveKit.
// It reads mixed audio from the egress participant, resamples from 24kHz to 48kHz,
// and publishes to LiveKit as PCM.
type Egress struct {
	room          *lksdk.Room
	participant   *workersdk.Participant
	egressTrack   *lkmedia.PCMLocalTrack
	egressPub     *lksdk.LocalTrackPublication
	egressMu      sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	resampler     *soxr.Resampler
	resamplerBuf  *bytes.Buffer
	resamplerMu   sync.Mutex
	// Preallocated buffers to avoid per-call allocations
	inputBytesBuf   []byte   // Reused for input byte conversion
	outputSamplesBuf []int16 // Reused for output samples (may grow)
	// Logging flag to avoid spam
	firstEgressLogged bool
}

// NewEgress creates a new egress handler.
func NewEgress(
	room *lksdk.Room,
	participant *workersdk.Participant,
) (*Egress, error) {
	// Create PCMLocalTrack for egress (48kHz, mono)
	track, err := lkmedia.NewPCMLocalTrack(48000, 1, nil)
	if err != nil {
		return nil, fmt.Errorf("create PCM track: %w", err)
	}

	// Publish track
	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   "conference-audio",
		Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		track.Close()
		return nil, fmt.Errorf("publish track: %w", err)
	}
	
	// Ensure track is unmuted
	pub.SetMuted(false)
	logging.Info(logging.CategoryBridge, "published egress track muted=%v", pub.IsMuted())

	// Create resampler for 24kHz -> 48kHz
	// IMPORTANT: Store *bytes.Buffer so resampler writes to the same buffer we read from
	resamplerBuf := &bytes.Buffer{}
	resampler, err := soxr.New(resamplerBuf, 24000.0, 48000.0, 1, soxr.I16, soxr.HighQ)
	if err != nil {
		track.Close()
		return nil, fmt.Errorf("create resampler: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
		return &Egress{
		room:            room,
		participant:     participant,
		egressTrack:     track,
		egressPub:       pub,
		ctx:             ctx,
		cancel:          cancel,
		resampler:       resampler,
		resamplerBuf:    resamplerBuf,
		inputBytesBuf:   make([]byte, 0, 960),   // Pre-allocate for 480 samples * 2 bytes
		outputSamplesBuf: make([]int16, 0, 960), // Pre-allocate for 960 samples
	}, nil
}

// Start starts the egress processing loop.
func (e *Egress) Start() {
	e.wg.Add(1)
	go e.processEgress()
	logging.Info(logging.CategoryBridge, "started egress processing")
}

// Stop stops the egress handler.
func (e *Egress) Stop() {
	e.cancel()

	// Stop egress track first to prevent further writes
	e.egressMu.Lock()
	if e.egressTrack != nil {
		e.egressTrack.Close()
		e.egressTrack = nil
	}
	e.egressPub = nil
	e.egressMu.Unlock()

	// Close resampler
	e.resamplerMu.Lock()
	if e.resampler != nil {
		e.resampler.Close()
	}
	e.resamplerMu.Unlock()

	e.wg.Wait()
	logging.Info(logging.CategoryBridge, "stopped egress processing")
}

// processEgress processes egress audio from conference.
// The conference service mixes all participants and sends mixed audio to the egress participant's EgressChan.
func (e *Egress) processEgress() {
	defer e.wg.Done()

	// Buffer for resampling
	frame24k := make([]int16, 480) // 480 samples = 20ms @ 24kHz
	hold := make([]int16, 0, 960)  // Buffer for partial frames

	for {
		select {
		case <-e.ctx.Done():
			return
		case frame, ok := <-e.participant.EgressChan:
			if !ok {
				logging.Info(logging.CategoryBridge, "egress channel closed")
				return
			}
			
			// Don't log every frame - too spammy

			// Conference sends 24kHz frames (480 samples)
			if len(frame) != 480 {
				logging.Warning(logging.CategoryBridge, "invalid frame size expected=%d got=%d", 480, len(frame))
				continue
			}

			copy(frame24k, frame)

			// Resample from 24kHz to 48kHz
			resampled48k, err := e.resample24kTo48k(frame24k)
			if err != nil {
				logging.Error(logging.CategoryBridge, "failed to resample: %v", err)
				continue
			}

			if len(resampled48k) == 0 {
				logging.Warning(logging.CategoryBridge, "resampler produced no output (buffering?) input=%d", len(frame24k))
				continue
			}
			
			// Expected: 480 samples @ 24kHz -> 960 samples @ 48kHz
			if len(resampled48k) != 960 {
				logging.Warning(logging.CategoryBridge, "unexpected resampler output size expected=960 got=%d", len(resampled48k))
			}

			// Buffer and write 960-sample frames to track
			hold = append(hold, resampled48k...)
			for len(hold) >= 960 {
				frame := hold[:960]
				e.egressMu.Lock()
				if e.egressTrack != nil {
					if err := e.egressTrack.WriteSample(frame); err != nil {
						// If track is closed, exit the loop
						errStr := err.Error()
						if strings.Contains(errStr, "track is closed") || strings.Contains(errStr, "Track is closed") || strings.Contains(errStr, "closed") {
							logging.Info(logging.CategoryBridge, "egress track closed, stopping egress loop: %v", err)
							e.egressMu.Unlock()
							return
						}
						logging.Error(logging.CategoryBridge, "failed to write sample: %v", err)
					} else {
						// Log first successful write
						if !e.firstEgressLogged {
							e.firstEgressLogged = true
							logging.Info(logging.CategoryBridge, "wrote first egress sample to LiveKit size=%d", len(frame))
						}
					}
				} else {
					// Track was removed, exit
					logging.Info(logging.CategoryBridge, "egress track removed, stopping egress loop")
					e.egressMu.Unlock()
					return
				}
				e.egressMu.Unlock()
				hold = hold[960:]
			}
		}
	}
}

// resample24kTo48k resamples PCM from 24kHz to 48kHz.
func (e *Egress) resample24kTo48k(samples24k []int16) ([]int16, error) {
	if len(samples24k) == 0 {
		return nil, nil
	}

	e.resamplerMu.Lock()
	defer e.resamplerMu.Unlock()

	// Reuse preallocated buffer for input bytes (grow if needed)
	inputSize := len(samples24k) * 2
	if cap(e.inputBytesBuf) < inputSize {
		e.inputBytesBuf = make([]byte, inputSize)
	}
	inputBytes := e.inputBytesBuf[:inputSize]
	for i, sample := range samples24k {
		binary.LittleEndian.PutUint16(inputBytes[i*2:], uint16(sample))
	}

	// Reset buffer and write input
	e.resamplerBuf.Reset()
	if _, err := e.resampler.Write(inputBytes); err != nil {
		return nil, fmt.Errorf("resampler write: %w", err)
	}

	// Read resampled output from buffer
	outputBytes := e.resamplerBuf.Bytes()
	if len(outputBytes) == 0 {
		return nil, nil
	}

	// Reuse preallocated buffer for output samples (grow if needed)
	outputSize := len(outputBytes) / 2
	if cap(e.outputSamplesBuf) < outputSize {
		e.outputSamplesBuf = make([]int16, outputSize)
	}
	outputSamples := e.outputSamplesBuf[:outputSize]
	for i := 0; i < outputSize; i++ {
		outputSamples[i] = int16(binary.LittleEndian.Uint16(outputBytes[i*2:]))
	}

	// Return a copy since the buffer is reused
	result := make([]int16, outputSize)
	copy(result, outputSamples)
	return result, nil
}
