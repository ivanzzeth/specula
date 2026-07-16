# Specula — Architecture (v0.2)

> 强化依据见 [DESIGN-REVIEW.md](./DESIGN-REVIEW.md)；需求见 [PRD.md](./PRD.md)。
> v0.2 关键变化：双平面架构、二层缓存(CAS)、verify-on-write 隔离区、流式验证接口、
> 两层 stampede 保护、缓存容量统计、新增 Helm/git、内嵌 WebUI + 邮箱认证。

---

## 1. Overview — 双平面架构 (Two-Plane)

Specula 是一个**无状态 Go 守护进程**，明确分为两个平面：

- **数据面 (Data Plane)**：8 协议拉取端点。无消费者认证（可信网段/mTLS/网络策略把边界），
  高吞吐。核心是"取回 → **验证** → 缓存 → 服务"管线。
- **管理面 (Control Plane)**：单一二进制内嵌 WebUI，带**邮箱认证**（首个=admin）。
  看缓存统计、验签告警、配策略、管上游健康、GC。

```mermaid
graph TB
    subgraph Consumers[数据面消费者]
        node[k8s node / containerd]
        ci[CI pipeline]
        dev[developer git/helm]
    end
    subgraph Operators[管理面]
        admin[运维 浏览器]
    end

    subgraph Specula[Specula 实例 x N]
        dp[Data Plane<br/>协议 handlers]
        cp[Control Plane<br/>WebUI + Admin API<br/>认证]
    end

    subgraph State[共享状态]
        blob[(Blob Store CAS<br/>S3 / MinIO / 本地盘)]
        db[(Metadata + Users + Config<br/>PostgreSQL / SQLite)]
    end

    subgraph Up[上游 CN 镜像优先]
        mirrors[daocloud / tuna / aliyun<br/>npmmirror / goproxy.cn / github]
    end

    node & ci & dev -->|无认证| dp
    admin -->|邮箱登录| cp
    dp & cp --> blob & db
    dp -.->|cache miss + fallback chain| mirrors
```

---

## 2. Internal Components

```mermaid
graph TD
    subgraph proc[specula 进程]
        router[Protocol Router<br/>port / path 分发]
        subgraph handlers[Data Plane Handlers]
            oci[OCI] & pypi[PyPI] & npm[npm] & gomod[Go]
            apt[apt] & helm[Helm] & tar[tarball] & git[git]
        end
        subgraph pipeline[核心管线]
            policy[Policy Engine]
            resolve[Resolver<br/>mutable→immutable 解析<br/>dep-confusion guard]
            coalesce[Singleflight + 分布式锁<br/>stampede 保护]
            upstream[Upstream Client<br/>fallback + retry + auto-block]
            verify[Verification Chain<br/>流式, verify-on-write]
            cache[Cache Manager<br/>二层: CAS blob + mutable meta]
        end
        subgraph stores[Storage Drivers]
            cas[BlobStore: S3 / LocalDisk<br/>content-addressed] 
            meta[MetadataStore: PG / SQLite]
        end
        subgraph ctl[Control Plane]
            webui[Embedded WebUI<br/>//go:embed]
            adminapi[Admin API + Auth<br/>bcrypt + JWT]
            stats[Stats Collector<br/>per-protocol + total]
        end
        metrics[Prometheus] & health[/healthz /readyz/]
    end

    router --> handlers --> policy --> resolve --> cache
    cache -->|hit 已验证| router
    cache -->|miss| coalesce --> upstream --> verify --> cache
    cache --> cas & meta
    adminapi --> stats --> meta
    webui --> adminapi
```

---

## 3. 二层缓存模型 (Two-Tier Cache) — 核心

所有成熟方案的共识不变式（DESIGN-REVIEW §3）：**把世界分成不可变内容与可变元数据**。

```mermaid
graph LR
    subgraph immutable[不可变层 — CAS]
        i1[blob / .deb / .tgz / .zip / git object<br/>按 sha256 内容寻址]
        i2[永久缓存, 绝不重验<br/>写时验 digest, 引用去重]
    end
    subgraph mutable[可变层 — 短 TTL]
        m1[tag→digest / index.yaml / packument<br/>simple 页 / refs / @v/list]
        m2[短 TTL + 条件 GET 重验<br/>ETag/If-None-Match→304<br/>上游失败 serve-stale]
    end
    mutable -->|解析出 digest| immutable
```

| 协议 | 不可变 (CAS 永久) | 可变 (短 TTL 重验) | 默认 mutable TTL |
|---|---|---|---|
| OCI | blob/config/manifest by digest | tag→digest（HEAD 探测，不耗限额） | 见 config |
| Go | `@v/<v>.{info,mod,zip}` | `@v/list`, `@latest` | 5min |
| PyPI | wheel/sdist 文件 | `/simple/<pkg>/` 页 | 30min |
| npm | `*.tgz` | packument | 2min |
| apt | `pool/*.deb` | InRelease/Packages | revalidate（by-hash 免race）|
| Helm | chart `*.tgz` | `index.yaml` | 30min |
| git | git object by SHA | refs | 30s |
| tarball | 内容 by sha256 | — | — |

**config 哨兵**：`ttl: -1` = 永不重验（不可变），`ttl: 0` = 每次重验。
**负缓存**：404 短 TTL 缓存（默认 30min），吸收 miss-stampede，且被 singleflight 合并。

---

## 4. 请求生命周期 — verify-on-write / quarantine

**修复 v0.1 C2（流式验证悖论）**：绝不把未验证的上游字节流式转发给客户端。

```mermaid
sequenceDiagram
    participant C as Consumer
    participant H as Handler
    participant R as Resolver
    participant SF as Singleflight/Lock
    participant U as Upstream
    participant V as Verify (流式)
    participant Q as Quarantine
    participant CAS as CAS Blob+Meta

    C->>H: GET (e.g. nginx:latest)
    H->>R: 解析 + policy + dep-confusion guard
    R-->>H: DENY→403 / 或 canonical ref
    R->>R: mutable? 解析 tag→digest (短TTL重验)

    alt 已验证缓存命中 (by digest)
        H->>CAS: 读 blob (支持 Range)
        CAS-->>C: 200 (只服务已验证内容)
    else Miss
        H->>SF: acquire(digest)  %% 进程内合并 + 跨实例锁
        SF->>U: fetch(ref, upstreams[] fallback)
        U-->>Q: 流式落盘到隔离区 + 边写边算 digest
        Q->>V: 流式验证 (io.Reader, 不全量入内存)
        alt 验证 FAIL
            V-->>Q: 删除隔离文件
            H-->>C: 502 + 验证错误 + tier
        else PASS (记录达到的 tier)
            Q->>CAS: 原子提升为可服务缓存
            Note over CAS: blob 先落, metadata(size,protocol,tier) 后写
            CAS-->>C: 200 (+ tee 流式给等待的 waiters)
        end
        SF->>SF: release(fenced)
    end
```

**要点**：
- **verify-on-write**：只从**已验证**的 CAS 缓存对外服务。隔离区文件验证通过才原子提升。
- **流式验证**（修 C3）：Verifier 拿 `io.Reader`/文件句柄，digest 用 `hash.Hash` 边写边算；
  签名验证对落盘文件做，不驻留内存。多 GB layer 不入内存。
- **写序**（修 M1）：blob 先落 CAS，metadata 后写；读路径把"meta 命中但 blob 缺失"当 miss；GC 清孤儿。
- **tee 流式**：大 blob 回填 CAS 的同时，喂给同一 digest 上等待的 waiters（zot 模式），单上游出口。

---

## 5. Verification Chain — 流式 + 四档

```go
// 修 C3：流式，不是 blob []byte
type Verifier interface {
    Name() string
    Tier() Tier // signed | consensus | tofu | checksum
    // 对隔离区文件流式验证；digest 已边写边算
    Verify(ctx context.Context, ref ArtifactRef, art *Artifact) (Result, error)
}

type Artifact struct {
    Path      string    // 隔离区文件路径（不驻留内存）
    Digest    string    // sha256:...（流式计算所得）
    Size      int64
    Meta      UpstreamMeta // ETag, Last-Modified, 来源 upstream, 签名/prov 附件
}

type Result struct {
    Status  Status // PASS | WARN | FAIL
    Tier    Tier   // 实际达到的档
    Message string
}
```

按协议注册相关 verifier，链式短路：

```mermaid
flowchart LR
    art[隔离区 artifact] --> cs[Checksum<br/>流式 digest]
    cs --> tier{按协议锚}
    tier -->|apt| gpg[GPG InRelease链]
    tier -->|go| sumdb[sumdb 签名tree head<br/>+inclusion/consistency]
    tier -->|helm| prov[.prov GPG]
    tier -->|oci| cosign[cosign keyed<br/>关闭tlog]
    tier -->|git| signed[签名 tag/commit<br/>allowed-signers]
    tier -->|npm/pypi/tarball| consensus[跨源 quorum<br/>+ origin-check]
    gpg & sumdb & prov & cosign & signed & consensus --> tofu[TOFU pin<br/>+ 变更告警]
```

**四档落地要点**：
- `signed`：见上表锚。cosign 默认 `keyed --insecure-ignore-tlog`（CN Rekor 被墙）；sumdb 走
  `sum.golang.google.cn`；apt/helm 用本地 keyring；git 用 allowed-signers。
- `consensus`：从 N 个独立镜像**并行取 digest/manifest**（HEAD/metadata 阶段，不下载全 blob），
  ≥quorum 一致才 PASS；可选 origin-check 经出口代理直连官方源比对。
- `tofu`：首次锁定 digest 入库，后续同一不可变版本变更即告警/fail。git 额外检测非快进 ref 更新（force-push/改史）。
- **anti-rollback**（修 H2）：per-channel 单调版本状态——拒绝比已见更低版本的**已签名索引**；
  **不做**按 artifact 年龄拒绝。

**dependency confusion guard**（修 H3/H4）：见 DESIGN-REVIEW §4。私有名私有源宕机 **fail-closed**。

---

## 6. Cache Manager & Storage — CAS

Cache Manager 协议无关，操作 `(canonical ref, digest)`，委托 `BlobStore`(CAS) + `MetadataStore`。

```go
// 修 M2：支持 Range（containerd 断点续传）；size 读写内联返回
type BlobStore interface {
    Get(ctx context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error)
    Put(ctx context.Context, digest string, r io.Reader, size int64) error // 幂等；同 digest 同字节
    Exists(ctx context.Context, digest string) (bool, error)
    Delete(ctx context.Context, digest string) error
    UsageBytes(ctx context.Context) (int64, error) // 后端总用量（可选/缓存）
}
```
实现：`S3Driver`（aws-sdk-v2，path-style，tmp→Head 取 size→Copy 提升，硬链接不可用时引用计数 DB）、
`LocalDiskDriver`（内容寻址分片目录 + 硬链接去重）。**复用 ai-sandbox `workspace.Backend` 接口 + StorageFactory**（补本地盘 driver）。

```go
type MetadataStore interface {
    Get(ctx, ref ArtifactRef) (*CacheEntry, error)
    Put(ctx, entry CacheEntry) error // 记录 digest, size, protocol, tier, upstream, verified_at, etag
    Delete(ctx, ref ArtifactRef) error
    // 修 H1：可变元数据带 TTL + 条件重验状态
    GetMutable(ctx, key string) (*MutableEntry, error) // tag→digest, index, packument...
    // G7：统计聚合
    CacheSizeByProtocol(ctx) (map[string]SizeStat, error) // SUM(size),COUNT GROUP BY protocol
}
```
- `PostgresStore`（并发安全，`ON CONFLICT` upsert）
- `SQLiteStore`（WAL 模式；**仅单实例节点本地，不跨实例共享**——修 L2）

**CAS 去重**（偷 Artifactory/zot）：相同字节物理存一份，path→digest 映射入 DB，
copy/move/delete = 引用操作，末次引用才物理删。

---

## 7. Stampede 保护 — 两层 (修 M3)

```mermaid
graph TB
    r1[请求1 miss] & r2[请求2 同digest] & r3[请求3 同digest] --> sf[进程内 singleflight<br/>key=digest/module@ver/pkg@ver]
    sf -->|leader| dl[跨实例分布式锁<br/>Redis SET NX PX / PG advisory / S3 lease<br/>TTL + owner-checked 释放]
    dl -->|获得| fetch[回源 + verify-on-write]
    dl -->|等待超时| fallback[waiter 自行回源<br/>幂等, digest 相同]
    fetch -->|tee| r1 & r2 & r3
```

- **进程内**：`golang.org/x/sync/singleflight`。**注意陷阱**：错误放大（一次失败拖垮整群）→ 用 `DoChan`+ctx 超时；
  panic 在 DoChan 新 goroutine 重抛可崩进程 → recover 包裹；poison 时 `Forget`；高 QPS 按 key 分片多 Group。
- **跨实例**：短 TTL 分布式锁，**必须 owner-checked/fenced 释放**（防崩溃 filler 卡死所有 waiter）。
  复用 ai-sandbox 的 GUDC `locker`(redsync goredis) 或 PG advisory lock。
- **有界等待**：waiter 等待超时后自行回源（幂等）。
- **可选**：可变元数据用 XFetch 概率提前刷新（避免同步过期悬崖）+ stale-while-revalidate（RFC 5861）。

---

## 8. Protocol Handlers

```
internal/handler/
  oci/    — Docker v2 + OCI Distribution v1；go-containerregistry 底座；tag HEAD 探测
  pypi/   — PEP 503/691；单 index 模式；dep-confusion guard
  npm/    — registry 协议；scope 绑定 + unscoped 黑名单
  gomod/  — GOPROXY(/@v/list,.info,.mod,.zip) + /sumdb/ 透传验证(x/mod/sumdb)
  apt/    — InRelease/Packages/pool；GPG 端到端链验证；by-hash 免race
  helm/   — OCI 形态转 oci handler；经典 repo: index.yaml + tgz + .prov
  tarball/— URL-keyed 内容寻址缓存
  git/    — 见 §9
```

**ArtifactRef**（canonical 内部类型）：

```go
type ArtifactRef struct {
    Protocol string
    Name     string  // image / package / module path / repo host+path
    Version  string  // tag / version / suite+component / ref
    Digest   string  // 解析后填充；CAS 键
    Upstream string  // 来源上游（M4：记录以检测跨源冲突）
    Mutable  bool    // tag/index/ref = true → 走可变层
}
```

---

## 9. git clone 加速 Handler (新)

**直接移植 ai-sandbox `internal/controlplane/ptc/gitproxy/`**（DESIGN-REVIEW §6）。

```mermaid
graph TD
    req[git clone → GET /info/refs / POST /git-upload-pack] --> allow{host 允许清单?}
    allow -->|否| deny[403]
    allow -->|是| kind{类型}
    kind -->|receive-pack push / 带 Authorization| bypass[passthrough 直传, 零缓存]
    kind -->|upload-pack| pub{公共仓? TTL探测}
    pub -->|否/探测失败 且 fail_closed| bypass
    pub -->|是| sync[EnsureSynced: bare mirror<br/>git clone --mirror / remote update --prune<br/>按 mirror path keyed mutex + 30s 陈旧窗口]
    sync --> serve[git http-backend CGI 服务]
    serve --> ok[200 packfile]
    sync -->|失败| bypass
```

- **缓存 = 磁盘 bare mirror**（非 blob store）：git objects 天然按 SHA 内容寻址=不可变；refs=可变短 TTL。
- **stampede**：按 mirror path 的 `sync.Mutex` + 陈旧窗口（并发 clone 不重复 fetch）。
- **信任**：`checksum`=git 固有 SHA Merkle；`tofu`=ref→SHA 锁定 + **force-push/改史告警**（非快进更新）；
  `signed`=验签名 tag/commit（allowed-signers）；`consensus`=跨镜像比对 ref→SHA。
- **透传**：partial/shallow clone（`filter=blob:none`）透传；私有仓/带 PAT → bypass 零缓存。
- 不用 `elazarl/goproxy`；用 `httputil.NewSingleHostReverseProxy` 做 passthrough。

---

## 10. 缓存容量统计 (G7)

**权威来源 = MetadataStore（写时记 size，非遍历 FS）** —— 偷 ai-sandbox `AllOrgStorageBytes` 模式。

```mermaid
graph LR
    put[blob Put 时] -->|记 size,protocol,tier| meta[(MetadataStore)]
    meta -->|SUM(size),COUNT GROUP BY protocol| agg[聚合 O(1) 精确]
    agg --> prom[Prometheus<br/>specula_cache_bytes{protocol}<br/>total = sum()]
    agg --> api[Admin API GET /admin/stats<br/>per-protocol {bytes,objects,oldest,newest}<br/>+ grand total + 后端容量]
    agg --> ts[DB 时序表<br/>历史曲线 环形缓冲]
    gc[GC/eviction] -->|同步扣减| meta
    disk[statfs gopsutil / S3 UsageBytes] --> api
    subgraph opaque[不透明缓存: git bare mirror]
        du[du -sb 兜底] --> agg
    end
```

- 原生 handler：`SUM(size) GROUP BY protocol`，O(1) 精确，per-protocol + total 天然。
- git bare mirror（不透明）：`du -sb` 兜底采集（ai-sandbox collector 模式）。
- 跨节点（DaemonSet/HA）：各实例本地统计，Admin API 按 protocol 求和 + 总量求和；CP 从不远端 du。
- 与 GC/eviction 联动扣减，保证统计与实际一致。

---

## 11. Control Plane — 内嵌 WebUI + 认证 (G6)

**整包复用 ai-sandbox `auth/` + `web/embed.go`**（DESIGN-REVIEW §6），裁掉多租户 org/acl。

```mermaid
graph TB
    root[根 http.ServeMux<br/>最长前缀分流]
    root -->|/api/ /healthz /readyz /metrics| api[Admin API]
    root -->|/ 兜底| spa[Embedded SPA<br/>//go:embed all:dist]
    api --> mw[Auth 中间件<br/>Bearer key / admin-key / session cookie]
    mw --> h[handlers]
    spa -->|真实文件| asset[assets/* immutable 1y]
    spa -->|路由回落| index[index.html no-cache]
```

- **用户模型**：`users(id,email,password_hash,system_role,token_gen,...)`；**首个 = admin**（`CountUsers()==0`）。
- **认证**：bcrypt.DefaultCost 密码（用户不存在跑 dummy bcrypt 防枚举）；手写 HS256 JWT（stdlib，拒 alg=none/RS*）
  in httpOnly + SameSite=Lax + Secure(HTTPS) cookie；`token_gen` 快照入 claims → logout 服务端 bump 撤销所有会话。
- **中间件三通道**：Bearer API key → Bearer admin-key(break-glass) → 本地 session。cookie+改状态+跨源 → 403。
- **WebUI**：React18 + Vite + Tailwind"工程控制台"暗色（IBM Plex Mono + 琥珀 #ffb02e + 发丝线 + 近直角）。
  页面：缓存统计仪表盘、验签/告警、策略配置、上游健康、GC 操作、用户管理。
- **构建**：Makefile `ui` 先 `vite build`→`web/dist`，再 `go build` 嵌入；`web/dist/.gitkeep` 让裸 clone 可编译。
- **密钥引导**：`ensureSecret` 首次运行随机生成 HS256/config 密钥并持久化加密配置库，不可持久大声告警。

---

## 12. HA & 部署

```mermaid
graph TB
    lb[L4 LB] --> s1[Specula] & s2[Specula] & s3[Specula]
    s1 & s2 & s3 -->|CAS blobs| minio[(MinIO/S3)]
    s1 & s2 & s3 -->|meta+users+config| pg[(PostgreSQL)]
    s1 & s2 & s3 -.->|分布式 stampede 锁| redis[(Redis/PG advisory)]
```

- **无 leader election**：实例同质；写同 blob 幂等（同 digest 同字节）；MetadataStore upsert。
- **stampede 去重**：分布式锁（§7），首个 miss 回源，others 等待/超时自行回源。
- **DaemonSet 模式**：`hostNetwork` + 本地 SQLite + 本地盘 CAS；节点本地缓存，冷启回源。
  统计各节点本地采集，Admin API 聚合。**修 L1**：hostNetwork 下同节点 pod 可访问 127.0.0.1，
  记入威胁模型；可选本地 token/unix socket。

---

## 13. Repository Layout

```
specula/
├── cmd/specula/            — 入口, flag, bootstrap, 根 ServeMux
├── internal/
│   ├── config/             — YAML 模型 + 校验 + 加密配置库
│   ├── handler/            — oci pypi npm gomod apt helm tarball git
│   ├── artifact/           — ArtifactRef, CacheEntry, Tier
│   ├── cache/              — CacheManager, 二层缓存, quarantine 提升
│   ├── store/
│   │   ├── s3/ local/      — BlobStore CAS drivers
│   │   └── postgres/ sqlite/ — MetadataStore
│   ├── upstream/           — fallback chain, retry, auto-block, 条件GET
│   ├── coalesce/           — singleflight + 分布式锁
│   ├── verify/
│   │   ├── checksum.go cosign.go gpg.go sumdb.go
│   │   ├── helmprov.go gitsigned.go
│   │   ├── consensus.go    — 多镜像 quorum + origin-check
│   │   ├── tofu.go         — 首次锁定 + 变更告警 + anti-rollback
│   │   └── depconfusion.go — 分生态 + fail-closed
│   ├── policy/             — 每协议策略评估
│   ├── stats/              — per-protocol + total 聚合, du 兜底
│   ├── auth/               — bcrypt + HS256 JWT (复用 ai-sandbox)
│   ├── admin/              — Admin API handlers
│   └── metrics/            — Prometheus
├── web/                    — React+Vite+Tailwind; embed.go //go:embed all:dist
├── deploy/k8s/             — daemonset.yaml, deployment.yaml, configmap.yaml
├── docs/                   — PRD.md, ARCHITECTURE.md, DESIGN-REVIEW.md
├── specula.example.yaml, Makefile, Dockerfile, LICENSE
```

---

## 14. Tech Stack

| Concern | Choice | Rationale |
|---|---|---|
| 语言 | Go 1.22+ | 单静态二进制 |
| HTTP | `net/http`（Go 1.22 method+pattern 路由，参照 ai-sandbox） | 无魔法 |
| OCI | `google/go-containerregistry` | crane/skopeo 同底座 |
| cosign | `sigstore/cosign`（keyed，关闭 tlog） | CN 离线可验 |
| Go sumdb | `golang.org/x/mod/sumdb` | 签名 tree head + 证明验证 |
| git | 移植 ai-sandbox `gitproxy`（`git http-backend` CGI + bare mirror） | 直接复用 |
| S3 | `aws-sdk-go-v2`（参照 ai-sandbox）| R2/MinIO/OSS 通用 |
| PostgreSQL | `jackc/pgx` | 最佳 PG driver |
| SQLite | `modernc.org/sqlite` | 纯 Go, CGO-free |
| 迁移 | `pressly/goose`（参照 ai-sandbox） | 内嵌 SQL 迁移 |
| stampede | `golang.org/x/sync/singleflight` + redsync/PG advisory | 两层去重 |
| 系统统计 | `shirou/gopsutil`（参照 ai-sandbox） | statfs 容量 |
| 认证 | `golang.org/x/crypto/bcrypt` + 手写 HS256 JWT（复用 ai-sandbox） | 无重依赖 |
| 前端 | React18 + Vite + Tailwind（复用 ai-sandbox 风格） | 工程控制台美学 |
| 配置 | `koanf`（YAML + env override）+ 加密配置库 | 多源 |
| 日志 | `log/slog` | 结构化 JSON |
| 测试 | `testify` + `testcontainers-go` | 真 S3/PG 集成 |
