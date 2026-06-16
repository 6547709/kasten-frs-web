# Kasten FRS Web 恢复向导（2026-06-16）

> 在 v0.2.x 之上扩展 helper：用户不再手工 `k10tools` 创建 FRS，
> 通过 web 单页向导选 VM → 选还原点 → 选 volume → helper 直接创建
> FRS 并跳到浏览页。FRS 过期时间在 sessions 页面有倒计时 + 颜色提示。

## 1. 背景与目标

仓库当前状态（v0.2.x）：
- helper 是个**只读**浏览器：登录 → 列已存在的 FRS → SFTP 浏览/下载
- FRS 由运营者用 `k10tools` 手工创建（k10_frs / k10_frs.pub SSH key、pvc 选
  择、RP 选 择），运维门槛高，且和 Web UI 完全脱钩
- session 超时 8h，session cookie `kfrs_sid`
- RBAC：`get,list,watch` on FRS（cluster-wide）、`get` on
  `kasten-frs-helper-private-key` secret（namespace-scoped）
- 部署：Ubi 9 镜像 + edge TLS Route + NetworkPolicy

**目标**：
- 运营者在 web 上**单页、向导式**地完成 FRS 生命周期
- SSH 密钥 helper 自管，**运营者不再手工 `ssh-keygen` + `oc create secret`**
- 过期时间在 UI 上"看得见"（倒计时 + 颜色）

**非目标**：
- 不实现 Application/Policy/RunAction 这些非 FRS 的 K10 资源操作
- 不实现集群级别 Application 列表（`applications.apps.kio.kasten.io`
  在 K10 8.5 是 developer preview 且 schema 还会变 — VM 列表用
  RestorePoint label 派生更稳定）
- 不做多 FRS 并发、helper 多用户、跨集群
- 不动 deploy/ 现有 SCC 修复（已 commit `2aed67f`）以外的清单文件
- 不动 `scripts/deploy-test.sh` 已稳定的 deploy 验证脚本逻辑

## 2. 用户决策记录（来自 brainstorm）

| 决策点 | 选择 | 备注 |
|---|---|---|
| VM/RP 数据源 | K8s CRD（dynamic client） | 与现有 FRS 列表同一条代码路径 |
| 向导 UI 形态 | 单页 master-detail（Veeam 现有风格） | 沿用 layout.html、veeam-theme.css |
| SSH 密钥生命周期 | helper 启动时 check-or-generate；保留手动注入能力 | 公钥/私钥同时存 Secret |
| FRS 清理 | 用户点"结束并删除" + K10 TTL 兜底 | sessions/browse 都加按钮 |
| 过期时间显示 | 倒计时 + 颜色（>1h 默认 / <1h 黄 / <15min 红） | 不加过滤开关 |

## 3. 架构

```
                ┌─────────────────────────────────────┐
                │  Browser (htmx)                    │
                │   /wizard    /sessions    /browse  │
                │   /wizard/create (POST)            │
                └────────────┬────────────────────────┘
                             │  cookie 鉴权 + htmx 局部刷新
                ┌────────────▼────────────────────────┐
                │  Helper (Go)                       │
                │  - handlers/wizard.go  (新)        │
                │  - handlers/sessions.go (改)       │
                │  - k8s/restorepoints.go (新)       │
                │  - k8s/frs.go (增 Create/Delete)   │
                │  - keymgr/keymgr.go  (新)          │
                │  - sftp pool: 已有                 │
                └────────────┬────────────────────────┘
                             │ dynamic client + corev1
                ┌────────────▼────────────────────────┐
                │  K8s API Server                    │
                │   restorepoints.apps.kio.kasten.io │
                │   filerecoverysessions.datamover…  │
                │   secrets (SSH key)                │
                │   pods (FRS SFTP :2222)            │
                └─────────────────────────────────────┘
```

## 4. 组件 / 模块

### 4.1 `internal/k8s/restorepoints.go`（新）

```go
// GVR for RestorePoint
var RestorePointGVR = schema.GroupVersionResource{
    Group: "apps.kio.kasten.io", Version: "v1alpha1",
    Resource: "restorepoints",
}

// VM = (appName, appNamespace) 出现在 appType=virtualMachine 的 RP 中
type VM struct {
    AppName      string
    AppNamespace string
    LastRPName   string
    LastRPTime   time.Time
    RPCount      int
}

type RestorePoint struct {
    Name      string
    Namespace string
    State     string  // "Bound" 等
    CreatedAt time.Time
    Labels    map[string]string
}

type VolumeArtifact struct {
    PVCName   string
    Size      int64
    Populated bool
}

func (c *Client) ListVMs(ctx, namespaces) ([]VM, error)         // appType=virtualMachine
func (c *Client) ListRestorePoints(ctx, ns, appName) ([]RP, error)
func (c *Client) GetRestorePointDetails(ctx, ns, name) ([]VolumeArtifact, error)
    // GET /apis/apps.kio.kasten.io/v1alpha1/namespaces/{ns}/restorepoints/{name}/details
    // 返回 .status.restorePointDetails.artifacts[]，过滤 kind:PersistentVolumeClaim
```

### 4.2 `internal/k8s/frs.go`（增）

```go
// CreateFRS 在给定 namespace 创建 FRS，spec 由 helper 拼好
func (c *Client) CreateFRS(ctx, ns string, spec FRSpec) (*FRSView, error)

// DeleteFRS 删 FRS；NotFound 不报错（幂等）
func (c *Client) DeleteFRS(ctx, ns, name string) error

// WaitForReady 轮询 status.state == "Ready" 或 "Failed"。
// timeout 由调用方传。返回最新 FRSView。
func (c *Client) WaitForReady(ctx, ns, name string, timeout time.Duration) (FRSView, error)

type FRSpec struct {
    Name             string                 // 空 → 用 generateName: "frs-wizard-"
    RestorePointName string                 // 必填
    PVCNames         []string               // 必填，可一个或多个
    SSHUserPublicKey string                 // helper 自己的公钥（authorized_keys 格式）
}
```

### 4.3 `internal/keymgr/keymgr.go`（新）

```go
type Manager struct {
    Signer    ssh.Signer
    PublicKey ssh.PublicKey
    PubKeyPEM []byte  // ssh 授权 key 格式，可直接塞 FRS spec
}

// LoadOrGenerate: 启动时调用。
// 流程见 §6。
func LoadOrGenerate(ctx, k8sClient, ns, name) (*Manager, error)
```

### 4.4 `internal/handlers/wizard.go`（新）

```go
// GET /wizard — 渲染空向导页（VM 列表由 htmx 进页面后自拉）
// 或返回 list 作为 HTML fragment 由 htmx 嵌入
func (s *Server) handleWizardPage(w, r)

// GET /wizard/vms — 拉 VM 列表（cluster-wide，过 nsWhitelist）
// 返回 HTML fragment
func (s *Server) handleWizardVMs(w, r)

// GET /wizard/vms/{ns}/{name}/restorepoints — 拉某 VM 的 RP
func (s *Server) handleWizardRPs(w, r)

// GET /wizard/rps/{ns}/{name}/details — 拉 RP 的 volume artifacts
func (s *Server) handleWizardVolumes(w, r)

// POST /wizard/create — body: vmNs, vmName, rpName, pvcNames[]
// 1) CreateFRS (FRSpec.Name 空 → generateName "frs-wizard-")
// 2) 内存里启动 wait goroutine（30s timeout，由 cfg.FRSWaitTimeout 配置）
// 3) 303 → /browse?frs={ns}/{generatedName}&path=/
// 4) browse 时如 FRS 还没 Ready，渲染"准备中"页（含 retry 链接）
func (s *Server) handleWizardCreate(w, r)

// POST /wizard/cancel — body: frs={ns}/{name}
// 调 DeleteFRS，从 map 移除 watch state，303 → /wizard
// 用户在 browse_preparing_body "取消" 按钮调用
func (s *Server) handleWizardCancel(w, r)
```

FRS 创建轮询的实现选择：**长轮询不阻塞 handler**。`handleWizardCreate`
立刻 303 跳到 `/browse?frs=...`，helper 内存里启动 watch goroutine
（`map[FRSRef]*watchState`，sync.Mutex 保护）。`/browse` 处理器
每次都从 K8s API 读最新 status，同时如果 watch 状态里 state=Ready
或 state=Failed 或 timeout，就用 watch 里的版本（避免再轮询 K8s）。

```
- handleWizardCreate: CreateFRS → 启 goroutine poll state →
  写 map[FRSRef]*watchState → 303 to /browse
- handleBrowse: 读 map[ref]，如果 hit 且 terminal（Ready/Failed/timeout）
  → 直接用；miss 或仍在 Starting/Processing → 走 K8s API 拉最新
- map pod 重启后丢失：browse handler miss → 走 K8s API（fail-safe）
```

> 替代方案：handler 内同步 `WaitForReady` 30s。否决 — 会占住 Route
> 30s 容易被 HAProxy 超时（30s 是 edge 路由默认 request timeout）。
> 替代方案：SSE。否决 — 增加复杂度，htmx-sse 集成尚不熟。

### 4.5 `web/templates/sessions.html`（改）

现有 `web/templates/sessions.html` 的 Expires 列改为 "剩余"，
由客户端 JS（htmx + 内嵌 `<script>`）每 1s 重算颜色。颜色用现有
`veeam-theme.css` 的 `.badge-warn` / `.badge-crit` 类。每行多一个
"结束"按钮列，form POST 到 `POST /sessions/{ns}/{name}/delete`。

### 4.6 `web/templates/browse.html`（改）

- 顶部多一个"结束并删除 FRS" 红色按钮（form POST 到
  `POST /sessions/{ns}/{name}/delete`）
- 新增 `browse_preparing_body` 定义（在同一文件里）：
  FRS 还没 Ready 时显示，含 htmx 自动刷新（每 2s
  `hx-get="/browse?frs=...&partial=ready"`，由
  `<div hx-get hx-trigger="every 2s">` 触发）
- `handleBrowse` 路由增 query 参数 `partial=ready`：
  当 FRS state=Ready 时返回正常片段（htmx 替换原 preparing 页）

### 4.7 模板

- `web/templates/wizard.html`（新）— 3 面板 + 顶部进度条 +
  FRS 名/创建按钮
- `web/templates/sessions.html`（改）— Expires 列改为 countdown cell
  + 每行多一个"结束"按钮列
- `web/templates/browse.html`（改）— 顶部加删除按钮；新增
  `browse_preparing_body` 定义 + `partial=ready` 渲染分支

## 5. 数据流（happy path）

```
1. 用户 GET /wizard
   → handler 拉 VM 列表（一次 cluster-wide list of RP with appType=virtualMachine）
   → 渲染 wizard 页面（3 面板 + VM 列表 + 搜索框）

2. 用户输入搜索词（client-side filter，无网络）
3. 用户点 VM
   → htmx GET /wizard/vms/{ns}/{name}/restorepoints
   → handler 拉该 (ns, appName) 所有 Bound RP，按 createdAt desc
   → 返回 fragment，htmx 注入中间面板

4. 用户点 RP
   → htmx GET /wizard/rps/{ns}/{name}/details
   → handler 拉 details 子资源，过滤 PersistentVolumeClaim 类型
   → 返回 fragment 注入右侧面板（checkbox list）

5. 用户勾选 volume（默认全选）、点 "创建 FRS"
   → POST /wizard/create  body: vmNs, vmName, rpName, pvcNames[]
   → handler:
      a) 调 CreateFRS（FRSpec.Name 为空 → 用 generateName "frs-wizard-"）
      b) 内存中启动 watch goroutine，poll status.state
         - state=Ready → 写 map[FRSRef]{state: Ready, view: latest}
         - state=Failed → 写 map[FRSRef]{state: Failed, view: latest}
         - 30s timeout → 写 map[FRSRef]{state: Timeout, view: latest}
         - map 用 sync.Mutex 保护
      c) 303 → /browse?frs={ns}/{name}&path=/

6. GET /browse?frs=...
   → 拿 view.State
   → state=Ready → 走现有 browse 流程
   → state∈{Starting,Processing} → 渲染 browse_preparing_body
   → state=Failed / timeout → 渲染错误页 + "重试" 链接
     （重试 = POST /wizard/create 用相同 vm/rp/pvc 参数 + 生成新 FRS 名字）
     + "取消" 链接（POST /wizard/cancel 删 FRS，303 → /wizard）
```

## 6. SSH 密钥管理细节

启动时 `keymgr.LoadOrGenerate` 的判定树：

```
read Secret kasten-frs-helper-private-key in kasten-io
├─ 不存在
│   └─ 生成 ed25519 keypair
│      ├─ 写 Secret { ssh-privatekey: PEM, ssh-publickey: authorized_keys }
│      └─ type: kubernetes.io/ssh-auth（保留兼容 deploy/10-secret.example）
├─ 存在，ssh-privatekey 有值
│   ├─ ssh-publickey 有值
│   │   └─ 直接 parse
│   └─ ssh-publickey 缺失
│       └─ parse 私钥 → 派生 pubkey → 写回 Secret 加 pubkey 字段
└─ 存在但 ssh-privatekey 缺失
    └─ 报错退出（运营者手动写坏了）
```

`CreateFRS` 时 `FRSpec.SSHUserPublicKey = string(manager.PubKeyPEM)`，
spec.transports.sftp.userPublicKey 直接塞进去。

**关键约束**：
- 启动时**不要**自动 rotate 已有密钥（避免 FRS 已有但 key 轮了 → SFTP
  拒绝）
- `kasten-frs-helper-private-key` secret 名称不变 — 升级路径不破坏
  现有 deploy
- 派生 pubkey 用 `ssh.NewPublicKey(priv.PublicKey)` 然后
  `ssh.MarshalAuthorizedKey()`

## 7. FRS 过期时间显示

`sessions.html` 每行 Expires cell：

```html
<td>
  <span class="badge" data-expiry="{{.ExpiryTime.Format "2006-01-02T15:04:05Z07:00"}}">
    <span class="exp-text">…</span>
  </span>
</td>
```

内嵌 `<script>`（在 `sessions.html` 末尾或 layout 的 workarea 注入）：

```js
setInterval(() => {
  document.querySelectorAll('[data-expiry]').forEach(el => {
    const exp = new Date(el.dataset.expiry).getTime();
    const ms = exp - Date.now();
    if (ms < 0) { el.className = 'badge crit'; el.firstChild.textContent = '已过期'; return; }
    if (ms < 15*60*1000) el.className = 'badge crit';
    else if (ms < 60*60*1000) el.className = 'badge warn';
    else el.className = 'badge';
    el.firstChild.textContent = '剩 ' + formatDuration(ms);
  });
}, 1000);
```

`formatDuration`：>24h 显示 "剩 Xd Yh"；否则 "剩 Xh Ym"。

`veeam-theme.css` 已有 `.badge-warn` / `.badge-crit` 类（如无则
补两行）。**不**做排序/过滤。

## 8. RBAC 变更（`deploy/06-rbac.yaml`）

```yaml
- apiGroups: ["datamover.kio.kasten.io"]
  resources: ["filerecoverysessions"]
  verbs: ["get", "list", "watch", "create", "delete"]   # +create,delete
- apiGroups: ["apps.kio.kasten.io"]
  resources: ["restorepoints"]
  verbs: ["get", "list"]
# subresource 必须独立成行
- apiGroups: ["apps.kio.kasten.io"]
  resources: ["restorepoints/details"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["kasten-frs-helper-private-key"]
  verbs: ["get", "create", "update", "patch"]            # +create,update,patch
```

> 决策点 (a) 客户端 filter 够不够用：10000+ VM 才需要 server-side
> 搜索。现状集群 <100 个 VM，client-side OK。如果未来扩到那个量级，
> 加 `/wizard/vms?q=...` server 端 filter（labelSelector
> `k10.kasten.io/appName~<q>`）。

## 9. 错误处理

| 场景 | UI 行为 |
|---|---|
| VM 列表为空 | "暂无可恢复的 VM（确认 K10 有 `appType=virtualMachine` 的 RestorePoint）" + 链接 Kasten |
| RP `/details` 拉取失败 | 右面板显示 "无法读取该 RP 的 volume 列表"，不禁用 wizard 其它部分 |
| CreateFRS 失败 | 错误页"创建 FRS 失败：<K8s 错误>"，返回 `/wizard` |
| WaitForReady 超时 30s | `browse_preparing_body` 显示 "FRS 还在准备中，已等待 30 秒" + "再等 30 秒" / "取消" 链接（"取消" = 调 `DELETE /wizard/cancel?frs=...` 调 `DeleteFRS`） |
| DeleteFRS 失败 | sessions 页 inline 错误（不变 FRS 状态），用户可重试 |

错误页全部走 `renderError` helper（已有），保持 Veeam 风格。

## 10. 测试策略

### 10.1 单元测试

- `internal/k8s/restorepoints_test.go`：`fake dynamic client` 测
  `ListVMs` 去重 + 排序，`GetRestorePointDetails` 解析 artifacts
- `internal/k8s/frs_test.go`（增）：`CreateFRS` 构造 spec 正确性
  （userPublicKey、generateName），`WaitForReady` 状态机
- `internal/keymgr/keymgr_test.go`：4 种 secret 状态（不存在 / 只私
  钥 / 全有 / 只公钥）的判定树
- `internal/handlers/wizard_test.go`：htmx fragment 渲染
  （VM/RP/Volume 列表 HTML 结构）

### 10.2 端到端

`scripts/deploy-test.sh` 不动。在新文件 `scripts/wizard-test.sh`
（不入库，仓库根 .gitignore）做：

1. preflight：同 deploy-test.sh
2. 启动 helper（用 fake K8s client + fake SFTP server，避免依赖真集
   群，参考 `internal/sftpclient/testserver.go`）
3. curl /wizard 验证 200 + 表格行
4. curl POST /wizard/create 用 mock VM/RP → 验证 FRS 写入 fake
   clientset
5. 验证 created FRS 的 spec.transports.sftp.userPublicKey ==
   helper 启动生成的 pubkey

### 10.3 手工 e2e

- 真集群：用 `scripts/deploy-test.sh` 拉起 helper
- 浏览器走完整路径：login → wizard → 选 VM → 选 RP → 选 volume → 创
  建 → browse → 下载小文件 → 结束
- 验证 sessions 页倒计时变化
- 故意把 K10 helm `frs.sessionExpiryTimeInMinutes` 调到 5 分钟，验
  证 K10 TTL 兜底

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 客户端 text filter 性能不够（VM 数量级到 10000+） | 当前 <100，留 `data-vm-name` attribute，将来加 server-side filter 走 labelSelector `appName~=q` |
| Watch goroutine pod 重启后丢失 | browse 处理器每次拉 view.State；不在内存中保存跨重启状态 |
| WaitForReady 30s 不够（kasten-io 资源紧张时 Starting→Ready 慢） | browse_preparing_body 提供"再等 30 秒"按钮，相当于手动续期 |
| Secret ssh-publickey 派生错（K8s 把私钥改了） | parse 失败立即 panic — 启动时 fail-fast |
| helper 自动生成 key 但 K10 已有用同密钥的 FRS | 不轮换；只在新部署场景才会写 secret |
| RestorePoint `/details` 子资源 RBAC 缺 | RBAC 单独写 `subresource` 行为需测试；如果 K8s 拒绝则改用 full get 后取 annotations |
| 多人共用 helper 时互相删 FRS | 单用户模型 — 文案明确；如果未来要 multi-user，引入"我的 FRS"标签（在 FRS spec.template.metadata.labels 加 `kasten-frs-web-helper/user=<cookie-hash>`，DeleteFRS 校验 owner） |
| htmx 1s 轮询导致 sessions 页发请求多 | 用户离开 /sessions 页时停 interval（visibilitychange 事件）；300 个 FRS × 1qps = 300 RPS，仍可接受 |

## 12. 不在范围内（后续工作）

- 集群级别 Application 列表 / 跨集群
- FRS 创建时自定义 TTL（K10 限制，全局 frs.sessionExpiryTimeInMinutes）
- FRS 创建时选多个 RP / 多 VM 单 FRS
- K10 Policy/Action 触发
- 离线 / 一次性"导出 FRS 全部内容到本地 zip" — 现有 /download-zip 已
  覆盖

## 13. 实施里程碑

1. **M1 RBAC + 验证**：更新 `deploy/06-rbac.yaml`，`scripts/deploy-test.sh` 加
   netpol 验证 helper 能 `create` FRS（用 `kubectl auth can-i`）
2. **M2 keymgr + 单元测试**：实现 `LoadOrGenerate`，覆盖 4 种 secret
   状态；改 `cmd/helper/main.go` 走 keymgr 而不是直接 LoadPrivateKey
3. **M3 k8s/restorepoints.go + k8s/frs.go 增 Create/Delete/Wait**：
   单元测试
4. **M4 wizard handlers + 模板 + htmx 局部刷新**：手工 e2e
5. **M5 sessions/browse 删除按钮 + 倒计时 + 颜色**：JS + 模板
6. **M6 deploy-test.sh 加 wizard 验证 step**：保持端到端可重放

## 14. 文档同步

- `README.md`：新增 "Wizard" 段落
- `DEPLOY.md`：删"先生成 SSH 密钥"步骤（现在 helper 自动管），更新
  post-flight 步骤加 "wizard 端到端冒烟"
- `CHANGELOG.md`：v0.3.0 段列出新增功能

### 14.1 部署 / 测试经验沉淀（新文件）

新建 `docs/superpowers/experience/2026-06-16-deploy-experience.md`
（**入库**），沉淀以下经验：

- OCP `restricted-v2` SCC + `MustRunAsRange` + 命名空间 UID range
  冲突：详见 commit `2aed67f` 描述
- K10 的 `default-deny` NetworkPolicy（selector={}）如何用 overlay
  注入旁路规则（参考 `scripts/deploy-test.sh` 现存 netpol patch）
- FRS SFTP pod 自动 NetworkPolicy（`frs-XXXXX`）只允许源 namespace
  = `default`；helper 在 `kasten-io` 也要通过 `frs-allow` 显式放行
- GHCR 匿名拉取要走 OCI Accept 头 + token 交换
- FRS 状态机：`Starting → Processing → Ready / Failed`；`Ready` 之
  前 host key / port 不可读
- `oc exec` 在 helper 容器内 `nslookup` / `curl` / `bash /dev/tcp`
  是 e2e 网络验证三件套
- Route edge TLS：默认 300s 响应超时对大文件不友好

## 15. 实施检查清单（spec 落地时的最后扫描项）

- [ ] §6 SSH key 兼容老 secret（只读 `ssh-privatekey` 派生 pubkey
      不重写）— 单元测试覆盖
- [ ] §8 RBAC `restorepoints/details` subresource 单独一行
- [ ] §4.4 watch goroutine map + handler fail-safe 行为
- [ ] §7 倒计时 JS 在 `/sessions` 离开页面时停 interval
- [ ] §9 wait timeout 后用户能"再等 30 秒"（不直接 fail）
- [ ] §14.1 部署经验文档就位（与本 spec 同批提交）
