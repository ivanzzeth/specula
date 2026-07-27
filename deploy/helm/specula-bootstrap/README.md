# Specula Bootstrap (China / air-gapped self-bootstrap)

Breaks the chicken-and-egg of deploying an image cache when the registries it
needs are blocked: a **zero-dependency** Specula (SQLite + local blob) comes up
first, then the node container runtime pulls **through** it.

Assumes Specula's **own** image (or binary) is already on the node — via
`docker load`, offline tar, `minikube image load`, or a China-reachable registry.

## One command (preferred)

```bash
# Build / load image first, then:
specula cluster install --cn --image specula:local --load-image --wait
specula cluster doctor
# Uninstall:
specula cluster uninstall
```

`--cn` applies [`values-cn.yaml`](values-cn.yaml) and embeds `registry.k8s.io` /
`k8s.gcr.io` remote_registries with `layout: huawei-ddn` (Huawei SWR nested path).

Minikube gate: [`scripts/cluster-install-minikube.sh`](../../scripts/cluster-install-minikube.sh)
(`make test-cluster-install`).

## Phases

| Phase | What | How |
|-------|------|-----|
| 0 | Land Specula image / binary | Offline tar, ACR, `minikube image load`, `--load-image` |
| 1 | Point containerd at bootstrap Specula | DaemonSet: `bootstrap-mirror write` |
| 1b | Full node integrate | DaemonSet: `bootstrap-node` (hosts.toml + CRI config_path + doctor) |
| 2 | Warm HA dependency manifests | Job: `bootstrap-prefetch` (opt-in) |
| 3 | Promote to HA chart | Manual `helm upgrade` or installer Job (**local chart path only**) |

Phase 1/1b/2 containers use the **same Specula image** — no busybox / alpine deps.

## Manual helm (same chart)

```bash
helm upgrade --install boot deploy/helm/specula-bootstrap \
  --namespace specula-boot --create-namespace \
  -f deploy/helm/specula-bootstrap/values-cn.yaml \
  --set regionProfile=cn \
  --set image.repository=specula \
  --set image.tag=local \
  --set image.pullPolicy=IfNotPresent \
  --set integrate.enabled=true
```

Legacy smoke: [`scripts/bootstrap-minikube.sh`](../../scripts/bootstrap-minikube.sh).

## China upstreams

A mirror cannot invent connectivity. Hub defaults use DaoCloud; `--cn` also wires
`registry.k8s.io` through Huawei SWR (`layout: huawei-ddn`) → DaoCloud → 1ms.

Confirm the node can reach those URLs.

## Node mirror details

- Endpoint must be `127.0.0.1:<NodePort>` — containerd does **not** use CoreDNS.
- containerd 1.7+ hot-reloads `certs.d`; CRI `config_path` rewrite uses
  `integrate.restartContainerd=once` (host stamp `/var/lib/specula/.cri-reload-hash`
  + wait for Specula `/healthz` before reload). Do not use `true` under helm wait.
- Persistence: `existingClaim` > `hostPath` > created PVC > emptyDir. Pin Specula
  with `nodeSelector` / `specula cluster install --pin-node` / `--pvc`.
- **k3s**: set `mirror.certsDir` to `/var/lib/rancher/k3s/agent/etc/containerd/certs.d`
  (CLI auto-detects). Integrate DS also writes the k3s agent tree on real k3s only.
- Docker runtime: DaemonSet is containerd-oriented.

## Security

`mirror.enabled` / `integrate.enabled` run **privileged** DaemonSets with hostPath
writes. Review before shared/production nodes.

## Promote to HA

Installer Job (`installer.enabled=true`) requires a **local** chart path mounted at
`/charts/specula` — remote `oci://ghcr.io/...` refs are rejected (CN footgun).

```bash
helm upgrade --install specula deploy/helm/specula \
  --namespace specula --create-namespace \
  -f deploy/helm/specula/values-minikube.yaml \
  -f deploy/helm/specula/values-cn.yaml \
  --set image.repository=specula --set image.tag=local
```
