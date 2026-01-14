# Build stage
FROM golang:1.25-alpine AS builder

# Install build dependencies for CGO
RUN apk add --no-cache gcc musl-dev pkgconfig soxr-dev opus-dev opusfile-dev

WORKDIR /build

# Copy dependencies (for replace directives)
COPY coralie-logging-go /coralie-logging-go
COPY coralie-conference-service /coralie-conference-service

# Patch TIOCGETA to use TCGETS for Linux compatibility
RUN sed -i 's/unix.TIOCGETA/unix.TCGETS/g' /coralie-logging-go/internal/term/tty.go || true

# Copy go mod files
COPY coralie-livekit-worker/go.mod coralie-livekit-worker/go.sum ./

# Fix replace directives to use container paths (before downloading)
RUN sed -i 's|../coralie-conference-service|/coralie-conference-service|g' go.mod || true

# Copy source code
COPY coralie-livekit-worker .

# Fix replace directives again in case they were overwritten
RUN sed -i 's|../coralie-conference-service|/coralie-conference-service|g' go.mod || true

# Download all dependencies
RUN go mod download

# Build
RUN CGO_ENABLED=1 GOOS=linux go build -a -o coralie-livekit-worker ./cmd/coralie-livekit-worker

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata opus opusfile soxr

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/coralie-livekit-worker .

ENTRYPOINT ["./coralie-livekit-worker"]
