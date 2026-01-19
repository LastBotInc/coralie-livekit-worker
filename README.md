# Coralie LiveKit Worker

A worker service that bridges audio between LiveKit rooms and Coralie conferences bidirectionally.

## Overview

This service:
1. Registers to `coralie-conference-service` as a Worker (via `coralie-conference-service/pkg/workersdk`)
2. Registers to LiveKit as an Agent Worker and listens for agent dispatch/job requests
3. For each dispatch/job:
   - Connects to the specified LiveKit room
   - Creates (or joins if exists) a Coralie conference with the SAME name as the LiveKit room (`roomName == conferenceName`)
   - Bridges audio BOTH ways:
     - **Ingress**: LiveKit room participants audio → decode Opus → resample to 24kHz → feed into Coralie conference via per-participant conference participants
     - **Egress**: Coralie conference mixed audio → resample to 48kHz → encode Opus → publish to LiveKit as the worker's outgoing audio track
4. Obeys Coralie opcodes:
   - When receiving participant control opcode that implies leaving/disconnect (e.g. `leave_conference` or `disconnect` targeting this worker/participant), it IMMEDIATELY disconnects from the LiveKit room and tears down the bridge for that job
5. Provides a clean hook for transcription:
   - Per-LiveKit-participant PCM (24kHz int16 frames) can be tapped BEFORE mixing via `internal/transcribe/tap.go`

## Architecture

```
LiveKit Room                    Coralie Conference
┌─────────────┐                ┌─────────────────┐
│ Participant │───Opus───────▶│  Ingress Track   │───24kHz PCM──▶│ Participant │
│   (48kHz)   │                │  (48k→24k)      │                │  (24kHz)   │
└─────────────┘                └─────────────────┘                └────────────┘
       ▲                                                                    │
       │                                                                    │
       │ Opus (48kHz)                                                      │ Mixed Audio
       │                                                                    │ (24kHz)
       │                                                                    ▼
┌─────────────┐                ┌─────────────────┐                ┌─────────────┐
│   Worker    │◀───Opus────────│  Egress Track   │◀───24kHz PCM───│  Egress     │
│  (48kHz)    │                │  (24k→48k)      │                │ Participant │
└─────────────┘                └─────────────────┘                └─────────────┘
```

## Environment Variables

### Required

- `LIVEKIT_URL`: LiveKit server URL (e.g., `ws://localhost:7880`)
- `LIVEKIT_API_KEY`: LiveKit API key
- `LIVEKIT_API_SECRET`: LiveKit API secret
- `CONFERENCE_GRPC`: Coralie conference service gRPC address (e.g., `localhost:9090`)

### Optional

- `CONFERENCE_WORKER_ID`: Worker ID for conference service registration (default: `coralie-livekit-worker-1`)
- `LK_AGENT_NAME`: Agent name for explicit dispatch mode
- `LK_NAMESPACE`: Namespace for worker registration
- `LK_JOB_TYPE`: Job type (`JT_ROOM` or `JT_PUBLISHER`, default: `JT_ROOM`)
- `LK_DRAIN_TIMEOUT`: Timeout for graceful shutdown (default: `30s`)
- `LK_MAX_CONCURRENT_JOBS`: Maximum concurrent jobs (default: `8`)
- `LK_LOG_LEVEL`: Log level (default: `info`)
- `LK_PPROF_ADDR`: pprof HTTP server address (e.g., `127.0.0.1:6060`)
- `LK_JOB_TIMEOUT`: Max runtime per job (default: `5m`)
- `AGGRESSIVE_LOGGING`: Enable aggressive logging (default: `false`)

Flags override `.env`/environment values; run `./bin/coralie-livekit-worker -h` to see flag names.

## Running Locally

1. Copy `env.example` to `.env` and fill in your values:
   ```bash
   cp env.example .env
   ```

2. Run the worker:
   ```bash
   go run ./cmd/coralie-livekit-worker
   ```

   Or using Make:
   ```bash
   make run
   ```

## Building

```bash
make build
```

The binary will be in `bin/coralie-livekit-worker`.

## Audio Formats

- **Coralie side**: 24kHz, 20ms frames, int16 little-endian samples (480 samples/frame)
- **LiveKit side**: Opus typically 48kHz mono
  - Decode Opus packets to 48kHz PCM16 mono
  - Resample 48k → 24k for Coralie ingress
  - Resample 24k → 48k for LiveKit egress
  - Encode to Opus and publish

## Performance

- **No per-frame allocations in hot paths**: Preallocated int16 buffers per track (ingress) and for egress pipeline
- **Bounded goroutine count per job**: One goroutine per ingress track + one egress loop
- **Context cancellation everywhere**: Shutdown is fast
- **pprof toggle via env var**: Set `LK_PPROF_ADDR` to enable profiling

## Profiling

### CPU Profiling

1. Start the worker with pprof enabled:
   ```bash
   LK_PPROF_ADDR=127.0.0.1:6060 go run ./cmd/coralie-livekit-worker
   ```

2. Collect CPU profile:
   ```bash
   go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
   ```

### Heap Profiling

1. Start the worker with pprof enabled (same as above)

2. Collect heap profile:
   ```bash
   go tool pprof http://localhost:6060/debug/pprof/heap
   ```

### Benchmarks

Run benchmarks:
```bash
make test-bench
```

Or directly:
```bash
go test -run Test... -bench . ./...
```

## Opcode Compliance

The worker implements the following opcodes:

- `join_conference`: For this worker/job, connect to LiveKit room (if not already) and ensure Coralie conference/participants exist
- `leave_conference` / `disconnect`:
  - If targets match this worker/job (by `conferenceID`/`participantID`/`targetWorkerId`):
    - Cancel job context immediately
    - Stop all tracks
    - Unpublish audio
    - Disconnect LiveKit room
    - Disconnect Coralie participants
- For all other opcodes: return "not implemented" in control response, but DO NOT crash

## Extending for Transcription

The service provides a clean hook to tap per-LiveKit-participant PCM (24kHz int16 frames) BEFORE mixing.

### Interface

See `internal/transcribe/tap.go`:

```go
type Tap interface {
    OnFrame(participantID string, frame []int16)
}
```

### Implementation

1. Implement the `Tap` interface:
   ```go
   type MySTTTap struct {
       // Your STT client
   }

   func (t *MySTTTap) OnFrame(participantID string, frame []int16) {
       // Process frame (480 samples at 24kHz)
       // Send to STT service
   }
   ```

2. Pass your tap to the bridge:
   ```go
   tap := &MySTTTap{}
   bridge := bridge.NewBridge(room, confClient, roomName, tap)
   ```

3. The bridge will call `OnFrame` for each 20ms frame from each participant before sending to the conference mixer.

### Example

See `internal/transcribe/tap.go` for the no-op implementation that can be used as a template.

## Logging

All logging uses `coralie-logging-go` (same convention as other Coralie services). No direct `log.Printf`. Use structured logging via the `internal/logging` package.

## Testing

Run unit tests:
```bash
make test
```

No automated tests are currently checked in; `make test` will run `go test ./...` once tests are added.

## Docker

Build Docker image:
```bash
docker build -t coralie-livekit-worker .
```

Run:
```bash
docker run --env-file .env coralie-livekit-worker
```

## Project Layout

```
coralie-livekit-worker/
├── cmd/
│   └── coralie-livekit-worker/
│       └── main.go
├── internal/
│   ├── bridge/
│   │   ├── bridge.go          # Main bridge logic
│   │   ├── ingress_track.go   # LiveKit → Coralie (Opus decode, 48k→24k)
│   │   └── egress.go          # Coralie → LiveKit (24k→48k, Opus encode)
│   ├── config/
│   │   └── config.go          # Environment parsing
│   ├── job/
│   │   └── job.go             # One job = one room bridge instance
│   ├── logging/
│   │   └── logging.go         # Logging wrapper
│   ├── transcribe/
│   │   └── tap.go             # Transcription hook interface
│   ├── version/
│   │   └── version.go         # Version info
│   └── worker/
│       └── worker.go           # LiveKit agent worker registration + job dispatch
├── docs/                       # Optional: architecture + opcodes compliance
├── Dockerfile
├── Makefile
├── README.md
└── env.example
```

## License

License not specified.
