# Changelog

All notable changes to Specula are documented here. The public library surface
is `pkg/**` — see [docs/LIBRARY.md](docs/LIBRARY.md).

## [Unreleased]

### Fixed — `docker`/`crane push` behind an Ingress with replicas > 1

A blob push is a stateful three-request protocol — POST opens an upload session,
PATCH streams chunks, PUT finalises — and upload sessions lived in one process's
memory with their bytes in that process's temp dir. Behind a Service or Ingress with
more than one replica the PATCH/PUT lands on a Pod that never saw the POST, so a
push that had authenticated fine died at the blob stage:

```
BLOB_UPLOAD_UNKNOWN: upload session not found
```

Scaling the Deployment to one replica made the identical push succeed, which is what
made it look like a networking problem. Cookie affinity does not fix it either: crane
and docker do not carry an Ingress's affinity cookie.

Sessions are now shared, so any replica can continue and finalise an upload another
one started:

- **Metadata** (repo, offset, chunk list) goes in the mutable metadata tier — shared
  already, implemented by both the sqlite and postgres drivers, and postgres in HA.
- **Chunk bytes** go in the blob store, which is S3 in HA and therefore readable by
  every replica.

Wired unconditionally, not only under `ha: true`: `replicaCount` is an independent
knob, and "works until someone scales the Deployment" is not a property worth having.

Two details that the implementation had to get right, both covered by tests:

- The blob store is a strict CAS — `Put` hashes what it receives — so chunks are
  staged under their own content digest rather than an opaque key. A monolithic
  upload's single chunk therefore has the **same digest as the finished blob**, so
  finishing a session now goes through `Complete(id, promotedDigest)`; cleaning up
  via `Delete` would have evicted the blob that was just pushed.
- A staged chunk whose bytes were already in the store is not ours to remove, or an
  abandoned push would evict a blob someone else depends on.

No new operational requirement: no `sessionAffinity`, no pinned `replicas: 1`.

## [0.12.1] — Every protocol is served unless you switch it off — 2026-07-28

### Fixed

A config that named only `protocols.oci` served OCI and answered `/npm`, `/pypi`,
`/go`, `/apt`, `/helm`, `/cargo`, `/conda` and `/hf` with **404**. Handler
registration tested for the protocol's key in the config, so a protocol the binary
fully implemented stayed dark unless the deployment happened to enumerate it — and
every chart and ConfigMap we ship enumerates OCI only. A CN mirror that mirrors one
protocol out of nine is the wrong default for a product whose thesis is that the
upstreams you need are unreachable.

Absence now means **enabled**. The built-in table is parsed from the embedded
`internal/config/example.yaml`, so the defaults are the same maintained China-first
chains (DaoCloud, npmmirror, goproxy.cn, tuna, rsproxy, hf-mirror) with the official
upstream last as fallback — one table, in one place, rather than a copy per chart.

Precedence:

| config says | result |
|---|---|
| nothing about a protocol | served, built-in chain |
| a block with no `upstreams` key | served, built-in chain, your other settings kept |
| `upstreams: [...]` | served, exactly your chain |
| `upstreams: []` | **validation error** — the message names both ways out |
| `enabled: false` | not served, no handler registered |

`git` is the one protocol not switched on implicitly under `server.ha: true`: it keeps
bare mirrors on local disk, so with several replicas each would answer from its own
clone. Name it in the config to opt in.

Charts gained `protocolOverrides`, a raw passthrough into `protocols:`, for retuning
or disabling one — not for enabling, which needs no configuration.

**SDK.** `pkg/embed` had the same defect in miniature: `cargo`, `conda` and `hf`
existed as `pkg/handler/*` façades but were missing from `build()`'s switch and from
the default protocol list, so `embed.Options{}` served eight of eleven protocols.
Wired, with a test that keeps the default list and the switch in step.

Verified against a real config that names only OCI: all nine handlers mount, and
`/npm`, `/pypi`, `/go`, `/hf`, `/apt/ubuntu/dists/jammy/Release`,
`/helm/bitnami/index.yaml` and `/conda/conda-forge/noarch/repodata.json` all return
200 with real payloads through the CN mirrors.

## [0.12.0] — One port: 7732 removed — 2026-07-28

### ⚠️ BREAKING — one port, 7732 removed

Specula listened on two ports: 7732 for artifact protocols, 7733 for the WebUI and
Admin API. It now listens on **7733 only**. Paths keep their separate meanings —
`/v2/`, `/helm/`, `/pypi/`, `/npm/`, `/go/`, `/apt/`, `/tarball/`, `/git/`, `/cargo/`,
`/conda/`, `/hf/`, `/token`, `/api/v1/**`, `/healthz`, `/readyz`, `/metrics`, and the
SPA at `/`. Nothing moved under `/api/v1`; there is simply no second TCP port to
publish, firewall or explain, and an Ingress in front of it is now ordinary
Kubernetes.

There is deliberately **no dual-listen compatibility period**.

**Config.** `server.listen_addr` (default `0.0.0.0:7733`) replaces the pair.
`server.control_plane_addr` still works as an alias, because every existing config and
ConfigMap sets it and it already named the surviving port.
`server.data_plane_addr` is a **startup error**, not a warning:

```
server.data_plane_addr: removed — Specula serves every protocol, the Admin API,
probes and the WebUI on ONE port. Delete this key and set server.listen_addr instead
```

Silently ignoring it would leave an operator believing 7732 is still served.

**Charts.** Both charts render exactly one Service port and one containerPort
(`service.port`, `service.nodePort`, default 7733/30733); probes moved to it.

**Clients.** `integrate` / `doctor` / `bootstrap-prefetch` default to 7733 (or an
Ingress URL). `integrate` also prunes stale `:7732` registry mirrors it finds, so a
node configured by an older Specula is repaired rather than left pointing at a dead
port.

`server.registry_public_host` is now **required behind an Ingress**: with one port a
direct deployment can derive it from the request, but an ingress hostname and :443
appear nowhere in the listen address.

Merging the muxes has one hazard worth naming: the SPA's `/` catch-all must not
swallow `GET /v2/`. When that happened on the old control-plane port, `docker login`
read the 200-with-HTML as "registry reachable, no auth required" and printed **Login
Succeeded** for an entirely bogus password. Four tests pin it (`/v2/` is not HTML,
carries a challenge, everything answers on one mux, no duplicate registration), and
the cluster gate asserts both the single Service port and a real `/v2/` challenge.


### Added

- **OCI `remote_registries[].upstreams`**: multi-mirror chain per host (e.g.
  DaoCloud → 1ms), with `https://<host>` always appended as official origin.
  Legacy single `base_url` still works. Fixes the class of CN allowlist 403s
  that a one-mirror remote could not fall through.
- **CN example upstream expansion**: Hub adds `docker.1ms.run`; remotes use
  daocloud→1ms; PyPI +USTC/Tencent; apt +USTC/Huawei; Cargo prefers rsproxy;
  conda +USTC cloud.

## [0.11.2] — S3-compatible stores actually work; hosted profile — 2026-07-27

### Fixed

- **`blob.driver: s3` could not write to any non-AWS store.** aws-sdk-go-v2 defaults to
  `aws-chunked` PutObject with a trailing checksum over an unsigned payload; Alibaba
  Cloud OSS answers `400 NotImplemented: Aws MultiChunkedEncoding
  STREAMING-UNSIGNED-PAYLOAD-TRAILER is not supported`, and R2 / older MinIO reject it
  too. Checksum calculation is now pinned to `WhenRequired`. Reads, listing and HEAD
  had all worked, so this presented as "cache stays empty".
- **`bootstrap-prefetch` stripped the registry host**, turning
  `registry.k8s.io/pause:3.10` into the Hub repo `pause` — so it could never warm a
  non-Hub registry, which is most of what matters in CN, and the chart's prefetch Job
  inherited that.
- **A `--values` profile was overridden by defaulted flags**: `--image` defaults to
  `specula:local`, and helm ranks `--set` above every `-f`, so the profile's registry
  coordinate was replaced and auto-pinning added a nodeSelector to a stateless
  multi-replica Deployment. Only flags actually typed now become `--set`.
- **Changing config or credentials did not restart anything** — a `helm upgrade`
  altering only the ConfigMap/Secret left the Pod template byte-identical. Added
  `checksum/config` and `checksum/creds`.

### Added

- **Hosted shape**: `blob.driver=s3` + `meta.driver=postgres` + `ha` + HPA in the
  bootstrap chart, so one stateless Specula can serve many clusters. Credentials come
  from the profile and the chart creates the Secret — one file, one command, no
  `kubectl create secret`.
- `deployment.enabled=false` for a client cluster that only needs the mirror
  DaemonSet, plus `service.type`/`annotations` for an internal LoadBalancer.
- Two tracked profiles under `deploy/profiles/`, and
  [docs/deploy/HOSTED.md](docs/deploy/HOSTED.md) — the runbook with the nine pitfalls
  hit deploying it on ACK + RDS + OSS.
- `cluster uninstall` removes the node `hosts.toml` it wrote
  (`bootstrap-mirror remove`); pin selection refuses up front when no node has room;
  `--wait` fails fast on `Unschedulable`.
- Gated live diagnostics: `TestLiveS3Diagnose` (HEAD has no body, so only
  GET/List reveal whether a 403 is authorization or signing) and `TestAutoPinNodeLive`.

## [0.11.1] — Real-cluster fixes: fsGroup, capacity-aware pin, uninstall cleanup — 2026-07-27

Everything here was found by installing v0.11.0 onto a real Alibaba Cloud ACK
cluster. None of it could be caught by the minikube gate.

### Fixed

- **`fsGroup` on both charts** — the image runs as distroless nonroot (65532) and a
  CSI-provisioned PVC mounts root:root 0755, so the daemon could not create
  `/var/lib/specula/{blobs,quarantine}` and crash-looped with "mkdir: permission
  denied". This broke one-command install on **every** cloud CSI driver; minikube's
  hostpath provisioner hands out 0777 and hid it completely.
- **Pin node picked by memory headroom** — `AutoPinHostname` took the first Ready
  worker regardless of capacity, pinned Specula to a node that could not fit its
  128Mi request, and left the Pod Pending *after* the mirror DaemonSet had already
  rewritten hosts.toml on every node — turning a capacity problem into broken node
  pulls. Now refuses before helm runs, quoting per-node headroom.
- **Fail fast on `Unschedulable`** — the earlier fail-fast watched container Waiting
  states, but an unschedulable Pod has no container statuses at all; "Insufficient
  memory" lives in the PodScheduled condition.
- **`cluster uninstall` removes the node hosts.toml it wrote** (new
  `bootstrap-mirror remove`) — otherwise the seven redirected registries keep
  resolving to a dead NodePort and fail, since CN mode keeps no public fallback.
- **`cluster doctor` probes through the API server** instead of a node IP, so it
  works from a laptop outside the VPC.

### Changed

- Image is **cross-compiled** rather than QEMU-emulated: builder stages pinned to
  `$BUILDPLATFORM`, Go targets `$TARGETARCH`, WebUI built once. The multi-arch
  release step was taking ~15 minutes under emulation.
- `docs/` reorganised by task, `docs/RELEASE.md` added (registry secrets, the ACR
  instance-vs-shared-domain trap, chart/binary version skew).

## [0.11.0] — CN bootstrap unblocked: multi-registry publish, single-host ops, quarantine on the data volume — 2026-07-27

### Fixed

- **Quarantine no longer lands in `os.TempDir()`** (`storage.quarantine_dir`, default
  `<data dir>/quarantine`). No handler was ever given `WithQuarantineDir`, so every
  cache fill streamed through `/tmp`: total failure in an image without `/tmp`
  (while `/healthz` stayed 200 — healthy-looking and caching nothing), and multi-GB
  OCI layers written to tmpfs (RAM) under systemd or the ephemeral container layer
  under Kubernetes. The daemon now creates the directory at startup and refuses to
  start if it cannot; both charts set it, and the HA chart mounts a dedicated
  emptyDir (`quarantineSizeLimit`).
- **`cluster doctor` probes through the API server**, not a node IP + NodePort. The
  old default was unreachable from a macOS/docker-driver minikube and from any
  laptop pointed at a cloud cluster whose nodes sit in a VPC — reporting healthy
  clusters as broken on the first command after install. `--addr` still probes
  directly.
- **A binary built without `web/dist` boots** instead of panicking on startup and
  taking the data plane down with it; it serves a committed placeholder page and
  logs a WARN.

### Added

- **Multi-registry release publish**: one multi-arch build pushed to every registry
  whose secrets are set — Docker Hub, **Aliyun ACR**, Huawei SWR, GHCR. Measured
  from ACK cn-chengdu, Docker Hub and every public CN mirror tried
  (`docker.m.daocloud.io` 403, `docker.1ms.run`, `docker.xuanyuan.me` timeouts)
  failed, so a cloud-region registry is the only way a CN cluster can bootstrap
  Specula's own image.
- **`specula upgrade` / `specula rollback`**: single-host ops path — rename-swap of
  a live binary (Linux refuses to write into a running executable), restart,
  `/healthz` gate, automatic rollback to `<binary>.prev`. `docs/deploy/SINGLE-HOST.md`
  covers the intranet one-VM deployment.
- **`make test-single-host`**: install/upgrade/rollback verified against real
  systemd in a throwaway container, including the ETXTBSY premise and the
  WebUI-less boot regression.

### Added (HA coalesce)

- **`coalesce.lock_driver=postgres`**: wires `PGAdvisoryLocker` on the shared meta
  pool for Redis-free HA stampede protection. HA accepts **redis** (default when
  `lock_driver` empty) **or** **postgres**; postgres lock requires
  `storage.meta.driver=postgres`. Advisory locks hold a pool connection for the
  lock duration — size the pool for concurrent cold misses.
- **Tarball `WithLocker` / `FetchLocked`**: cold path matches npm/pypi (Acquire →
  cache recheck → fetch), and `mountTarball` receives the stampede locker.

### Docs

- ARCHITECTURE §7: tier-2 marked wired (redis primary; postgres advisory optional).
- PRD milestone **v0.11** done; example YAML documents both lock drivers.

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
