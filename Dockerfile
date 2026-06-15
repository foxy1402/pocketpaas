# ── Build stage ──────────────────────────────────────────────────────────────
# Always run on the host (builder) platform.
# TARGETOS / TARGETARCH are injected by Buildx for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

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
    -o apphive \
    ./cmd/server

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates: for HTTPS image pulls from registries.
# tzdata: for correct time zone handling in logs.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /build/apphive .

# DATA_DIR is where the SQLite database and extracted rootfs live.
# Mount a persistent volume here to survive container restarts.
VOLUME ["/data"]
ENV DATA_DIR=/data
ENV PORT=8080

EXPOSE 8080

ENTRYPOINT ["./apphive"]
