# Specula container image — multi-stage build.
# Version identity matches release binaries (internal/version via -ldflags).

# ── WebUI ─────────────────────────────────────────────────────────────────────
FROM node:20-bookworm AS web
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web/ ./web/
RUN cd web && npm run build

# ── Go binary ─────────────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ENV CGO_ENABLED=0
RUN mkdir -p /out/var/lib/specula/blobs \
 && go build -trimpath \
      -ldflags "-s -w \
        -X github.com/ivanzzeth/specula/internal/version.Version=${VERSION} \
        -X github.com/ivanzzeth/specula/internal/version.Commit=${COMMIT} \
        -X github.com/ivanzzeth/specula/internal/version.BuildDate=${DATE}" \
      -o /out/specula ./cmd/specula

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/specula /specula
COPY --from=build --chown=65532:65532 /out/var/lib/specula /var/lib/specula
COPY contrib/docker/specula.yaml /etc/specula/specula.yaml
EXPOSE 7732 7733
VOLUME ["/var/lib/specula"]
USER nonroot:nonroot
ENTRYPOINT ["/specula"]
CMD ["--config", "/etc/specula/specula.yaml"]
