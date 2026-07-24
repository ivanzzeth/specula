# Changelog

All notable changes to Specula are documented here. The public library surface
is `pkg/**` — see [docs/LIBRARY.md](docs/LIBRARY.md).

## [Unreleased]

## [0.10.0] — Supply-chain entry gates (maturity + sole-index + Events) — 2026-07-24

### Added

- **Maturity / cool-down gate** (`verification.maturity`): block or warn on
  package versions younger than `min_age` (npm / PyPI / Cargo). Policy gate —
  not a cryptographic trust tier; closes the post-publish malware window that
  checksum/TOFU alone cannot (aligned with JFrog Curation / Socket cool-down).
  Publish age prefers registry metadata (`packument.time`, PEP 691
  `upload-time`, Warehouse JSON, crates.io `created_at`) over CDN
  `Last-Modified`.
- **`upstream.WithAcceptHeader`**: PEP 691 JSON simple-index negotiation
  (closes the prior known limitation that only served cached JSON).
- **Events `kind`**: Admin Events API + WebUI distinguish `maturity` vs `tofu`
  vs other verify outcomes (summary lamps + Kind column).
- **CI / realclient**: `scripts/realclient-multisource.sh` (named-source
  path-strip) and hermetic `scripts/realclient-maturity.sh` (enforce young
  reject / old allow + Events kind check).
- **Dep-confusion fail-safe UX**: `specula integrate` no longer promotes the
  previous pip index to `extra-index-url` (that pattern *enables* confusion);
  `integrate status` audits dangerous client dual-index configs.
- **Verification Events persistence**: Admin Events survive process restart
  (SQLite/Postgres); TOFU first-lock warns and digest-change fails remain
  actionable alongside maturity policy hits.

### Docs

- PRD **v0.10** marked done; TRUST cookbook: maturity + sole-index
  anti-patterns; Events kind operator notes.

## [0.7.0] — Multi-registry OCI, offline mode, ops polish — 2026-07-24

### Added

- **OCI multi-registry pull-through**: path-style
  `docker pull 127.0.0.1:7732/<registryHost>/<repo>:<tag>`; host stripped on
  upstream fetch; SSRF allowlist `protocols.oci.oci.remote_registries`.
  Bootstrap / `integrate --protocols oci` write containerd `hosts.toml` with
  `override_path` for non-`docker.io` registries.
- **`server.mode: offline`**: cache hits only; misses 404; no outbound fetch
  (git: no clone/refresh / passthrough). Gate: `scripts/realclient-offline.sh`.
- **Dashboard capacity**: Admin stats expose `max_bytes`, `evicted_bytes` /
  `evicted_objects`; WebUI gauge for Specula cache ceiling.
- **HA acceptance**: `scripts/ha-minikube.sh` warms a manifest, kills a replica,
  re-fetches (shared CAS hit). PRD v0.7 marked done.
- **Trust cookbook**: [`docs/TRUST.md`](docs/TRUST.md) — cosign keyed, apt GPG,
  Helm `.prov`, dep-confusion fail-closed. Example apt keyring path enabled;
  CI runs `test-trust-oracle` + `test-trust-oracle-signed` on `main`/PRs.

### Docs

- README (EN/ZH): offline / multi-registry notes; TRUST.md linked.

## [0.6.0] — Cargo, conda, Hugging Face — 2026-07-23

### Added

- **Cargo** sparse registry (`/cargo/`), **conda** channel (`/conda/`),
  **Hugging Face Hub** (`/hf/`, `HF_ENDPOINT`).
- `specula integrate` for cargo / conda / hf; realclient scripts + Makefile.

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
