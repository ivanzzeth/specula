# Specula — Product Requirements Document

> **Tagline**: Mirror everything. Trust nothing.
>
> Specula is a lightweight, high-availability, multi-protocol artifact proxy
> that caches OCI images, PyPI packages, npm modules, Go modules, and apt
> packages in a single Go binary — with built-in supply chain attack
> protection at every protocol layer.

---

## 1. Problem Statement

Modern software supply chains span dozens of package ecosystems. Teams running
air-gapped or CN-region clusters face two compounding problems:

1. **Connectivity**: upstream registries (docker.io, pypi.org, registry.npmjs.org,
   sum.golang.org, archive.ubuntu.com) are slow, throttled, or outright
   unreachable from certain regions or private networks.

2. **Trust**: a single compromised upstream package, a dependency-confusion
   attack, or a silent update can inject malicious code into production.
   No existing lightweight proxy verifies artifact integrity across all
   ecosystems simultaneously.

Existing solutions solve one or the other, never both:

| Tool | Protocols | Supply Chain | Weight |
|---|---|---|---|
| Nexus Repository | OCI, Maven, npm, PyPI, apt | None built-in | ~2 GB JVM |
| Harbor | OCI only | Trivy scan | Heavy |
| Sonatype OSS | Multiple | None | JVM |
| zot | OCI only | None | Light |
| goproxy.io | Go only | None | Light |
| verdaccio | npm only | None | Light |

Specula fills the gap: **one Go binary, all protocols, supply chain
verification built in, horizontal scale from day one**.

---

## 2. Goals

### G1 — Multi-protocol in a single binary

Serve at least:

- **OCI** — container images (Docker v2 + OCI Distribution Spec v1)
- **PyPI** — Python packages (Simple API, PEP 503 / PEP 691)
- **npm** — Node packages (npm registry protocol)
- **Go modules** — Go module proxy (GOPROXY protocol, GONOSUMCHECK)
- **apt** — Debian/Ubuntu packages (InRelease, Packages, pool/)
- **tarball** — Generic binary download cache (URL-keyed)

### G2 — Supply chain attack protection

Every artifact that passes through Specula can be gated by:

- **Checksum verification** — SHA-256 / SHA-512 match against upstream manifest
- **Signature verification** — cosign (OCI), GPG detached signatures (apt, PyPI)
- **Provenance attestation** — SLSA level 1–3 attestation check (OCI)
- **Allowlist / denylist** — per-protocol policy (block known-bad packages)
- **Dependency confusion guard** — private-first resolution for configured
  namespace prefixes; public packages with those names are blocked
- **Freshness gate** — reject artifacts whose upstream metadata is older than a
  configurable window (prevents frozen/stale attack)

Verification policy is per-protocol and per-upstream, expressed in a single
YAML config file.

### G3 — High availability without shared state in the process

Specula instances are **stateless**. All persistent state lives in:

- **Blob storage** — S3-compatible (MinIO, AWS S3, Ceph RGW) for artifact
  blobs and cache
- **Metadata DB** — PostgreSQL (HA via Patroni / CloudNativePG) or SQLite
  (single-node)

Any number of Specula instances can be placed behind a load balancer. There is
no leader election, no gossip protocol, no embedded consensus — just a shared
blob store and a shared DB.

### G4 — Lightweight and operable

- **Single static binary** (`CGO_ENABLED=0`), < 30 MB
- **Low idle memory** — < 64 MB RSS with no active requests
- **Single config file** — one YAML drives all protocols and upstreams
- **Health / readiness endpoints** — `/healthz`, `/readyz`
- **Prometheus metrics** — per-protocol request count, cache hit/miss, upstream
  latency, verification pass/fail
- **Structured JSON logs**
- **DaemonSet-friendly** — can run `hostNetwork: true` on every cluster node;
  clients hit `127.0.0.1:<port>`

### G5 — CN-region first

- Default upstream lists include CN mirror alternatives (DaoCloud, tuna, aliyun,
  npmmirror.com, goproxy.cn)
- Mirror fallback: if primary upstream fails, try next in list
- Configurable per-protocol upstream chain

---

## 3. Non-Goals (v1)

- **No Web UI** — Specula is a daemon. Observability via metrics + logs only.
- **No image scanning** — vulnerability scanning (Trivy, Grype) is out of scope;
  Specula verifies provenance and signatures, not CVE databases.
- **No user authentication for consumers** — Specula is deployed in a trusted
  network segment (cluster-internal or private subnet); mTLS / network policy
  handles perimeter.
- **No Maven / Cargo / Hex support** — future protocol plugins, not v1.
- **No GUI-based policy management** — policies are YAML files, committed to
  git.

---

## 4. Target Users

| User | Scenario |
|---|---|
| **Platform engineer** | Runs Specula as a DaemonSet; every node gets local cache on 127.0.0.1 |
| **Air-gapped cluster operator** | Pre-seeds Specula's blob store; instances serve from cache only (`offline: true`) |
| **Security engineer** | Writes verification policies; receives alerts when signature check fails |
| **CI/CD pipeline** | Points `GOPROXY`, `PIP_INDEX_URL`, `NPM_REGISTRY`, etc. at Specula |

---

## 5. User Stories

### US-1 OCI cache with signature verification
> As a platform engineer, I configure Specula with `cosign_policy: enforce` for
> `docker.io`, so that any image without a valid cosign signature is rejected
> before it reaches the node's containerd.

### US-2 Dependency confusion guard
> As a security engineer, I declare `private_namespaces: ["myorg"]` for npm,
> so that `npm install @myorg/utils` always resolves from my internal registry
> and the public npm mirror is blocked for that namespace.

### US-3 CN-region fast pull
> As a CN cluster operator, I add DaoCloud and tuna mirrors as primary upstreams.
> Specula tries them in order and caches the first successful response. Subsequent
> pulls are served from local blob store at LAN speed.

### US-4 HA with MinIO backend
> As an SRE, I run 3 Specula replicas behind an L4 LB. Each replica reads and
> writes blobs to MinIO and reads metadata from a shared PostgreSQL. Killing any
> single replica does not interrupt artifact delivery.

### US-5 Offline mode
> As an air-gapped operator, I set `mode: offline` per-protocol. Specula serves
> only what is already in blob store and returns 404 for anything missing, without
> making any outbound connection.

### US-6 Apt cache for node bootstrap
> As a cluster node bootstrap script, I set `HTTP_PROXY` pointing to Specula's
> apt endpoint. Package installation speed improves from 2 MB/s (internet) to
> 100 MB/s (LAN), and packages are GPG-verified against the distribution keyring.

---

## 6. Verification Policy Model

```yaml
# specula.yaml (excerpt)
protocols:
  oci:
    upstreams:
      - url: https://docker.m.daocloud.io   # CN mirror, tried first
      - url: https://registry-1.docker.io   # fallback
    verification:
      cosign:
        policy: warn           # warn | enforce | off
        keys: []               # empty = keyless (sigstore)
      checksum: enforce
      allowlist: []            # empty = allow all
      denylist:
        - "docker.io/library/ubuntu:14.04"

  pypi:
    upstreams:
      - url: https://pypi.tuna.tsinghua.edu.cn/simple
      - url: https://pypi.org/simple
    verification:
      checksum: enforce
      dependency_confusion:
        private_namespaces: []
        private_upstream: ""   # url of internal PyPI

  npm:
    upstreams:
      - url: https://registry.npmmirror.com
      - url: https://registry.npmjs.org
    verification:
      checksum: enforce
      dependency_confusion:
        private_namespaces: ["@myorg"]
        private_upstream: "https://npm.internal.example.com"

  go:
    upstreams:
      - url: https://goproxy.cn
      - url: https://proxy.golang.org
    sumdb:
      url: https://sum.golang.org
      policy: enforce          # reject if sum.golang.org disagrees

  apt:
    upstreams:
      - url: http://mirrors.aliyun.com/ubuntu
      - url: http://archive.ubuntu.com/ubuntu
    verification:
      gpg: enforce             # verify InRelease signature
      keyring: /etc/specula/ubuntu-keyring.gpg
```

---

## 7. Metrics (Prometheus)

| Metric | Labels |
|---|---|
| `specula_requests_total` | `protocol`, `method`, `status` |
| `specula_cache_hits_total` | `protocol` |
| `specula_cache_misses_total` | `protocol` |
| `specula_upstream_latency_seconds` | `protocol`, `upstream` |
| `specula_verification_pass_total` | `protocol`, `check` |
| `specula_verification_fail_total` | `protocol`, `check` |
| `specula_blob_store_bytes` | `protocol` |

---

## 8. Protocol Port Defaults

| Protocol | Default Port |
|---|---|
| OCI (Docker v2 + OCI Distribution) | `5000` |
| PyPI Simple API | `5001` |
| npm Registry | `5002` |
| Go Module Proxy | `5003` |
| apt HTTP | `5004` |
| Tarball cache | `5005` |
| Metrics | `9090` |
| Admin / health | `8080` |

All ports are configurable. A single `--bind-all` flag multiplexes everything
on one port using path-based routing (useful for DaemonSet with `hostNetwork`).

---

## 9. Milestones

| Phase | Scope |
|---|---|
| **v0.1** | OCI proxy + blob cache (S3 backend) + checksum verification |
| **v0.2** | Go module proxy + PyPI proxy |
| **v0.3** | npm proxy + apt cache |
| **v0.4** | cosign signature verification (OCI) + GPG (apt) |
| **v0.5** | Dependency confusion guard (npm, PyPI) |
| **v0.6** | PostgreSQL metadata backend + HA deployment manifests |
| **v0.7** | Tarball cache + CN mirror profile built-in |
| **v1.0** | SLSA provenance check + SBOM generation |
