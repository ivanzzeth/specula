# Specula Bootstrap (China / air-gapped self-bootstrap)

Breaks the chicken-and-egg of deploying an image cache when the registries it
needs are blocked: a **zero-dependency** Specula (SQLite + local blob) comes up
first, then the node container runtime pulls **through** it.

Assumes Specula's **own** image (or binary) is already on the node — via
`docker load`, offline tar, or a China-reachable registry (Aliyun ACR, …).

## Phases

| Phase | What | How |
|-------|------|-----|
| 0 | Land Specula image / binary | Offline tar, ACR, `minikube image load` |
| 1 | Point containerd at bootstrap Specula | Privileged DaemonSet: `specula bootstrap-mirror write` |
| 2 | Warm HA dependency manifests | Job: `specula bootstrap-prefetch` (opt-in) |
| 3 | Promote to HA chart | Manual `helm upgrade` or installer Job (opt-in, needs chart URL) |

Phase 1/2 containers use the **same Specula image** — no busybox / alpine deps.

## Install

```bash
# Phase 0: image already present as specula:ha-local (example)
helm upgrade --install boot deploy/helm/specula-bootstrap \
  --namespace specula-boot --create-namespace \
  --set image.repository=specula \
  --set image.tag=ha-local \
  --set image.pullPolicy=IfNotPresent

# Optional Phase 2
helm upgrade boot deploy/helm/specula-bootstrap -n specula-boot \
  --reuse-values --set prefetch.enabled=true
```

Local smoke (containerd minikube): [`scripts/bootstrap-minikube.sh`](../../scripts/bootstrap-minikube.sh).

## China upstreams

A mirror cannot invent connectivity. Defaults use DaoCloud:

```yaml
upstreams:
  oci:
    - name: daocloud
      base_url: https://docker.m.daocloud.io
      priority: 1
```

Override if your region differs. Confirm the node can reach that URL.

## Node mirror details

- Endpoint must be `127.0.0.1:<NodePort>` — containerd does **not** use CoreDNS.
- containerd 1.7+ hot-reloads `certs.d`.
- **k3s**: set `mirror.certsDir` to `/var/lib/rancher/k3s/agent/etc/containerd/certs.d`
  (editing `registries.yaml` still needs a k3s restart — prefer certs.d when possible).
- Docker runtime: DaemonSet is containerd-oriented; configure `daemon.json`
  `registry-mirrors` manually, or use a containerd cluster for automated Phase 1.

## Security

`mirror.enabled=true` runs a **privileged** DaemonSet with hostPath write to
`certs.d`. Review before shared/production nodes. Disable with
`--set mirror.enabled=false` and apply hosts.toml yourself.

## Promote to HA

After dependency images are cached / pullable through the mirror:

```bash
helm upgrade --install specula deploy/helm/specula \
  --namespace specula --create-namespace \
  -f deploy/helm/specula/values-minikube.yaml \
  --set image.repository=specula --set image.tag=ha-local
```

Installer Job (`installer.enabled=true`) is optional and needs a reachable chart ref.
