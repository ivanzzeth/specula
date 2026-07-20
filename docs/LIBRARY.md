# Specula as a Go Library

> Library Preview (v0.x). Public API lives under `pkg/`. Anything in `internal/`
> has **no** compatibility promise.

Specula can be consumed three ways:

| Layer | Use case | Entry |
|---|---|---|
| **L1 SDK** | CI, agents, custom gateways | `pkg/specula` — `Get` / `Open` / `VerifyFile` |
| **L2 Embed** | Existing Go HTTP servers | `pkg/embed` or `pkg/handler/*` |
| **L3 Daemon** | Ops deployment | `cmd/specula` (thin shell over the same core) |

## Import paths (stable surface)

| Package | Role |
|---|---|
| `pkg/artifact` | Foundation types: `Tier`, `ArtifactRef`, `Result`, `CacheEntry` |
| `pkg/store/blob` | `BlobStore` CAS interface |
| `pkg/store/meta` | `MetadataStore` interface |
| `pkg/store/local` | Local-disk blob driver (default, light) |
| `pkg/store/sqlite` | SQLite metadata driver (default, light) |
| `pkg/store/s3` | S3 blob driver (**opt-in** — pull AWS SDK) |
| `pkg/store/postgres` | Postgres metadata driver (**opt-in**) |
| `pkg/verify` | `Verifier`, `Chain`, verifier constructors |
| `pkg/cache` | `CacheManager`, quarantine, verify-on-write |
| `pkg/upstream` | Fallback-chain upstream client |
| `pkg/coalesce` | Stampede protection |
| `pkg/specula` | One-shot facade: `New` → SDK (`Get` / `Open` / `VerifyFile`) |
| `pkg/embed` | HTTP mount helpers (`Mount` / `Handler`) — opt-in protocol deps |
| `pkg/handler/...` | Protocol `http.Handler`s (oci, gomod, …) |

## Honesty contract

Every successful `Get` / cached entry exposes the **tier actually achieved**:

| Tier | Guarantee |
|---|---|
| `signed` | Cryptographic authenticity (sumdb, apt GPG, cosign keyed, …) |
| `consensus` | Independent mirrors agree — **not** cryptographic authenticity |
| `tofu` | First-seen digest pin + change alert |
| `checksum` | Transport integrity only — **never** a supply-chain control alone |

Library callers must not treat `checksum` as provenance.

## Error contract (`errors.As`)

These types are part of the public surface and remain unwrappable:

| Type | Package | Meaning |
|---|---|---|
| `*cache.VerifyError` | `pkg/cache` | Verification chain rejected the artifact |
| `*cache.PinMismatchError` | `pkg/cache` | Caller digest pin ≠ cached digest |
| `*upstream.StatusError` | `pkg/upstream` | Definitive upstream HTTP status (404/403/…) |
| `cache.ErrCacheMiss` | `pkg/cache` | Absent or TTL-expired |

## Stability (v0.x)

- Breaking changes to `pkg/**` are allowed in 0.x but **must** be listed in `CHANGELOG.md`.
- `internal/**` may change without notice.
- Prefer the `pkg/specula` facade; lower-level packages are for advanced wiring.

## Non-goals (not exported)

- Embedded WebUI / email auth / JWT sessions
- Org / API key / grant / writable multi-tenant registry
- YAML config hot-reload (daemon-only; library uses `specula.Options`)
- E2E / groundtruth test harnesses

## Minimal dependency set

Default facade wiring pulls **local disk + SQLite** only.

Heavy optional drivers are separate import paths — do **not** blank-import them
unless you need them:

```go
import (
  _ "github.com/ivanzzeth/specula/pkg/store/s3"       // registers "s3" blob driver
  _ "github.com/ivanzzeth/specula/pkg/store/postgres" // registers "postgres" meta driver
)
```

Or construct drivers yourself and pass `Options.Blob` / `Options.Meta`.

## Quick start

See [`examples/sdk-get-module`](../examples/sdk-get-module) and
[`examples/embed-mux`](../examples/embed-mux).

## Release checklist (Library Preview)

```bash
./scripts/check-api.sh
# optional:
#   go install golang.org/x/exp/cmd/gorelease@latest
#   gorelease -base=v0.2.0 -version=v0.3.0
# Tag after review: git tag v0.3.0 && git push origin v0.3.0
# pkg.go.dev: https://pkg.go.dev/github.com/ivanzzeth/specula@v0.3.0
```
