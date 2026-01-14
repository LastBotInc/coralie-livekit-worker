// Package transcribe provides interfaces and hooks for transcription.
// This package provides a clean hook point to tap per-participant PCM audio
// before mixing, enabling easy integration of STT providers.
package transcribe

// Tap is the interface for tapping audio frames for transcription.
// Implementations should handle per-participant PCM frames (24kHz, int16, 480 samples per frame).
type Tap interface {
	// OnFrame is called for each audio frame from a participant.
	// participantID: the LiveKit participant identity (e.g., "lk:user-123")
	// frame: 480 samples of int16 PCM at 24kHz (20ms frame)
	OnFrame(participantID string, frame []int16)
}

// NoopTap is a no-op implementation that does nothing.
type NoopTap struct{}

// OnFrame implements Tap interface (no-op).
func (n *NoopTap) OnFrame(participantID string, frame []int16) {
	// No-op: do nothing
}

// NewNoopTap creates a new no-op tap.
func NewNoopTap() Tap {
	return &NoopTap{}
}
