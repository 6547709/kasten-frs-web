# Kasten FRS Web Helper 全面代码分析报告

本报告基于对项目所有源代码、部署配置、前端模板、以及 Kasten 官方 API 文档的深入分析。

---

## 一、代码问题分析

### 🔴 高优先级问题

#### 1. XSS 注入风险 — `app.js:169-171`

[app.js](file:///Users/liguoqiang/project/kasten-frs-web/web/static/app.js#L168-L172) 中 `enableSubmitIfVolumesPresent()` 使用 `innerHTML` 直接拼接 checkbox 的 `value` 属性：

```javascript
pvcFields.innerHTML = checked.map(function (v) {
  return '<input type="hidden" name="pvcNames" value="' + v.value + '">';
}).join('');
```

> [!CAUTION]
> `v.value` 来自 PVC 名称（K8s API 返回），虽然 K8s 对资源名称有格式限制（`[a-z0-9.-]`），但如果 SFTP 服务端被控制或 K8s API 返回被篡改，恶意的 PVC 名称（如 `" onload="alert(1)"`）可能导致 XSS。

**修复建议**：使用 `document.createElement` 代替 `innerHTML`：
```javascript
pvcFields.innerHTML = '';
checked.forEach(function(v) {
  var inp = document.createElement('input');
  inp.type = 'hidden'; inp.name = 'pvcNames'; inp.value = v.value;
  pvcFields.appendChild(inp);
});
```

#### 2. Session Cookie 仅靠客户端 Max-Age 控制过期

[session.go](file:///Users/liguoqiang/project/kasten-frs-web/internal/auth/session.go#L46-L67) 的 `Verify()` 方法**只校验 HMAC 签名，不检查服务端过期时间**：

```go
// Expiry is enforced by the client's Max-Age cookie attribute.
return true
```

> [!WARNING]
> 客户端可以修改 Cookie 的 Max-Age 来延长会话有效期。攻击者获取一个有效的 Session Cookie 后可以无限期使用。

**修复建议**：在 Cookie 值中嵌入签发时间戳，`Verify()` 时校验 `issue_time + TTL > now`。

#### 3. LookupFRSSource 低效的 List-then-filter 模式

[frs.go:154](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/frs.go#L154) 中 `LookupFRSSource` 对**每个** FRS 都执行一次 `List` 全量查询：

```go
items, err := c.dyn.Resource(FRSGroupVersionResource).Namespace(v.Ref.Namespace).List(ctx, metav1.ListOptions{})
```

> [!WARNING]
> 当 sessions 列表有 N 个 FRS 时，`enrichFRSContext` 会发起 N 次 List 请求 + N 次 RestorePoint Get 请求。大规模集群下会显著影响页面加载速度并增加 API Server 负载。

**修复建议**：使用 `Get(name)` 替代 `List` + 遍历查找，或者在 `enrichFRSContext` 中预批量查询。

#### 4. `ListActiveFRS` 集群级无过滤 List

[frs.go:53](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/frs.go#L53) 使用 `Namespace("")` 做集群级 List：

```go
u, err := c.dyn.Resource(FRSGroupVersionResource).Namespace("").List(ctx, metav1.ListOptions{})
```

> [!IMPORTANT]
> 没有 LabelSelector，会返回集群中**所有命名空间**的所有 FRS 资源。在大规模集群中（100+ 命名空间、每个有多个 FRS），这会导致大量内存消耗和 API Server 压力。

**修复建议**：如果 `nsWhitelist` 不为空，按命名空间逐个查询或添加 LabelSelector 过滤。

#### 5. `buildFRSView` 在服务信息缺失时返回 false

[frs.go:117-119](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/frs.go#L117-L119)：

```go
if svc == "" || svcNS == "" || port == 0 {
    return v, false
}
```

> [!WARNING]
> 正在创建中但尚未就绪的 FRS（Pending 状态）没有 `status.transports.sftp` 字段，`buildFRSView` 会返回 `false`，导致 `ListActiveFRS` **静默跳过**这些正在创建中的 FRS。Sessions 列表上看不到正在创建中的会话，用户体验不佳。

**修复建议**：分离"可构建视图"和"可连接"的判断。Pending 状态的 FRS 也应该显示在列表中，只是 Browse 按钮禁用。

---

### 🟡 中优先级问题

#### 6. `doK8sRequest` 仅检查 `BearerToken` 字段

[client.go:122-124](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/client.go#L122-L124)：

```go
if c.cfg == nil || c.cfg.BearerToken == "" {
    return nil, fmt.Errorf("doK8sRequest: no in-cluster bearer token")
}
```

> [!IMPORTANT]
> Kubernetes 1.21+ 默认使用 **BearerTokenFile**（`/var/run/secrets/kubernetes.io/serviceaccount/token`），而非 `BearerToken` 字符串字段。`rest.InClusterConfig()` 设置的是 `BearerTokenFile`，`BearerToken` 可能为空。

**修复建议**：使用 `rest.TransportFor(cfg)` 构建 `http.Client`，让 client-go 自动处理 token 刷新，而非手动拼接 Authorization 头。

#### 7. `ListVMs` 和 `ListVMNamespaces` 重复全量 List

[restorepoints.go:54](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/restorepoints.go#L54) 和 [restorepoints.go:107](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/restorepoints.go#L107)：两个函数都执行 `Namespace("").List` 查询**所有** RestorePoints，然后在客户端做过滤。

**修复建议**：使用 LabelSelector `k10.kasten.io/appType=virtualMachine` 在 API Server 侧过滤，减少网络传输。

#### 8. SFTP `validatePath` 路径遍历检查不完整

[client.go:229-233](file:///Users/liguoqiang/project/kasten-frs-web/internal/sftpclient/client.go#L229-L233)：

```go
func validatePath(p string) error {
    if strings.Contains(p, "..") {
        return errors.New("invalid path")
    }
    return nil
}
```

> [!WARNING]
> `strings.Contains(p, "..")` 会误拦合法路径（如 `file..name`），但同时也可能遗漏编码绕过（如 URL 编码的 `%2e%2e`）。

**修复建议**：使用 `path.Clean(p)` 后检查是否以 `/` 开头，确保规范化路径不逃出根目录。

#### 9. watchMap 无大小限制和过期清理

[wizard.go:28-50](file:///Users/liguoqiang/project/kasten-frs-web/internal/handlers/wizard.go#L28-L50)：`watchMap` 只有 `set`/`get`/`del`，没有自动过期和大小限制。

**影响**：如果用户通过 wizard 大量创建 FRS 但从不删除 watch 条目，map 会无限增长导致内存泄漏。

**修复建议**：给 `watchState` 添加创建时间戳，定期 Sweep 超过一定时间（如 1 小时）的条目。

#### 10. Recoverer 使用 `fmt.Printf` 而非结构化日志

[middleware.go:31](file:///Users/liguoqiang/project/kasten-frs-web/internal/server/middleware.go#L31)：

```go
fmt.Printf("PANIC %s %s: %v\n%s\n", r.Method, r.URL.Path, rec, debug.Stack())
```

**修复建议**：使用 `slog.Error("panic.recovered", ...)` 保持日志格式统一。

---

### 🟢 低优先级问题

| # | 问题 | 文件位置 | 说明 |
|---|------|----------|------|
| 11 | `initBrowsePreparing` 的 `setInterval` 没有清理 | [app.js:67](file:///Users/liguoqiang/project/kasten-frs-web/web/static/app.js#L67) | 页面离开时无 `clearInterval` |
| 12 | `handleDownload` 忽略了 `sess.Stat` 的错误 | [handlers.go:553](file:///Users/liguoqiang/project/kasten-frs-web/internal/handlers/handlers.go#L553) | `stat, _ := sess.Stat(path)` |
| 13 | `sanitizeArchiveName` 未检查文件名中的特殊字符 | [handlers.go:690-700](file:///Users/liguoqiang/project/kasten-frs-web/internal/handlers/handlers.go#L690-L700) | Content-Disposition 中的文件名应做 RFC 5987 编码 |
| 14 | Prometheus metrics 已注册但未在 handler 中实际更新 | [metrics.go](file:///Users/liguoqiang/project/kasten-frs-web/internal/metrics/metrics.go) | `LoginAttemptsTotal`、`DownloadBytesTotal` 等 Counter 定义了但没找到 `.Inc()` 调用点 |

---

## 二、日志输出分析

### ✅ 做得好的方面

| 方面 | 说明 |
|------|------|
| **结构化 JSON 日志** | 使用 `log/slog`，JSON 格式输出，便于 ELK/Splunk 解析 |
| **访问日志** | `AccessLog` 中间件记录 method/path/query/status/bytes/duration/remote/ua |
| **探针过滤** | 跳过 `/healthz`、`/readyz` 探针日志，避免日志噪音 |
| **SFTP 操作日志** | 每次 dial/listdir/open 都有日志，且截断了 hostKeySig 避免密钥泄露 |
| **FRS 生命周期日志** | 创建/就绪/超时/失败 都有日志，关键状态可追踪 |
| **Wizard 步骤日志** | namespace/vm/rp 列表操作都有计数日志 |

### ⚠️ 需要改进的方面

#### 1. 缺少 Request ID / Correlation ID（**关键缺陷**）

> [!IMPORTANT]
> [logging.go](file:///Users/liguoqiang/project/kasten-frs-web/internal/logging/logging.go#L37-L57) 定义了 `WithRequestID` 和 `WithSessionID`，但**全局没有任何代码调用它们**。
> 
> 在客户环境排查问题时，无法将一个用户请求的多条日志关联在一起。例如：wizard 创建 FRS 涉及多个后端调用，日志中看不出哪些调用属于同一个用户操作。

**修复建议**：在 `AccessLog` 中间件中生成 UUID request_id 注入 context，所有后续日志通过 `logging.FromContext` 取 logger。

#### 2. handler 层日志使用 `slog.XXX` 而非从 context 取 logger

[handlers.go](file:///Users/liguoqiang/project/kasten-frs-web/internal/handlers/handlers.go) 全部使用全局 `slog.Info/slog.Error`，丢失了 request context：

```go
slog.Info("sftp.connect.start", "user", s.auth.Username, ...)
slog.Error("render browse", "err", err)
```

**影响**：日志中没有 request_id，无法追踪单次请求的完整调用链。

#### 3. 缺少启动配置摘要日志

[main.go:101](file:///Users/liguoqiang/project/kasten-frs-web/cmd/helper/main.go#L101) 仅日志 `addr` 和 `version`：

```go
logger.Info("helper starting", "addr", l.Addr().String(), "version", version)
```

**修复建议**：增加关键配置摘要（掩码敏感值），便于部署验证：
```go
logger.Info("helper starting",
    "addr", l.Addr().String(),
    "version", version,
    "k8s_in_cluster", cfg.K8sInCluster,
    "frs_port", cfg.FRSPort,
    "sftp_timeout", cfg.SFTPConnectTimeout,
    "frs_wait_timeout", cfg.FRSWaitTimeout,
    "ns_whitelist", cfg.FRSNamespaceWhitelist,
    "session_ttl", cfg.SessionTTL,
    "pool_ttl", cfg.SFTPPoolTTL,
)
```

#### 4. K8s API 错误日志不足

[frs.go:55](file:///Users/liguoqiang/project/kasten-frs-web/internal/k8s/frs.go#L55) `ListActiveFRS` 仅 `%w` 包装错误，没有日志：

```go
return nil, fmt.Errorf("list FRS: %w", err)
```

**影响**：K8s API 403/404/500 错误只在 handler 层被渲染为用户页面，在日志中可能被淹没。

**修复建议**：在 k8s 包层面添加 `slog.Warn` 日志，包含 GVR、namespace、HTTP 状态码。

#### 5. 日志级别使用建议

| 当前日志 | 建议调整 |
|----------|----------|
| `slog.Error("render login", "err", err)` | 模板渲染失败是 **程序级 bug**，保持 Error |
| `slog.Info("sftp.listdir", ...)` | 文件浏览是高频操作，建议降为 **Debug** |
| `slog.Info("sftp.open", ...)` | 文件下载操作应保持 **Info**（审计需要） |
| `slog.Info("sessions.enrich.start", ...)` | 内部遍历，建议降为 **Debug** |
| `slog.Info("rp.details.ok", ...)` | 正常响应，建议降为 **Debug** |

---

## 三、Web 样式和逻辑分析

### ✅ 优点

| 方面 | 评价 |
|------|------|
| **架构选型** | Go template + htmx 避免了前端框架的复杂性，适合内部工具 |
| **Veeam 品牌一致性** | CSS 主题与 K10 仪表盘风格保持一致 |
| **Wizard 三级级联** | VM → RestorePoint → Volume 选择流程直观 |
| **SFTP 连接池** | 避免每次请求重新建立 SSH 连接 |
| **安全头部** | HSTS/CSP/X-Frame-Options/Permissions-Policy 完善 |
| **路径遍历防护** | `handleDownloadZip` 有 `path.Clean` + 前缀检查 |
| **HMAC 会话** | Cookie 使用 HMAC 签名，防篡改 |

### ⚠️ 需要改进的方面

#### 1. 缺少 CSRF 保护（**安全风险**）

> [!CAUTION]
> 所有 POST 表单（login、wizard/create、delete、connect、extend、cancel）都没有 CSRF token。虽然 Cookie 设置了 `SameSite=Strict`，但旧浏览器可能不支持 SameSite。

**修复方案**：在 session 中生成 CSRF token，表单中添加隐藏字段，服务端校验。

#### 2. 缺少 `<meta name="viewport">`

[layout.html](file:///Users/liguoqiang/project/kasten-frs-web/web/templates/layout.html) 缺少 viewport 元标签：

```html
<meta name="viewport" content="width=device-width, initial-scale=1.0">
```

#### 3. 无响应式设计

[veeam-theme.css](file:///Users/liguoqiang/project/kasten-frs-web/web/static/veeam-theme.css) 完全没有 `@media` 查询。wizard 的三列 grid 在平板/手机上会挤压。

**建议**：至少添加 `@media (max-width: 768px)` 下的单列布局。

#### 4. 文件大小显示

browse 页面显示原始 bytes（`{{.Size}} bytes`），大文件不友好。

**建议**：添加 Go template 函数 `humanSize` 或在 JS 中格式化。

#### 5. script 标签阻塞渲染

layout.html 中 `<script src="/static/htmx.min.js">` 和 `<script src="/static/app.js">` 在 `<head>` 中**同步加载**。

**修复**：添加 `defer` 属性。

---

## 四、OpenShift 部署详细步骤

> [!NOTE]
> 以下步骤基于项目代码中的 [DEPLOY.md](file:///Users/liguoqiang/project/kasten-frs-web/DEPLOY.md)、[deploy/ 目录](file:///Users/liguoqiang/project/kasten-frs-web/deploy)、[kustomization.yaml](file:///Users/liguoqiang/project/kasten-frs-web/deploy/kustomization.yaml) 和 [Dockerfile](file:///Users/liguoqiang/project/kasten-frs-web/Dockerfile) 整理。

### 前置条件

| 条件 | 说明 |
|------|------|
| OpenShift ≥ 4.11 | 需要支持 `restricted-v2` SCC |
| Kasten K10 已安装 | 需要 `filerecoverysessions.datamover.kio.kasten.io` CRD |
| K10 已有导出型 RestorePoint | FRS **仅支持已导出的还原点** |
| `oc` CLI 已登录集群 | 需要 cluster-admin 或等效权限 |
| 容器镜像仓库可访问 | 需要能推送和拉取镜像 |

### Step 1: 构建并推送镜像

```bash
# 克隆代码
git clone <repo-url> && cd kasten-frs-web

# 修改 kustomization.yaml 中的镜像仓库地址
# 编辑 deploy/kustomization.yaml，将 registry.example.com 改为实际仓库
# images:
#   - name: kasten-frs-helper
#     newName: your-registry.com/kasten-frs-helper
#     newTag: "0.3.0"

# 构建镜像（基于 UBI9-minimal，兼容 RHEL/OCP）
podman build -t your-registry.com/kasten-frs-helper:0.3.0 .

# 推送到镜像仓库
podman push your-registry.com/kasten-frs-helper:0.3.0
```

### Step 2: 创建凭据 Secret

```bash
# 生成随机密码和 Cookie Secret
HELPER_USER="admin"
HELPER_PASS=$(openssl rand -base64 24)
COOKIE_SECRET=$(openssl rand -base64 32)

echo "Helper Username: $HELPER_USER"
echo "Helper Password: $HELPER_PASS"
echo "请妥善保管以上凭据！"

# 创建 Credentials Secret（部署文件中通过 envFrom 引用）
oc create secret generic kasten-frs-web-helper-credentials \
  --from-literal=HELPER_USERNAME="$HELPER_USER" \
  --from-literal=HELPER_PASSWORD="$HELPER_PASS" \
  --from-literal=HELPER_COOKIE_SECRET="$COOKIE_SECRET" \
  -n kasten-io
```

> [!IMPORTANT]
> SSH 密钥对**无需手动创建**。Helper 首次启动时会自动生成 Ed25519 密钥对并持久化到 K8s Secret `kasten-frs-helper-private-key`。公钥会自动嵌入到每个通过 wizard 创建的 FRS 中。

### Step 3: 配置 OpenShift SCC（关键步骤）

Helper pod 本身使用 `restricted-v2` SCC，无需额外配置。但 K10 FRS mounter pod 需要 privileged 权限：

```bash
# 方案 1（推荐）：标记 kasten-io 命名空间为 privileged
oc label namespace kasten-io \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/audit=privileged \
    pod-security.kubernetes.io/warn=privileged \
    --overwrite

# 验证标签
oc get ns kasten-io --show-labels
```

> [!CAUTION]
> 如果不做此配置，wizard 创建的 FRS 会因为 `violates PodSecurity "restricted:latest"` 而失败（state=Failed），SFTP 连接永远无法建立。

### Step 4: 部署应用

```bash
# 使用 Kustomize 一键部署所有资源
oc apply -k deploy/

# 部署包含以下资源：
#   - ServiceAccount: kasten-frs-web-helper
#   - ClusterRole/ClusterRoleBinding: FRS/RP 操作权限
#   - Role/RoleBinding: kasten-io 内 Secret 管理权限
#   - Deployment: helper pod（1 副本）
#   - Service: 端口 80 → 8080
#   - Route: TLS edge termination
#   - NetworkPolicy (x2): 入站/出站规则 + FRS pod 访问策略
```

### Step 5: 部署 FRS 访问 NetworkPolicy

> [!IMPORTANT]
> 这是最容易遗漏的步骤！K10 datamover 为每个 FRS 创建的 NetworkPolicy 仅允许应用命名空间（如 `default`）的入站连接。Helper pod 在 `kasten-io` 命名空间，默认被阻断。

```bash
# 确认 55-networkpolicy-helper-access-frs.yaml 已包含在 kustomize 中
# 此策略允许 helper pod 通过端口 2222 访问 FRS pod

# 如果使用独立部署（非 kustomize），手动应用：
oc apply -f deploy/55-networkpolicy-helper-access-frs.yaml -n kasten-io
```

### Step 6: 验证部署

```bash
# 1. 等待 pod 就绪
HELPER_POD=$(oc get pod -n kasten-io -l app=kasten-frs-web-helper \
  -o jsonpath='{.items[0].metadata.name}')
oc wait --for=condition=Ready pod/$HELPER_POD -n kasten-io --timeout=120s

# 2. 检查 pod 日志（应看到启动信息）
oc logs -n kasten-io $HELPER_POD | head -5
# 期望输出：{"level":"INFO","msg":"helper starting","addr":"0.0.0.0:8080","version":"0.3.0"}

# 3. 检查健康探针
oc exec -n kasten-io $HELPER_POD -- curl -s http://localhost:8080/healthz
# 期望输出：ok

# 4. 验证 NetworkPolicy 连通性
# DNS 解析
oc exec -n kasten-io $HELPER_POD -- nslookup kubernetes.default
# K8s API 连通性
oc exec -n kasten-io $HELPER_POD -- curl -sk https://kubernetes.default.svc/api

# 5. 获取 Route URL
ROUTE_URL=$(oc get route kasten-frs-web-helper -n kasten-io \
  -o jsonpath='{.spec.host}')
echo "Helper URL: https://$ROUTE_URL"
```

### Step 7: 功能冒烟测试

```bash
# 1. 浏览器访问 Route URL
# 2. 使用 Step 2 中的 Username/Password 登录
# 3. 进入 /wizard 页面
# 4. 选择 VM → RestorePoint → Volume → 创建 FRS
# 5. 等待 FRS 就绪（约 30 秒）
# 6. 浏览文件目录树
# 7. 下载测试文件验证

# 或使用自动化测试脚本：
./scripts/deploy-test.sh
```

### 常见问题排查

| 症状 | 原因 | 解决方案 |
|------|------|----------|
| FRS 创建后 state=Failed | SCC 不允许 privileged pod | 执行 Step 3 标记命名空间 |
| Browse 页面 i/o timeout | NetworkPolicy 阻断 2222 端口 | 确认 55-networkpolicy 已应用 |
| 登录后重定向循环 | Cookie Secret 不足 16 字节 | 重新生成 HELPER_COOKIE_SECRET |
| wizard 看不到 VM | 没有 appType=virtualMachine 的 RP | 确认 K10 有虚拟机类型的导出备份 |
| 镜像拉取失败 | 镜像仓库地址错误 | 检查 kustomization.yaml 中的 images |
| Helper pod CrashLoopBackOff | 配置缺失 | 检查 Secret 是否包含所有必需环境变量 |
| DNS 解析失败 | NetworkPolicy 缺少 DNS 出站规则 | 检查 50-networkpolicy.yaml 是否包含 UDP 53 出站 |

### 环境变量参考

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `HELPER_USERNAME` | （必需） | Web 登录用户名 |
| `HELPER_PASSWORD` | （必需） | Web 登录密码 |
| `HELPER_COOKIE_SECRET` | （必需，≥16字节） | Session Cookie HMAC 密钥 |
| `HELPER_PORT` | 8080 | 监听端口 |
| `HELPER_SESSION_TTL` | 8h | 会话有效期 |
| `HELPER_SFTP_TTL` | 30m | SFTP 连接池空闲超时 |
| `HELPER_SFTP_TIMEOUT` | 10s | SFTP 连接超时 |
| `HELPER_FRS_WAIT_TIMEOUT` | 120s | 等待 FRS 就绪超时 |
| `HELPER_FRS_PORT` | 2222 | FRS SFTP 服务端口 |
| `HELPER_FRS_NAMESPACES` | （空=全部） | 限制可见命名空间，逗号分隔 |
| `HELPER_LOG_LEVEL` | info | 日志级别：debug/info/warn/error |
| `HELPER_K8S_INCLUSTER` | true | 是否使用 in-cluster K8s 配置 |

---

## 总结

### 整体评价

项目代码质量**较高**，属于企业级内部工具水准。架构选型（Go + htmx + slog）合理，部署配置对 OpenShift 特殊需求（SCC、NetworkPolicy、restricted-v2）有深入理解，安全防护（HSTS/CSP/HMAC Cookie/路径校验/安全上下文）做得相当到位。

### 建议优先处理事项

| 优先级 | 事项 | 工作量 |
|--------|------|--------|
| 🔴 P0 | 修复 XSS (app.js innerHTML) | 0.5h |
| 🔴 P0 | 添加服务端 Session 过期校验 | 1h |
| 🔴 P0 | 修复 `doK8sRequest` BearerToken vs BearerTokenFile | 1h |
| 🟡 P1 | 添加 Request ID 关联日志 | 2h |
| 🟡 P1 | 优化 LookupFRSSource List→Get | 1h |
| 🟡 P1 | 添加 CSRF 保护 | 2h |
| 🟡 P1 | ListVMs 添加 LabelSelector | 0.5h |
| 🟡 P1 | 添加启动配置摘要日志 | 0.5h |
| 🟢 P2 | 添加 viewport meta + 基础响应式 | 1h |
| 🟢 P2 | watchMap 自动过期清理 | 1h |
| 🟢 P2 | Prometheus metrics 实际接入 | 2h |
| 🟢 P2 | 文件大小友好格式化 | 0.5h |

> [!TIP]
> 如果需要，我可以逐项实现以上修复。请告知需要优先处理的事项。

---

## 五、修复实施记录（v0.3.26）

本轮已按 P0 → P1 → P2 全量修复上述 14 项问题，全部测试通过（`go vet` 干净，`go test ./...` 全绿）。

| # | 问题 | 状态 | 关键改动 |
|---|------|------|----------|
| 1 | XSS（app.js innerHTML） | ✅ | 改用 `createElement`/`textContent` 构建隐藏 `pvcNames` 字段 |
| 2 | Session Cookie 仅靠 Max-Age | ✅ | Cookie 内嵌签发时间戳并纳入 HMAC；`Verify` 服务端校验 `issued+TTL>now` |
| 3 | doK8sRequest BearerToken | ✅ | 改用 `rest.HTTPClientFor(cfg)`，自动处理 BearerTokenFile 与刷新 |
| 4 | buildFRSView 隐藏 Pending | ✅ | 新增 `FRSView.Connectable`；Pending FRS 仍列出，Browse 按钮禁用 |
| 5 | LookupFRSSource List→Get | ✅ | 用 `Get(name)` 替代全量 List + 遍历 |
| 6 | ListVMs 无 LabelSelector | ✅ | 加 `k10.kasten.io/appType=virtualMachine` 服务端过滤 |
| 7 | 缺 Request ID 关联日志 | ✅ | `AccessLog` 生成 `request_id` 注入 context + `X-Request-Id` 响应头；handler 改用上下文 logger |
| 8 | 缺 CSRF 保护 | ✅ | 基于 session cookie 的无状态 HMAC CSRF token；`RequireAuth` 对不安全方法强制校验；表单注入隐藏字段 |
| 9 | 缺启动配置摘要日志 | ✅ | `main.go` 启动日志补全关键配置（掩码敏感值） |
| 10 | Recoverer 用 fmt.Printf | ✅ | 改用 `slog.Error("panic.recovered", ...)` + request_id |
| 11 | initBrowsePreparing 不清理 interval | ✅ | 页面隐藏/元素移除时 `clearInterval` |
| 12 | handleDownload 忽略 Stat 错误 | ✅ | 记录 warn 日志，仍可流式下载 |
| 13 | 文件名未 RFC 5987 编码 | ✅ | `contentDispositionFilename` 输出 ASCII 回退 + `filename*=UTF-8''` |
| — | validatePath 不完整 | ✅ | 改为按 `/` 分段拒绝 `..` 段，允许 `file..name` 等合法名 |
| — | watchMap 无过期清理 | ✅ | `watchState.createdAt` + 后台 sweeper（10m 扫描、1h 过期） |
| — | viewport / 响应式 / defer | ✅ | 加 viewport meta、`script defer`、`@media(max-width:768px)` 单列布局 |
| — | 文件大小友好格式化 | ✅ | 新增 `humanSize` 模板函数（KiB/MiB/…） |
| — | Prometheus metrics 接入 | ✅ | login / FRS list / SFTP connect / download / K8s error 实际打点 |

新增/强化的单元测试：会话过期与时间戳防篡改、CSRF token 往返与跨会话拒绝、Pending FRS 可见性、`validatePath` 表驱动用例、watchMap sweep、CSRF 端到端（mux 层 403/放行）。
