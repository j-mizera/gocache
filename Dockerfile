# syntax=docker/dockerfile:1.6

# =============================================================================
# gocache Dockerfile
#
# Linux-only container build. The set of embedded plugins compiled into the
# binary is controlled by the PLUGINS build-arg — a comma-separated list of
# tag names. Each tag corresponds to one embedded plugin package under
# plugins/ (see plugins/*/doc.go).
#
# Examples:
#   docker build -t gocache:minimal .                              # no embedded
#   docker build --build-arg PLUGINS=crashdump -t gocache:default .
#   docker build --build-arg PLUGINS=crashdump,otlp -t gocache:full .
#
# Runtime config is still env-var-based (GOCACHE_*) — see the README /
# gocache.yaml for the full set. Embedded plugins self-disable when their
# required env var is absent (e.g. GOCACHE_EMBEDDED_OTLP_ENDPOINT unset →
# OTLP compiled in but dormant), so a "full" image can be deployed without
# any observability backend and will still serve cache traffic.
# =============================================================================

# ----- builder -------------------------------------------------------------
FROM golang:1.25.5-alpine AS builder

# Tools only needed during build. git is required for go modules that
# resolve commit hashes; ca-certs for any https fetches during go mod.
RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Module cache layer — only re-runs when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Full source.
COPY . .

# Build-arg controls the set of embedded plugins via go build tags.
# Empty → zero embedded plugins (minimal variant).
ARG PLUGINS=""
ARG VERSION="dev"

# Convert the comma-separated PLUGINS list to a space-separated -tags value.
# -trimpath strips host filesystem paths from the binary.
# -ldflags "-s -w" removes symbol + DWARF tables (~20% binary size cut).
RUN TAGS=$(echo "$PLUGINS" | tr ',' ' ') && \
    echo "Building with tags: [$TAGS]" && \
    CGO_ENABLED=0 GOOS=linux go build \
        -tags "$TAGS" \
        -trimpath \
        -ldflags="-s -w -X gocache/pkg/version.version=$VERSION" \
        -o /build/bin/gocache \
        ./cmd/server

# ----- runtime -------------------------------------------------------------
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S gocache && adduser -S -G gocache gocache

# Datadir — where snapshots, crashes/, and boot.state live by default.
# Named volume recommended in compose/helm.
WORKDIR /var/lib/gocache
RUN chown -R gocache:gocache /var/lib/gocache

COPY --from=builder /build/bin/gocache /usr/local/bin/gocache

USER gocache

# RESP default port.
EXPOSE 6379

# Crash survivability paths relative to the datadir.
ENV GOCACHE_CRASHDUMP_DIR=/var/lib/gocache/crashes \
    GOCACHE_BOOT_STATE_FILE=/var/lib/gocache/boot.state

ENTRYPOINT ["/usr/local/bin/gocache"]
