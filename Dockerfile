# ─── Stage 1: Build ───────────────────────────────────────────────────────────
#
# Cross-compile for the target architecture (default: arm64 for Raspberry Pi 4).
# Set --build-arg TARGETARCH=amd64 for x86-64 mini PCs.
FROM golang:1.21-alpine AS builder

# git is needed by `go mod download` for VCS-hosted modules (periph.io).
RUN apk add --no-cache git

WORKDIR /app

# Resolve dependencies first — this layer is cached as long as go.mod/go.sum
# don't change, making rebuilds after source edits very fast.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy source and compile.
# CGO_ENABLED=0 → fully static binary; no glibc dependency in the final image.
# -ldflags="-s -w" → strip debug info (~30 % smaller binary).
# -trimpath → remove local build paths from stack traces.
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=arm64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
        -ldflags="-s -w" \
        -trimpath \
        -o /replay-agent \
        .

# ─── Stage 2: Runtime ─────────────────────────────────────────────────────────
#
# debian:bookworm-slim is chosen over Alpine because:
#  • FFmpeg's Debian package pulls in all required codecs (libx264, aac, …)
#    without manual flag tuning.
#  • Raspberry Pi OS is Debian-based → same libc ABI for any future CGO needs.
FROM debian:bookworm-slim

LABEL org.opencontainers.image.description="Replay Agent – amateur football edge recorder"

# Install FFmpeg and TLS root certificates (needed for Telegram HTTPS uploads).
# --no-install-recommends keeps the image lean.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# ── Directories ───────────────────────────────────────────────────────────────
# /tmp/replay_buffer  – rolling .ts segment ring buffer
# /tmp/replays        – finished .mp4 clips waiting for upload
# /etc/replay         – configuration files (agenda.json mounted here)
RUN mkdir -p /tmp/replay_buffer /tmp/replays /etc/replay

# ── Binary ────────────────────────────────────────────────────────────────────
COPY --from=builder /replay-agent /usr/local/bin/replay-agent

# ── Healthcheck ───────────────────────────────────────────────────────────────
# Verifies the process is still alive. Docker/Compose will restart the
# container if this fails 3 times in a row.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD pgrep -x replay-agent > /dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/replay-agent"]
