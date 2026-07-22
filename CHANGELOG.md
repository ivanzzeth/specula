# Changelog

All notable changes to Specula are documented here. The public library surface
is `pkg/**` — see [docs/LIBRARY.md](docs/LIBRARY.md).

## [Unreleased]

## [0.5.0] — HA & China bootstrap — 2026-07-22

### Added

- **HA multi-replica**: `server.ha` requires Postgres meta + Redis (redsync)
  stampede lock + shared CAS (S3-compatible or `local.shared=true`). Helm chart
  `deploy/helm/specula` (Bitnami PG/Redis, optional MinIO, HPA). Smoke:
  `./scripts/ha-minikube.sh`.
- **China / air-gapped self-bootstrap**: chart `deploy/helm/specula-bootstrap`
  (SQLite + local blob, NodePort). CLI `specula bootstrap-mirror write` and
  `specula bootstrap-prefetch` run from the Specula image (no busybox). Default
  OCI upstream DaoCloud. Smoke: `./scripts/bootstrap-minikube.sh` (containerd).
- **Runtime state persistence** (HA): series + upstream blocks via
  `internal/store/runtimestate` + migration `009_runtime_state`.
- **`FetchLocked`**: cross-replica coalesce via redsync; wired into OCI / Go /
  npm / PyPI / apt / Helm handlers (`WithLocker`).
- **Runtime traffic stats**: Prometheus + `GET /api/v1/traffic` + `specula stats`.
- **`specula integrate`**: one-click client wiring for all protocols.
- **`specula service install`**, **`specula bench`**, version ldflags, container
  image release CI, `cache.max_bytes` eviction.

### Docs

- README (EN/ZH): HA + China bootstrap entry points; ARCHITECTURE §12 bootstrap.

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
