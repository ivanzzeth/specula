# Changelog

All notable changes to Specula are documented here. The public library surface
is `pkg/**` — see [docs/LIBRARY.md](docs/LIBRARY.md).

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
