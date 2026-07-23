# Specula

[English](README.md)

**能镜像的都镜像。能验证的就验证。其余绝不谎称。**

Specula 是一个轻量的多协议制品代理，也是可嵌入的 Go 库。它缓存 OCI 镜像、Go modules、PyPI、npm、apt、Helm、通用 tarball、公共 git clone、Cargo crate、conda 包与 Hugging Face Hub 制品，并采用**诚实分级**的供应链信任模型。可作守护进程部署、嵌入你的 HTTP 服务，或在任意 Go 项目里直接调用 SDK。

## 核心功能

- **单二进制 11 协议** — OCI、Go modules（GOPROXY）、PyPI、npm、apt、Helm、tarball、git、Cargo（sparse）、conda、Hugging Face Hub
- **诚实分级信任** — `signed` → `consensus` → `tofu` → `checksum`（达到哪档就只报哪档）
- **写时验证** — 只对外服务已验证字节；流式隔离区，多 GB 工件不全量入内存
- **二层缓存** — 不可变 CAS（永久）+ 可变元数据（短 TTL / 条件重验）；可选 `cache.max_bytes` 自动淘汰最旧未 pin 条目
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
make run                               # 或: ./bin/specula  （无配置时从内嵌示例写出 specula.yaml）
```

发版二进制同样：放到任意目录直接跑——缺少 `specula.yaml` 时从内嵌示例自动创建（数据在 `~/.specula`）。

- 数据面（协议）：`http://127.0.0.1:7732`
- 管理面（WebUI）：`http://127.0.0.1:7733`

**一键接入客户端** — **一条命令接齐全部协议**（`go` / `npm` / `pypi` / `oci` /
`helm` / `git` / `apt`）。只做增量写入，不覆盖已有镜像配置。

```bash
make build-go
./bin/specula integrate --addr http://127.0.0.1:7732
# 仅预览：          ./bin/specula integrate --dry-run
# 查看状态：        ./bin/specula integrate status
# 只接部分协议：    ./bin/specula integrate --protocols go,npm
# Docker 需 sudo：  sudo ./bin/specula integrate --protocols oci   # 然后重启 dockerd
```

本机 / 开发机用上面这一条即可。下文各协议手动片段是给 CI 镜像、Kubernetes
或完全手控用的——能跑 `integrate` 时不必逐段照抄。

### 安装为系统守护进程（开机自启）

```bash
make build-go
sudo ./bin/specula install                  # ≡ service install；内嵌配置 → /etc/specula
# 或: make install

sudo systemctl status specula
./bin/specula version                       # 版本来自 git tag（发版构建）
```

打版本 tag 后 GitHub Actions 会发布**多架构二进制**与**容器镜像**：

```bash
git tag v0.4.0 && git push origin v0.4.0    # 触发 .github/workflows/release.yml
```

配置仓库 secrets `DOCKERHUB_USERNAME` + `DOCKERHUB_TOKEN` 后会推送
`ivanzz/specula`（多架构）。镜像 job 会先跑 **hosted OCI 冒烟**
（构建 → 推进临时 Specula → 拉回校验），再推 Docker Hub。

### 容器镜像

```bash
docker pull ivanzz/specula:v0.4.0          # 稳定 tag 另有 :latest
docker run --rm -p 7732:7732 -p 7733:7733 \
  -v specula-data:/var/lib/specula \
  ivanzz/specula:v0.4.0
```

默认配置在 `/etc/specula/specula.yaml`（容器数据目录 `/var/lib/specula`）。
本地 / `make run` 默认走 `~/.specula`（无需 root）。
可用挂载 + `--config`，或 `SPECULA_*` 环境变量覆盖。

本地构建 / 用自家 hosted registry 冒烟：

```bash
make image                # → specula:<version> 与 specula:local
make image-smoke          # 把镜像推进临时 Specula，再 pull 比对 digest
docker run --rm specula:local version
```

### CLI API key（类 npm 全局凭证）

控制面自动化（`specula stats`、对 `/api/v1/*` 的 `curl`）使用 **Specula API key**
（`spck_…`）鉴权——与 WebUI / `POST /api/v1/keys` 创建的是同一套 key。

**创建 key**（只需一次），需已登录会话：

1. 打开 WebUI `http://127.0.0.1:7733` → Settings → API keys，或
2. HTTP（session cookie / Bearer JWT + 当前组织）：

```bash
# 浏览器登录后，或持有 session JWT：
curl -s -X POST http://127.0.0.1:7733/api/v1/keys \
  -H "Authorization: Bearer <session-jwt>" \
  -H "X-Org-Id: <org-id>" \
  -H 'Content-Type: application/json' \
  -d '{"label":"cli"}'
# 响应里的 raw_key 只出现一次，请立刻保存。
```

**持久化到 CLI**（类似 `npm login`）：

```bash
./bin/specula login --token spck_… --addr http://127.0.0.1:7733
./bin/specula logout                    # 删除本地凭证
```

| 来源 | 作用 |
|------|------|
| `~/.config/specula/credentials.json` | 默认存储（`control_plane` + `token`，权限 `0600`） |
| `SPECULA_TOKEN` | 覆盖 token（CI / shell） |
| `SPECULA_CONTROL_PLANE` 或 `SPECULA_ADDR` | 覆盖控制面地址 |
| `--token` / `--addr` 参数 | 单次调用优先级最高 |

### 运行时统计（缓存占用 + 吞吐）

daemon 在服务流量时会持续记录各协议的**写出字节数**和**请求耗时**。
配置 API key 后，`stats` 还会显示**缓存占用**：

```bash
./bin/specula stats                     # 缓存 + 吞吐（读凭证 / 环境变量）
./bin/specula stats --watch 2s          # 每 2 秒刷新
./bin/specula stats --traffic-only      # 公开 GET /api/v1/traffic（无需鉴权）
curl -s -H "Authorization: Bearer $SPECULA_TOKEN" \
  http://127.0.0.1:7733/api/v1/stats | jq
# Prometheus: specula_response_bytes_total / specula_request_duration_seconds
```

- `GET /api/v1/stats` — 缓存 + 吞吐（需 API key 或 session）
- `GET /api/v1/traffic` — 仅吞吐（无需鉴权）

**证明流量进了 Specula（而不是环境里的 `HTTP_PROXY`）：** 数据面响应带
`X-Specula-Protocol` 与 `Via: 1.1 specula`。`integrate` 还会把 Specula 主机写进
`~/.config/specula/env.sh` 的 `NO_PROXY`/`no_proxy`——source 后客户端直连 Specula，
不经 Clash/公司代理。

```bash
curl -sI http://127.0.0.1:7732/go/ | grep -iE 'x-specula|via:'
source ~/.config/specula/env.sh
```

### 一次性拉取探针

```bash
./bin/specula bench --addr http://127.0.0.1:7732   # 仅冷/热探针，不是实时统计
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

## 配置各协议上游镜像

复制 [`specula.example.yaml`](specula.example.yaml) → `specula.yaml`。在 `protocols.<名称>.upstreams` 下配置镜像：按 `priority` 升序尝试，失败则 fallback（自动封禁/解封）。权威源标记 `official: true`（供共识 / origin-check 使用）。

```yaml
protocols:
  oci:
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1          # 越小越优先
        official: false
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 3
        official: true
```

| 协议（配置键） | 数据面挂载路径 | 常用镜像 (`base_url`) |
|----------------|----------------|----------------------|
| `oci` | `/v2/` | DaoCloud、阿里云、`registry-1.docker.io` |
| `go` | `/go/` | `goproxy.cn`、`goproxy.io`、`proxy.golang.org` |
| `pypi` | `/pypi/` | 清华 Tuna、阿里云、`pypi.org` |
| `npm` | `/npm/` | `registry.npmmirror.com`、`registry.npmjs.org` |
| `apt` | `/apt/<archive>/` | 多归档白名单（`apt.repositories`） |
| `helm` | `/helm/<repo>/` | 多仓库白名单（`helm.repositories`） |
| `tarball` | `/tarball/` | 允许的下载主机 + URL 缓存 |
| `git` | `/git/` | 主机白名单（`git.allowed_upstreams`） |

**Go sumdb**（与 module 上游列表分开配）：

```yaml
protocols:
  go:
    sumdb:
      url: https://sum.golang.google.cn   # 或 goproxy.cn 的 /sumdb/ 基址
      policy: enforce                     # enforce | warn — 绝不要 off
```

**git** 以主机白名单为准（不只看通用 `upstreams`）：

```yaml
protocols:
  git:
    git:
      allowed_upstreams: [github.com, gitlab.com, gitee.com, codeberg.org, git.sr.ht, bitbucket.org]
      mirror_dir: /var/specula/git
      public_only: true
```

**缓存容量上限**（可选）：

```yaml
cache:
  max_bytes: 10737418240   # 10 GiB；0 = 不限制
```

完整示例见 [`specula.example.yaml`](specula.example.yaml)。环境变量覆盖：`SPECULA_PROTOCOLS__OCI__…`（见文件头说明）。

## 把客户端接到 Specula

**本机 / 开发机：一条命令接齐全部协议**（见快速开始）：

```bash
./bin/specula integrate --addr http://127.0.0.1:7732
```

只**新增** Specula（列表前置、apt drop-in、保留无关配置键），不必再按协议
逐段配置。下面的片段留给 CI 镜像、Kubernetes 或完全手控。

以下默认数据面为 `http://127.0.0.1:7732`（本机 / DaemonSet）。生产请换成实际 Specula 地址。数据面**无消费者认证**——放在可信网段或用 mTLS/网络策略挡边界。

### OCI（Docker / containerd / nerdctl）

一键接入（与其它协议相同，增量写入；**需要 sudo**，dockerd 才读得到）：

```bash
sudo ./bin/specula integrate --protocols oci --addr http://127.0.0.1:7732
sudo systemctl restart docker   # 让 daemon.json 生效
# 验证：
docker info | grep -A5 'Registry Mirrors'
curl -sI http://127.0.0.1:7732/v2/ | grep -i x-specula
```

会更新：
- `/etc/docker/daemon.json`：`registry-mirrors`（**仅 docker.io**）与 `insecure-registries`
- `/etc/containerd/certs.d/<registry>/hosts.toml`：非 Hub 仓库带 `override_path`，透明重定向到 Specula 路径式回源

无 sudo 时仍会写用户目录下的 daemon.json / `~/.config/specula/certs.d/`，但 **dockerd/containerd 默认不读用户路径** — 真正一键请加 sudo。

手动等价配置：

```jsonc
// /etc/docker/daemon.json — 仅 docker.io 走 Specula
{
  "registry-mirrors": ["http://127.0.0.1:7732"],
  "insecure-registries": ["127.0.0.1:7732"]
}
```

`registry-mirrors` **不会**拦截 `ghcr.io` / `codeberg.org` / `quay.io` 等拉取。对这些仓库用路径式，或 containerd `certs.d`：

```bash
# 路径式 — 普通 dockerd 可用（镜像名含 registry host）
docker pull 127.0.0.1:7732/codeberg.org/forgejo/forgejo:12
docker pull 127.0.0.1:7732/registry.k8s.io/pause:3.9
```

```toml
# containerd：docker.io 不用 override_path；其它 registry 要用
# /etc/containerd/certs.d/codeberg.org/hosts.toml
server = "https://codeberg.org"
[host."http://127.0.0.1:7732/v2/codeberg.org"]
  capabilities = ["pull", "resolve"]
  override_path = true
  skip_verify = true
```

允许的 host 见 `protocols.oci.oci.remote_registries`（`specula.example.yaml`）。未知 host 前缀直接 404（SSRF allowlist）。

```bash
# Hub 一次性按「具名 registry」拉取
docker pull 127.0.0.1:7732/library/nginx:latest
```

Specula 在 `/v2/` 提供 OCI Distribution API。

### Go modules

```bash
export GOPROXY=http://127.0.0.1:7732/go,direct
export GOSUMDB=sum.golang.google.cn
# 私有模块不要走公网 sumdb（并在 Specula sumdb.private_patterns 中配置）
# export GONOSUMDB=git.internal.corp/*
```

```bash
go env GOPROXY
go mod download
```

### PyPI（pip / uv / poetry）

```bash
# 环境变量（pip / uv）
export PIP_INDEX_URL=http://127.0.0.1:7732/pypi/simple
export PIP_TRUSTED_HOST=127.0.0.1

# 或 pip.conf / ~/.config/pip/pip.conf
# [global]
# index-url = http://127.0.0.1:7732/pypi/simple
# trusted-host = 127.0.0.1
```

把 Specula 当作**唯一** index（只用 `--index-url`，避免 `--extra-index-url`，降低依赖混淆风险）。

### npm / yarn / pnpm

```bash
npm config set registry http://127.0.0.1:7732/npm/
yarn config set registry http://127.0.0.1:7732/npm/
pnpm config set registry http://127.0.0.1:7732/npm/
```

```ini
# .npmrc
registry=http://127.0.0.1:7732/npm/
```

### apt（Debian / Ubuntu）

`sources.list` 指向 Specula 的 apt 挂载点（归档前缀须匹配 `apt.repositories`，例如 `ubuntu`；其后与普通源布局一致：`dists/`、`pool/`）：

```text
deb http://127.0.0.1:7732/apt/ubuntu/ jammy main restricted universe multiverse
deb http://127.0.0.1:7732/apt/ubuntu/ jammy-updates main restricted universe multiverse
```

```bash
sudo apt-get update && sudo apt-get install <pkg>
```

请保证 `protocols.apt.apt.repositories` 包含 URL 中的归档名（如 `ubuntu`），且 `base_url` 指向对应发行版树。

### Helm

```bash
# 经典 HTTP chart 仓库（index.yaml + .tgz）
helm repo add bitnami http://127.0.0.1:7732/helm/bitnami
helm repo update
helm pull bitnami/nginx

# 扁平仓库（index 在挂载根）
# helm repo add charts http://127.0.0.1:7732/helm/
```

OCI 形态的 Helm chart 走 **OCI** 路径（`/v2/`），不是 `/helm/`。

### Tarball（通用下载）

```bash
# 路径编码 host + 远端路径；host 须在 Specula 白名单内
curl -fL 'http://127.0.0.1:7732/tarball/github.com/example/proj/releases/download/v1.0.0/app.tar.gz'
# 可选 digest pin
curl -fL 'http://127.0.0.1:7732/tarball/…/app.tar.gz?digest=sha256:…'
```

### git

```bash
# 经 Specula 克隆（Smart HTTP）
git clone http://127.0.0.1:7732/git/github.com/golang/go.git

# 把所有 github.com HTTPS 克隆改写到 Specula
git config --global url."http://127.0.0.1:7732/git/github.com/".insteadOf "https://github.com/"
```

主机须在 `protocols.git.git.allowed_upstreams` 中。私有仓 / push 会透传、不缓存。

### Cargo（sparse registry）

```bash
./bin/specula integrate --protocols cargo --addr http://127.0.0.1:7732
# 写入 ~/.cargo/config.toml source replace → sparse+http://127.0.0.1:7732/cargo/index/
cargo fetch
```

### conda

```bash
./bin/specula integrate --protocols conda --addr http://127.0.0.1:7732
# 在 ~/.condarc 前置 channel http://127.0.0.1:7732/conda/conda-forge
micromamba create -y -n demo -c http://127.0.0.1:7732/conda/conda-forge --override-channels ca-certificates
```

### Hugging Face Hub

```bash
./bin/specula integrate --protocols hf --addr http://127.0.0.1:7732
# 经 ~/.config/specula/env.sh 导出 HF_ENDPOINT=http://127.0.0.1:7732/hf
source ~/.config/specula/env.sh
huggingface-cli download hf-internal-testing/tiny-random-bert --local-dir /tmp/hf-tiny
```

## 离线 / 气隙（`server.mode: offline`）

先在 online 暖缓存，再改配置重启为 `mode: offline`。命中继续服务；缺失返回
**404**，且**零外连**（git 只读已有 bare mirror，不 clone/刷新）。

```yaml
server:
  mode: offline   # 切换需重启；空 / online = 正常 pull-through
```

```bash
# 1) online 暖缓存
docker pull 127.0.0.1:7732/registry.k8s.io/pause:3.9
# 2) mode: offline 后重启
# 3) 命中成功；未缓存 tag 快速失败
./scripts/realclient-offline.sh
```

## HA（多副本）

只用成熟库：Postgres 元数据 + Redis（redsync）跨副本锁 + 共享 CAS
（S3 兼容 **或** 共享 PVC）。Chart：[`deploy/helm/specula`](deploy/helm/specula)。
本机验收：`./scripts/ha-minikube.sh`。细节见 [ARCHITECTURE §12](docs/ARCHITECTURE.md)。

## 自举（中国 / 离线）

当 `docker.io` / `registry.k8s.io` 不可达时：先落地 Specula（离线 tar / ACR /
`docker load`），再让其它镜像**透过 Specula** 进来。Chart：
[`deploy/helm/specula-bootstrap`](deploy/helm/specula-bootstrap)（SQLite + 本地盘、
NodePort、containerd `certs.d` DaemonSet，无 busybox）。本机验收（containerd）：
`./scripts/bootstrap-minikube.sh`。

## 文档

| 文档 | 内容 |
|------|------|
| [docs/LIBRARY.md](docs/LIBRARY.md) | 公开 `pkg/` API、稳定性、错误契约 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 双平面、缓存、验证、**HA 矩阵** |
| [docs/TRUST.md](docs/TRUST.md) | cosign / apt GPG / Helm `.prov` / 依赖混淆一页纸 + oracle |
| [deploy/helm/specula/README.md](deploy/helm/specula/README.md) | Helm 安装（Bitnami PG/Redis，可选 MinIO） |
| [deploy/helm/specula-bootstrap/README.md](deploy/helm/specula-bootstrap/README.md) | 中国 / 离线自举 |
| [docs/PRD.md](docs/PRD.md) | 产品需求 |
| [CHANGELOG.md](CHANGELOG.md) | 变更记录 |

## 许可证

见 [LICENSE](LICENSE)。
