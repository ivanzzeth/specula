# Specula

[English](README.md)

**能镜像的都镜像。能验证的就验证。其余绝不谎称。**

Specula 是一个轻量的多协议制品代理，也是可嵌入的 Go 库。它缓存 OCI 镜像、Go modules、PyPI、npm、apt、Helm、通用 tarball 与公共 git clone，并采用**诚实分级**的供应链信任模型。可作守护进程部署、嵌入你的 HTTP 服务，或在任意 Go 项目里直接调用 SDK。

## 核心功能

- **单二进制 8 协议** — OCI、Go modules（GOPROXY）、PyPI、npm、apt、Helm、tarball、git
- **诚实分级信任** — `signed` → `consensus` → `tofu` → `checksum`（达到哪档就只报哪档）
- **写时验证** — 只对外服务已验证字节；流式隔离区，多 GB 工件不全量入内存
- **二层缓存** — 不可变 CAS（永久）+ 可变元数据（短 TTL / 条件重验）
- **CN 区域友好** — 镜像 fallback、自动封禁/解封、Go sumdb 透传
- **三种接入方式** — 守护进程 · 嵌入 mux · 程序化 SDK

| 档位 | 含义 |
|------|------|
| `signed` | 密码学真实性（sumdb、apt GPG、cosign keyed、Helm `.prov` 等） |
| `consensus` | 多镜像 digest 一致 — **非**密码学真实性 |
| `tofu` | 首次锁定 digest + 变更告警 |
| `checksum` | 仅防传输损坏 — **绝不能单独当作供应链防护** |

## 快速开始

### 守护进程

```bash
git clone https://github.com/ivanzzeth/specula.git
cd specula
cp specula.example.yaml specula.yaml   # 改存储路径 / 上游
make run                               # 或: go run ./cmd/specula -config specula.yaml
```

- 数据面（协议）：`http://127.0.0.1:7732`
- 管理面（WebUI）：`http://127.0.0.1:7733`

客户端指向 Specula，例如：

```bash
export GOPROXY=http://127.0.0.1:7732/go,direct
# OCI：把 containerd/docker registry mirror 指到 http://127.0.0.1:7732
```

### Go 库（SDK）

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

### 嵌入 HTTP Handler

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

示例：[`examples/sdk-get-module`](examples/sdk-get-module)、[`examples/embed-mux`](examples/embed-mux)。

## 文档

| 文档 | 内容 |
|------|------|
| [docs/LIBRARY.md](docs/LIBRARY.md) | 公开 `pkg/` API、稳定性、错误契约 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 双平面、缓存、验证管线 |
| [docs/PRD.md](docs/PRD.md) | 产品需求 |
| [CHANGELOG.md](CHANGELOG.md) | 变更记录 |

## 许可证

见 [LICENSE](LICENSE)。
