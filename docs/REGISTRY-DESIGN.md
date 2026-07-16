# Specula — 可写多租户 Registry + 多租户模型 + HA 设计增补

> 本文档是对 [ARCHITECTURE.md](./ARCHITECTURE.md) 的增补，覆盖：把 Specula 从只读拉取穿透缓存
> 扩展为**同时可推送托管的多租户私有 registry**、多组织多成员权限模型、高可用、以及符合用户心智
> 的 registry-UX WebUI。实现资产大量复用 ai-sandbox（见各节 verdict）。

---

## 0. 需求与决策

| 需求 | 决策 |
|---|---|
| push 上传托管私有镜像（不再搞两套服务） | OCI push 原生实现，写入**现有 CAS**（已按 digest 内容寻址，天然契合） |
| 可见性 | 每 repo `private`（仅 org 成员）/ `public`（匿名可 pull），默认 **private** |
| push 认证 | **用户自建 API key** → Docker token 流（`docker login` 邮箱 + apikey）|
| 权限架构 | **多组织多成员 RBAC**，port ai-sandbox（orgs/members/roles/apikeys/ACL/grants）|
| HA | 多副本 + 共享 MinIO(CAS)+PG + 分布式锁；**本地 minikube 实测** |
| WebUI | **符合用户心智的 registry UX**：组织→仓库→标签、push/pull 命令、可见性、成员、token |

**架构总纲：hosted-first 混合库**（学 Artifactory virtual repo：local→remote-cache→remote）
- **push** → 创建 org 拥有的 **hosted** repo（存 CAS，标 hosted，**不被 GC 驱逐**）
- **pull** → 先查 **hosted**（鉴权可见性）→ 未命中回落配置的**上游拉取缓存**（现有能力）
- 副作用红利：hosted 本地 shadow 上游，天然再防依赖混淆一层

---

## 1. 可写 Registry（OCI Distribution 写路径）

现有 OCI handler 仅 GET/HEAD。新增写端点（对现有 `cache`/`BlobStore` CAS 写）：

| 端点 | 作用 |
|---|---|
| `POST /v2/<name>/blobs/uploads/` | 开启上传会话 → `202` + `Location: .../uploads/<uuid>` |
| `PATCH .../blobs/uploads/<uuid>` | 分块上传（chunked，`Content-Range`）|
| `PUT .../blobs/uploads/<uuid>?digest=sha256:…` | 完成上传（单体或收尾），校验 digest 后落 CAS |
| `HEAD /v2/<name>/blobs/<digest>` | 存在性（已有）|
| `PUT /v2/<name>/manifests/<ref>` | 推送 manifest（tag 或 digest）→ 写 manifest blob + tag→digest 映射 |
| `DELETE /v2/<name>/manifests|blobs/...` | 删除（org admin）|

**上传会话状态**：uuid → 临时分块区（隔离区复用 `quarantine`），`PUT` 完成时校验声明 digest → 原子提升进 CAS。**blob 天然按 digest 去重**：push 与 pull-cache 共享同一 CAS，同 digest 只存一份。

**hosted vs cached 生命周期**：MetadataStore 的 CacheEntry 增 `origin`(hosted|cached) + `org_id` + `repo_visibility`。GC/eviction **只驱逐 cached**，hosted 永不驱逐（是权威数据不是缓存）。stats 分别计 hosted/cached 容量。

**新包**：`internal/handler/registry/`（写路径 + 会话）；hosted 元数据 `internal/registry/`（repos、tags、visibility、ownership）。pull 路径改造：先查 hosted 再回落 oci pull-cache。

---

## 2. 多租户模型（port ai-sandbox，copy-as-is）

`internal/org/`、`internal/apikey/`、`internal/acl/`、`internal/grant/` 直接移植（ai-sandbox 对应包干净、零/少依赖）。

### 2.1 Schema（goose 迁移，扩现有 sqlite/postgres）
- `orgs(id,name,slug,status,created_by,created_at)`
- `users`（已存在，扩 `token_gen` 已有）
- `org_members(id,org_id,email,role,invited_by,created_at)` — 唯一索引 `(org_id,email)`
- `org_invitations(...)` — 邀请后接受才写 member
- `resource_grants(resource_type,resource_id,subject_type,subject_id,access,...)` — 跨 org/成员共享
- `api_keys(key_hash PK,id,label,prefix,org_id,user_id,expires_at,revoked,...)`
- **新增 `repos(id,org_id,name,visibility,owner_user_id,created_at)`** + tags 映射（或复用 MetadataStore mutable 层）

### 2.2 角色阶梯
org role：`viewer<editor<admin<owner`（owner 独有：计费/转移/删 org，末位 owner 不可移除）。
system role：`''|viewer|editor|admin`（跨 org 隐式只读 viewer）。
**首个注册用户 = system admin + 默认 org(`org_default`) owner**（已有 CountUsers()==0，扩为同时建默认 org + owner member）。

### 2.3 API Key（copy-as-is）
- 格式 `spck_`+base64url(18B)；**仅存 SHA-256**；明文仅创建时返回一次。
- 绑定 `org_id`(+可选 `user_id`)；`LookupSubject(token)→(orgID, subject="apikey:<id>", ok)`。
- 用户自建：`POST /api/v1/keys`（needOrgRole editor），`GET/DELETE`。WebUI Settings 页创建。
- **Registry 用途扩展**：key 作为 `docker login` 的密码；其 org 决定可访问的 hosted repo。
- **per-key scope（build-fresh）**：ai-sandbox 无 scope（key=org-admin-in-org）。Registry 需 `pull`/`push`
  粒度 → apikey 增可选 `scopes` 列（默认 push+pull within org）；MVP 可先用"org 成员即可 push"。

### 2.4 ACL（copy-as-is，~160 行 acl.go）
`Visibility: private|org(read|write)|public`；`Subject{UserID,OrgID,Admin}`；`Resource{OwnerUserID,OrgID,Visibility,Access}`。
`CanAccess` fail-closed 到 private/read。匿名 `Subject{}` 仅能读 public。**registry 逐 repo 判权即调此**。

---

## 3. Registry Token 认证（build-fresh 胶水，复用两块原语）

ai-sandbox 无 Docker token 流，自建。标准 v2 Bearer flow：

```
docker login specula.local -u <email> -p <apikey>
docker push specula.local/myorg/app:v1
  │
  ├─ GET /v2/  → 401  WWW-Authenticate: Bearer realm="https://specula/token",service="specula"
  ├─ GET /token?service=specula&scope=repository:myorg/app:push,pull
  │     Authorization: Basic base64(email:apikey)
  │        ├─ 认证：apikey.LookupSubject(apikey) → (orgID, subject)   ← 复用 §2.3
  │        │        （或 email+password 亦可，走 auth.Verify）
  │        ├─ 授权：对每个请求 scope repository:<repo>:<action>
  │        │        acl.CanAccess(repoResource(repo), subject, needWrite)  ← 复用 §2.4
  │        │        repo namespace ↔ org 绑定（myorg/app 的 owner org = myorg）
  │        └─ 签发 registry JWT（RS256，access claims: [{type:repository,name:myorg/app,actions:[pull,push]}]，短 TTL）
  └─ 带 Bearer <jwt> 重试 push；每个 /v2/ 写端点校验 JWT 的 access claims
```

**要点**：
- token JWT 用 RS256（registry 标准；与会话 HS256 JWT 分开，独立密钥对，`ensureSecret` 生成持久化）。
- `/v2/` 中间件：解析 Bearer JWT → 校验 access claims 覆盖请求的 repo+action；public repo 的 pull 允许匿名（无 token → 仅 public）。
- 匿名 pull public repo：challenge 时给匿名 token 或直接放行 pull。
- `docker login` 兼容：`/token` 接受 Basic（email:apikey 或 email:password）。
- 参照 ai-sandbox `notebook/share.go` 的 HMAC 签发风格（换成 registry JWT 格式）。

---

## 4. 高可用（HA）+ 本地 minikube 实测

### 4.1 HA 设计
- **无状态副本**：所有持久态在共享 MinIO(CAS blob)+PostgreSQL(元数据/users/orgs/repos)。
- **分布式锁**（落地 M3 / 之前的 TODO）：stampede 去重 + push 会话/tag 写串行化 →
  PG advisory lock（已有 helper）或 redis(redsync，复用 ai-sandbox GUDC locker)。owner-checked/TTL 释放。
- **并发 push 一致性**：blob 按 digest 幂等；manifest/tag 写用分布式锁 + MetadataStore upsert。
- **滚动升级**：新副本就绪再排空旧副本，零中断。

### 4.2 交付物
- `Dockerfile`（多阶段：vite build → CGO_ENABLED=0 go build → distroless/静态）
- `deploy/k8s/`：`deployment.yaml`(replicas≥3)、`service.yaml`、`configmap.yaml`、`secret.yaml`、
  `minio.yaml`、`postgres.yaml`（或用 CloudNativePG）、`ingress.yaml`
- `deploy/helm/`：Helm chart（可配 replicas/存储/上游/信任策略）
- `deploy/k8s/daemonset.yaml`：节点本地缓存模式（hostNetwork + 本地盘 + SQLite）

### 4.3 minikube HA 验收剧本（**本地真实执行，我独立核验**）
1. `minikube start`；构建镜像 `minikube image build` 或 `eval $(minikube docker-env)`。
2. 部署 MinIO + PostgreSQL + Specula(replicas=3) + Service。
3. **`docker login` → `docker push` 私有镜像 → `docker pull`** 端到端（经 minikube service/ingress）。
4. **HA 断言**：`kubectl delete pod <一个副本>` 后 pull/push 不中断（LB 打到存活副本，共享态一致）。
5. **并发一致性**：并发 push 同/异 tag，断言无损坏、digest 幂等、tag 最终一致。
6. **可见性断言**：private repo 未授权 pull → 401/404；public repo 匿名 pull → 成功。
7. **滚动升级**：改 image tag `kubectl rollout` → 期间持续 pull 不断。
8. `minikube stop`。全过程日志与 kubectl 输出留证。

---

## 5. WebUI（符合用户心智 —— 覆盖全部 8 协议，不止镜像）

信息架构对齐用户既有心智（Docker Hub / Harbor / GHCR / 各语言 registry），沿用工程控制台主题
（IBM Plex Mono + 琥珀 + 暗色）。**两条主线 + 运维分区**：

### 5.1 主线 A —— Registry（OCI，可写托管）
- **组织切换器**（顶栏，X-Org-Id）+ 组织列表/创建
- **Repositories**（当前 org 仓库）：名称、可见性徽章(private/public)、大小、tag 数、最近推送
  - **Repository 详情**：Tags 列表（tag、digest、大小、架构、推送时间）、`docker pull` 一键复制、
    可见性开关(owner/admin)、删除 tag
- **Push 引导**：`docker login … && docker tag && docker push` 分步命令（带 org 前缀）

### 5.2 主线 B —— Cache Browser（全部 8 协议的缓存可浏览 / "看得见"）
**每个协议一个缓存浏览视图**（不只聚合数字，能看到具体缓存了什么）：

| 协议 | 浏览维度（列表 + 详情）|
|---|---|
| OCI | 已缓存 image/tag、digest、层大小、来源上游、信任档 |
| PyPI | 已缓存 package / version / wheel-sdist 文件、sha256、大小、tier(tofu) |
| npm | 已缓存 package(scope)/version/tarball、integrity、大小、tier |
| Go | 已缓存 module@version(.info/.mod/.zip)、sumdb 验证状态、tier(signed) |
| apt | 已缓存 suite/component/Packages/pool deb、GPG 链状态、tier(signed) |
| Helm | 已缓存 chart/version、.prov 状态、tier |
| git | 已镜像仓库、refs、last-seen SHA、force-push 告警、大小 |
| tarball | 已缓存 URL→digest、大小、tier |

- **通用列**：名称/版本、大小、拉取次数/命中、首次/最近拉取时间、**达到的信任档徽章**(signed/consensus/tofu/checksum)、来源上游。
- **操作**：查看详情、手动清除某条缓存(admin)、pin/protect(不被 GC 驱逐)。
- **过滤/搜索**：按协议、按大小、按 tier、按上游、按时间。
- **数据来源**：MetadataStore（已按 protocol 记 size/tier/upstream/时间）——新增分页 List 查询即可，无需遍历 FS。

### 5.3 管理 / 运维分区
- **Members**（org admin）：成员、角色、邀请
- **Access Tokens**（用户自建 apikey）：创建(明文仅一次+复制)、列表、吊销、**附 docker login/pip/npm/go 用法**
- **Cache 仪表盘**：per-protocol + total 容量、命中率、上游健康(auto-block)、验签告警趋势
- **Upstreams / 镜像源**（每协议一张列表 —— 这是核心运维视图）：
  每个协议展示其**有序的上游镜像列表**（fallback 链），逐条给出：
  | 列 | 说明 |
  |---|---|
  | 顺序 | fallback 优先级（第 1 个先试）|
  | URL | 镜像地址（如 oci: daocloud→docker.io；pypi: tuna→pypi.org；go: goproxy.cn→proxy.golang.org；apt: aliyun→archive.ubuntu；npm: npmmirror→npmjs；helm/git/tarball 同理）|
  | 健康 | up / **blocked**(auto-block 触发) / 探测中 |
  | 最近延迟 | 上次上游请求耗时 |
  | 命中占比 | 该镜像服务了多少 miss 回源 |
  | 最近served | 上次成功由哪个镜像回源 |
  - **操作**（admin，运行时可变配置，YAML 仍是声明式基线）：启用/禁用某镜像、拖拽**调整 fallback 顺序**、手动 unblock、手动探测。
  - 数据来源：upstream 客户端已有的 auto-block 状态 + 延迟/命中埋点；config 的 per-protocol upstreams 列表。
  - 顶部按协议 tab 或分组：8 个协议各自一段镜像列表，一眼看清"每个协议在从哪些源拉、哪个挂了"。
- **Settings**：org 资料、默认可见性、GC/eviction 策略

**心智要点**：
1. 用户想"推/拉私有镜像" → 主线 A 即拿即用（org→repo→tag + 命令）。
2. 用户想"这代理到底缓存了些什么、验没验" → 主线 B 每个协议都能翻查到**具体条目 + 信任档**，而不是只有一个总字节数。
3. 用户想"配 token/管成员/看健康" → 运维分区。
每个协议的缓存都"看得见"（visibility into the cache），降低"黑盒代理"的不信任感。

---

## 6. 分阶段构建计划

| 阶段 | 范围 | 验收 |
|---|---|---|
| **R1 多租户内核** | port org/apikey/acl/grant + 迁移 + 首个用户建默认 org owner + 现有 Admin API 接入 org 上下文 | 单测 + orgs/members/keys CRUD e2e |
| **R2 可写 registry + token 认证** | OCI push 端点 + 上传会话 + hosted 元数据 + hosted-first pull + `/token` + `/v2/` challenge + acl 逐 repo 判权 | hermetic e2e：docker/oras push→pull、private 拒绝、public 匿名 |
| **R3 WebUI（全协议）** | 主线A registry(org/repo/tag/可见性/push命令) + 主线B **8协议缓存浏览器** + 成员/token + 运维分区；后端加 MetadataStore 分页 `ListEntries(protocol,filter,page)` + Admin API `/admin/cache/{protocol}` | 构建 + 交互冒烟 + 各协议列表可翻查 |
| **R4 HA + minikube 实测** | 分布式锁 + Dockerfile + k8s/Helm + **minikube 真实跑 §4.3 剧本** | 杀副本不中断 + 并发一致 + 滚动升级 |

信任模型四档、8 协议、拉取穿透缓存均已具备（见 PRD/ARCHITECTURE）；本增补聚焦"可写托管 + 多租户 + HA"。

---

## 7. 复用清单（ai-sandbox）

| 需求 | 来源 | verdict |
|---|---|---|
| orgs+members+roles+invitations | `internal/controlplane/org/*` + org_auth 角色助手 + baseline 迁移 | copy-as-is（改名）|
| 用户自建 API key | `internal/controlplane/apikey/*` + `api/keys.go` + LookupSubject | copy-as-is；**per-key scope 需自建** |
| ACL（private/org/public+owner+跨org grant） | `internal/controlplane/acl/acl.go` + `grant/grant.go` + `api/authz.go` | copy-as-is |
| 匿名签名访问 | `notebook/share.go` HMAC signer | adapt（→ registry JWT）|
| Docker v2 token 认证 | 无（仅 stock registry:2 + htpasswd） | **build-fresh**（胶水，复用上面两块原语）|
| 分布式锁 HA | GUDC `locker`(redsync) / PG advisory | copy-as-is |
| WebUI 组件/主题 | `web/src/ui/*` + tailwind 工程控制台主题 | 沿用 |
