# Changelog

All notable changes to Specula are documented here. The public library surface
is `pkg/**` — see [docs/LIBRARY.md](docs/LIBRARY.md).

## [0.12.7] — Git mirror caching actually works: ship git in the image — 2026-08-01

### Fixed — the git protocol now caches instead of silently passing through

The git handler keeps node-local bare mirrors by shelling out to `git clone
--bare` (`internal/handler/git` `serveMirror` → `EnsureSynced`). The runtime
image was `gcr.io/distroless/static-debian12`, which ships **no `git`
binary**, so every mirror sync failed with `exec: "git": executable file not
found in $PATH` and the request silently degraded to a live reverse-proxy
passthrough — correct results, but **zero node-local caching**. Inside CN that
means every clone hits the upstream directly, defeating the entire point of a
Specula mirror. The runtime base is now `debian:12-slim` + `git` +
`ca-certificates`, preserving the nonroot (uid/gid 65532) identity.

### Hardened — `git.enabled=true` alone is a valid opt-in

The `specula-bootstrap` chart now defaults every git sub-value in-template, so
`helm upgrade --reuse-values --set git.enabled=true` renders a complete,
Validate-passing `protocols.git` block (github.com upstream, `mirror_dir`,
`sync_stale_after`, `public_only`, `fail_closed`) even when the stored release
values predate the git block. Regression tests:
`internal/cluster/chart_git_render_test.go`.

## [0.12.6] — Git: serve gzipped upload-pack requests instead of 502 — 2026-08-01

### Fixed — a large `git clone` over protocol v0 no longer 502s

Under git protocol **v0** the client buffers the whole want/have request and,
once it crosses git's gzip threshold (a large ref advertisement — many tags or
branches), sends the upload-pack POST with `Content-Encoding: gzip`. Two stacked
defects turned that into a 502:

1. `serveGitHTTPBackend` never propagated the request's `Content-Encoding` into
   the CGI environment, so `git-http-backend` fed the raw gzip bytes straight to
   the pkt-line parser and died with *"bad line length character"* on the gzip
   magic (`0x1f 0x8b`).
2. On the serve-fail fallback to the reverse proxy, `r.Body` had already been
   consumed to EOF with no `GetBody` set, so the proxied request carried an empty
   body against the original `Content-Length` → *"Body length 0"* → 502.

The upload-pack body is kilobytes, so it is now buffered and `GetBody` is set,
and `HTTP_CONTENT_ENCODING` is propagated into the CGI env. Modern git defaults
to protocol **v2** (streams uncompressed, no gzip) so everyday clones never hit
this; the fix restores correctness for v0 clients and large ref sets. Regression
test: `TestRealClient_GzippedUploadPackClone` adds 80 tags to cross the gzip
threshold and clones under `protocol.version=0`.

## [0.12.5] — Per-upstream proxy; doctor states the hosts.toml scope — 2026-07-28

### Added — per-upstream proxy, so the origin can be a paid last resort

`HTTPS_PROXY` already worked — the upstream transport has always used
`http.ProxyFromEnvironment` — but it applies to **every** upstream at once, which
sends the CN mirror traffic through the proxy too. The mirrors carry the bulk of the
bytes and exist precisely so that traffic is free, so on a metered proxy that is the
expensive mistake.

A proxy is therefore configured per upstream:

```yaml
protocols:
  oci:
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io   # direct, as before
        priority: 1
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 10
        official: true
        proxy: http://10.0.0.5:3128              # or socks5://127.0.0.1:1080
```

The official origin sits last in the chain, so its proxy is reached only after every
mirror has failed: a genuine fallback, paid for only when it is used. `http`, `https`
and `socks5` are accepted.

Details that matter in practice: clients are cached per (proxy, hop budget) so a
proxied origin keeps its connection pool instead of re-dialling per layer; the
fast-fail budget for non-final hops still applies through a proxy; a malformed proxy
is a **config error at load**, naming the exact key, rather than a silent fall back to
a direct dial that would make a proxy-only origin look like a dead origin.

### Changed — doctor says which consumers hosts.toml cannot govern

A node reported all-green — hosts.toml written for every registry, CRI *and*
`transfer.v1.local` `config_path` both `/etc/containerd/certs.d`, Specula answering
401 on `/v2/` — and a consumer on it still failed to pull. It called
`containerd.Client.Pull` directly, and that path builds its own resolver: it bypasses
hosts.toml entirely and dials `registry-1.docker.io`, which in CN times out and reads
as "Specula is broken" when the truth is "this client never asked Specula".

Doctor was not wrong, it was answering a narrower question than operators heard.
Green meant "the paths I check are wired", and nothing said which those are. It now
states the scope on every run:

| governed by certs.d/hosts.toml | not governed |
|---|---|
| kubelet, crictl (CRI `config_path`) | `containerd.Client.Pull` from Go |
| `ctr images pull` and other transfer-service clients (`transfer.v1.local config_path`) | any client building a `docker.Resolver` without hosts config |
| `ctr --hosts-dir <dir>` | |

with the fix for the caller: a resolver from
`docker.ConfigureDefaultRegistries(docker.WithHostsDir(…))` passed via
`containerd.WithResolver`, or pulling through the transfer service.

Reported as `advice`, not `risk`: it is a property of the containerd API rather than
a misconfiguration, and a red line on every correctly wired node is how operators
learn to ignore doctor. The cases an operator *can* fix — colon `config_path`, empty
transfer `config_path`, residual `server=`, wrong certs.d root — remain risks and
still exit 1.

## [0.12.4] — Cache import, cross-name CAS reuse, dedup-safe eviction — 2026-07-28

### Added — `specula cache import`: seed the cache from an image pulled elsewhere

For when the upstreams a cluster needs are unreachable but *some* machine can reach
them — and for an air-gapped install, where there is no upstream by design.

```bash
# on a machine that can reach the registry
crane pull --format=oci docker.io/library/redis:7-alpine redis.tar

# where Specula's stores are reachable
specula cache import --config specula.yaml \
    --from redis.tar --as docker.io/library/redis:7-alpine
```

`--from -` reads the archive from stdin, which is how a layout reaches a distroless
Pod — `kubectl cp` cannot, because copying into a container shells out to `tar` inside
the image:

```bash
kubectl exec -i -n specula-boot <pod> -- /specula cache import \
    --config /etc/specula/specula.yaml \
    --from - --as docker.io/library/redis:7-alpine < redis.tar
```

Afterwards a pull of `redis:7-alpine` is served entirely from cache. Verified with a
real 133 MB multi-arch image against a Specula whose only upstream pointed at the
discard port: 17 manifests and 79 blobs imported, `crane pull` returned the identical
index digest, and the process logged **no upstream request at all**.

Being seeded has to mean all three things a pull asks for, because a partial job looks
like success and fails at pull time: the tag→digest pointer, the manifest by digest,
and every config and layer blob. The importer writes all three, keyed exactly as the
read path looks them up.

A legacy `docker save` archive is **refused**, with the command that produces a usable
layout. That format re-packs layers as uncompressed tars whose digests differ from the
registry's, so importing one would fill the cache under digests no client ever asks
for — a silent no-op that looks like it worked.

An archive is expanded once rather than re-scanned per object: reading blobs straight
out of the tar cost several gigabytes of IO for a 130 MB image, growing with the square
of the layer count. It expands into the quarantine directory by default, not the
container's ephemeral layer, where a large layout would get the Pod evicted and look
like a crash rather than a full disk. Archive members that try to escape that directory
are refused.

Other things the implementation is careful about: bytes already in the store (a shared
base layer, a re-import) cost a metadata row rather than a second copy; a layout with
tampered bytes is rejected by the same verify-on-write path an upstream fetch uses; and
a layout holding several tagged manifests is refused rather than guessed at, since
publishing a tag that points at the wrong image is worse than stopping.

### Fixed — the same bytes under a different name went back to upstream

Blob and manifest storage is content-addressed and name-independent: one object per
digest, however many repositories reference it. The cache LOOKUP, though, keys on
`(protocol, name, digest)`. So a pull of `library/redis` missed on bytes Specula
physically held — arrived earlier as `docker.io/library/redis`, or as a layer shared
with another image — went to upstream, re-downloaded the whole layer, and discovered
at `Put` time that the object was already there.

Storage was never duplicated (the CAS deduplicates, and `Put` head-checks first), so
this cost bandwidth and latency rather than space. The part that broke a cluster: with
every Hub mirror failing, a blob already sitting in the store answered **502**.

A miss on `(name, digest)` now looks for the same digest under any other name whose
content is public pull-through, serves those bytes, and records a metadata row for the
new name so the next pull is a direct hit — a row, never a second copy. Manifests get
the same treatment, since one image pulled under two names has one manifest digest.

Excluded, deliberately: hosted repos and owned namespaces. Hosted content may be a
private org's push and a blob digest is readable from any manifest, so a digest-keyed
shortcut across names would otherwise be a private-content read primitive. The safety
argument rests on pull-through content being public, which holds while every upstream
is anonymous; `sharedcas.go` states the invariant so per-upstream credentials cannot
quietly break it.

### Fixed — CAS dedup could delete bytes another entry still referenced

Blob storage is content-addressed: the key is `blobs/<algo>/<hex>` with no
protocol, repo, org or tag in it, so identical bytes are ONE object shared across
images, repositories, protocols and — in the hosted shape — clusters. `Put`
head-checks first and returns early when the object is there, so a second reference
uploads nothing. That is the storage saving, and it was doing its job.

Deletion did not respect it. `enforceCapacity` deleted the metadata row and then
deleted the blob unconditionally, so evicting one of several entries sharing a
digest destroyed the bytes for all of them and left the survivors pointing at
nothing. Two tags of one image share base layers; that is the normal case, not a
corner one. (`BlobStore.Delete`'s doc comment promised "or decrements its refcount
for CAS dedup" — no driver ever did.)

Eviction now asks the metadata store whether anything else still references the
digest, via a new `EntryFilter.Digest`, and keeps the object when it does. On any
error it keeps the object: an unreferenced blob is space the next pass reclaims,
whereas a deleted-but-referenced blob is a pull that fails for content the cache
still advertises.

A metadata row whose blob has gone missing anyway — an S3 lifecycle rule, a bucket
sweep, an older build's eviction — now reads as a **cache miss** so the caller
re-fetches, instead of surfacing as a 502 for content the upstream still has. Only
"not found" errors are treated this way; `AccessDenied` and timeouts stay hard
failures rather than becoming a silent re-fetch storm that hides an outage.

## [0.12.3] — Charts stop restating the built-in upstream table — 2026-07-28

### Fixed — `502 upstream fetch failed` pulling from Docker Hub in CN

A node pulling `redis:7-alpine` through Specula got, on every retry:

```
GET .../v2/library/redis/blobs/sha256:2a5181…?ns=docker.io: 502 Bad Gateway
unknown: upstream fetch failed
```

The cause was not the pull path. The chart's `upstreams.oci` pinned **DaoCloud +
Docker Hub only**, while the binary's built-in chain is DaoCloud → 1ms → Aliyun →
Docker Hub. `specula_upstream_blocked{upstream="daocloud"} 1` — DaoCloud had tripped
its circuit breaker on dial/header timeouts — so the one remaining path was the
official origin, which CN cannot reach. Two mirrors is not a chain; it is a mirror
and a cliff.

Neither chart writes a protocol table any more. Everything — mirror chains,
per-protocol TTLs, verification tiers, the OCI `remote_registries` allowlist for
ghcr.io / quay.io / registry.k8s.io / gcr.io / k8s.gcr.io / mcr.microsoft.com /
codeberg.org — comes from the binary, and the chart emits only what an operator
explicitly set (`upstreams.oci`, `remoteRegistries`, `protocolOverrides`). Deleted
with it: the bootstrap chart's 64-line `_remote_registries.tpl` (a byte-for-byte
duplicate of the built-in host set), the main chart's 81-line `regionProfile=cn`
branch, and `values-cn.yaml`'s three-entry Hub chain. The one table that remains is
the non-CN override, which pins official upstreams *because* the defaults are
China-first.

`regionProfile=cn` is now a no-op for the bootstrap chart's protocol config: the
built-in defaults already are the CN ones.

### Changed — a failed upstream chain names every mirror, not just the last hop

`upstream: all upstreams failed: … last error: dial registry-1.docker.io: i/o
timeout` reported only the final upstream. In CN the final upstream is the official
origin nothing can reach, so every chain failure read as "the origin is down" and the
CN mirror's actual reason — 403, 404, circuit-broken, TLS — was invisible. Found
while a `curlimages/curl` pull failed with a Hub timeout: DaoCloud had failed first,
for a reason the log did not keep.

The message now carries a per-upstream summary, e.g.
`[tried daocloud: 403 Forbidden; dockerhub: dial tcp …: i/o timeout]`. Which error is
*wrapped* is unchanged, so a definitive 404 still wins over a later transport failure
and does not turn into a 502.

An upstream **skipped because its circuit breaker is open** is listed too
(`daocloud: skipped: circuit breaker open`). It produces no error of its own, so
without that line the message showed only the origin's timeout and read as "the
origin is down" — while the real story was a broken mirror and no fallback left.

## [0.12.2] — Push works behind an Ingress with more than one replica — 2026-07-28

### Fixed

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
