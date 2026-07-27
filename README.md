# Specula

[中文](README.zh-CN.md)

**Mirror everything. Verify what you can. Never lie about the rest.**

Specula is a lightweight multi-protocol artifact proxy and Go library. It caches OCI images, Go modules, PyPI, npm, apt, Helm, tarballs, public git clones, Cargo crates, conda packages, and Hugging Face Hub artifacts — with an **honest, tiered** supply-chain trust model. Use it as a daemon, embed the HTTP handlers, or call the SDK from any Go program.

## Core features

- **11 protocols in one binary** — OCI, Go modules (GOPROXY), PyPI, npm, apt, Helm, tarball, git, Cargo (sparse), conda, Hugging Face Hub
- **Honest tiered trust** — `signed` → `consensus` → `tofu` → `checksum` (never claim more than you verified)
- **Optional maturity cool-down** — warn/block package versions younger than `min_age` (npm/PyPI/Cargo policy gate)
- **Verify-on-write** — only verified bytes are served; streaming quarantine, no multi-GB blobs in memory
- **Two-tier cache** — immutable CAS (permanent) + mutable metadata (short TTL / revalidate); optional `cache.max_bytes` auto-evicts oldest unpinned entries
- **CN-friendly upstreams** — fallback mirrors, auto-block/unblock, Go sumdb passthrough
- **Three integration modes** — daemon · embed into your mux · programmatic SDK

| Tier | Meaning |
|------|---------|
| `signed` | Cryptographic authenticity (sumdb, apt GPG, cosign keyed, Helm `.prov`, …) |
| `consensus` | Independent mirrors agree — not cryptographic authenticity |
| `tofu` | First-seen digest pin + change alert |
| `checksum` | Transport integrity only — **not** a supply-chain control alone |

## Quick start

### Daemon

```bash
git clone https://github.com/ivanzzeth/specula.git
cd specula
make run                               # or: ./bin/specula  (writes specula.yaml from embed if missing)
```

A release binary is the same: drop it anywhere and run — missing `specula.yaml`
is created from the embedded example (data under `~/.specula`).

- Data plane (protocols): `http://127.0.0.1:7733`
- Control plane (WebUI): `http://127.0.0.1:7733`

**One-click client wiring** — a single command wires **all** supported protocols
(`go`, `npm`, `pypi`, `oci`, `helm`, `git`, `apt`). Additive only: it does not wipe
existing mirrors.

```bash
make build-go
./bin/specula integrate --addr https://127.0.0.1:7733
# preview only:     ./bin/specula integrate --dry-run
# check state:      ./bin/specula integrate status
# OCI/CRI preflight: ./bin/specula doctor   # exit 1 on colon config_path / server= / unreachable
# subset only:      ./bin/specula integrate --protocols go,npm
# docker needs sudo: sudo ./bin/specula integrate --protocols oci   # then restart dockerd/containerd
```

That is the local/dev path. The per-protocol snippets further down are for CI images,
Kubernetes, or full manual control — not required when `integrate` works for you.

**Upgrade config (opt-in)** — binary upgrades never rewrite an existing
`specula.yaml`. To pull new reference defaults (multi-archive apt, helm repos,
conda channels, extra git hosts, cargo rsproxy, …) into your file:

```bash
./bin/specula config apply-example --dry-run          # preview
./bin/specula config apply-example --section apt,helm # subset only
./bin/specula config apply-example                    # merge + .bak.<timestamp>
# then restart Specula; follow any printed `integrate` hints for clients
```

Default merge is additive (your values win; string lists union; `{name:…}` lists
merge by name). Comments on existing keys are preserved. Use `--fill-empty` /
`--overwrite` only when you intend to.

### Install as a system daemon (starts on boot)

```bash
make build-go
sudo ./bin/specula install                  # ≡ service install; embeds config → /etc/specula
# or: make install

sudo systemctl status specula
./bin/specula version                       # identity from git tag (release builds)
```

Later upgrades are `scp` + one command — rename-swap (works while the daemon is
live), restart, `/healthz` gate, automatic rollback to `<binary>.prev` on failure:

```bash
scp specula host:/tmp/specula
ssh host 'sudo /tmp/specula upgrade'        # sudo specula rollback  to revert
```

For a single intranet VM serving a whole cluster (TLS, private DNS, disk caps,
fallback mirror), see [docs/deploy/SINGLE-HOST.md](docs/deploy/SINGLE-HOST.md).

Push a version tag to publish multi-arch binaries **and** the container image via GitHub Actions:

```bash
git tag v0.4.0 && git push origin v0.4.0    # triggers .github/workflows/release.yml
```

One build is pushed to **every registry whose secrets are set**; targets with no
credentials are skipped silently. The image job always runs a **hosted OCI smoke**
first (build → push into an ephemeral Specula → pull back) before any push.

| Registry | Repo secrets | Notes |
|----------|--------------|-------|
| Docker Hub | `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` | `ivanzz/specula` |
| **Aliyun ACR** | `ACR_REGISTRY`, `ACR_NAMESPACE`, `ACR_USERNAME`, `ACR_PASSWORD` | **required for CN clusters** — e.g. `registry.cn-chengdu.aliyuncs.com` |
| Huawei SWR | `SWR_REGISTRY`, `SWR_NAMESPACE`, `SWR_USERNAME`, `SWR_PASSWORD` | second CN option |
| GHCR | none (`GITHUB_TOKEN`) | always on; not reachable from CN |

**Inside China a cloud-region registry is the only working source, not a
preference.** Measured from an Alibaba Cloud ACK cluster in cn-chengdu:
`registry-1.docker.io` unreachable, `docker.m.daocloud.io` → 403,
`docker.1ms.run` and `docker.xuanyuan.me` → timeout. Only the cloud's own
registry pulled. Without `ACR_*` (or `SWR_*`) set, a CN cluster cannot bootstrap
Specula's own image — every other layer of the CN story depends on this one.

Pull the image over the **VPC** endpoint from inside the cluster
(`registry-<region>-vpc.aliyuncs.com`): same bytes, no public egress charge.

### Container image

```bash
docker pull ivanzz/specula:v0.4.0          # or :latest on stable tags
docker run --rm -p 7733:7733 -p 7733:7733 \
  -v specula-data:/var/lib/specula \
  ivanzz/specula:v0.4.0
```

Default config is baked at `/etc/specula/specula.yaml` (container data under
`/var/lib/specula`). Local / `make run` defaults to `~/.specula` (no root).
Override with a bind-mount and `--config`, or `SPECULA_*` env vars.

Local build / dogfood your own hosted registry:

```bash
make image                # → specula:<version> and specula:local
make image-smoke          # push that image into ephemeral Specula, pull + digest check
docker run --rm specula:local version
```

### CLI API key (npm-style)

Control-plane automation (`specula stats`, `curl` against `/api/v1/*`) authenticates with a
**Specula API key** (`spck_…`) — the same keys created in the WebUI or via `POST /api/v1/keys`.

**Create a key** (once), from a logged-in session:

1. Open the WebUI at `http://127.0.0.1:7733` → Settings → API keys, **or**
2. HTTP (session cookie / Bearer JWT + active org):

```bash
# After browser login, or with a session JWT:
curl -s -X POST http://127.0.0.1:7733/api/v1/keys \
  -H "Authorization: Bearer <session-jwt>" \
  -H "X-Org-Id: <org-id>" \
  -H 'Content-Type: application/json' \
  -d '{"label":"cli"}'
# Response includes raw_key once — copy it; it is never shown again.
```

**Persist for the CLI** (like `npm login`):

```bash
./bin/specula login --token spck_… --addr http://127.0.0.1:7733
./bin/specula logout                    # remove stored credentials
```

| Source | Purpose |
|--------|---------|
| `~/.config/specula/credentials.json` | Default store (`control_plane` + `token`, mode `0600`) |
| `SPECULA_TOKEN` | Override token (CI / shells) |
| `SPECULA_CONTROL_PLANE` or `SPECULA_ADDR` | Override control-plane base URL |
| `--token` / `--addr` flags | Highest priority for that invocation |

### Live stats (cache + throughput)

While the daemon is serving traffic, Specula continuously records **bytes written** and
**request duration** per protocol. With an API key, `stats` also shows **cache occupancy**:

```bash
./bin/specula stats                     # cache + traffic (uses credentials / env)
./bin/specula stats --watch 2s          # refresh every 2s
./bin/specula stats --traffic-only      # public GET /api/v1/traffic (no auth)
curl -s -H "Authorization: Bearer $SPECULA_TOKEN" \
  http://127.0.0.1:7733/api/v1/stats | jq
# Prometheus: specula_response_bytes_total / specula_request_duration_seconds
```

- `GET /api/v1/stats` — cache + traffic (requires API key or session)
- `GET /api/v1/traffic` — traffic only (unauthenticated)

**Proving traffic hit Specula (not ambient `HTTP_PROXY`):** every data-plane response
includes `X-Specula-Protocol` and `Via: 1.1 specula`. `integrate` also writes
`NO_PROXY`/`no_proxy` for the Specula host into `~/.config/specula/env.sh` — source it
so clients connect to Specula directly instead of via Clash/corporate proxy.

```bash
curl -sI http://127.0.0.1:7733/go/ | grep -iE 'x-specula|via:'
source ~/.config/specula/env.sh
```

### One-shot pull probe

```bash
./bin/specula bench --addr http://127.0.0.1:7733   # cold/warm probe only — not live stats
```

### Go library (SDK)

```bash
go get github.com/ivanzzeth/specula@latest
```

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ivanzzeth/specula/pkg/artifact"
	"github.com/ivanzzeth/specula/pkg/specula"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

func main() {
	ctx := context.Background()
	s, err := specula.New(ctx, specula.Options{
		DataDir: "./data",
		Upstreams: map[string][]upstream.Upstream{
			"gomod": {{Name: "goproxy.cn", BaseURL: "https://goproxy.cn", Priority: 1}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	entry, err := s.Get(ctx, artifact.ArtifactRef{
		Protocol: "gomod",
		Name:     "golang.org/x/mod",
		Version:  "v0.20.0.info",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("tier:", entry.Tier, "digest:", entry.Digest)
}
```

### Embed HTTP handlers

```go
import (
	"github.com/ivanzzeth/specula/pkg/embed"
	"github.com/ivanzzeth/specula/pkg/specula"
)

s, _ := specula.New(ctx, specula.Options{DataDir: "./data", Upstreams: ups})
mux := http.NewServeMux()
embed.Mount(mux, s, embed.Options{Protocols: []string{"gomod", "oci"}})
http.ListenAndServe(":7733", mux)
```

Examples: [`examples/sdk-get-module`](examples/sdk-get-module), [`examples/embed-mux`](examples/embed-mux).

## Configure upstream mirrors

Copy [`specula.example.yaml`](specula.example.yaml) → `specula.yaml`. Under `protocols.<name>.upstreams`, Specula tries mirrors in ascending `priority` and falls back on failure (auto-block / unblock). Mark the authoritative origin with `official: true` (used by consensus / origin checks).

```yaml
protocols:
  oci:
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1          # lower = tried first
        official: false
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 3
        official: true
```

| Protocol (config key) | Mount on data plane | Typical mirrors (`base_url`) |
|-----------------------|---------------------|------------------------------|
| `oci` | `/v2/` | DaoCloud, Aliyun, `registry-1.docker.io` |
| `go` | `/go/` | `goproxy.cn`, `goproxy.io`, `proxy.golang.org` |
| `pypi` | `/pypi/` | Tuna, Aliyun, `pypi.org` |
| `npm` | `/npm/` | `registry.npmmirror.com`, `registry.npmjs.org` |
| `apt` | `/apt/<archive>/` | multi-archive allowlist (`apt.repositories`) |
| `helm` | `/helm/<repo>/` | multi-repo allowlist (`helm.repositories`) |
| `tarball` | `/tarball/` | host allowlist + URL cache |
| `git` | `/git/` | host allowlist (`git.allowed_upstreams`) |

**Go sumdb** (separate from module proxy upstreams):

```yaml
protocols:
  go:
    sumdb:
      url: https://sum.golang.google.cn   # or a goproxy.cn /sumdb/ base
      policy: enforce                     # enforce | warn — never "off"
```

**git** uses a host allowlist (not only the generic `upstreams` list):

```yaml
protocols:
  git:
    git:
      allowed_upstreams: [github.com, gitlab.com, gitee.com, codeberg.org, git.sr.ht, bitbucket.org]
      mirror_dir: /var/specula/git
      public_only: true
```

**Cache size limit** (optional):

```yaml
cache:
  max_bytes: 10737418240   # 10 GiB; 0 = unlimited
```

Full reference: [`specula.example.yaml`](specula.example.yaml). Env overrides: `SPECULA_PROTOCOLS__OCI__…` (see file header).

## Point clients at Specula

**Local/dev: one command for every protocol** (see Quick start):

```bash
./bin/specula integrate --addr http://127.0.0.1:7733
```

It only **adds** Specula: prepends to lists, uses drop-in files
(`/etc/apt/sources.list.d/specula.list`), preserves unrelated keys, and never
requires running the sections below one-by-one. Use those snippets for CI images,
Kubernetes, or when you want full manual control.

Assume data plane `http://127.0.0.1:7733` (DaemonSet / localhost). Replace with your Specula host in real deployments. Data plane has **no consumer auth** — put it on a trusted network / mTLS perimeter.

### OCI (Docker / containerd / nerdctl)

One-click (same as other protocols — additive; needs **sudo** so live dockerd picks it up):

```bash
sudo ./bin/specula integrate --protocols oci --addr http://127.0.0.1:7733
sudo systemctl restart docker   # apply daemon.json
# verify:
docker info | grep -A5 'Registry Mirrors'
curl -sI http://127.0.0.1:7733/v2/ | grep -i x-specula
```

This updates:
- `/etc/docker/daemon.json`: `registry-mirrors` (**docker.io only**) and `insecure-registries`
- `/etc/containerd/certs.d/<registry>/hosts.toml`: non-Hub registries get `override_path` so pulls reach Specula with the host in the path
- `/etc/containerd/config.toml`: forces CRI **and** `transfer.v1.local` `config_path` to the same **single** certs.d directory (containerd 2.2’s default colon CRI path is ignored by transfer; empty transfer `config_path` also skips hosts.toml). Restart containerd after integrate.
- Default `--addr` is `https://127.0.0.1:7733`. If you pass `http://` but the port only speaks TLS, integrate auto-upgrades to `https://` (avoids HTTP 400 in hosts.toml).
- Preflight: `./bin/specula doctor` (alias `integrate doctor`) flags colon/empty CRI+transfer `config_path`, stale effective dump (forgot restart), residual `server=`, k3s wrong certs.d root, missing `registry.k8s.io` hosts, and Specula `/v2/` down — before kubeadm hangs.

Without sudo, Specula still writes user-dir daemon.json / `~/.config/specula/certs.d/`, but
**dockerd/containerd ignore those paths** — re-run with sudo for a real one-click.

Manual equivalent:

```jsonc
// /etc/docker/daemon.json — pull-through for docker.io ONLY
{
  "registry-mirrors": ["http://127.0.0.1:7733"],
  "insecure-registries": ["127.0.0.1:7733"]
}
```

`registry-mirrors` does **not** intercept pulls from other registries (`ghcr.io`,
`codeberg.org`, `quay.io`, …). For those, use path-style pulls or containerd
`certs.d` (see below). `integrate --protocols oci` writes both daemon.json and
containerd hosts.toml.

```bash
# Path-style — works with plain dockerd (image name includes the registry host)
docker pull 127.0.0.1:7733/codeberg.org/forgejo/forgejo:12
docker pull 127.0.0.1:7733/registry.k8s.io/pause:3.9
docker pull 127.0.0.1:7733/ghcr.io/OWNER/IMAGE:tag
```

```toml
# containerd hosts.toml — transparent pull (override_path for non-docker.io)
# Written by: sudo specula integrate --protocols oci
#   or: specula bootstrap-mirror write --endpoint http://127.0.0.1:7733
#
# /etc/containerd/certs.d/docker.io/hosts.toml  (Hub-relative paths)
server = "https://registry-1.docker.io"
[host."http://127.0.0.1:7733"]
  capabilities = ["pull", "resolve"]
  skip_verify = true

# /etc/containerd/certs.d/codeberg.org/hosts.toml
server = "https://codeberg.org"
[host."http://127.0.0.1:7733/v2/codeberg.org"]
  capabilities = ["pull", "resolve"]
  override_path = true
  skip_verify = true
```

Allowlisted hosts are configured under `protocols.oci.oci.remote_registries`
(see `specula.example.yaml`). Unknown host prefixes are rejected (SSRF allowlist).

```bash
# Hub one-off via Specula as a named registry
docker pull 127.0.0.1:7733/library/nginx:latest
```

Specula serves the OCI Distribution API at `/v2/`.

### Go modules

```bash
export GOPROXY=http://127.0.0.1:7733/go,direct
export GOSUMDB=sum.golang.google.cn
# Private modules: keep them off the public sumdb (also configure sumdb.private_patterns)
# export GONOSUMDB=git.internal.corp/*
```

```bash
# verify
go env GOPROXY
go mod download
```

### PyPI (pip / uv / poetry)

```bash
# env (pip / uv)
export PIP_INDEX_URL=http://127.0.0.1:7733/pypi/simple
export PIP_TRUSTED_HOST=127.0.0.1

# or pip.conf / ~/.config/pip/pip.conf
# [global]
# index-url = http://127.0.0.1:7733/pypi/simple
# trusted-host = 127.0.0.1
```

Use Specula as the **sole** index (`--index-url` only — avoid `--extra-index-url` for dep-confusion safety).

### npm / yarn / pnpm

```bash
npm config set registry http://127.0.0.1:7733/npm/
# yarn
yarn config set registry http://127.0.0.1:7733/npm/
# pnpm
pnpm config set registry http://127.0.0.1:7733/npm/
```

```ini
# .npmrc
registry=http://127.0.0.1:7733/npm/
```

### apt (Debian / Ubuntu)

Point `sources.list` at Specula’s apt mount (archive prefix must match `apt.repositories`, e.g. `ubuntu`; paths after that mirror a normal archive root: `dists/`, `pool/`):

```text
deb http://127.0.0.1:7733/apt/ubuntu/ jammy main restricted universe multiverse
deb http://127.0.0.1:7733/apt/ubuntu/ jammy-updates main restricted universe multiverse
```

```bash
sudo apt-get update && sudo apt-get install <pkg>
```

Ensure Specula’s `protocols.apt.apt.repositories` includes the archive name you use in the URL (e.g. `ubuntu`), and that `base_url` points at that distro tree.

### Helm

```bash
# classic HTTP chart repo (index.yaml + .tgz)
helm repo add bitnami http://127.0.0.1:7733/helm/bitnami
helm repo update
helm pull bitnami/nginx

# flat repo (index at mount root)
# helm repo add charts http://127.0.0.1:7733/helm/
```

OCI Helm charts use the **OCI** path (`/v2/`), not `/helm/`.

### Tarball (generic downloads)

```bash
# Path encodes host + remote path; host must be allowlisted on Specula
curl -fL 'http://127.0.0.1:7733/tarball/github.com/example/proj/releases/download/v1.0.0/app.tar.gz'
# optional digest pin
curl -fL 'http://127.0.0.1:7733/tarball/…/app.tar.gz?digest=sha256:…'
```

### git

```bash
# clone via Specula (Smart HTTP)
git clone http://127.0.0.1:7733/git/github.com/golang/go.git

# rewrite all github.com HTTPS clones through Specula
git config --global url."http://127.0.0.1:7733/git/github.com/".insteadOf "https://github.com/"
```

Host must be in `protocols.git.git.allowed_upstreams`. Private / push traffic is passed through and not cached. With `public_only: true`, hosts that lack a visibility probe (anything outside github.com, gitlab.com, gitee.com, codeberg.org, bitbucket.org, git.sr.ht) are treated as non-public and fail closed — not mirrored.

### Cargo (sparse registry)

```bash
./bin/specula integrate --protocols cargo --addr http://127.0.0.1:7733
# writes ~/.cargo/config.toml source replace → sparse+http://127.0.0.1:7733/cargo/index/
cargo fetch
```

### conda

```bash
./bin/specula integrate --protocols conda --addr http://127.0.0.1:7733
# prepends channel http://127.0.0.1:7733/conda/conda-forge in ~/.condarc
micromamba create -y -n demo -c http://127.0.0.1:7733/conda/conda-forge --override-channels ca-certificates
```

### Hugging Face Hub

```bash
./bin/specula integrate --protocols hf --addr http://127.0.0.1:7733
# exports HF_ENDPOINT=http://127.0.0.1:7733/hf via ~/.config/specula/env.sh
source ~/.config/specula/env.sh
huggingface-cli download hf-internal-testing/tiny-random-bert --local-dir /tmp/hf-tiny
```

## Offline / air-gap (`server.mode: offline`)

Warm Specula while online, then restart with `mode: offline`. Cache hits keep
working; misses return **404** and Specula makes **no outbound** fetches (git
mirrors are served as-is — no clone/refresh).

Full operator cookbook (prefetch, containerd `certs.d`, checklist):
**[docs/deploy/OFFLINE.md](docs/deploy/OFFLINE.md)**.

```yaml
server:
  mode: offline   # restart required to switch; empty/online = normal pull-through
```

```bash
# 1) online: warm
docker pull 127.0.0.1:7733/registry.k8s.io/pause:3.9
# 2) set mode: offline, restart daemon
# 3) hit works; uncached tag fails fast
./scripts/realclient-offline.sh
```

## HA (multi-replica)

Mature-library stack only: Postgres meta + Redis (redsync) stampede lock + shared CAS
(S3-compatible **or** shared PVC). Chart: [`deploy/helm/specula`](deploy/helm/specula).
Local smoke: `./scripts/ha-minikube.sh`. Details: [ARCHITECTURE §12](docs/ARCHITECTURE.md).

## Bootstrap (China / air-gapped)

When `docker.io` / `registry.k8s.io` are unreachable, land Specula first (offline tar /
ACR / `docker load`), then pull **everything else through Specula**.

**One command** (local chart + node integrate DaemonSet — no remote OCI charts):

```bash
specula cluster install --cn --image specula:local --load-image --wait
# smoke: ./scripts/cluster-install-minikube.sh  or  make test-cluster-install
```

Chart: [`deploy/helm/specula-bootstrap`](deploy/helm/specula-bootstrap). Details:
[docs/deploy/CLUSTER.md](docs/deploy/CLUSTER.md).

## Docs

Full map with categories: **[docs/README.md](docs/README.md)**.

**Deploy** — pick the shape first, it drives TLS, disk and failure domain:

| Shape | Doc |
|-------|-----|
| One VM + systemd (intranet, `scp` + `specula upgrade`) | [docs/deploy/SINGLE-HOST.md](docs/deploy/SINGLE-HOST.md) |
| Into a cluster (`specula cluster install --cn`, one command with a kubeconfig) | [docs/deploy/CLUSTER.md](docs/deploy/CLUSTER.md) |
| Hosted — one stateless instance serving many clusters | [docs/deploy/HOSTED.md](docs/deploy/HOSTED.md) |
| Air-gapped / offline | [docs/deploy/OFFLINE.md](docs/deploy/OFFLINE.md) |

**Operate / build on it:**

| Doc | Contents |
|-----|----------|
| [docs/RELEASE.md](docs/RELEASE.md) | Release process, multi-registry secrets, **why CN needs ACR** |
| [docs/TRUST.md](docs/TRUST.md) | Cosign / apt GPG / Helm `.prov` / dep-confusion cookbook + oracles |
| [docs/LIBRARY.md](docs/LIBRARY.md) | Public `pkg/` API, stability, error contract |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Two-plane design, cache, verify, **HA matrix** |
| [deploy/helm/specula-bootstrap/README.md](deploy/helm/specula-bootstrap/README.md) | Bootstrap chart (China / air-gapped self-bootstrap) |
| [deploy/helm/specula/README.md](deploy/helm/specula/README.md) | HA chart (Postgres/Redis, optional MinIO) |
| [docs/PRD.md](docs/PRD.md) | Product requirements |
| [CHANGELOG.md](CHANGELOG.md) | Release notes |

## License

See [LICENSE](LICENSE).
