# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=1 go build -o coralie-livekit-worker ./cmd/coralie-livekit-worker

# Runtime stage
FROM alpine:latest

# Install runtime dependencies (libsoxr, etc.)
RUN apk add --no-cache \
    libsoxr \
    ca-certificates

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/coralie-livekit-worker .

# Run as non-root user
RUN addgroup -g 1000 appuser && \
    adduser -D -u 1000 -G appuser appuser && \
    chown -R appuser:appuser /app

USER appuser

ENTRYPOINT ["./coralie-livekit-worker"]
