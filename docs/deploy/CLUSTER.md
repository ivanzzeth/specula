# Cluster install (`specula cluster`)

CN-safe one-command bootstrap of Specula onto an existing Kubernetes cluster.

## Why

Installing Specula with Helm traditionally hits a chicken-and-egg: the HA chart
pulls Bitnami images from `docker.io`, and the bootstrap installer Job defaulted
to `oci://ghcr.io/...` charts — both blocked in many CN networks. Chorei worked
around this by SSH-ing onto every node to push a binary and run `integrate`.

That orchestration belongs in Specula itself.

## One command, given a kubeconfig

```bash
specula cluster install --cn --wait \
  --kubeconfig /path/to/kubeconfig \
  --image registry-cn-<region>-vpc.aliyuncs.com/<ns>/specula:<version> \
  --storage-class alicloud-disk-essd
specula cluster doctor --kubeconfig /path/to/kubeconfig
```

That is the whole install: Specula Deployment + NodePort, the mirror DaemonSet
writing `certs.d/<registry>/hosts.toml` on every node, and the integrate
DaemonSet fixing the CRI `config_path` and reloading containerd once.

**Two things decide whether it works, and both are about the cluster, not
Specula:**

1. **The image must be pullable from the nodes.** This is the chicken-and-egg —
   see [Image source](#image-source) below. It is the only step that has ever
   failed for a reason outside Specula.
2. **A StorageClass, or explicitly no persistence.** Managed clusters often ship
   several StorageClasses with *none* marked default; `--persist` then leaves the
   PVC Pending forever. Pass `--storage-class` explicitly, or `--host-path`, or
   accept `emptyDir`.

## Image source

Nodes must be able to pull Specula's own image before Specula can mirror anything.

Measured from an Alibaba Cloud ACK cluster in cn-chengdu (2 ECS nodes, Alibaba
Cloud Linux 3, containerd 2.1.9):

| Source | Result |
|--------|--------|
| `registry-1.docker.io` (plain `ivanzz/specula`) | unreachable — ACK nodes had no docker.io mirror |
| `docker.m.daocloud.io/ivanzz/specula` | **403 Forbidden** |
| `docker.1ms.run/...`, `docker.xuanyuan.me/...` | timeout |
| `registry-cn-chengdu.ack.aliyuncs.com/...` | **pulled fine** |

So inside China: publish to your own cloud-region registry and pull over its
**VPC** endpoint — same bytes, no public egress charge. For an ACR personal-edition
instance that is
`crpi-<id>-vpc.cn-<region>.personal.cr.aliyuncs.com`; copy the exact hostname from
the ACR console rather than composing it (the shared
`registry.cn-<region>.aliyuncs.com` domain rejects instance credentials with a
403 that looks like a bad password). [RELEASE.md](../RELEASE.md) wires that into CI; the repo secrets are
a one-time setup.

Outside a cloud, or for a local cluster, `--load-image` side-loads into
minikube/kind/k3d instead.

**Chart and image must be the same version.** The config loader rejects unknown
keys (`ErrorUnused: true`), so a newer chart against an older image crash-loops the
Pod. See the version-skew section of [RELEASE.md](../RELEASE.md).

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
Pod crash and same-node reboot.

### What a lost node actually costs

| Mode | Node lost |
|------|-----------|
| Created PVC / `--pvc` (cloud disk) | **Data survives** — the disk exists independently of the node. But the Deployment carries `nodeSelector: kubernetes.io/hostname=<gone>`, so the Pod stays **Pending forever** until you re-run install with `--pin-node <new-node>`. The disk is zonal: the new node must be in the same zone. |
| `--host-path` | Data is gone with the node. |
| `emptyDir` | Data is gone as soon as the Pod is replaced. |

So the failure mode is **"data intact, service does not self-recover"**, not data loss —
except for host-path and emptyDir, where it is data loss.

### The metadata store is not purely a cache

`meta.db` holds three different things, and only the first is a cache:

- **cache entries** — losing them costs a re-download, nothing more;
- **TOFU pins** — first-seen digests. Losing them resets the trust baseline, so a
  later upstream substitution raises no alert;
- **users, orgs, API keys** — losing them means everyone re-registers, and
  first-registrant-becomes-admin applies again.

`helm uninstall` **deletes the chart-created PVC** along with the release, so an
uninstall discards all of the above. Use `--pvc <your-own-claim>`
(`persistence.existingClaim`) when it should outlive the release.

Genuine node-failure tolerance means leaving local storage: `blob.driver: s3`
(OSS/COS/S3) plus `meta.driver: postgres`, which makes the Pod stateless and
reschedulable. The bootstrap chart does not template that today.

## What gets installed

1. Helm release of the **local** bootstrap chart (never a remote OCI chart).
2. Specula Deployment (NodePort `30733` / `30733`), pinned + optional PVC.
3. `bootstrap-mirror` DaemonSet — writes `certs.d/<registry>/hosts.toml`.
4. `bootstrap-node` integrate DaemonSet — hosts.toml, `integrate --protocols oci`,
   CRI `config_path`, `restart-containerd=once` (stamp + `/healthz` gate), hold +
   reconcile every 5m.

## Managed-cluster notes (ACK and friends)

Verified against Alibaba Cloud ACK, cn-chengdu, kubelet v1.36.1-aliyun.1,
containerd 2.1.9, 2 ECS worker nodes:

- **No default StorageClass.** Four `alicloud-disk-*` classes exist, none marked
  default → pass `--storage-class alicloud-disk-essd` (or `-ssd` / `-efficiency`).
  A created PVC binds normally once the class is explicit.
- **Nodes are real ECS**, so the DaemonSet path applies. On **virtual nodes**
  (ACS / ECI) there is no containerd to configure and this whole approach does not
  apply — image references must be rewritten instead, which Specula does not do yet.
- **Node-pool scaling drifts.** A node that joins later gets its `hosts.toml` only
  once the DaemonSet lands on it, so the first images it pulls go straight out to
  the public internet. The integrate DaemonSet reconciles every 5m, which repairs
  the steady state but not that first window. A node-pool startup script is the
  real fix (not implemented).
- `cluster doctor` probes through the API server, so it works from a laptop
  outside the VPC. Node-side `127.0.0.1:30733` reachability is checked by the
  integrate DaemonSet instead — read its logs, or `kubectl logs ds/...-integrate`.

## Acceptance

```bash
make test-cluster-install
# or: ./scripts/cluster-install-minikube.sh
```

Asserts: install + doctor OK, reload stamp present, Pod delete recovers, hosts.toml
→ `127.0.0.1:30733`, ConfigMap has `huawei-ddn`, `crictl pull registry.k8s.io/pause:3.9`.

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
