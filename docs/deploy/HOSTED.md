# Hosted Specula (one instance, many clusters)

Run **one** stateless Specula and point every other cluster at it, instead of
installing a mirror in each. Verified on Alibaba Cloud ACK + RDS PostgreSQL + OSS,
cn-chengdu.

```bash
cp deploy/profiles/hosted-aliyun.example.yaml specula-deploy.yaml
$EDITOR specula-deploy.yaml                       # fill the <> placeholders
specula cluster install --values specula-deploy.yaml --wait --kubeconfig <path>
```

One file, one command. The chart creates the credential Secret from the profile, so
there is no separate `kubectl create secret` step.

## Why stateless

| | Per-cluster mirror (default) | Hosted (this doc) |
|---|---|---|
| Metadata | SQLite on a volume | PostgreSQL |
| Blobs | local disk CAS | S3-compatible object store |
| Replicas | 1, pinned to a node | HPA, reschedulable |
| Node loss | data survives but the Pod stays Pending until re-pinned | nothing to lose |
| Who it serves | its own cluster's nodes | any cluster that can reach it |

Multi-replica needs a cross-replica stampede lock. `lockDriver: postgres` uses
advisory locks on the metadata pool, so **no Redis** is required.

## The pitfalls, in the order you hit them

Every one of these was hit for real. None is obvious from the config file.

### 1. The image must be pullable from the nodes

Inside China that means your own cloud-region registry over its **VPC** endpoint.
Measured from an ACK cluster in cn-chengdu:

| Source | Result |
|--------|--------|
| `registry-1.docker.io` | unreachable |
| `docker.m.daocloud.io` | **403** |
| `docker.1ms.run`, `docker.xuanyuan.me` | timeout |
| ACR (VPC endpoint) | pulled in 1.9s |

See [RELEASE.md](../RELEASE.md) for wiring ACR into CI, including the trap that an
ACR **instance** domain (`crpi-<id>.cn-<region>.personal.cr.aliyuncs.com`) is not
interchangeable with the legacy shared domain — instance credentials get a 403
against the shared one, which reads like a wrong password.

### 2. RDS PostgreSQL ships with SSL **off**

`sslmode=require` then fails with:

```
tls error: server refused TLS connection
```

Enable it (Aliyun: 数据安全性 → SSL), or use `sslmode=disable` for VPC-internal
traffic. Enabling it is one toggle; prefer that.

### 3. PostgreSQL 15+ revoked `CREATE` on schema `public`

An ordinary account dies at startup:

```
create version table "goose_db_version": permission denied for schema public (SQLSTATE 42501)
```

On PG 15+ the `public` schema is owned by `pg_database_owner`, so **CREATE follows
database ownership**, not account privilege level.

### 4. Aliyun's "高权限账号" is not a superuser

Switching to a privileged account does **not** fix #3 by itself — if the database was
created by someone else, the privileged account still has no CREATE on `public`. What
works is making it the database owner:

```sql
ALTER DATABASE specula OWNER TO specula_admin;
-- or, from scratch:
DROP DATABASE specula; CREATE DATABASE specula OWNER specula_admin;
```

Run it from DMS (RDS console → 登录数据库) as a privileged account.

Note the follow-on: tables are then owned by that account. Moving back to an ordinary
account later needs `REASSIGN OWNED BY specula_admin TO specula`.

### 5. The RAM policy must cover HEAD, not just GET/PUT

The S3 `HeadObject` that Specula uses for its exists-check maps to OSS
`oss:GetObjectMeta`. Without it every cache write fails:

```
s3 HeadObject blobs/sha256/…: StatusCode: 403, api error Forbidden
```

Note this is an **authorization** failure, not a signature one — the request was
signed, reached OSS and came back with a RequestID. Minimum object-level actions:

```
oss:GetObject  oss:GetObjectMeta  oss:PutObject  oss:DeleteObject
oss:AbortMultipartUpload  oss:ListParts
```

plus bucket-level `oss:GetBucketInfo`, `oss:GetBucketLocation`, `oss:ListObjects`.
`AbortMultipartUpload` / `ListParts` are not optional: multi-GB layers use multipart
uploads.

To split a policy problem from a signature problem in one step, attach
`AliyunOSSFullAccess` temporarily. If it works, it was the policy.

### 6. `cache.maxBytes` defaults to 0 — never evict

Object storage then grows without bound. Specula evicts oldest-first and only
**unpinned, cached-origin** entries; pinned entries and content pushed into Specula
are never touched.

**Do not add an OSS lifecycle rule that deletes objects.** Capacity accounting reads
the metadata store, never the bucket, so objects deleted behind Specula's back stay
"present" in metadata and reads fail. A rule that aborts **incomplete** multipart
uploads (7 days) is safe and worth having — an interrupted layer upload otherwise
leaves billable fragments.

### 7. Non-Hub registries need `regionProfile: cn` (or an explicit allowlist)

`remote_registries` is empty by default, and that allowlist is also the SSRF guard —
so a path-style pull of a non-Hub registry returns **404 by design**:

```
warm registry.k8s.io/pause:3.10 (path=registry.k8s.io/pause) -> ERR: manifest GET: HTTP 404
```

Docker Hub works without it (it is the default namespace); ghcr.io, quay.io, gcr.io and
registry.k8s.io do not. `regionProfile: cn` enables the CN mirror chain for all of them
(Huawei SWR `layout: huawei-ddn` → DaoCloud → 1ms), or set `remoteRegistries`
explicitly for a chain of your own.

### 8. Verify with a real fetch, and read the error carefully

A hosted Specula is the only thing fetching from the outside world, so verify its
upstream chain with an actual pull rather than assuming:

```bash
kubectl -n specula-boot run ossprobe --restart=Never --image=<same image> --command -- \
  /specula bootstrap-prefetch --addr http://boot-specula-bootstrap:7733 \
  --images registry.k8s.io/pause:3.10
kubectl -n specula-boot logs ossprobe
```

This is the only client-side check that does the Docker token handshake;
`kubectl get --raw …/v2/…` gets a 401 and proves nothing.

**Read the error before blaming the upstream.** Two failures in the same probe run
looked identical from the client (both HTTP 502) and were completely different:

```
image=library/hello-world  err="cache store manifest: s3 HeadObject …: 403 Forbidden"
image=pause                err="upstream daocloud: HTTP 403"
```

The first had **already fetched from the upstream** and failed writing to object
storage — the upstream was fine. The second was a bad probe: prefetch had stripped the
registry host, so `registry.k8s.io/pause:3.10` was asked of Docker Hub as the repo
`pause`, which does not exist. Two 403s, neither meaning "the mirror is down". That
host-stripping was a real bug and is fixed; the lesson is that the per-image server-side
error says which layer failed, and the client-side 502 does not.

### 9. Changing a credential does not restart anything (fixed)

`helm upgrade` that only alters the ConfigMap or Secret leaves the Pod template
byte-identical, so nothing rolls and CrashLoopBackOff Pods keep the old value. The
chart now carries `checksum/config` and `checksum/creds` annotations, so a content
change is a spec change. If you edit a Secret by hand, you still need
`kubectl rollout restart`.

### 10. A CSI volume is root-owned (fixed)

Not applicable to the stateless shape, but for the record: the image runs as nonroot
(65532) while a freshly provisioned CSI volume mounts `root:root 0755`, so the daemon
could not create its data directories. Both charts now set `fsGroup: 65532`.
minikube's hostpath provisioner hands out 0777 and hides this entirely.

## Node integration is not optional

**Never deploy with `integrate.enabled=false` or `mirror.enabled=false` in a CN
cluster.** The reason Specula is in the cluster at all is that the cluster's nodes pull
images through it; with the node-side agents off, containerd goes straight to the
upstream registries, which in CN means it mostly cannot pull.

```yaml
mirror:
  enabled: true
  endpoint: http://<internal-lb-ip>:7733
  skipVerify: false   # plain http:// — skip_verify would make containerd speak TLS
integrate:
  enabled: true
  protocols: oci
  restartContainerd: once
  reconcileInterval: 5m
```

Both are DaemonSets, so every worker that joins is covered automatically and the 5m
reconcile repairs drift.

Two details that decide whether it works:

- **The endpoint must be reachable from the node.** A profile that exposes an internal
  LoadBalancer has no NodePort, so `http://127.0.0.1:30733` points at nothing — dial the
  CLB address instead. That one address then serves this cluster and every other cluster
  in the VPC.
- **`skipVerify: false` for an http endpoint.** `skip_verify` tells containerd to use
  TLS; against a plain HTTP port that fails every pull.

The only shape where disabling them is defensible is a Specula that exclusively fronts
*other* clusters and whose own nodes are deliberately excluded. That is rare, and it is
not what "deploy Specula in my CN cluster" means.

`--wait` no longer blocks on the integrate DaemonSet when it is absent — it used to poll
for the full five minutes and then report a healthy install as failed — but it now says
plainly that the cluster's nodes are not pointed at Specula, because that is the
consequence you need to notice.

## Letting other clusters in## Letting other clusters in

Expose a VPC address — no public IP, no egress bill:

```yaml
service:
  type: LoadBalancer
  annotations:
    service.beta.kubernetes.io/alibaba-cloud-loadbalancer-address-type: intranet
```

```bash
kubectl -n specula-boot get svc boot-specula-bootstrap \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

Then in each client cluster, deploy the node-side agent only — see
[`deploy/profiles/client-mirror.example.yaml`](../../deploy/profiles/client-mirror.example.yaml).
It is a DaemonSet, so every worker that joins is covered automatically and the 5m
reconcile repairs drift; there is no per-node manual step.

Prefer a private DNS name over the raw LoadBalancer IP: the IP changes if the CLB is
recreated, and although the DaemonSet rewrites `hosts.toml` automatically, pulls fail
in between.

## First login

There is no default account. The first account created while the user count is zero
becomes admin and owner of the default org — so **register immediately** after
install. Set `auth.configSecret` (base64 of exactly 32 bytes) or the JWT secret is
regenerated on every restart: every rollout signs everyone out, and replicas do not
share sessions.

## Acceptance

```bash
specula cluster doctor --kubeconfig <path>     # ready=N/N, /healthz OK, /v2/ → 401
# real fetch through Specula (token handshake), then confirm bytes landed:
kubectl get --raw "/api/v1/namespaces/specula-boot/services/boot-specula-bootstrap:7733/proxy/metrics" \
  | grep 'specula_cache_bytes{protocol="oci"}'
```

`specula_cache_bytes{protocol="oci"} 0` after a successful fetch means the blob store
rejected the write — check the Specula logs for an S3 error before believing the
deployment is healthy.
