# Specula — Product Requirements Document (v0.2)

> **Tagline**: Mirror everything. Verify what you can. Never lie about the rest.
>
> Specula is a lightweight, high-availability, multi-protocol artifact proxy that
> caches OCI images, PyPI/npm/Go/apt packages, Helm charts, generic tarballs,
> public git clones, Cargo crates, conda packages, and Hugging Face Hub artifacts —
> in a single Go binary with an **honest, tiered supply-chain trust model** and an
> embedded management WebUI.
>
> **v0.2 changes**: honest tiered trust model (整改 v0.1 把 checksum 当供应链防护的根本错误)、
> 新增 Helm 与 git 协议、双平面架构（数据面 + 带认证 WebUI 的管理面）、缓存容量统计。
> 设计审查依据见 [DESIGN-REVIEW.md](./DESIGN-REVIEW.md)。

---

## 1. Problem Statement

现代软件供应链横跨数十个包生态。运行气隙/CN 区域集群的团队面临两个叠加问题：

1. **连通性**：上游 registry（docker.io、pypi.org、registry.npmjs.org、proxy.golang.org、
   archive.ubuntu.com、github.com）在特定区域慢、被限流或不可达。

2. **信任**：单个被投毒的上游包、依赖混淆攻击或静默更新，可将恶意代码注入生产。

现有方案要么只解决其一，要么**在"信任"上做出无法兑现的承诺**——很多轻量代理宣称"校验 checksum
即防篡改"，但若 checksum 与 blob 来自同一被信任镜像，这只是循环验证（详见 DESIGN-REVIEW §1）。

| Tool | Protocols | Supply Chain | Weight |
|---|---|---|---|
| Nexus / Artifactory | 多 | 无内置真实性验证 | ~2 GB JVM |
| Harbor | OCI | Trivy 扫描（CVE，非来源） | Heavy |
| zot / goproxy.io / verdaccio | 单协议 | 无 | Light |

**Specula 的定位**：一个 Go 二进制、11 协议、**诚实分级的供应链验证**（能验真实性就验，不能就明确标注
仅 TOFU/共识，绝不谎称防篡改）、从第一天起横向扩展、内嵌管理 WebUI。

---

## 2. Goals

### G1 — 单一二进制多协议

原生服务 11 个协议（用成熟 Go 库做底座，非编排容器——见 DESIGN-REVIEW §5）：

- **OCI** — 容器镜像（Docker v2 + OCI Distribution v1）
- **PyPI** — Python 包（PEP 503 / 691）
- **npm** — Node 包
- **Go modules** — GOPROXY 协议 + sumdb 透传
- **apt** — Debian/Ubuntu（InRelease/Packages/pool）
- **Helm** — chart（OCI 形态复用 OCI handler；经典 HTTP repo：index.yaml + tgz + .prov）
- **tarball** — URL-keyed 通用下载缓存
- **git** — 公共仓库 `git clone` 加速（smart-HTTP + 本地 bare mirror）
- **Cargo** — sparse registry（RFC 2789；`config.json` dl/api 重写）
- **conda** — channel 代理（`repodata.json` + 包 digest pin）
- **Hugging Face Hub** — `HF_ENDPOINT` 兼容反向代理（CDN/LFS URL 重写；带 Authorization 透传不入 CAS）

### G2 — 诚实分级的供应链信任 (Honest Tiered Trust)

**核心原则**：区分**完整性 (integrity，防传输损坏)** 与**真实性 (authenticity，防来源造假)**。
真实性只能靠攻击者无法控制的**独立信任锚**。Specula 对每个 artifact 标注其达到的信任档，
**绝不让"上游 checksum 匹配"冒充来源证明**。

**四档信任模型**（从高到低）：

| 档 | 机制 | 保证 |
|---|---|---|
| `signed` | 密码学锚：apt keyring / Go sumdb / Helm `.prov` / cosign keyed / git 签名 tag | **防来源造假**（真实性） |
| `consensus` | 跨源共识：多镜像 quorum 比对 digest，或代理直连官方源比对 | 抬高门槛（需一致投毒所有源），**非密码学真实性** |
| `tofu` | 首次锁定 digest + 变更告警（含 git force-push/改史检测） | 防"事后静默篡改" |
| `checksum` | 仅比对哈希 | 仅防传输损坏，**绝不单独作为供应链防护** |

**每协议在 CN+联网场景下可达的最高档**：

| 协议 | 可达最高档 | 信任锚 | 备注 |
|---|---|---|---|
| apt | **signed** | 发行版 keyring（预置，离线可验） | 端到端金标准 |
| Go | **signed** | sumdb Ed25519 签名 tree head + Merkle 证明 | 经代理透传仍可验 |
| Helm (repo) | **signed**（需配 keyring 且上游发布 `.prov`） | `.prov` GPG 签名 + keyring | 无 `.prov` 降级到 tofu（不谎称 signed）。**已验证**：`helm package --sign` 产出的真实 `.prov` 经 Specula 验证达到 signed（`TestHelmProv_RealHelm_*` + `scripts/trust-oracle-signed.sh`）。**注意**：这证明的是「我们的验证器接受真实 helm 产出的 `.prov`」，**不**代表任何公网 CN 镜像发布 `.prov`——`mirror.azure.cn/kubernetes/charts` 一个都不发（见 `scripts/trust-oracle.sh`）。故 CN 实务中 helm 通常停在 tofu，除非 operator 指向自建/发布 `.prov` 的仓库并配 keyring |
| OCI | **signed**（需配公钥且镜像被签名） | cosign keyed（关闭 tlog）；否则 consensus/tofu | keyless 在 CN 默认不可用。**已端到端验证**：真实 `cosign sign --key --tlog-upload=false` 签名的镜像经 Specula 拉取达到 signed，未签名/被篡改（他人密钥签名）均 fail-closed 且真正到达验证器（`TestCosign_RealBinary_*`、`TestCosignVerifier_RealFetcher_*` + `scripts/trust-oracle-signed.sh`）。cosign CLI 在 CN 可经 goproxy.cn 从源码构建。**验证器只作用于镜像 manifest/index**（按上游 content-type 门控），layer/config blob 跳过——否则每个未签名的 layer 会 fail-close 整个 pull |
| git | **signed**（可选） | 签名 tag/commit（配 allowed-signers）；否则 tofu | git 对象天然 Merkle |
| npm | **consensus / tofu** | provenance CN 不可用且覆盖 ~3–12% | 实务上 TOFU + 依赖混淆 guard；metadata 仅暴露 sha512 integrity/sha1 shasum，metadata-only 的 sha256 跨源共识不可用，故默认停在 tofu |
| PyPI | **consensus / tofu** | PEP 740 CN 不可用且覆盖 ~5% | 已接线：consensus（PEP 503 simple-index `#sha256=` 跨源 quorum 比对，metadata-only）+ TOFU |
| tarball | **consensus / tofu** | 无原生签名 | metadata 无 sha256，metadata-only 共识不可用，默认停在 tofu；可配官方源比对 |
| Cargo | **tofu / checksum** | sparse 索引无跨源 sha256 共识 | 索引 TOFU；`.crate` 流式 checksum，有上游 digest 则 pin |
| conda | **checksum / tofu** | repodata 提供 sha256/md5 | 包按 repodata digest pin；repodata 本身短 TTL + TOFU |
| Hugging Face | **tofu / checksum** | 公开 Hub 对象 | 带 Authorization/cookie 透传不缓存；CDN/LFS URL 重写到 `/hf/` |

其余供应链控制：
- **Allowlist / denylist** — 每协议策略
- **Dependency confusion guard** — 分生态（npm scope 绑定 / PyPI 唯一 index private-first / Go 域名限定），
  私有名**私有源宕机时 fail-closed 绝不回落公网**（见 DESIGN-REVIEW §4）
- **Anti-rollback** — TUF 式**单调版本状态**（拒绝比已见更低版本的已签名索引），
  **不是**"按 artifact 年龄拒绝"（后者误杀钉版依赖且不防 rollback）

### G3 — 高可用，进程内无共享状态

Specula 实例**无状态**。持久状态在：
- **Blob storage** — S3 兼容（MinIO/AWS S3/Ceph RGW），**CAS 内容寻址**
- **Metadata DB** — PostgreSQL（HA）或 SQLite（单节点/DaemonSet 节点本地）

无 leader election、无 gossip、无内嵌共识——共享 blob store + 共享 DB。
（管理平面的用户/配置数据也存 DB。）

### G4 — 轻量可运维

- 单一静态二进制（`CGO_ENABLED=0`），含内嵌 WebUI；目标体积复核（前端资源计入）
- 低空闲内存（**验证走流式，不全量入内存**——见 DESIGN-REVIEW C3）
- 单 YAML 驱动所有协议与上游 + 加密配置库存运行时可变配置
- `/healthz`、`/readyz`、Prometheus `/metrics`、结构化 JSON 日志
- DaemonSet 友好（`hostNetwork`，客户端打 `127.0.0.1`）

### G5 — CN 区域优先

- 默认上游含 CN 镜像（DaoCloud、tuna、aliyun、npmmirror、goproxy.cn）
- 镜像 fallback：主源失败试下一个；自动 block/unblock（Nexus 模式）
- Go sumdb 走 `sum.golang.google.cn` 或 goproxy.cn `/sumdb/` 透传
- cosign 默认 keyed（keyless 需连 Fulcio/Rekor，CN 被墙）

### G6 — 管理平面（WebUI + 用户管理）【v0.2 新增】

单一二进制经 `//go:embed` 内嵌 WebUI，与数据面分层：

- **邮箱注册/登录**；**首个注册邮箱默认 admin**（`CountUsers()==0` 引导）
- 认证：bcrypt 密码 + HS256 JWT（httpOnly cookie，拒绝 alg=none）+ 会话代际撤销
- WebUI 功能：**缓存统计仪表盘（per-protocol + total）**、验签/告警查看、策略配置、
  上游/镜像健康、GC/eviction 操作
- 参照 ai-sandbox 实现（DESIGN-REVIEW §6）

### G7 — 缓存容量统计【v0.2 新增】

- **按子服务（协议）统计已缓存数据量 + 总量汇总**
- 权威来源：MetadataStore，写时记 `size` + `protocol`，`SUM(size) GROUP BY protocol`（O(1) 精确）
- 曝露：Prometheus `specula_cache_bytes{protocol}`（total = Grafana `sum()`）、
  Admin API `GET /admin/stats`、DB 时序表（历史曲线）
- 底层容量：本地盘 statfs（gopsutil）/ S3 用量；与 GC/eviction 联动扣减

---

## 3. Non-Goals (v1)

- **数据面无消费者认证** — 部署在可信网段（cluster-internal / 私有子网），mTLS/网络策略把边界。
  （**注意**：管理平面 WebUI 有认证——这与数据面是两个平面。）
- **无镜像 CVE 扫描** — Specula 验证 provenance/签名，不查 CVE 库（Trivy/Grype 另行）
- **无 Maven / NuGet / RubyGems / Hex** — 下一波协议扩展（仍要求官方 layout + 真客户端脚本）
- **无 Cargo git index** — 仅 sparse registry；git index 与现有 git handler 职责重叠- **无 GUI 策略编辑器写回 git** — WebUI 可编辑运行时配置，但声明式 YAML 仍是 source of truth

---

## 4. Target Users

| User | Scenario |
|---|---|
| **平台工程师** | Specula 作 DaemonSet，每节点 127.0.0.1 本地缓存；WebUI 看统计 |
| **气隙集群操作员** | 预置 blob store，`mode: offline` 只从缓存服务 |
| **安全工程师** | 写验证策略；WebUI 看验签失败告警；配 dep-confusion 私有名清单 |
| **CI/CD** | 把 GOPROXY/PIP_INDEX_URL/NPM_REGISTRY/HELM repo/git 指向 Specula |

---

## 5. Key User Stories

- **US-1 OCI 签名验证**：配 `cosign.policy: enforce` + 预置发布方公钥（keyed，CN 可用），
  无有效签名的镜像在到达 containerd 前被拒。
- **US-2 依赖混淆守卫**：声明 npm `@myorg` 私有 + PyPI 内部名清单；私有源宕机时 fail-closed，
  绝不回落公网（攻击者无法靠 DoS 私有源绕过）。
- **US-3 CN 快拉**：DaoCloud/tuna 作主上游，按序尝试并缓存首个成功响应，后续 LAN 速度。
- **US-4 HA + MinIO**：3 副本 L4 LB 后，共享 MinIO(CAS) + PostgreSQL，杀单副本不中断。
- **US-5 离线模式**：`mode: offline` 只服务已缓存内容，缺失返 404，零外连。
- **US-6 apt 节点引导**：GPG 端到端验证（本地 keyring），2MB/s→100MB/s。
- **US-7 git clone 加速**【新】：`git clone` 公共仓库走 Specula 本地 bare mirror，
  force-push/改史触发告警。
- **US-8 缓存统计**【新】：WebUI 仪表盘看每协议已用容量 + 总量 + 命中率。
- **US-9 首个管理员**【新】：首位注册邮箱自动成 admin，配置其余策略与用户。

---

## 6. Verification Policy Model (excerpt)

> **IMPORTANT**: This YAML block is machine-tested — `TestPRDSection6_YAMLLoads` in
> `internal/config/config_test.go` parses it on every CI run. Do NOT edit the schema
> without also updating `specula.example.yaml` and the config structs in
> `internal/config/config.go`.
>
> **Derived from** `specula.example.yaml` (the authoritative tested source of truth).
> The authoritative field reference is `internal/config/config.go`.

```yaml
protocols:
  # ── OCI ─────────────────────────────────────────────────────────────────────
  # 无 cosign 密钥时最高达 tofu 档；配公钥后可达 signed 档。
  # Upstreams each require: name, base_url, priority, official.
  oci:
    mutable_ttl_seconds: 300
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1
        official: false
      - name: docker-hub
        base_url: https://registry-1.docker.io
        priority: 2
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 2
      tofu: enforce          # 首次锁定 digest，变更告警
      # 达到 signed 档需配 cosign 公钥。keyed 模式（CN 可用）；
      # keyless 需 Fulcio/Rekor（CN 被墙，不支持）。
      # cosign:
      #   keys: [/etc/specula/keys/cosign.pub]
      #   tlog: false          # keyless/tlog 在 CN 默认不可用
      # 跨源共识（可选，抬高攻击门槛）:
      # consensus:
      #   quorum: 2
      #   origin_check:
      #     url: https://registry-1.docker.io
      #     via_proxy: https://your.egress.proxy:3128

  # ── PyPI ─────────────────────────────────────────────────────────────────────
  # CN 下可达 consensus + tofu 档。Specula 作唯一 index 防依赖混淆。
  pypi:
    mutable_ttl_seconds: 1800
    # consensus quorum:2 needs TWO independent (non-official) mirrors to vote —
    # the `official: true` upstream becomes the origin WITNESS, not a mirror, so
    # a single non-official upstream would make quorum:2 unsatisfiable and the
    # server would refuse to start. tuna + aliyun are the two voters.
    upstreams:
      - name: tuna
        base_url: https://pypi.tuna.tsinghua.edu.cn
        priority: 1
        official: false
      - name: aliyun
        base_url: https://mirrors.aliyun.com/pypi
        priority: 2
        official: false
      - name: pypi-org
        base_url: https://pypi.org
        priority: 3
        official: true
    verification:
      tiers: [consensus, tofu, checksum]
      quorum: 2
      tofu: enforce
      dependency_confusion:
        private_names: ["mycompany-*"]        # 精确清单（非"信任前缀"）
        private_upstream: https://pypi.internal.example.com/simple
        on_private_down: fail_closed          # 绝不回落公网

  # ── npm ──────────────────────────────────────────────────────────────────────
  # scope 绑定防依赖混淆；unscoped 私有名用显式 denylist。
  npm:
    mutable_ttl_seconds: 120
    upstreams:
      - name: npmmirror
        base_url: https://registry.npmmirror.com
        priority: 1
        official: false
      - name: npm-registry
        base_url: https://registry.npmjs.org
        priority: 2
        official: true
    verification:
      tiers: [consensus, tofu, checksum]
      quorum: 2
      tofu: enforce
      dependency_confusion:
        private_scopes: ["@myorg"]            # scope 绑定（npm 有效）
        private_unscoped: ["internal-svc"]    # unscoped 显式 no-upstream
        private_upstream: https://npm.internal.example.com
        on_private_down: fail_closed

  # ── Go modules ───────────────────────────────────────────────────────────────
  # sumdb 提供 Ed25519 签名 Merkle tree proof — CN 下可达 signed 档。
  # sumdb 是 ProtocolConfig 的直接子块（与 upstreams 平级），不在 verification 下。
  go:
    mutable_ttl_seconds: 300
    upstreams:
      - name: goproxy-cn
        base_url: https://goproxy.cn
        priority: 1
        official: false
      - name: golang-proxy
        base_url: https://proxy.golang.org
        priority: 2
        official: true
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
    sumdb:
      url: https://sum.golang.google.cn      # CN 可达；或 goproxy.cn /sumdb/ 透传
      policy: enforce                        # 验签 tree head + inclusion/consistency
      private_patterns:
        - git.internal.corp/*               # GONOSUMDB：返 403，不转发公网

  # ── apt ──────────────────────────────────────────────────────────────────────
  # GPG 端到端验证（InRelease → Packages → per-file hash），CN 下 signed 档。
  # gpg 为结构体块 {policy, keyring}，非裸字符串。
  apt:
    mutable_ttl_seconds: 0
    upstreams:
      - name: aliyun
        base_url: https://mirrors.aliyun.com/ubuntu
        priority: 1
        official: false
      - name: ubuntu-archive
        base_url: https://archive.ubuntu.com/ubuntu
        priority: 2
        official: true
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      tofu: enforce
      gpg:
        policy: enforce
        keyring: /etc/specula/ubuntu-archive-keyring.gpg   # 本地锚，带外获取

  # ── Helm ─────────────────────────────────────────────────────────────────────
  # .prov 文件 GPG 验签可达 signed 档；无 .prov 时降级（policy: warn）。
  helm:
    mutable_ttl_seconds: 1800
    upstreams:
      - name: charts-example
        base_url: https://charts.example.com
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
      tofu: enforce
      provenance:                            # .prov GPG 验证
        policy: warn
        keyring: /etc/specula/helm-keyring.gpg

  # ── git ──────────────────────────────────────────────────────────────────────
  # bare mirror 加速；git 对象天然内容寻址。
  # git 特有配置在 git: 子块下（与 upstreams/verification 平级）；
  # upstreams 仅为满足通用字段校验，实际路由由 git.allowed_upstreams 控制。
  git:
    mutable_ttl_seconds: 30
    upstreams:
      - name: github
        base_url: https://github.com
        priority: 1
        official: true
    git:
      allowed_upstreams: [github.com, gitlab.com]
      mirror_dir: /var/specula/git
      sync_stale_after: 30s
      public_only: true                      # 私有仓/带 Authorization → bypass
      fail_closed: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
      tofu: enforce
      signed_refs:                           # 可选：验签名 tag/commit
        policy: warn
        allowed_signers: /etc/specula/git-allowed-signers
```

---

## 7. Metrics (Prometheus)

暴露在控制面 `/metrics`。全部在 `internal/metrics` 的包初始化时注册到进程默认
registry —— **注册不得依赖于构造某个对象或某次请求**（`specula_cache_bytes{protocol="git"}`
曾经只有人打开 WebUI 才出现，见 7600a0e）。

| Metric | Type | Labels |
|---|---|---|
| `specula_requests_total` | counter | `protocol, method, status` |
| `specula_cache_hits_total` / `_misses_total` | counter | `protocol` |
| `specula_cache_bytes` | gauge | `protocol`（total = `sum()`）【新】 |
| `specula_cache_objects` | gauge | `protocol`【新】 |
| `specula_upstream_latency_seconds` | histogram | `protocol, upstream` | TTFB only |
| `specula_response_bytes_total` | counter | `protocol` | body bytes to clients |
| `specula_request_duration_seconds` | histogram | `protocol` | e2e request time incl. body |
| `specula_verification_total` | counter | `protocol, check, tier, result`（tier=signed/consensus/tofu/checksum）【改】 |
| `specula_upstream_blocked` | **gauge** | `protocol, upstream`（auto-block 状态）【新】 |

### 7.1 标签基数 (cardinality)

`protocol` 有界（11）、`method`/`status` 有界、`upstream` 由 config 限定、`check` 由已注册
verifier 集合限定、`tier`/`result` 各为固定枚举。

**绝不**用作标签：repo / package name / module path / image name / tag / digest / URL / path。
这些由请求方（含攻击者）控制且无界——一次 typosquat 扫描就能为每个包名生成一条新时间序列并
压垮 Prometheus。按包粒度的问题属于日志与 Admin API，不属于标签。

### 7.2 hit / miss 的定义（二层缓存下）

判据是**响应体字节的来源**：

- **hit** —— 响应体**未从上游取 body** 即产生。
- **miss** —— 响应体**必须从上游取 body**。

三个 ARCHITECTURE §3 引出的歧义情形，按此判据裁定：

| 情形 | 判定 | 理由 |
|---|---|---|
| mutable 重验返回 304 | **hit** | 与上游通了话，但上游没发 body；服务的字节来自缓存 |
| 上游失败 serve-stale（fix H1） | **hit** | body 来自缓存；上游故障由 `upstream_blocked` 与 latency 缺失观测反映 |
| meta 命中但 blob 缺失 → 回源（fix M1） | **miss** | 元数据行存在但字节不存在，仍需回源；算 hit 等于统计账本而非缓存 |

**此定义的边界（必须明说）**：`hit` **不**等于"没碰上游"。304 重验是 hit，却仍花掉一次
完整 CN 往返（实测 0.25s ~ 5.7s）。95% 的 hit ratio 只意味着 95% 的请求省下了 **body**
传输，不意味着 95% 的请求没碰上游。"到底多少次碰了上游"由
`specula_upstream_latency_seconds` 的 `_count` 回答（304 也观测）。两个指标合起来才诚实。

从不查缓存的请求（`/v2/` ping、坏路径 404、方法不允许）**两个计数器都不动**，只计入
`requests_total`。故 hit/miss 的分母是"查过缓存的请求数"——这是该比值唯一有意义的分母。

计数绑定在**请求**上，不在 `cache.Lookup` 上：handler 每请求会多次 Lookup，在那里计数会让
分母变成"lookup 次数"，得到一个没人问过的量。

### 7.3 `specula_upstream_latency_seconds`

测量的是 **time-to-response-headers（上游响应及时性）**，**不是 body 传输时间**——body 是流式
透传给下游客户端的，其耗时反映的是客户端链路速度而非镜像速度。

因此**不可**将其读作"这个镜像有多慢"：镜像可以 250ms 就回 header，然后以 27 kB/s 传 body
（aliyun 实测），一个 50MB 的 .deb 要传半小时，而本指标仍报 0.25s。它只回答一个问题：
**上游多久开始响应**。

Bucket 依据实测 CN TTFB 选取（非 `prometheus.DefBuckets`）：实测呈双峰——warm 簇 0.24–0.81s、
stall 簇 5.0–5.7s。DefBuckets 在此有害：它把 5 个 bucket 花在 100ms 以下（跨境往返不可能这么快）、
1s 与 2.5s 之间没有任何边界、且止于 10s 而 upstream client 超时是 30s（10–30s 的请求会全部落进
`+Inf`，与挂死无法区分）。取值见 `internal/metrics.cnLatencyBuckets`。

### 7.4 `specula_upstream_blocked` 是 gauge

它是**状态**（PRD 原文即"auto-block 状态"），有升有降 ⇒ gauge。名称不带 `_total` 是**正确**的
（Prometheus 将 `_total` 保留给 counter），故沿用原名。

含义精确为：*该 upstream 已连续 5 次**瞬时(transient)**失败，正处于 30s 的拒绝重试窗口内*。
它**不**等于"该 upstream 不可达"。`internal/upstream/client.go` 的失败分类决定了它：

| 失败 | 分类 | 是否计入 auto-block |
|---|---|---|
| connection refused | 网络错误 | ✅ transient |
| DNS 失败 | 网络错误 | ✅ transient |
| TCP/TLS 超时 | 网络错误 | ✅ transient |
| HTTP 5xx、429 | | ✅ transient |
| HTTP 451、403、404 | 4xx | ❌ **永不触发** |

**CN 场景（G5）的结论**：GFW 式干扰通常表现为连接超时/重置，属 transient，**会**把该
gauge 打到 1。但 HTTP 451（法律封锁）是格式良好的 4xx——client 立刻换下一个 upstream，
失败streak 永不递增，故一个永远回 451 的 upstream 会**永远报 blocked=0**。
所以 `blocked=0` **不**代表健康，也可能代表"正在以一种我们故意不重试的方式失败"。

### 7.5 `specula_verification_total` —— 让 G2 可被独立验证

这是让**诚实分级信任模型（G2）可观测**的指标。没有它，operator 只能听我们自称验了什么。

- `tier` —— **必须**是 verify chain 实际达成的档（`artifact.Result.Tier`），
  **绝不**从 config 断言，**绝不**出现 G2 四档以外的第五个值。
  给一个仅通过 checksum 的 artifact 打上 `tier="signed"`，是本代码库能犯的最严重的错误。
- `check` —— 单个 verifier 的 `Name()`（checksum / tofu / sumdb / gpg / helm-prov /
  git-signed / consensus / cosign），或保留值 **`chain`** 表示整条链的聚合裁决。
  `check="chain"` 的 `tier` 就是 `cache.Store` 写入 `CacheEntry.Tier` 的那个值——
  指标与 DB 是同一个数的两种呈现，故可互相交叉核对，二者不一致即为真实信号。
- `result` —— `pass` / `warn` / `fail`。
- **被跳过(skip)的 check 不产生任何 series**。自门控退出的 verifier 没有达成任何档；
  为它记 `tier="checksum", result="pass"`（这正是 `StatusSkip` 出现之前
  `artifact.Result` 对 skip 的取值）等于在 /metrics 上宣称 gpg 检查通过了我们服务过的
  每一个 npm 包。**缺席**就是本指标表达"这项检查没在这里跑过"的方式。

### 7.6 注册 ≠ 出现在 /metrics

一个已注册但没有任何子 series 的 `*Vec` **不会**向 `Gather()` 贡献任何 metric family，
因此根本不出现在 `/metrics` 上。二者的桥梁是**预初始化**标签组合，而这仅在标签集
**有界且无需流量即可知**时才可能：

| Metric | 预初始化 | 无流量时可见 |
|---|---|---|
| `cache_hits/misses_total{protocol}` | 11 个 protocol，init() 时 | ✅ 真实的 0 |
| `upstream_blocked{protocol,upstream}` | 由 config 声明（`PreInitUpstream`） | ✅ 真实的 0 |
| `cache_bytes{protocol}` | 11 个 protocol，init() 时 → **启动同步测量**覆写 | ✅ 真实的 0（冷）/ 真实值（热） |
| `cache_objects{protocol}` | ❌ 不透明缓存对象数不可数 | 首次可数测量后 |
| `requests_total{protocol,method,status}` | ❌ status 不可预知 | 首次请求后 |
| `verification_total{...,tier,result}` | ❌ 预初始化会捏造从未发生的组合 | 首次验证后 |

`requests_total` / `verification_total` 的**缺席是正确且诚实的**：它表示"尚未发生"，而非
"坏了"。为它们预初始化等于捏造从未出现过的组合，与本节要根除的是同一类谎言。

`cache_bytes` **可以**预初始化，且这不是捏造——**字节永远可测**（CAS 行 `SUM(size)`；
git 不透明镜像 `du -sb`），冷缓存下"0 字节"是一次真实测量，与 `cache_hits=0` 同类。
两处避免了 7600a0e 那类谎言：(1) 注册与预初始化都在 `internal/metrics` 的 **init()**，
不再是构造 `stats.Collector` 的副作用（曾导致 `cache_bytes{protocol="git"}` 要等人点开
WebUI 才出现）；(2) **首次测量在启动时同步完成**（`cmd/specula` 在控制面开始监听前调用
`collector.Refresh`），所以热/持久存储（SQLite 文件、HA 共享库）重启后不会在头 30s 里被
刮到过时的 0——init() 那批 0 在服务器可达之前就已被真实 `SUM(size)` 覆写。
`cache_objects` **不**预初始化：不透明缓存（git bare mirror）的对象藏在 packfile 里、
非可数 CAS 行（`ObjectsCountable=false`），预初始化 `cache_objects{git}=0` 会把"不可数"
谎报成"零个"，正是 e181e5a 立下的"渲染 '—'，绝不捏造零"那条规矩要根除的。**缺席**才是
本 gauge 表达"不可数/未测量"的方式。

> **已知缺口（未解决，不得当作已解决）**：`verify.Chain` 在**没有任何 verifier**时返回
> `TierChecksum` + pass，即在一次哈希都没比过的情况下声称 checksum 档。该 Result 会被
> `cache.Store` 写进 `CacheEntry.Tier`。此路径在生产不可达（`cmd/specula` 必然注册
> ChecksumVerifier + TofuVerifier，空 chain 是 operator 无法构造的退化配置），且已从
> `verification_total` 中排除（不记 series），但**这个声称本身仍是不诚实的**。
> 诚实的修法需要 G2 增加"什么都没验"的词汇，属规格问题，不在本次以静默代码改动了结。

---

## 8. Ports

单一数据面端口按**路径前缀**分发全部协议（非每协议一个端口）。

| 平面 | 端口 | 内容 |
|---|---|---|
| **数据面** | **7732** | 11 协议:`/v2/`(OCI+registry) `/pypi/` `/npm/` `/go/` `/apt/` `/helm/` `/tarball/` `/git/` `/cargo/` `/conda/` `/hf/` + `/token` |
| **控制面** | **7733** | 内嵌 WebUI + Admin API + `/healthz` `/readyz` `/metrics` + `/token` |

**为什么是 7732/7733**：电话键盘上 `S-P-E-C` = `7-7-3-2` —— 既是 **Spec**ula，也是这个代理
赖以立身的那些 **spec**（OCI Distribution / PEP 503 / Debian Repository Format …）。

刻意**不用** 5000/8080：`5000` 是 Docker registry 与 zot 的默认端口，也就是"想装 OCI 缓存的
主机上最可能已经在跑的东西"；`8080` 不必解释。同时避开 `7760-7766`（常见的 pull-through-cache
组件栈占用该段）。

两个端口均可配置（`server.data_plane_addr` / `server.control_plane_addr`）。

## 9. Milestones

| Phase | Scope | Status |
|---|---|---|
| **v0.1** | OCI proxy + **CAS blob store** + 二层缓存 + verify-on-write + checksum/tofu 档 | ✅ done |
| **v0.2** | 管理平面：内嵌 WebUI + 邮箱认证（首个=admin）+ 缓存统计仪表盘 | ✅ done |
| **v0.3** | Go module proxy（sumdb 透传验证）+ PyPI（单 index + 共识 + dep-confusion）| ✅ done（PyPI consensus 已接线，metadata-only sha256）|
| **v0.4** | npm（scope 绑定 + 共识）+ apt（GPG 端到端验证）| ✅ done（npm scope/dep-confusion + apt GPG；npm consensus 受 sha512-only metadata 限制停在 tofu）|
| **v0.5** | cosign keyed（OCI signed 档）+ Helm（.prov signed 档）| ✅ done（cosign keyed 校验 + 签名发现均已接线）|
| **v0.6** | git clone 加速（bare mirror + 签名 ref 验证 + force-push 告警）| ✅ done |
| **v0.7** | PostgreSQL HA + 分布式 stampede 锁 + 跨节点统计聚合 | ✅ done（PG + redis coalesce + `scripts/ha-minikube.sh`：暖缓存 / 杀副本 / 再拉命中）|
| **v0.8** | tarball + consensus 档（多镜像 quorum + origin-check）+ CN mirror profile | ◐ partial（tarball + consensus 引擎已落地；tarball metadata-only 共识不可用停在 tofu）|
| **v0.9** | Cargo sparse + conda channel + Hugging Face Hub（`HF_ENDPOINT`）| ✅ done |
| **v0.10** | 供应链入口治理：新版本冷静期（maturity）+ 依赖混淆防呆 + Events 可行动化 | ✅ done |
| **v1.0** | anti-rollback 单调版本状态 + SBOM 生成 + 自建 sigstore 栈（气隙 keyless 可选）| ☐ planned |

> **v0.10 动机（相对竞品 / 攻击面）**：Harbor/Spegel 主打缓存与扫描；JFrog Curation / Socket
> 主打「新包冷静期」。Specula 已有诚实档（signed/consensus/tofu/checksum）与 dep-confusion
> fail-closed，但挡不住「合法维护者账号被劫持后立刻发毒版」（Shai-Hulud / chalk 类窗口）。
> v0.10 用 **maturity（min_age 策略闸，非密码学档）** 对齐该窗口；用 **sole-index 防呆**
> 堵住客户端双源；用 **Events 持久化 + TOFU 漂移** 让告警可行动。明确不承诺防 XZ 类
> 「仍带合法签名的长线投毒」。

> v0.2-hardening / main 说明：11 协议数据面 + 四档诚实信任模型（checksum/tofu/consensus/signed）已端到端接线。consensus 引擎（quorum + origin-check + 并行取 digest）与 cosign keyed 锚均已实现并从 config 自动装配、按协议自门控。metadata-only 的 sha256 跨源共识当前仅对 OCI（Docker-Content-Digest）与 PyPI（PEP 503 `#sha256=`）可用；npm/tarball/cargo 因 metadata 不暴露跨源 sha256 默认停在 tofu，绝不 fail-close 真实拉取。
