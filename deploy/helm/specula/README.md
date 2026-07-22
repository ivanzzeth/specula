# Specula HA Helm Chart

Helm 3 umbrella chart for running Specula in multi-replica HA mode. The chart is intentionally thin: it deploys only the Specula `Deployment` and wires mature Bitnami subcharts for shared infrastructure.

## Mature-lib matrix

| Concern | Library / component | Chart / config |
|--------|---------------------|----------------|
| Cross-replica stampede lock | [redsync](https://github.com/go-redsync/redsync) over go-redis | Bitnami Redis (`coalesce.lock_driver=redis`) |
| Metadata store | [pgx](https://github.com/jackc/pgx) | Bitnami PostgreSQL (`storage.meta.driver=postgres`) |
| Shared CAS blobs | AWS SDK S3 **or** local disk on RWX PVC | Bitnami MinIO (optional) **or** `blob.driver=local` + `local.shared=true` |
| HA validation | Go `internal/config` | `server.ha=true` requires postgres + redis + shared blob |

Git protocol is **not** enabled in HA values (bare mirrors are node-local; disable git in the Deployment).

## Prerequisites

- Kubernetes 1.24+
- Helm 3
- Vendored Bitnami subcharts under `charts/` (offline / CN safe):

```bash
./scripts/vendor-helm-deps.sh   # helm pull oci://…/bitnamicharts → charts/
# or (online only, often fails behind the GFW — prefer the script once, commit charts/):
helm dependency update deploy/helm/specula
```

## Install

```bash
cd deploy/helm/specula
./../../../scripts/vendor-helm-deps.sh   # no-op if charts/ already present

# Production-style (S3 via bundled MinIO — swap for external S3 by disabling minio)
helm upgrade --install specula . \
  --namespace specula --create-namespace \
  --set auth.configSecret="$(openssl rand -base64 32)"

# China / GFW — use the Specula-owned CN overlay (protocol upstreams + Bitnami image mirrors):
helm upgrade --install specula . \
  --namespace specula --create-namespace \
  -f values.yaml -f values-cn.yaml \
  --set auth.configSecret="$(openssl rand -base64 32)" \
  --set blob.local.storageClass=longhorn   # or your RWX class

# Minikube demo (builds local image, enables metrics-server, verifies HA + HPA)
./scripts/ha-minikube.sh
```

## Autoscaling (HPA)

Set `autoscaling.enabled=true` and **CPU/memory requests** (required for utilization metrics).
When HPA is on, the Deployment omits `replicas` — the HPA owns the count between `minReplicas` and `maxReplicas`.

Minikube values enable HPA by default (`min=2`, `max=5`, CPU target 60%). Requires the
`metrics-server` addon (`minikube addons enable metrics-server`).

## Blob storage modes

Production does **not** require S3. Choose one:

### S3-compatible (MinIO or external)

```yaml
blob:
  driver: s3
minio:
  enabled: true   # in-cluster MinIO
```

For external object storage, disable MinIO and set endpoint + credentials:

```yaml
minio:
  enabled: false
blob:
  driver: s3
  s3:
    endpoint: https://s3.example.com
    bucket: specula-blobs
    existingSecret: specula-s3-credentials
```

### Local shared PVC (NFS / RWX StorageClass)

```yaml
minio:
  enabled: false
blob:
  driver: local
  local:
    shared: true
    storageClass: nfs-client   # must support ReadWriteMany
    size: 50Gi
```

All replicas mount the same PVC at `blob.local.root` (default `/var/lib/specula/blobs`).

## Ports

| Plane | Port |
|-------|------|
| Data (OCI, npm, pypi, …) | 7732 |
| Control (WebUI + Admin API) | 7733 |

## Bitnami dependencies

| Chart | Version (pinned) | Purpose |
|-------|------------------|---------|
| bitnami/postgresql | 18.8.0 | Metadata |
| bitnami/redis | 27.0.15 | Stampede lock |
| bitnami/minio | 17.0.21 | Optional S3 CAS |

Update versions with `helm search repo bitnami/<chart> --versions` and edit `Chart.yaml`, then `helm dependency update`.

## Minikube quick start

```bash
./scripts/ha-minikube.sh
```

See script output for port-forward commands and an acceptance checklist.

## Caveats

- **RWX storage**: Minikube’s default `standard` StorageClass is `ReadWriteOnce` only. The bundled `values-minikube.yaml` uses MinIO (S3) to avoid RWX. For local+shared blob on minikube, install an NFS (or similar) provisioner and set `blob.local.storageClass`.
- **auth.configSecret**: Required for HA (encrypted runtime settings + shared JWT). Generate with `openssl rand -base64 32`.
- **Image**: Defaults to `ivanzz/specula:latest`; override with `--set image.tag=…`.
- **Resources**: Bitnami charts pull several images; ensure the cluster has enough CPU/memory (minikube: `--memory=8192 --cpus=4` recommended).

## Uninstall

```bash
helm uninstall specula -n specula
# PVCs from Bitnami subcharts may remain unless you delete them manually.
```
