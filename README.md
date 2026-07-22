# Specula

[中文](README.zh-CN.md)

**Mirror everything. Verify what you can. Never lie about the rest.**

Specula is a lightweight multi-protocol artifact proxy and Go library. It caches OCI images, Go modules, PyPI, npm, apt, Helm, tarballs, and public git clones — with an **honest, tiered** supply-chain trust model. Use it as a daemon, embed the HTTP handlers, or call the SDK from any Go program.

## Core features

- **8 protocols in one binary** — OCI, Go modules (GOPROXY), PyPI, npm, apt, Helm, tarball, git
- **Honest tiered trust** — `signed` → `consensus` → `tofu` → `checksum` (never claim more than you verified)
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
cp specula.example.yaml specula.yaml   # edit storage paths / upstreams
make run                               # or: go run ./cmd/specula -config specula.yaml
```

- Data plane (protocols): `http://127.0.0.1:7732`
- Control plane (WebUI): `http://127.0.0.1:7733`

**One-click client wiring** — a single command wires **all** supported protocols
(`go`, `npm`, `pypi`, `oci`, `helm`, `git`, `apt`). Additive only: it does not wipe
existing mirrors.

```bash
make build-go
./bin/specula integrate --addr http://127.0.0.1:7732
# preview only:     ./bin/specula integrate --dry-run
# check state:      ./bin/specula integrate status
# subset only:      ./bin/specula integrate --protocols go,npm
# docker needs sudo: sudo ./bin/specula integrate --protocols oci   # then restart dockerd
```

That is the local/dev path. The per-protocol snippets further down are for CI images,
Kubernetes, or full manual control — not required when `integrate` works for you.

### Install as a system daemon (starts on boot)

```bash
make build-go
sudo ./bin/specula service install          # binary → /usr/local/bin, unit enabled
# or: make install

sudo systemctl status specula
./bin/specula version                       # identity from git tag (release builds)
```

Push a version tag to publish multi-arch binaries **and** the container image via GitHub Actions:

```bash
git tag v0.4.0 && git push origin v0.4.0    # triggers .github/workflows/release.yml
```

Configure repo secrets `DOCKERHUB_USERNAME` + `DOCKERHUB_TOKEN` to publish
`ivanzz/specula` (multi-arch). The image job always runs a **hosted OCI smoke**
first (build → push into an ephemeral Specula → pull back) before Hub.

### Container image

```bash
docker pull ivanzz/specula:v0.4.0          # or :latest on stable tags
docker run --rm -p 7732:7732 -p 7733:7733 \
  -v specula-data:/var/lib/specula \
  ivanzz/specula:v0.4.0
```

Default config is baked at `/etc/specula/specula.yaml` (data under `/var/lib/specula`).
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
curl -sI http://127.0.0.1:7732/go/ | grep -iE 'x-specula|via:'
source ~/.config/specula/env.sh
```

### One-shot pull probe

```bash
./bin/specula bench --addr http://127.0.0.1:7732   # cold/warm probe only — not live stats
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
http.ListenAndServe(":7732", mux)
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
| `apt` | `/apt/` | Tuna / Aliyun Ubuntu, `archive.ubuntu.com` |
| `helm` | `/helm/` | chart repo root (e.g. Bitnami) |
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
      allowed_upstreams: [github.com, gitlab.com, gitee.com]
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
./bin/specula integrate --addr http://127.0.0.1:7732
```

It only **adds** Specula: prepends to lists, uses drop-in files
(`/etc/apt/sources.list.d/specula.list`), preserves unrelated keys, and never
requires running the sections below one-by-one. Use those snippets for CI images,
Kubernetes, or when you want full manual control.

Assume data plane `http://127.0.0.1:7732` (DaemonSet / localhost). Replace with your Specula host in real deployments. Data plane has **no consumer auth** — put it on a trusted network / mTLS perimeter.

### OCI (Docker / containerd / nerdctl)

One-click (same as other protocols — additive; needs **sudo** so live dockerd picks it up):

```bash
sudo ./bin/specula integrate --protocols oci --addr http://127.0.0.1:7732
sudo systemctl restart docker   # apply daemon.json
# verify:
docker info | grep -A5 'Registry Mirrors'
curl -sI http://127.0.0.1:7732/v2/ | grep -i x-specula
```

This updates `/etc/docker/daemon.json`:
- `registry-mirrors` — Specula first (existing mirrors kept)
- `insecure-registries` — `127.0.0.1:7732` when Specula is `http://`

Without sudo, Specula still writes `~/.config/docker/daemon.json` and
`~/.config/specula/docker-daemon.snippet.json`, but **dockerd ignores the user
path** — re-run with sudo for a real one-click.

Manual equivalent:

```jsonc
// /etc/docker/daemon.json — pull-through for docker.io
{
  "registry-mirrors": ["http://127.0.0.1:7732"],
  "insecure-registries": ["127.0.0.1:7732"]
}
```

```toml
# containerd hosts.toml (example for docker.io)
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://docker.io"

[host."http://127.0.0.1:7732"]
  capabilities = ["pull", "resolve"]
```

```bash
# one-off
docker pull 127.0.0.1:7732/library/nginx:latest   # if using as a named registry
```

Specula serves the OCI Distribution API at `/v2/`.

### Go modules

```bash
export GOPROXY=http://127.0.0.1:7732/go,direct
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
export PIP_INDEX_URL=http://127.0.0.1:7732/pypi/simple
export PIP_TRUSTED_HOST=127.0.0.1

# or pip.conf / ~/.config/pip/pip.conf
# [global]
# index-url = http://127.0.0.1:7732/pypi/simple
# trusted-host = 127.0.0.1
```

Use Specula as the **sole** index (`--index-url` only — avoid `--extra-index-url` for dep-confusion safety).

### npm / yarn / pnpm

```bash
npm config set registry http://127.0.0.1:7732/npm/
# yarn
yarn config set registry http://127.0.0.1:7732/npm/
# pnpm
pnpm config set registry http://127.0.0.1:7732/npm/
```

```ini
# .npmrc
registry=http://127.0.0.1:7732/npm/
```

### apt (Debian / Ubuntu)

Point `sources.list` at Specula’s apt mount (paths after `/apt/` mirror a normal Ubuntu archive root: `dists/`, `pool/`):

```text
deb http://127.0.0.1:7732/apt/ jammy main restricted universe multiverse
deb http://127.0.0.1:7732/apt/ jammy-updates main restricted universe multiverse
```

```bash
sudo apt-get update && sudo apt-get install <pkg>
```

Ensure Specula’s `protocols.apt.upstreams` `base_url` matches the distro tree you expose (e.g. `…/ubuntu`).

### Helm

```bash
# classic HTTP chart repo (index.yaml + .tgz)
helm repo add bitnami http://127.0.0.1:7732/helm/bitnami
helm repo update
helm pull bitnami/nginx

# flat repo (index at mount root)
# helm repo add charts http://127.0.0.1:7732/helm/
```

OCI Helm charts use the **OCI** path (`/v2/`), not `/helm/`.

### Tarball (generic downloads)

```bash
# Path encodes host + remote path; host must be allowlisted on Specula
curl -fL 'http://127.0.0.1:7732/tarball/github.com/example/proj/releases/download/v1.0.0/app.tar.gz'
# optional digest pin
curl -fL 'http://127.0.0.1:7732/tarball/…/app.tar.gz?digest=sha256:…'
```

### git

```bash
# clone via Specula (Smart HTTP)
git clone http://127.0.0.1:7732/git/github.com/golang/go.git

# rewrite all github.com HTTPS clones through Specula
git config --global url."http://127.0.0.1:7732/git/github.com/".insteadOf "https://github.com/"
```

Host must be in `protocols.git.git.allowed_upstreams`. Private / push traffic is passed through and not cached.

## HA (multi-replica)

Mature-library stack only: Postgres meta + Redis (redsync) stampede lock + shared CAS
(S3-compatible **or** shared PVC). Chart: [`deploy/helm/specula`](deploy/helm/specula).
Local smoke: `./scripts/ha-minikube.sh`. Details: [ARCHITECTURE §12](docs/ARCHITECTURE.md).

## Bootstrap (China / air-gapped)

When `docker.io` / `registry.k8s.io` are unreachable, land Specula first (offline tar /
ACR / `docker load`), then pull **everything else through Specula**. Chart:
[`deploy/helm/specula-bootstrap`](deploy/helm/specula-bootstrap) (SQLite + local blob,
NodePort, containerd `certs.d` DaemonSet — no busybox). Local smoke (containerd):
`./scripts/bootstrap-minikube.sh`.

## Docs

| Doc | Contents |
|-----|----------|
| [docs/LIBRARY.md](docs/LIBRARY.md) | Public `pkg/` API, stability, error contract |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Two-plane design, cache, verify, **HA matrix** |
| [deploy/helm/specula/README.md](deploy/helm/specula/README.md) | Helm install (Bitnami PG/Redis, optional MinIO) |
| [deploy/helm/specula-bootstrap/README.md](deploy/helm/specula-bootstrap/README.md) | China / air-gapped self-bootstrap |
| [docs/PRD.md](docs/PRD.md) | Product requirements |
| [CHANGELOG.md](CHANGELOG.md) | Release notes |

## License

See [LICENSE](LICENSE).
