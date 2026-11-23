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
# Debian rather than Alpine, for three reasons:
#   1. proot — the only isolation tier that works when a sandbox drops every
#      capability and denies chroot(2) + clone(CLONE_NEWUSER) — is packaged by
#      Debian and not available for Alpine;
#   2. prefix mode can exec glibc images directly (the host's ELF interpreter
#      exists), and the musl package keeps /lib/ld-musl-*.so.1 for Alpine images;
#   3. proot itself is only well-tested against a glibc host.
# NOTE: runs as root intentionally — chroot(2) for app isolation requires
# CAP_SYS_CHROOT. Where chroot is denied the runtime demotes to proot or
# rootfs-prefix mode automatically.
FROM debian:bookworm-slim

# ca-certificates: for HTTPS image pulls from registries.
# tzdata: for correct time zone handling in logs.
# proot: unprivileged path-translating isolation tier (see internal/runtime).
# musl: /lib/ld-musl-*.so.1 for musl-based app images in prefix mode.
# tini: PID 1. Apps spawn grandchildren (proot tracees, npm -> node, backgrounded
#       servers) that get reparented to init when their leader exits. As PID 1
#       pocketpaas would never reap them — tini does, and forwards signals.
RUN apt-get update \
 && apt-get install -y --no-install-recommends proot musl ca-certificates tzdata tini \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /build/pocketpaas .

# DATA_DIR is where the SQLite database and extracted rootfs live.
# Mount a persistent volume here to survive container restarts.
# Ownership 1000 makes `--user 1000` work (and named volumes mounted here
# inherit the image directory's ownership); world-writable is also needed for
# the sandbox case where the process is uid 0 but has no capabilities at all —
# without CAP_DAC_OVERRIDE, root cannot write a directory owned by someone else.
RUN mkdir -p /data && chown 1000:1000 /data && chmod 0777 /data
VOLUME ["/data"]
ENV DATA_DIR=/data
ENV PORT=8080

EXPOSE 8080

# Liveness probe — the binary probes its own login page, so the image needs no
# curl/wget (see cmd/server/main.go --healthcheck).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["/app/pocketpaas", "--healthcheck"]

# tini runs as PID 1 so orphaned app grandchildren are reaped; pocketpaas is its
# child and handles SIGTERM/SIGINT itself (tini forwards them). -s registers
# tini as a child subreaper too, so reaping still works on platforms that wrap
# the entrypoint and therefore do not give tini PID 1 (it warns otherwise).
ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/app/pocketpaas"]
