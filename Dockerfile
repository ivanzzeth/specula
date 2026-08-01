# Specula container image — multi-stage, cross-compiled.
# Version identity matches release binaries (internal/version via -ldflags).
#
# The builder stages are pinned to $BUILDPLATFORM and Go cross-compiles to
# $TARGETARCH. Without that pin, `buildx --platform linux/amd64,linux/arm64` runs
# the whole node + Go toolchain under QEMU emulation for the non-native target —
# `npm ci` alone made the release image job take ~15 minutes. The WebUI bundle is
# architecture-independent, so this also builds it exactly once for all targets.

# ── WebUI (native, arch-independent output) ────────────────────────────────────
FROM --platform=$BUILDPLATFORM node:20-bookworm AS web
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web/ ./web/
RUN cd web && npm run build

# ── Go binary (native toolchain, cross-compiled output) ───────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# Provided by buildx for each --platform entry.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ENV CGO_ENABLED=0
RUN mkdir -p /out/var/lib/specula/blobs /out/var/lib/specula/quarantine \
 && GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
      -ldflags "-s -w \
        -X github.com/ivanzzeth/specula/internal/version.Version=${VERSION} \
        -X github.com/ivanzzeth/specula/internal/version.Commit=${COMMIT} \
        -X github.com/ivanzzeth/specula/internal/version.BuildDate=${DATE}" \
      -o /out/specula ./cmd/specula

# ── Runtime ───────────────────────────────────────────────────────────────────
# NOT distroless/static: the git protocol shells out to the real `git` binary at
# runtime (internal/handler/git serveMirror → `git clone --bare`) to keep
# node-local bare mirrors of public repos. A static-distroless image ships no git,
# so `exec: "git": executable file not found in $PATH` makes every git request
# silently degrade to live passthrough with zero caching — which in CN means every
# clone hits the upstream directly (slow/throttled), defeating the mirror. Use
# debian-slim so we ship a real git + CA bundle while staying small and nonroot
# (uid/gid 65532, matching the distroless `nonroot` identity the chart expects).
FROM debian:12-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 65532 nonroot \
 && useradd -u 65532 -g 65532 -m -d /home/nonroot -s /usr/sbin/nologin nonroot
ENV HOME=/home/nonroot
COPY --from=build /out/specula /specula
COPY --from=build --chown=65532:65532 /out/var/lib/specula /var/lib/specula
COPY contrib/docker/specula.yaml /etc/specula/specula.yaml
EXPOSE 7733
VOLUME ["/var/lib/specula"]
USER nonroot:nonroot
ENTRYPOINT ["/specula"]
CMD ["--config", "/etc/specula/specula.yaml"]
