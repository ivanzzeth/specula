# Specula — Architecture

---

## 1. Overview

Specula is a **stateless Go daemon** that sits between consumers (cluster nodes,
CI pipelines, developer machines) and upstream artifact registries. It speaks
multiple artifact protocols simultaneously, caches blobs in an S3-compatible
store, and enforces a verification policy before serving any artifact to a
consumer.

```mermaid
graph TB
    subgraph Consumers
        node1[k8s node<br/>containerd]
        ci[CI pipeline]
        dev[developer]
    end

    subgraph Specula Cluster
        direction TB
        s1[Specula instance]
        s2[Specula instance]
        s3[Specula instance]
        lb[Load Balancer / NodePort]
    end

    subgraph Shared State
        blob[(Blob Store<br/>S3 / MinIO)]
        db[(Metadata DB<br/>PostgreSQL / SQLite)]
    end

    subgraph Upstreams
        docker[docker.io<br/>DaoCloud mirror]
        pypi[pypi.org<br/>tuna mirror]
        npm[registry.npmjs.org<br/>npmmirror.com]
        goproxy[proxy.golang.org<br/>goproxy.cn]
        apt[archive.ubuntu.com<br/>aliyun mirror]
    end

    node1 -->|OCI :5000| lb
    ci -->|Go/PyPI/npm| lb
    dev -->|apt :5004| lb

    lb --> s1 & s2 & s3

    s1 & s2 & s3 --> blob
    s1 & s2 & s3 --> db

    s1 -.->|cache miss| docker & pypi & npm & goproxy & apt
```

---

## 2. Internal Component Architecture

```mermaid
graph TD
    subgraph "specula process"
        router[Protocol Router<br/>path / port dispatch]

        subgraph "Protocol Handlers"
            oci[OCI Handler]
            pypi_h[PyPI Handler]
            npm_h[npm Handler]
            go_h[Go Module Handler]
            apt_h[apt Handler]
            tar_h[Tarball Handler]
        end

        subgraph "Core Pipeline"
            policy[Policy Engine]
            verify[Verification Chain]
            cache[Cache Manager]
            upstream[Upstream Client<br/>with fallback + retry]
        end

        subgraph "Storage Drivers"
            s3[S3Driver]
            local[LocalDisk Driver]
            db_driver[DB Driver<br/>PG / SQLite]
        end

        metrics[Metrics Collector<br/>Prometheus]
        health[Health / Admin API]
    end

    router --> oci & pypi_h & npm_h & go_h & apt_h & tar_h
    oci & pypi_h & npm_h & go_h & apt_h & tar_h --> policy
    policy --> verify
    verify --> cache
    cache -->|hit| router
    cache -->|miss| upstream
    upstream -->|blob| cache
    cache --> s3 & local
    cache --> db_driver
```

---

## 3. Request Lifecycle

Every inbound request follows the same pipeline regardless of protocol:

```mermaid
sequenceDiagram
    participant C as Consumer
    participant H as Protocol Handler
    participant P as Policy Engine
    participant V as Verification Chain
    participant CM as Cache Manager
    participant U as Upstream Client
    participant BS as Blob Store

    C->>H: GET /v2/library/nginx/manifests/latest (OCI example)
    H->>P: evaluate(request, artifact_ref)
    P-->>H: ALLOW / DENY / WARN

    alt DENY
        H-->>C: 403 Forbidden + reason
    end

    H->>CM: lookup(artifact_ref)

    alt Cache HIT
        CM->>BS: fetch_blob(digest)
        BS-->>CM: bytes
        CM->>V: verify(blob, metadata)
        V-->>CM: PASS / FAIL
        CM-->>H: bytes
        H-->>C: 200 + artifact
    end

    alt Cache MISS
        CM->>U: fetch(artifact_ref, upstreams[])
        U-->>CM: bytes + upstream_metadata
        CM->>V: verify(bytes, upstream_metadata)
        V-->>CM: PASS / FAIL

        alt FAIL
            H-->>C: 502 + verification error
        end

        CM->>BS: store_blob(digest, bytes)
        CM->>DB: store_metadata(artifact_ref, digest, verified_at)
        CM-->>H: bytes
        H-->>C: 200 + artifact
    end
```

---

## 4. Protocol Handlers

Each protocol handler is a self-contained package that:
1. Implements `http.Handler`
2. Translates its own URL scheme to a canonical `ArtifactRef`
3. Knows how to parse upstream responses for that protocol

```
internal/
  handler/
    oci/       — Docker v2 + OCI Distribution Spec v1
    pypi/      — PEP 503 Simple API + PEP 691 JSON API
    npm/       — npm registry protocol (GET /pkg/-/pkg-ver.tgz)
    gomod/     — GOPROXY protocol (/@v/list, /@v/{version}.info, .mod, .zip)
    apt/       — InRelease, Packages.gz, pool/ fetch
    tarball/   — URL-keyed generic cache
```

### ArtifactRef (canonical internal type)

```go
type ArtifactRef struct {
    Protocol string   // "oci" | "pypi" | "npm" | "go" | "apt" | "tarball"
    Name     string   // image name, package name, module path, …
    Version  string   // tag, version string, suite+component, …
    Digest   string   // sha256:… if known; empty on first lookup
}
```

---

## 5. Verification Chain

The `Verification Chain` is a pluggable pipeline of `Verifier` implementations.
Each verifier receives the blob bytes and the upstream metadata and returns
`PASS`, `WARN`, or `FAIL`. The policy engine decides whether `WARN` is treated
as `FAIL` for a given protocol.

```mermaid
flowchart LR
    blob[blob bytes\n+ metadata]
    cs[ChecksumVerifier\nSHA-256 match]
    cosign_v[CosignVerifier\nkeyless / keyed]
    gpg_v[GPGVerifier\nfor apt InRelease]
    slsa_v[SLSAVerifier\nprovenance attestation]
    allow_v[AllowlistVerifier]
    dep_v[DepConfusionVerifier]

    blob --> cs --> cosign_v --> gpg_v --> slsa_v --> allow_v --> dep_v
```

Verifiers are registered per-protocol. Only relevant verifiers run (e.g. cosign
only for OCI, GPG only for apt).

```go
type Verifier interface {
    Name() string
    Verify(ctx context.Context, ref ArtifactRef, blob []byte, meta UpstreamMeta) (Result, error)
}

type Result struct {
    Status  Status // PASS | WARN | FAIL
    Message string
}
```

---

## 6. Cache Manager

The Cache Manager is protocol-agnostic. It operates on `(ArtifactRef, digest)`
pairs and delegates persistence to a `BlobStore` and a `MetadataStore`.

```mermaid
graph LR
    subgraph CacheManager
        L[lookup\ncheck MetadataStore]
        S[store\nwrite BlobStore + MetadataStore]
        E[evict\nTTL / LRU policy]
    end

    L -->|hit| BlobStore
    S --> BlobStore
    S --> MetadataStore
    E --> BlobStore
    E --> MetadataStore
```

### BlobStore interface

```go
type BlobStore interface {
    Get(ctx context.Context, digest string) (io.ReadCloser, error)
    Put(ctx context.Context, digest string, r io.Reader, size int64) error
    Exists(ctx context.Context, digest string) (bool, error)
    Delete(ctx context.Context, digest string) error
}
```

Implementations:
- `S3Driver` — talks to any S3-compatible endpoint (MinIO, AWS, Ceph)
- `LocalDiskDriver` — stores under a configurable directory (dev / single-node)

### MetadataStore interface

```go
type MetadataStore interface {
    Get(ctx context.Context, ref ArtifactRef) (*CacheEntry, error)
    Put(ctx context.Context, entry CacheEntry) error
    Delete(ctx context.Context, ref ArtifactRef) error
    List(ctx context.Context, protocol string) ([]CacheEntry, error)
}
```

Implementations:
- `PostgresStore` — uses a single `cache_entries` table; safe for concurrent
  Specula instances
- `SQLiteStore` — embedded; suitable for single-instance / DaemonSet deployments
  where each node has its own DB

---

## 7. High-Availability Design

```mermaid
graph TB
    lb[L4 Load Balancer]
    s1[Specula :8080]
    s2[Specula :8080]
    s3[Specula :8080]

    subgraph "Shared State"
        minio[MinIO cluster\nor AWS S3]
        pg[PostgreSQL\nCloudNativePG / Patroni]
    end

    lb --> s1 & s2 & s3
    s1 & s2 & s3 -->|blobs| minio
    s1 & s2 & s3 -->|metadata| pg
```

Key properties:
- **No leader election** — every instance is identical; any instance can serve
  any request
- **Concurrent cache writes are safe** — writing the same blob twice is
  idempotent (same digest → same bytes); the MetadataStore upserts on conflict
- **Upstream fetch deduplication** — a distributed lock (via PostgreSQL advisory
  lock or a short TTL Redis key) prevents cache stampede: the first instance to
  miss fetches from upstream, others wait and then read from cache
- **Rolling upgrades** — new instances come up before old ones drain; zero
  downtime

---

## 8. DaemonSet Deployment (Single-node per-node cache)

For clusters where each node should have a local cache (zero-hop latency), run
Specula as a DaemonSet with `hostNetwork: true`. Each instance uses a local
SQLite metadata store and local-disk blob store (or a shared MinIO).

```mermaid
graph TD
    subgraph "k8s Node A"
        containerd_a[containerd]
        specula_a[Specula DaemonSet pod\nhostNetwork:true\n127.0.0.1:5000]
        disk_a[hostPath /var/specula]
        containerd_a -->|OCI 127.0.0.1:5000| specula_a
        specula_a --> disk_a
    end

    subgraph "k8s Node B"
        containerd_b[containerd]
        specula_b[Specula DaemonSet pod]
        disk_b[hostPath /var/specula]
        containerd_b --> specula_b
        specula_b --> disk_b
    end

    specula_a & specula_b -.->|cache miss| upstream[upstream registries]
```

In this mode, blobs are stored node-local. No shared storage is needed. Each
node fetches from upstream on cold start and caches locally thereafter.

---

## 9. Repository Layout

```
specula/
├── cmd/
│   └── specula/          — main entry point, flag parsing, server bootstrap
├── internal/
│   ├── config/           — YAML config model + validation
│   ├── handler/
│   │   ├── oci/          — OCI Distribution Spec handler
│   │   ├── pypi/         — PyPI Simple API handler
│   │   ├── npm/          — npm registry handler
│   │   ├── gomod/        — Go module proxy handler
│   │   ├── apt/          — apt HTTP handler
│   │   └── tarball/      — generic URL cache handler
│   ├── artifact/         — ArtifactRef, CacheEntry types
│   ├── cache/            — CacheManager, BlobStore, MetadataStore interfaces
│   ├── store/
│   │   ├── s3/           — S3Driver
│   │   ├── local/        — LocalDiskDriver
│   │   ├── postgres/     — PostgresStore
│   │   └── sqlite/       — SQLiteStore
│   ├── upstream/         — UpstreamClient, fallback chain, retry
│   ├── verify/
│   │   ├── checksum.go   — SHA-256/512 verifier
│   │   ├── cosign.go     — cosign keyless + keyed
│   │   ├── gpg.go        — GPG InRelease verifier
│   │   ├── slsa.go       — SLSA provenance verifier
│   │   └── depconfusion.go — dependency confusion guard
│   ├── policy/           — PolicyEngine, per-protocol rule evaluation
│   └── metrics/          — Prometheus collector
├── deploy/
│   ├── k8s/
│   │   ├── daemonset.yaml
│   │   ├── deployment.yaml   — HA deployment
│   │   └── configmap.yaml
│   └── helm/             — Helm chart (future)
├── docs/
│   ├── PRD.md
│   └── ARCHITECTURE.md
├── specula.example.yaml  — annotated config reference
├── Makefile
├── Dockerfile
└── LICENSE
```

---

## 10. Configuration Reference (abbreviated)

```yaml
# specula.yaml
server:
  bind: "0.0.0.0"
  port: 8080           # single-port mode (path-based routing)
  # Or per-protocol ports:
  ports:
    oci: 5000
    pypi: 5001
    npm: 5002
    go: 5003
    apt: 5004
    tarball: 5005
    admin: 8080
    metrics: 9090

storage:
  blobs:
    driver: s3           # s3 | local
    s3:
      endpoint: "http://minio:9000"
      bucket: specula-blobs
      access_key_id: "${MINIO_ACCESS_KEY}"
      secret_access_key: "${MINIO_SECRET_KEY}"
  metadata:
    driver: postgres     # postgres | sqlite
    postgres:
      dsn: "${POSTGRES_DSN}"
    sqlite:
      path: /var/specula/meta.db

cache:
  ttl: 24h
  max_blob_size: 10GB
  eviction: lru

protocols:
  oci:
    enabled: true
    upstreams:
      - https://docker.m.daocloud.io
      - https://registry-1.docker.io
    verification:
      checksum: enforce
      cosign:
        policy: warn
  pypi:
    enabled: true
    upstreams:
      - https://pypi.tuna.tsinghua.edu.cn/simple
      - https://pypi.org/simple
    verification:
      checksum: enforce
  npm:
    enabled: true
    upstreams:
      - https://registry.npmmirror.com
      - https://registry.npmjs.org
    verification:
      checksum: enforce
      dependency_confusion:
        private_namespaces: []
        private_upstream: ""
  go:
    enabled: true
    upstreams:
      - https://goproxy.cn
      - https://proxy.golang.org
    sumdb: https://sum.golang.org
    verification:
      sumdb: enforce
  apt:
    enabled: true
    upstreams:
      - http://mirrors.aliyun.com/ubuntu
      - http://archive.ubuntu.com/ubuntu
    verification:
      gpg: enforce
      keyring: /etc/specula/ubuntu-archive-keyring.gpg
```

---

## 11. Tech Stack

| Concern | Choice | Rationale |
|---|---|---|
| Language | Go 1.22+ | Single static binary, fast HTTP, OCI SDK in Go |
| HTTP router | `net/http` + `gorilla/mux` | Standard, no magic |
| S3 client | `minio-go` | Works with any S3-compatible backend |
| OCI client | `google/go-containerregistry` | Battle-tested, used by crane/skopeo |
| cosign | `sigstore/cosign` (library) | Keyless + keyed signature verification |
| PostgreSQL | `jackc/pgx` | Best-in-class Go PG driver |
| SQLite | `mattn/go-sqlite3` or `modernc.org/sqlite` | Pure Go option for CGO-free builds |
| Metrics | `prometheus/client_golang` | Standard |
| Config | `koanf` | Flexible multi-source config (YAML + env override) |
| Logging | `log/slog` | Structured JSON, stdlib since Go 1.21 |
| Testing | `testify` + `testcontainers-go` | Integration tests against real S3 / PG |
