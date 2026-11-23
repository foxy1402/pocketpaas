# ── Build stage ──────────────────────────────────────────────────────────────
# Always run on the host (builder) platform.
# TARGETOS / TARGETARCH are injected by Buildx for the target platform.
# NOTE: for release builds, pin base images by digest (e.g.
# golang:1.25.7-alpine@sha256:... and alpine:3.20@sha256:...) so builds are
# reproducible and immune to upstream tag mutation.
FROM --platform=$BUILDPLATFORM golang:1.25.7-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG BUILDTIME
ARG REVISION

WORKDIR /build

# Download dependencies before copying source so this layer is cached.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Cross-compile a fully static binary for the target platform.
RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build \
    -ldflags="-s -w -X main.buildTime=${BUILDTIME} -X main.revision=${REVISION}" \
    -o pocketpaas \
    ./cmd/server

# ── Runtime stage ─────────────────────────────────────────────────────────────
# NOTE: runs as root intentionally — chroot(2) for app isolation requires
# CAP_SYS_CHROOT which is root-only. When chroot is unavailable the runtime
# falls back to rootfs-prefix mode automatically.
FROM alpine:3.20

# ca-certificates: for HTTPS image pulls from registries.
# tzdata: for correct time zone handling in logs.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /build/pocketpaas .

# DATA_DIR is where the SQLite database and extracted rootfs live.
# Mount a persistent volume here to survive container restarts.
# Pre-create it owned by an unprivileged uid so `--user 1000` runs (and named
# volumes mounted here, which inherit the image directory's ownership) work
# without extra setup. When the runtime is still not writable — e.g. a
# pre-existing root-owned volume — pocketpaas falls back to a writable path.
RUN mkdir -p /data && chown 1000:1000 /data
VOLUME ["/data"]
ENV DATA_DIR=/data
ENV PORT=8080

EXPOSE 8080

# Liveness probe — hit the login page on PORT (app default: 8080, see cmd/server/main.go).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -q --spider "http://127.0.0.1:${PORT:-8080}/login" || exit 1

ENTRYPOINT ["./pocketpaas"]
