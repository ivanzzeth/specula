# Specula — Product Requirements Document (v0.2)

> **Tagline**: Mirror everything. Verify what you can. Never lie about the rest.
>
> Specula is a lightweight, high-availability, multi-protocol artifact proxy that
> caches OCI images, PyPI/npm/Go/apt packages, Helm charts, generic tarballs, and
> accelerates public `git clone` — in a single Go binary with an **honest, tiered
> supply-chain trust model** and an embedded management WebUI.
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

**Specula 的定位**：一个 Go 二进制、8 协议、**诚实分级的供应链验证**（能验真实性就验，不能就明确标注
仅 TOFU/共识，绝不谎称防篡改）、从第一天起横向扩展、内嵌管理 WebUI。

---

## 2. Goals

### G1 — 单一二进制多协议

原生服务 8 个协议（用成熟 Go 库做底座，非编排容器——见 DESIGN-REVIEW §5）：

- **OCI** — 容器镜像（Docker v2 + OCI Distribution v1）
- **PyPI** — Python 包（PEP 503 / 691）
- **npm** — Node 包
- **Go modules** — GOPROXY 协议 + sumdb 透传
- **apt** — Debian/Ubuntu（InRelease/Packages/pool）
- **Helm** — chart（OCI 形态复用 OCI handler；经典 HTTP repo：index.yaml + tgz + .prov）
- **tarball** — URL-keyed 通用下载缓存
- **git** — 公共仓库 `git clone` 加速（smart-HTTP + 本地 bare mirror）

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
| Helm (repo) | **signed** | `.prov` GPG 签名 + keyring | 无 .prov 降级 |
| OCI | **signed**（需配公钥） | cosign keyed（关闭 tlog）；否则 consensus/tofu | keyless 在 CN 默认不可用 |
| git | **signed**（可选） | 签名 tag/commit（配 allowed-signers）；否则 tofu | git 对象天然 Merkle |
| npm | **consensus / tofu** | provenance CN 不可用且覆盖 ~3–12% | 实务上共识 + TOFU |
| PyPI | **consensus / tofu** | PEP 740 CN 不可用且覆盖 ~5% | 实务上共识 + TOFU |
| tarball | **consensus / tofu** | 无原生签名 | 可配官方源比对 |

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
- **无 Maven / Cargo / Hex** — 未来协议插件
- **无 GUI 策略编辑器写回 git** — WebUI 可编辑运行时配置，但声明式 YAML 仍是 source of truth

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

```yaml
protocols:
  oci:
    upstreams:
      - url: https://docker.m.daocloud.io   # CN 镜像优先
      - url: https://registry-1.docker.io   # fallback
    verification:
      cosign:
        mode: keyed          # keyed（CN 可用）| keyless（需 Fulcio/Rekor）| off
        keys: [/etc/specula/publisher.pub]
        tlog: false          # CN 下关闭（Rekor 被墙）
        policy: warn         # warn | enforce
      consensus:
        enabled: true
        quorum: 2            # ≥2 独立镜像 digest 一致
        origin_check:        # 可选：代理直连官方源比对
          url: https://registry-1.docker.io
          via_proxy: ${HTTPS_PROXY}
      tofu: enforce          # 首次锁定 digest，变更告警

  pypi:
    upstreams:
      - url: https://pypi.tuna.tsinghua.edu.cn/simple
      - url: https://pypi.org/simple
    mode: single_index       # Specula 作唯一 index（防依赖混淆）
    verification:
      consensus: { enabled: true, quorum: 2 }
      tofu: enforce
      dependency_confusion:
        private_names: ["mycompany-*"]   # 精确清单（非"信任前缀"）
        private_upstream: "https://pypi.internal.example.com/simple"
        on_private_down: fail_closed     # 绝不回落公网

  npm:
    upstreams:
      - url: https://registry.npmmirror.com
      - url: https://registry.npmjs.org
    verification:
      consensus: { enabled: true, quorum: 2 }
      tofu: enforce
      dependency_confusion:
        private_scopes: ["@myorg"]       # scope 绑定（npm 有效）
        private_unscoped: ["internal-svc"]  # unscoped 显式 no-upstream
        private_upstream: "https://npm.internal.example.com"
        on_private_down: fail_closed

  go:
    upstreams: [https://goproxy.cn, https://proxy.golang.org]
    sumdb:
      url: sum.golang.google.cn          # CN 可达；或 goproxy.cn /sumdb/ 透传
      policy: enforce                    # 验签 tree head + inclusion/consistency
      private_patterns: ["git.internal.corp/*"]  # GONOSUMDB：返 403，不转发公网

  apt:
    upstreams: [http://mirrors.aliyun.com/ubuntu, http://archive.ubuntu.com/ubuntu]
    verification:
      gpg: enforce
      keyring: /etc/specula/ubuntu-archive-keyring.gpg   # 本地锚

  helm:
    upstreams: [https://charts.example.com]
    verification:
      provenance:            # .prov GPG 验证
        policy: warn
        keyring: /etc/specula/helm-keyring.gpg
      tofu: enforce

  git:
    allowed_upstreams: [github.com, gitlab.com]
    mirror_dir: /var/specula/git
    sync_stale_after: 30s
    public_only: true        # 私有仓/带 Authorization → bypass passthrough
    fail_closed: true
    verification:
      signed_refs:           # 可选：验签名 tag/commit
        policy: warn
        allowed_signers: /etc/specula/git-allowed-signers
      tofu: enforce          # ref→SHA 锁定，非快进更新告警
```

---

## 7. Metrics (Prometheus)

| Metric | Labels |
|---|---|
| `specula_requests_total` | `protocol, method, status` |
| `specula_cache_hits_total` / `_misses_total` | `protocol` |
| `specula_cache_bytes` | `protocol`（total = `sum()`）【新】 |
| `specula_cache_objects` | `protocol`【新】 |
| `specula_upstream_latency_seconds` | `protocol, upstream` |
| `specula_verification_total` | `protocol, check, tier, result`（tier=signed/consensus/tofu/checksum）【改】 |
| `specula_upstream_blocked` | `protocol, upstream`（auto-block 状态）【新】 |

---

## 8. Protocol Port Defaults

| Protocol | Port | | Protocol | Port |
|---|---|---|---|---|
| OCI | 5000 | | apt | 5004 |
| PyPI | 5001 | | tarball | 5005 |
| npm | 5002 | | helm | 5006 【新】 |
| Go | 5003 | | git | 5007 【新】 |
| Admin/WebUI | 8080 | | Metrics | 9090 |

`--bind-all` 单端口路径路由（DaemonSet hostNetwork 友好）。

---

## 9. Milestones

| Phase | Scope |
|---|---|
| **v0.1** | OCI proxy + **CAS blob store** + 二层缓存 + verify-on-write + checksum/tofu 档 |
| **v0.2** | 管理平面：内嵌 WebUI + 邮箱认证（首个=admin）+ 缓存统计仪表盘 |
| **v0.3** | Go module proxy（sumdb 透传验证）+ PyPI（单 index + 共识 + dep-confusion）|
| **v0.4** | npm（scope 绑定 + 共识）+ apt（GPG 端到端验证）|
| **v0.5** | cosign keyed（OCI signed 档）+ Helm（.prov signed 档）|
| **v0.6** | git clone 加速（bare mirror + 签名 ref 验证 + force-push 告警）|
| **v0.7** | PostgreSQL HA + 分布式 stampede 锁 + 跨节点统计聚合 |
| **v0.8** | tarball + consensus 档（多镜像 quorum + origin-check）+ CN mirror profile |
| **v1.0** | anti-rollback 单调版本状态 + SBOM 生成 + 自建 sigstore 栈（气隙 keyless 可选）|
