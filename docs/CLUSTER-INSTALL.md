# Cluster install (`specula cluster`)

CN-safe one-command bootstrap of Specula onto an existing Kubernetes cluster.

## Why

Installing Specula with Helm traditionally hits a chicken-and-egg: the HA chart
pulls Bitnami images from `docker.io`, and the bootstrap installer Job defaulted
to `oci://ghcr.io/...` charts — both blocked in many CN networks. Chorei worked
around this by SSH-ing onto every node to push a binary and run `integrate`.

That orchestration belongs in Specula itself.

## Commands

```bash
specula cluster install --cn --image specula:local --load-image --wait
specula cluster install --cn --pvc my-cache-pvc --wait   # existing PVC
specula cluster install --cn --host-path /var/lib/specula-bootstrap --wait
specula cluster integrate --wait          # re-run node DaemonSet
specula cluster doctor                    # /healthz + /v2/ + DS ready
specula cluster uninstall
```

| Flag | Meaning |
|------|---------|
| `--cn` | `values-cn.yaml` + `regionProfile=cn` (Huawei SWR `layout: huawei-ddn` for k8s) |
| `--load-image` | `minikube image load` / `kind load` / `k3d image import` |
| `--pvc` | Mount existing PVC (`persistence.existingClaim`) |
| `--host-path` | Node-local dir when there is no StorageClass |
| `--persist` / `--storage-class` / `--pvc-size` | Created PVC controls (default: persist on if default SC exists) |
| `--pin-node` / `--skip-pin-node` | Pin Specula Deployment to a hostname (default: auto worker) |
| `--chart-dir` | Path to `deploy/helm/specula-bootstrap` (or `SPECULA_BOOTSTRAP_CHART`) |
| `--kubeconfig` / `--context` | Standard kubectl targeting |

Default path is **bootstrap-only** (SQLite, single replica). `--ha` only prints
guidance — promote manually after preloading dependency images.

## Persistence modes

Priority: `--pvc` / `existingClaim` > `--host-path` > created PVC > `emptyDir`.

Specula is pinned to one node so RWO volumes stay schedulable. **Self-heal** covers
Pod crash and same-node reboot; a lost node does not migrate the cache.

## What gets installed

1. Helm release of the **local** bootstrap chart (never a remote OCI chart).
2. Specula Deployment (NodePort `30732` / `30733`), pinned + optional PVC.
3. `bootstrap-mirror` DaemonSet — writes `certs.d/<registry>/hosts.toml`.
4. `bootstrap-node` integrate DaemonSet — hosts.toml, `integrate --protocols oci`,
   CRI `config_path`, `restart-containerd=once` (stamp + `/healthz` gate), hold +
   reconcile every 5m.

## Acceptance

```bash
make test-cluster-install
# or: ./scripts/cluster-install-minikube.sh
```

Asserts: install + doctor OK, reload stamp present, Pod delete recovers, hosts.toml
→ `127.0.0.1:30732`, ConfigMap has `huawei-ddn`, `crictl pull registry.k8s.io/pause:3.9`.

## Chorei migration

Replace `ensure_standalone_specula` / `reintegrate_specula` SSH loops with:

```python
subprocess.run(
    ["specula", "cluster", "install", "--cn", "--kubeconfig", path, "--wait"],
    check=True,
)
subprocess.run(
    ["specula", "cluster", "integrate", "--kubeconfig", path, "--wait"],
    check=True,
)
```

Device inventory, vault push credentials, and TLS SAN minting can stay in chorei;
node OCI wiring should not.
