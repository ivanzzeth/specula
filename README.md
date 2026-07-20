# Specula

[中文](README.zh-CN.md)

**Mirror everything. Verify what you can. Never lie about the rest.**

Specula is a lightweight multi-protocol artifact proxy and Go library. It caches OCI images, Go modules, PyPI, npm, apt, Helm, tarballs, and public git clones — with an **honest, tiered** supply-chain trust model. Use it as a daemon, embed the HTTP handlers, or call the SDK from any Go program.

## Core features

- **8 protocols in one binary** — OCI, Go modules (GOPROXY), PyPI, npm, apt, Helm, tarball, git
- **Honest tiered trust** — `signed` → `consensus` → `tofu` → `checksum` (never claim more than you verified)
- **Verify-on-write** — only verified bytes are served; streaming quarantine, no multi-GB blobs in memory
- **Two-tier cache** — immutable CAS (permanent) + mutable metadata (short TTL / revalidate)
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

Point clients at Specula, for example:

```bash
export GOPROXY=http://127.0.0.1:7732/go,direct
# OCI: configure containerd/docker registry mirror → http://127.0.0.1:7732
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

## Docs

| Doc | Contents |
|-----|----------|
| [docs/LIBRARY.md](docs/LIBRARY.md) | Public `pkg/` API, stability, error contract |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Two-plane design, cache, verify pipeline |
| [docs/PRD.md](docs/PRD.md) | Product requirements |
| [CHANGELOG.md](CHANGELOG.md) | Release notes |

## License

See [LICENSE](LICENSE).
