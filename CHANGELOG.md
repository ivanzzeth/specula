# Changelog

All notable changes to Specula are documented here. The public library surface
is `pkg/**` — see [docs/LIBRARY.md](docs/LIBRARY.md).

## [Unreleased]

### Added

- **Runtime traffic stats**: `specula_response_bytes_total` + `specula_request_duration_seconds`
  (Prometheus), `GET /api/v1/traffic`, and `specula stats [--watch]` for live
  per-protocol bytes / seconds / MB/s (lifetime + last 60s window).
- **`GET /api/v1/stats`**: authenticated cache occupancy + traffic (session JWT or
  API key `Bearer spck_…`). CLI: `specula login` / `logout` persists the key in
  `~/.config/specula/credentials.json`; `specula stats` reads it (or `SPECULA_TOKEN`).
- **`specula bench`**: one-shot cold/warm pull probe (not a substitute for runtime stats).
- **`specula integrate`**: additive one-click client wiring (go/npm/pypi/oci/helm/git/apt)
  without destroying existing mirrors.
- **`specula service install`**: systemd unit under `multi-user.target` (boot start),
  plus `contrib/systemd/specula.service`.
- **Version from git tags**: `internal/version` via `-ldflags`; `specula version` /
  Makefile / `.github/workflows/release.yml` (publishes multi-arch binaries on `v*` tags).
- **Container image**: `Dockerfile` + `make image` / `make image-smoke` (push the
  product image into an ephemeral Specula hosted OCI, pull + digest check). On `v*`
  tags, release CI publishes `ivanzz/specula` to Docker Hub (`linux/amd64` +
  `linux/arm64`) when `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` secrets are set.
- **Cache capacity limit**: `cache.max_bytes` (0 = unlimited). After each successful
  Store, Specula evicts the oldest unpinned immutable entries (meta + CAS blob)
  until total `SUM(size)` is at or below the ceiling. Pinned entries are never
  evicted. Wire via daemon YAML or `specula.Options.MaxBytes` / `cache.WithMaxBytes`.

## [0.3.0] — Library Preview — 2026-07-20

### Added

- **Public Go library** under `pkg/` so any Go module can `go get` Specula:
  - `pkg/artifact` — foundation types (`Tier`, `ArtifactRef`, `Result`, …)
  - `pkg/cache`, `pkg/verify`, `pkg/upstream`, `pkg/coalesce` — core pipeline
  - `pkg/store/{blob,meta,local,sqlite}` — default light-weight drivers
  - `pkg/store/{s3,postgres}` — **opt-in** heavy drivers (blank-import registers)
  - `pkg/handler/{oci,gomod,pypi,npm,apt,helm,tarball,git}` — embeddable HTTP handlers
  - `pkg/specula` — one-shot facade (`New`, `Get`, `Open`, `VerifyFile`)
  - `pkg/embed` — HTTP `Mount` / `Handler` (opt-in; pulls protocol handlers)
  - `pkg/metrics` — opt-in Prometheus HTTP middleware
- Examples: `examples/sdk-get-module`, `examples/embed-mux`
- Compatibility shim: `internal/artifact` re-exports `pkg/artifact`
- Docs: [docs/LIBRARY.md](docs/LIBRARY.md) (stability, honesty contract, error types)

### Changed

- `cmd/specula` is a thin shell: data-plane handlers and core types are imported
  from `pkg/*`; control plane (WebUI / multi-tenant registry) remains `internal/`

### Notes

- v0.x: breaking changes to `pkg/**` are allowed but must be listed here
- `internal/**` has no compatibility promise
- Default facade dependency set is local disk + SQLite; do not blank-import
  `pkg/store/s3` or `pkg/store/postgres` unless needed
