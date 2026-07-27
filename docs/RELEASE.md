# Release process

Cutting a release is one tag push. Everything else is CI
([`.github/workflows/release.yml`](../.github/workflows/release.yml)).

## One-time setup: repo secrets

One multi-arch build is pushed to **every registry whose secrets are set**;
targets with no credentials are skipped silently, so a fork with none still
releases binaries and passes.

| Registry | Secrets | Notes |
|----------|---------|-------|
| Docker Hub | `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` | `ivanzz/specula` |
| **Aliyun ACR** | `ACR_REGISTRY`, `ACR_NAMESPACE`, `ACR_USERNAME`, `ACR_PASSWORD` | **required for CN clusters** |
| Huawei SWR | `SWR_REGISTRY`, `SWR_NAMESPACE`, `SWR_USERNAME`, `SWR_PASSWORD` | second CN option |
| GHCR | none — uses `GITHUB_TOKEN` | always on; unreachable from CN |

`ACR_REGISTRY` is the **public** endpoint (`registry.cn-<region>.aliyuncs.com`)
because CI pushes from GitHub runners. Clusters *pull* over the VPC endpoint
(`registry-cn-<region>-vpc.aliyuncs.com`) — same bytes, no public egress charge.

### Why a CN registry is mandatory, not preferred

Measured from an Alibaba Cloud ACK cluster in cn-chengdu (2 ECS nodes,
containerd 2.1.9), pulling Specula's own image:

| Source | Result |
|--------|--------|
| `registry-1.docker.io` | unreachable |
| `docker.m.daocloud.io` | **403 Forbidden** |
| `docker.1ms.run` | timeout |
| `docker.xuanyuan.me` | timeout |
| `registry-cn-chengdu.ack.aliyuncs.com` | **pulled fine** |

Every other layer of the CN story — `cluster install`, the mirror DaemonSet, node
`integrate` — sits on top of that first pull. Without `ACR_*` (or `SWR_*`) a CN
cluster cannot bootstrap Specula at all.

## Cutting a release

```bash
# 1. Gates green (see the table in CLAUDE.md for what each covers)
make test-unit
make test-integration
make test-single-host        # systemd install/upgrade/rollback, real systemd in a container
make test-cluster-install    # cluster install --cn on minikube + containerd

# 2. CHANGELOG.md: turn the pending section into a dated one
#    ## [0.11.0] — <headline> — YYYY-MM-DD

# 3. Tag and push. Only the tag triggers the workflow.
git tag v0.11.0
git push origin main
git push origin v0.11.0
```

Version identity comes from the tag via `-ldflags` into `internal/version`, so a
release binary reports the tag and `specula version` is the check that it took.

## What CI does

Two jobs, both from the tag:

**`release`** — builds the WebUI, cross-compiles five targets
(`linux/{amd64,arm64}`, `darwin/{amd64,arm64}`, `windows/amd64`), asserts the
linux/amd64 binary prints the tag, writes `SHA256SUMS`, and creates the GitHub
Release with the binaries attached.

**`image`** — builds the WebUI, builds a linux/amd64 image, runs the **hosted OCI
smoke** (`scripts/publish-image-smoke.sh`: push the product image into an
ephemeral Specula, pull it back) *before* any registry push, then logs in to each
configured registry and does one multi-arch (`amd64` + `arm64`) build pushed to
all of them. Published coordinates land in the job summary and the release body.

`:latest` is added only for stable `vX.Y.Z` — never for `-rc` / `-beta` /
`-alpha`.

## Verifying a release

```bash
gh run watch <run-id> --exit-status          # or: gh run list --workflow=release.yml

# Pull each coordinate you care about
docker pull ivanzz/specula:v0.11.0
docker pull registry.cn-<region>.aliyuncs.com/<ns>/specula:v0.11.0

# Then deploy it — see docs/deploy/CLUSTER.md
specula cluster install --cn --wait \
  --image registry-cn-<region>-vpc.aliyuncs.com/<ns>/specula:v0.11.0 \
  --storage-class alicloud-disk-essd
```

## Chart / binary version skew

The config loader runs with `ErrorUnused: true` — an unknown YAML key is a hard
startup error, not a warning. So a chart that emits a key the image's binary does
not know will crash-loop the Pod. Concretely: the chart in this repo emits
`storage.quarantine_dir`, which binaries before v0.11.0 reject.

**Keep the chart and the image on the same version.** When testing an older
published image, point `--chart-dir` at a worktree of that tag:

```bash
git worktree add /tmp/specula-v0.10.0 v0.10.0
specula cluster install --chart-dir /tmp/specula-v0.10.0/deploy/helm/specula-bootstrap ...
```

## Release checklist

- [ ] `make test-unit` / `test-integration` green
- [ ] `make test-single-host` green (systemd upgrade + rollback)
- [ ] `make test-cluster-install` green (cluster install on minikube)
- [ ] `CHANGELOG.md` section dated, headline written
- [ ] `docs/LIBRARY.md` updated if `pkg/**` changed
- [ ] tag pushed; `gh run watch` green
- [ ] every configured coordinate pulls
- [ ] chart and image versions match for any deployment you hand to someone else
