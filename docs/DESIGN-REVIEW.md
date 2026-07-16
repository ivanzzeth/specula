# Specula — 对抗性设计审查 (Adversarial Design Review)

> 本文档记录对 v0.1 PRD/ARCHITECTURE 的红队审查、成熟方案调研，以及由此推导出的强化设计原则。
> 强化后的规格见 [PRD.md](./PRD.md) 与 [ARCHITECTURE.md](./ARCHITECTURE.md)。

---

## 0. 定盘结论 (Decisions)

| 维度 | 决策 | 影响 |
|---|---|---|
| 首要部署场景 | **CN 区域 + 联网**（走镜像加速，可访问部分公网信任锚） | 决定哪些信任锚可用 |
| 加速 vs 安全冲突 | **诚实分级验签**：能验则验，不能验则明确标注"仅 TOFU"，绝不谎称"防篡改" | 信任模型的核心原则 |
| 产出范围 | **重做整体架构 + 信任模型（全协议）** | 8 协议全覆盖 |
| 协议清单 | oci, pypi, npm, go, apt, tarball, **helm**, **git**（6→8） | 新增 Helm、git clone 加速 |
| 架构路线 | **原生单一二进制**（各协议用成熟 Go 库做底座），非 docker-compose 编排 | 见 §5 |
| 管理平面 | 单一二进制内嵌 WebUI + 邮箱注册/登录 + 首个邮箱默认 admin | 参照 ai-sandbox |

---

## 1. 核心信任模型缺陷 (Trust Model — 根本性)

v0.1 PRD 的 G2 把"SHA-256 match against upstream manifest"当作供应链防护。**这是根本性的信任模型错误。**

**Integrity ≠ Authenticity：**
- **完整性 (Integrity)**：字节相对某个参考值未损坏。哈希提供的只是这个——且只相对你手里那个参考值。
- **真实性 (Authenticity)**：字节确实来自真实发布者，可对抗攻击者无法控制的信任锚验证。

若 blob 和它的 checksum/manifest 都来自**同一个被信任的镜像**（daocloud/aliyun），攻击者控制镜像即可同时改写两者，校验必过——这是**循环验证**：让攻击者给自己的作业打分。checksum 只防传输损坏，不防来源造假。

> 学术依据 (Kalu & Davis, arXiv 2510.04964, 2025)："Checksums verify content integrity but not origin or publisher identity … network or mirror-level attacks have historically subverted distribution paths." 真实性要求发布者用**独立信任锚**（带外获取的公钥）签名了**承诺 artifact 字节的东西**。

### 1.1 CN 场景对信任锚的硬约束

| 信任锚 | CN 可用性 | 结论 |
|---|---|---|
| **apt 发行版 keyring**（随 OS 预置，带外） | ✅ 完全离线可验 | **端到端真实性金标准**——本地 keyring → InRelease 签名 → Packages SHA256 → 每个 .deb SHA256。恶意镜像无发行方私钥，无法伪造。 |
| **Go sumdb**（Ed25519 签名 tree head + Merkle 证明，公钥编进 `go`） | ✅ 经 goproxy.cn `/sumdb/` 或 `sum.golang.google.cn` 代理仍可验 | **代理"只能拒绝、无法伪造"**——tree head 由 Google 私钥签名，客户端验 inclusion/consistency 证明。 |
| **Helm `.prov`**（GPG 签名 + keyring） | ✅ 离线可验 | 又一 `signed` 金标准，与 apt 同级 |
| **cosign keyed**（预置发布方长期公钥，关闭 tlog） | ✅ 离线可验 | OCI 真实性可选锚，`--key --insecure-ignore-tlog` |
| **cosign keyless**（Fulcio/Rekor/OIDC） | ❌ Rekor/Fulcio/TUF CDN 通常被墙 | CN 下**默认不可用**，除非自建 sigstore 栈或预置 trusted_root.json |
| **npm provenance**（sigstore/Rekor） | ❌ 需 Rekor | CN 下不可用；且覆盖率仅 ~3–12% → npm 实务上仅 TOFU |
| **PyPI PEP 740 attestations**（sigstore） | ❌ 需 Rekor | CN 下不可用；覆盖率 ~5% → PyPI 实务上仅 TOFU |

**结论**：apt / Go / Helm 三个生态开箱可得**真实的、完全离线的、防镜像作恶的真实性**；OCI 需预置公钥（keyed cosign）；**npm/PyPI 的绝大多数包在 CN 下只能做 TOFU + 跨源共识**，不能谎称真实性。

### 1.2 四档诚实信任模型 (Honest Tiered Trust)

```
signed     密码学锚（apt keyring / Go sumdb / Helm .prov / cosign keyed）—— 最高，防来源造假
   ↑
consensus  跨源共识（多镜像 quorum 比对 digest，或代理直连官方源比对）—— 抬高门槛，非密码学真实性
   ↑
tofu       首次锁定 digest + 变更告警（force-push/改史/版本重写检测）—— 防"事后静默篡改"
   ↑
checksum   仅防传输损坏 —— 最低，绝不单独充当供应链防护
```

**跨源共识 (Cross-Source Consensus)** — 由需求方提出，填补无签名生态的空白：
- **多镜像 quorum**：同一 artifact 从 N 个**相互独立**（不同 CDN/运营方）的镜像并行取 digest，≥quorum 一致才 PASS。攻击者需一致地投毒所有配置镜像才能绕过。
- **官方源比对 (origin-check)**：经出口代理直连官方源（pypi.org / registry.npmjs.org / registry-1.docker.io）取 digest 与镜像结果比对，官方源作权威见证人。
- **只比 digest/manifest，不下载全 blob**：HEAD/manifest 阶段即可发现分歧，一致后才落一份盘。
- 诚实定位：**不是密码学真实性**，但在无签名生态里是最强的可得防护。此思路与信任模型研究独立给出的"first-fetch 用 ≥2 独立上游交叉核对 digest 再 pin"完全一致。

**每协议可达等级矩阵**见 [PRD.md §信任模型](./PRD.md)。

---

## 2. 红队发现清单 (Findings)

严重度：**C**ritical（阻断，必须修）/ **H**igh / **M**edium / **L**ow。

### C1 — checksum 当作供应链防护（信任模型根本错误）
见 §1。**修复**：区分 integrity/authenticity，落地四档诚实信任模型，文档诚实标注每协议可达等级。

### C2 — 流式验证悖论：时序允许把未验证字节发给客户端
v0.1 ARCHITECTURE §3 时序图 cache-miss 分支先把 bytes 交给 client 再 verify，FAIL 时返回 502——但字节可能已下发，无法收回。
**修复**：**verify-on-write / quarantine**。miss 时先落盘到隔离区，验证通过才提升为可服务缓存并下发；**只从已验证缓存对外服务**，绝不流式转发未验证上游字节。对大 blob 的 tee 流式仅在"已验证缓存回填"路径使用。

### C3 — `Verify(..., blob []byte, ...)` 全量入内存
多 GB 的 OCI layer / deb / npm tgz 全读进内存，违背 G4（<64MB RSS），且是 trivial 内存 DoS。
**修复**：接口改为 `io.Reader` / 临时文件句柄 + 流式 digest（`hash.Hash` 边写边算）；签名验证对落盘文件做，不驻留内存。

### H1 — mutable tag / index 一致性缺失
cache key = {Protocol,Name,Version,Digest}，但 OCI tag(`latest`)、npm dist-tag、PyPI index、apt InRelease 都**可变**；v0.1 只有一个全局 `cache.ttl:24h`。会长时间服务过期/错误 manifest（正是 Harbor #19429 的坑）。
**修复**：**二层缓存**——(a) 不可变 blob 按 digest 内容寻址、永不过期；(b) 可变元数据短 TTL + 条件 GET 重验（ETag/If-None-Match、If-Modified-Since→304）+ 上游失败时 serve-stale。见 §3。

### H2 — freshness gate 语义反了，会破坏可复现构建
v0.1 G2 "reject artifacts older than N days" 想防 stale/rollback，但：钉版依赖本就旧、会被误杀；且这根本不防 rollback（rollback 是回退到已知漏洞版本，与年龄无关）。
**修复**：删除"按年龄拒绝"。改为 **TUF 式单调版本状态**——"绝不接受比已见更低版本的**已签名索引**"（有状态、per-channel）；对无签名生态仅做 TOFU digest 变更告警。metadata 保持新鲜（证明镜像没冻结你）但允许 artifact 本身很旧。

### H3 — dependency confusion guard 对 PyPI 失效
PyPI 是**扁平命名空间**，无 scope/org；`private_namespaces` 前缀规则是**安全剧场**（攻击者也能在公网注册该前缀）。攻击本质是 pip 向所有 index 查询取最高版本，`--extra-index-url` 是元凶。
**修复**：分生态策略（见 §4）。

### H4 — 私有上游宕机时 guard 的 fail 行为未定义
若私有 upstream 404/超时就回落公网，攻击者只要 DoS 私有源即可绕过。
**修复**：配置为私有的命名空间/包名，私有源不可达时 **fail-closed**（返回 5xx，绝不回落公网）——照抄 Go 的"仅 404/410 才回落，其余硬失败"。

### H5 — Go sumdb "enforce against sum.golang.org" 与 CN-first 矛盾
sum.golang.org 在 CN 通常不可达。
**修复**：sumdb 校验走 GOPROXY 的 `/sumdb/` 代理端点或 `sum.golang.google.cn`，验证 Ed25519 签名 + inclusion/consistency proof；本地持久化已见 tree size 做一致性（防分叉/回滚）。私有模块用 `GONOSUMDB` glob 返回 403，绝不把私有名转发公网 sumdb（Athens `NoSumPatterns` 模式）。**绝不默认 `GOSUMDB=off`**。

### M1 — blob/metadata 写序与孤儿一致性
**修复**：**blob 先落、metadata 后写**；读路径把"metadata 命中但 blob 不存在"当作 miss；后台 GC 清孤儿 blob。

### M2 — BlobStore 无 Range，破坏 containerd 断点续传
**修复**：`Get(ctx, digest, offset, length)` 支持 Range；S3 driver 透传 Range 头。

### M3 — cache stampede 锁缺少 fencing / 有界等待
**修复**：进程内 `singleflight`（按不可变身份 digest/module@version/pkg@version 键）+ 跨实例短 TTL 分布式锁（带 owner-checked 释放）+ 有界等待。注意 singleflight 的**错误放大**（一次上游失败拖垮整群 waiter）与 **panic 语义**（DoChan 在新 goroutine 重抛可崩进程）——用 DoChan+ctx 超时、poison 时 `Forget`、按 key 分片多 Group。大 blob 用 tee 流式，负缓存 404。

### M4 — 缓存键未含 upstream，镜像不一致时歧义
**修复**：blob 内容寻址天然按 digest 隔离；tag→digest 解析记录来源 upstream + 首次 TOFU 锁定，跨 upstream digest 冲突时告警并按策略 fail（正是 consensus 档）。

### L1 — 无消费者认证 + hostNetwork 127.0.0.1 横向面
DaemonSet hostNetwork 下同节点任意 pod 可访问 127.0.0.1:5000 投毒缓存。
**修复**：记入威胁模型显式假设；可选本地 token / unix socket 权限。注意这与新增的**管理平面认证**是两回事——数据面仍无消费者认证。

### L2 — SQLite(DaemonSet) 与 PG(HA) 语义差异
**修复**：明确 SQLite 仅单实例节点本地、不跨实例共享；给出 WAL 模式与容量说明。

---

## 3. 二层缓存模型 (成熟方案共识)

所有成熟方案（Artifactory / Nexus / zot / Harbor / Athens / devpi / verdaccio / apt-cacher-ng）都收敛到同一不变式：

| 协议 | 不可变（CAS，永久缓存，绝不重验） | 可变（短 TTL + 条件 GET 重验 + serve-stale） |
|---|---|---|
| OCI | blob/config/manifest **by digest** | **tag → digest** 映射（`latest`） |
| Go | `@v/<v>.info/.mod/.zip` | `@v/list`, `@latest` |
| PyPI | wheel/sdist 文件（永不变） | `/simple/<pkg>/` index 页 |
| npm | `*.tgz` tarball | packument 元数据 |
| apt | `pool/*.deb` | InRelease/Release/Packages |
| Helm | chart `*.tgz` | `index.yaml` |
| git | git objects（按 SHA） | refs（branch/tag → commit SHA） |

**可借鉴的具体数字**（作为 Specula 默认值参考）：
- Artifactory：元数据 TTL **7200s(2h)**、负缓存(404) **1800s(30min)**、元数据取回超时 60s；不可变永不重验。
- Nexus：metadata max age **1440min(24h)**、Release artifact max age **-1(never)**、Not Found TTL 24h。
- devpi：`mirror_cache_expiry` **1800s(30min)**（仅 index），文件永久缓存，上游失败 serve-stale。
- verdaccio：packument `maxage` **2min**（ETag→304 重验），tarball 永久不重验。
- Harbor：tag 用 rate-limit-aware **HEAD** 探测新鲜度（HEAD 不耗 Docker Hub 限额）。**教训（#19429）**：tag TTL 过长会服务过期 digest。

**config 哨兵值**（偷 Nexus）：`-1` = 永不重验（不可变），`0` = 每次请求重验。

**CAS 存储**（偷 Artifactory/zot）：按内容哈希存储，path→hash 映射入 DB，copy/move/delete = 引用操作，末次引用才物理删；写入时验证 digest。本地 FS 用硬链接去重，S3 后端用引用计数 DB。

**untrusted-cache 原则**（偷 apt-cacher-ng）：**缓存本身不做验签，信任上游生态的签名链，让客户端做端到端验证**。缓存被攻破最坏只能造成 staleness/DoS，无法注入包。TLS 被代理天然打断——但 apt 的保证是签名哈希链，transport-agnostic，能穿透不可信中间人。

---

## 4. 依赖混淆 — 分生态防御 (成熟方案)

**攻击本质**（Birsan 2021，$130K+ 赏金）：多源解析 bug，非坏包 bug。仅当客户端能同时访问私有源和公网源的**同名**包、且无任何东西把该名绑定到私有源时才存在。攻击者注册泄露的内部名到公网、版本号拉到 `9999.0.0`、带 install hook。

| 生态 | 命名空间模型 | 默认可混淆? | 代理侧防御 |
|---|---|---|---|
| **npm** | 两层：unscoped `pkg` / scoped `@org/pkg` | **unscoped: 是；scoped: 否**（攻击者无法在你的 scope 下发布） | scope→registry 绑定；unscoped 私有名显式 no-upstream 黑名单 |
| **PyPI** | **扁平**（无 scope/org） | **是——结构性最差** | Specula 作**唯一 index**，服务端 private-first 合并，manifest 名**公网根本不查询**；`--extra-index-url` 在 CI 里 lint 禁用；公网防御性抢注内部名 |
| **Go** | **域名限定** `github.com/org/mod` | **基本免疫**（要影子必须控制该域名/路径） | `GOPROXY=proxy,direct` + 精确 `GOPRIVATE`；未知内部路径返 404 不重定向公网 |

**通用代理规则**：
1. 客户端只指向**一个**端点（`registry=` / 仅 `--index-url` / `GOPROXY=proxy,direct`）；绝不让客户端携带 `--extra-index-url` 或多源列表。
2. 维护显式**私有名清单**（org 拥有的 names/scopes/paths）。
3. 清单内的名：**私有解析，绝不回落公网**，无视版本号（更高版本正是攻击信号）。
4. 清单外的名：代理公网、缓存、**记录首见来源并 pin（first-source-wins）**。

**fail-open vs fail-closed**（关键）：

| 场景 | 行为 |
|---|---|
| 私有名，私有源 UP，公网也有 | 服务私有，绝不查公网 |
| 私有名，私有源 **DOWN/errored** | **FAIL CLOSED** 硬错误（或仅从本地缓存服务）——宕机正是攻击者公网副本获胜的窗口 |
| 私有名，私有源真 404 | 返回 404，**不试公网**（那是混淆路径）；这是发布/配置错误，应暴露 |
| 公网名，公网源 down | 有缓存则服务，否则失败（fail-open-to-cache 可接受） |

**明确判死的破碎设计**：PyPI 前缀约定当信任边界；带 `--extra-index-url` 到公网；私有源 error/404 时回落公网；npm 对 unscoped 私有名 blanket upstream:true；靠版本比较"偏好"私有副本。

---

## 5. 架构路线抉择：build-native vs wrap-proven

ai-sandbox 的 PTC 走 **wrap-proven** 路线：docker-compose 编排现成缓存（registry / apt-cacher-ng / proxpi / verdaccio / goproxy / nginx），仅 git-proxy 自研 Go + 一个 Go collector 采集统计。

| | build-native（Specula 采纳） | wrap-proven（ai-sandbox PTC） |
|---|---|---|
| 交付物 | 单一 Go 二进制 | 多容器 docker-compose |
| 统一验签管线 | ✅ 验证在请求路径内，四档信任模型可实现 | ❌ opaque 第三方缓存无法插入统一验签 |
| 内嵌 WebUI/单二进制 | ✅ 天然契合 | ❌ 与"单一二进制"互斥 |
| 实现工作量 | 高（各协议 handler，但用成熟 Go 库做底座） | 低（复用现成缓存） |
| 运维复杂度 | 低（单进程） | 高（多容器编排） |

**决策：build-native**。由"单一二进制内嵌 WebUI"要求 + "统一诚实验签管线"核心卖点共同锁定。各协议用成熟 Go 库做底座（OCI 用 `go-containerregistry`、git 直接移植 ai-sandbox `gitproxy`、S3 用 aws-sdk-v2、sumdb 用 `x/mod/sumdb`）。wrap 路线作为实现参照与降级备选记录，不作主架构。

---

## 6. 复用 ai-sandbox 的实现资产

| 能力 | 来源（ai-sandbox） | 复用方式 |
|---|---|---|
| git PTC | `internal/controlplane/ptc/gitproxy/{proxy,mirror,serve,path,config}.go` | 直接移植为 git handler：host 允许清单→auth/push bypass→公共仓探测→按 mirror path keyed mutex + 陈旧窗口→`git http-backend` CGI 服务→失败回落 passthrough |
| 缓存统计 | `workspace/sql.go` `AllOrgStorageBytes`（写时记 size，`SUM GROUP BY`）+ `ptc/{collector,aggregator,series}.go`（du 兜底 + 时序） | 原生 handler 用 SUM 法；不透明缓存用 du 兜底 |
| 存储抽象 | `workspace` 6 方法 `Backend` 接口（size 读写内联返回）+ `StorageFactory`（多后端/热重载/迁移）+ S3 driver | 复用接口，补本地磁盘 driver |
| 分布式锁 | 单节点 keyed mutex+TTL；多副本 GUDC `locker`(redsync goredis) | 照搬两层 stampede |
| 用户管理 | `auth/{auth.go,password.go}`（bcrypt + HS256 JWT httpOnly cookie + token_gen 撤销）+ `auth_local.go` 首个=admin（CountUsers()==0） | 整包拷贝，裁掉 org/acl 多租户 |
| 内嵌 WebUI | `web/embed.go`（`//go:embed all:dist` + SPA fallback + 哈希资源 immutable/index no-cache）+ 根 ServeMux 最长前缀分流 | drop-in |
| UI 风格 | React18+Vite+Tailwind"工程控制台"暗色（IBM Plex Mono + 琥珀 #ffb02e + 发丝线 + 近直角） | 照抄再改品牌色 |
| 密钥引导 | `ensureSecret`（首次运行随机生成并持久化加密配置库，不可持久大声告警） | 照搬 |
| Prometheus 命名 | `ptc_cache_bytes{service}` + Grafana `sum()` 求总量 | 镜像为 `specula_cache_bytes{protocol}` |

---

## 7. 引用 (Sources)

**信任模型**：Kalu & Davis arXiv 2510.04964；Sigstore docs（cosign signing/verifying/security-model、Fulcio CT、offline bundle）；go.dev/blog/module-mirror-launch、research.swtch.com/tlog、x/mod/sumdb/note；npm provenance docs、PEP 740、PyPI PGP removal blog；apt-secure(8)、wiki.debian.org/SecureApt；TUF spec + CCS 2010 paper。
**依赖混淆**：Birsan (medium)、pip #9612/#11458、PEP 708/766、uv/Pipenv indexes、GitHub/Snyk npm substitution、Go FAQ GOPROXY、JFrog Priority Resolution。
**成熟方案**：jfrog.com/help 与 docs.jfrog.com checksum-based-storage；help.sonatype.com；zotregistry.dev、goharbor.io、Harbor #19429/#21122/#22184；docs.gomods.io、gomods/athens；devpi-server docs、verdaccio caching/packages；apt-cacher-ng manual；golang/sync singleflight；Vattani VLDB 2015；RFC 5861。
**实现参照**：ai-sandbox（github.com/ivanzzeth/sandbox）。
