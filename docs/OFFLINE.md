# Offline / air-gap cookbook

Specula can run as a **warm cache with no outbound network** (`server.mode: offline`).
This doc is the operator path from “first online warm” to “air-gapped serve”, including
prefetch and containerd mirror wiring. Specula **never** auto-rewrites your config.

## Mental model

| Mode | Cache hit | Cache miss | Outbound fetch |
|------|-----------|------------|----------------|
| `online` (default) | serve | pull-through + verify | yes |
| `offline` | serve | **404** | **no** |

Git bare mirrors: offline serves what is already on disk; no clone/refresh.

Switching modes requires a **daemon restart** (mode is read at process start).

## End-to-end checklist

### 1. Online warm (connected environment)

Run Specula with normal upstreams. Prefer multi-source allowlists
(`remote_registries`, `apt.repositories`, …) so path-style pulls match production.

```bash
# Optional: merge example allowlists into an existing config (creates .bak.<ts>)
./bin/specula config apply-example --section oci,apt,helm
```

Warm the artifacts you will need offline:

```bash
# OCI (docker / nerdctl / crane through Specula)
docker pull 127.0.0.1:7732/registry.k8s.io/pause:3.9

# Or bulk-warm manifests via the prefetch helper (token + manifest GET):
./bin/specula bootstrap-prefetch \
  --addr http://127.0.0.1:7732 \
  --images docker.io/library/hello-world:latest,registry.k8s.io/pause:3.9
```

Apt / Helm / Conda / Cargo / git: hit Specula once per object while online
(`apt-get update` against Specula, `helm pull`, `git clone` via insteadOf, …).

Smoke gate (warm → offline → hit + miss):

```bash
./scripts/realclient-offline.sh
```

### 2. Flip to offline

```yaml
server:
  mode: offline   # empty / online = normal pull-through
```

Restart the daemon. Confirm logs show:

`specula: offline mode — cache hits only; misses return 404; no outbound fetch`

### 3. Verify offline behaviour

- Cached tag / digest → **200** (or registry 200 with digest headers).
- Uncached ref → **404** with no upstream attempt.
- Admin Events (`GET /api/v1/admin/events`): verify failures from the warm phase
  (if any) remain visible on that process until restart (in-memory ring).

### 4. containerd / kubelet mirror (air-gap nodes)

Point node pulls at Specula instead of the public registry. Specula can write
containerd `certs.d` host.toml files:

```bash
./bin/specula bootstrap-mirror write \
  --endpoint http://127.0.0.1:7732 \
  --certs-dir /etc/containerd/certs.d \
  --registries docker.io,registry.k8s.io,ghcr.io
```

China / first-land Specula itself: see
[`deploy/helm/specula-bootstrap`](../deploy/helm/specula-bootstrap/README.md)
and `./scripts/bootstrap-minikube.sh`.

### 5. Prefetch before cutting the wire

Use prefetch when HA charts or kubelet will demand manifests immediately after
offline cutover:

```bash
./bin/specula bootstrap-prefetch \
  --addr http://specula.example:7732 \
  --images registry.k8s.io/kube-apiserver:v1.29.0,... \
  --timeout 10m
```

Prefetch warms **manifest metadata** through Specula’s OCI path; layer blobs
still need a full client pull (or a prior `docker pull` through Specula) if
you need layers offline too.

## Operator tips

- **Capacity**: size `cache.max_bytes` for the offline corpus; eviction still
  runs in offline mode on new stores (there should be none) but warm content
  can be pinned via normal pin APIs when configured.
- **Upgrade hints**: on startup Specula warns if allowlists are empty and
  suggests `specula config apply-example --section …` — opt-in only.
- **Do not** rely on offline mode as a substitute for an allowlist; SSRF
  allowlists still matter on the online warm phase.
- **HA**: offline is per-replica process config. Shared CAS + Postgres meta
  mean any replica can serve warmed blobs; each replica needs `mode: offline`.

## Related

- README § Offline / air-gap
- [`deploy/helm/specula-bootstrap`](../deploy/helm/specula-bootstrap/README.md)
- `scripts/realclient-offline.sh`
- ARCHITECTURE § cache + verify-on-write
