# Kasten FRS Web — 部署测试设计（2026-06-15）

> 一份可重放的端到端部署测试脚本，针对 `kasten-frs-web` 在 OCP 4
> 集群的 `kasten-io` 命名空间做"部署 + 端到端功能验证"。

## 1. 背景与目标

仓库 `6547709/kasten-frs-web`（位于 GitHub，CI 推送镜像到
`ghcr.io/6547709/kasten-frs-web`，tag 包括 `main`、`latest`、`sha-*`）
对外暴露 Kasten FRS（File Recovery Session）数据浏览/下载的 Web UI。
`deploy/` 下的 Kustomize 资源已经把 Pod 配好（受限 ServiceAccount、
只读根文件系统、NetworkPolicy、Route），但缺少一个可重放的端到端验证
脚本。

**目标**：在不动仓库文件的前提下，编写 `scripts/deploy-test.sh`，
从凭据注入、镜像部署，到 Route 端到端访问、登录、列 FRS、列文件，全程
自动化。任何一步失败，脚本立即退出并打印定位信息。

**非目标**：

- 不修改 `deploy/` 下的任何 manifest。
- 不替换 deploy/20-deployment.yaml 默认的 `v0.1.0` 标签（脚本使用
  Kustomize image override 临时改成 `:main`，不落盘）。
- 不清理已部署资源（除非显式 `--cleanup`）。

## 2. 设计决策与依据

| 决策 | 依据 |
| --- | --- |
| 脚本形式（不入库可执行） | 一次性验证工具，价值在于"跑过"，而非"长期维护" |
| Kustomize image override | `deploy/20-deployment.yaml` 写死 `:v0.1.0`，但 GHCR 上无此 tag；不能改仓库文件 |
| 不动 `deploy/` 文件 | 当前是本地部署测试，不应污染 git 历史 |
| 默认不清理 | 让用户能在测试后继续手玩 / debug；按需 `--cleanup` |
| 走 Route（edge TLS）| 用户确认要做公网访问验证 |
| `set -euo pipefail` | 失败立即退出；未定义变量 / 管道错误均视为失败 |
| 失败打印 pod/log | 加速定位，不依赖用户手动重跑 |
| 日志到 `deploy-test.log` | 失败时方便贴到工单 |

## 3. 架构

```
scripts/deploy-test.sh
├── preflight    凭据长度、oc 鉴权、镜像可达、FRS 存在
├── secrets      写 credentials + private-key 两个 Secret
├── overlay      临时 kustomize 目录，set image 改为 :main
├── apply        oc apply -k overlay/
├── wait         oc wait --for=condition=Ready
├── probe        /healthz /readyz（pod 内或经 Service）
├── netpol       DNS / K8s API / FRS:2222 三个 egress
├── e2e          Route TLS → login → /frs → /frs/my-frs-2
└── summary      PASS / FAIL 总结 + 清理 one-liner
```

## 4. 步骤详情

### 4.1 preflight

- 校验环境变量：
  - `HELPER_USERNAME`（必填）
  - `HELPER_PASSWORD`（必填，长度 ≥ 16）
  - `HELPER_COOKIE_SECRET`（必填，长度 ≥ 16）
- 校验文件：`k10_frs`（mode 0600，project 根目录）
- 校验集群：
  - `oc whoami` 非空
  - `oc get frs my-frs-2 -n default -o jsonpath='{.status.conditions[?(@.type=="IsActive")].status}'` = `True`
  - `oc get svc -n kasten-io -l k10.kasten.io/frs-name=my-frs-2 -o name` 至少一条
- 校验镜像：用 ghcr.io 匿名 token GET
  `https://ghcr.io/v2/6547709/kasten-frs-web/manifests/main` 期望 200

### 4.2 secrets

```
HELPER_COOKIE_SECRET_FILE=$(mktemp)
printf '%s' "$HELPER_COOKIE_SECRET" > "$HELPER_COOKIE_SECRET_FILE"

oc -n kasten-io create secret generic kasten-frs-web-helper-credentials \
    --from-literal=HELPER_USERNAME="$HELPER_USERNAME" \
    --from-literal=HELPER_PASSWORD="$HELPER_PASSWORD" \
    --from-literal=HELPER_COOKIE_SECRET="$HELPER_COOKIE_SECRET" \
    --dry-run=client -o yaml | oc apply -f -

oc -n kasten-io create secret generic kasten-frs-helper-private-key \
    --type=kubernetes.io/ssh-auth \
    --from-file=ssh-privatekey=./k10_frs \
    --dry-run=client -o yaml | oc apply -f -
```

幂等：用 `--dry-run=client -o yaml | oc apply -f -` 而非 `create`。

### 4.3 overlay & apply

`scripts/deploy-test.sh` 内联生成临时 overlay 目录（`mktemp -d`），内容：

```
<overlay>/
└── kustomization.yaml
    namespace: kasten-io
    resources:
      - ../../deploy/
    images:
      - name: ghcr.io/liguoqiang/kasten-frs-web
        newName: ghcr.io/6547709/kasten-frs-web
        newTag: main
```

`oc apply -k <overlay>/`。

### 4.4 wait

```
oc -n kasten-io wait --for=condition=Ready \
    pod -l app=kasten-frs-web-helper --timeout=120s
```

### 4.5 probe

```
POD=$(oc -n kasten-io get pod -l app=kasten-frs-web-helper -o jsonpath='{.items[0].metadata.name}')
oc -n kasten-io exec "$POD" -- curl -fsS http://127.0.0.1:8080/healthz
oc -n kasten-io exec "$POD" -- curl -fsS http://127.0.0.1:8080/readyz
```

### 4.6 netpol

```
# DNS
oc -n kasten-io exec "$POD" -- nslookup kubernetes.default

# K8s API
oc -n kasten-io exec "$POD" -- curl -fsSk -o /dev/null \
    -w '%{http_code}\n' https://kubernetes.default.svc/api

# FRS :2222
FRS_SVC=$(oc -n kasten-io get svc -l k10.kasten.io/frs-name=my-frs-2 \
    -o jsonpath='{.items[0].metadata.name}')
oc -n kasten-io exec "$POD" -- bash -c \
    "timeout 3 bash -c '</dev/tcp/${FRS_SVC}.kasten-io.svc.cluster.local/2222' && echo OK"
```

### 4.7 e2e (Route)

> 实际路径（来自 internal/handlers/handlers.go）：
> - `GET /healthz` / `GET /readyz` — 探活
> - `GET /login` — 登录页（HTML 表单）
> - `POST /login` — 提交 username/password，成功 303 → /sessions，Set-Cookie: kfrs_sid
> - `GET /sessions` — FRS 列表（HTML 表格）
> - `POST /sessions/{ns}/{name}/connect` — 建立 SFTP 会话，303 → /browse
> - `GET /browse?frs={ns}/{name}&path=/` — 目录列表（HTML 表格）

```
ROUTE_HOST=$(oc -n kasten-io get route kasten-frs-web-helper \
    -o jsonpath='{.spec.host}')
BASE="https://${ROUTE_HOST}"
COOKIE_JAR=$(mktemp)

# 1. 登录页可达
curl -fsSk -o /dev/null -w '%{http_code}\n' "$BASE/login"   # 期望 200

# 2. POST /login 拿 cookie（Set-Cookie: kfrs_sid=...）
LOGIN_CODE=$(curl -fsSk -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
    -o /dev/null -w '%{http_code}' \
    --data-urlencode "username=$HELPER_USERNAME" \
    --data-urlencode "password=$HELPER_PASSWORD" \
    "$BASE/login")
[ "$LOGIN_CODE" = "303" ] || { echo "login expected 303, got $LOGIN_CODE"; exit 1; }
grep -q '^[^#].*kfrs_sid' "$COOKIE_JAR" \
    || { echo "no kfrs_sid cookie issued"; exit 1; }

# 3. 列 FRS（带 cookie 访问 /sessions）
SESSIONS_HTML=$(curl -fsSk -b "$COOKIE_JAR" "$BASE/sessions")
echo "$SESSIONS_HTML" | grep -q 'my-frs-2' \
    || { echo "FRS my-frs-2 not in /sessions listing"; exit 1; }
echo "$SESSIONS_HTML" | grep -qiE 'class="data"|<table' \
    || { echo "/sessions does not look like a table"; exit 1; }

# 4. 触发 SFTP 连接（POST /sessions/default/my-frs-2/connect）
CONNECT_CODE=$(curl -fsSk -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
    -o /dev/null -w '%{http_code}' -X POST \
    "$BASE/sessions/default/my-frs-2/connect")
[ "$CONNECT_CODE" = "303" ] || { echo "connect expected 303, got $CONNECT_CODE"; exit 1; }

# 5. 访问 /browse 拿根目录
BROWSE_HTML=$(curl -fsSk -b "$COOKIE_JAR" \
    "$BASE/browse?frs=default/my-frs-2&path=/")
echo "$BROWSE_HTML" | grep -qiE '<tr|<td' \
    || { echo "/browse does not look like a directory listing"; exit 1; }
echo "$BROWSE_HTML" | grep -q 'my-frs-2' \
    || { echo "browse page does not name my-frs-2"; exit 1; }
```

> 步骤 4 必做——`handleBrowse` 若 pool 中没 session 会 303 把请求重定向
> 到 `/sessions/{ns}/{name}/connect`，没有 SFTP 握手直接 GET /browse 会被
> 跳走，看不到目录列表。
> spec 在此固化"必须能列目录"作为验收标志，模板是表格形态。

### 4.8 summary

```
=== DEPLOY TEST SUMMARY ===
image:        ghcr.io/6547709/kasten-frs-web:main
pod:          kasten-io/<pod-name>
route:        https://<route-host>
frs tested:   my-frs-2 (default)
overall:      PASS

# 手动清理 one-liner:
# oc -n kasten-io delete deploy,svc,route,sa,networkpolicy \
#   -l app=kasten-frs-web-helper
# oc -n kasten-io delete secret kasten-frs-web-helper-credentials \
#   kasten-frs-helper-private-key
```

## 5. 错误处理

- `set -euo pipefail`。
- 顶层 trap `ERR`：打印 `=== FAIL at step N: <name> (line L) ===` + 最近 30 行 `deploy-test.log`，并 `oc describe pod "$POD" -n kasten-io`。
- `curl` 命令统一用 `curl -fsSk`（fail、silent、insecure-skip-verify 跳过自签证书），失败时带 `-v` 重写到 `deploy-test.log`。
- 每个 step 用 `STEP_NAME=...; log ">>> $STEP_NAME"` 标记，方便定位。
- 最终退出码：成功 = 0；失败 = 对应 step 编号（1..7，preflight/secrets/apply/wait-probe/netpol/e2e/summary）。

## 6. 输入参数 / 环境变量

| 变量 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `HELPER_USERNAME` | ✅ | — | Web 登录用户名 |
| `HELPER_PASSWORD` | ✅ | — | Web 登录密码，长度 ≥ 16 |
| `HELPER_PASSWORD_MIN` | 否 | 16 | 覆盖最小长度；仅当用户显式 `export HELPER_PASSWORD_MIN=8` 时才生效；脚本会打印 WARNING |
| `HELPER_COOKIE_SECRET` | ✅ | — | session 签名密钥，长度 ≥ 16 |
| `KUBECONFIG` | 否 | 现有 | 集群访问配置 |
| `--cleanup` | 否 | off | 退出前删除本次部署的全部资源 |

> `HELPER_PASSWORD=VMware1!`（用户提供，长度 9）小于默认 16。
> 用户可在调用前 `export HELPER_PASSWORD_MIN=8` 显式放宽，脚本会打印
> WARNING 并继续；这是测试环境的明确选择。

## 7. 验证标准

测试通过需满足：

1. 所有 8 个 step 退出码 = 0。
2. `e2e` 步骤 5（`/browse?frs=default/my-frs-2&path=/`）返回 HTML 含 `<tr` 或 `<td` 表格行。
3. 退出码 = 0；summary 输出 `overall: PASS`。

## 8. 不在范围内

- 跨集群迁移 / 灾备演练。
- SFTP 文件下载完整性校验（仅做"列表可见"）。
- 长时间稳定性（liveness probe、内存、CPU 趋势）观察。
- GitHub Packages 拉取速度 / 缓存命中率。
- 任何 `deploy/` 下的文件修改。

## 9. 风险与缓解

| 风险 | 缓解 |
| --- | --- |
| `VMware1!` 长度 < 16，preflight 会拒绝 | 实施前与用户确认是否放宽（HELPER_PASSWORD_MIN=8 仅限测试环境） |
| `ghcr.io` 拉取被网络层拦截 | preflight 阶段先 HEAD 一下 manifests/main |
| Route 主机名不可被容器/客户端解析 | e2e 步骤加 `--resolve` 提示，如不可达则给"用 `oc port-forward`"的备选方案 |
| FRS :2222 实际没数据 | 步骤 4 加宽松断言（任意 200 + 包含目录标识） |
| Secret 已经被别人创建过 | 使用 `oc apply` 幂等更新，覆盖 `stringData` 内容 |
| `k10_frs` 私钥 mode 不是 0600 | 脚本以 `chmod 600` 强制，但记录 warning |
| main tag 在 CI 重跑时漂移 | 用 `oci://ghcr.io/...:main@sha256:...` 形式 pin digest（preflight 阶段捕获 digest 并写入 overlay）|

## 10. 实施时再细化

- 步骤 4.7 e2e 的最终断言（实际模板结构）→ 已修：用 `<tr` / `<td` 表格行 + `my-frs-2` 名称出现。
- `oc exec` 探活命令的最终形式：spec 已明确走 `curl http://127.0.0.1:8080/{healthz,readyz}`。
- 是否 pin digest：默认**不 pin**（用 `:main`），脚本在 preflight 打印 `digest=sha256:...` 供调试。
- 是否在 preflight 里把 FRS 公钥指纹与本地 `k10_frs.pub` 的指纹对比：spec 步骤 4.1 **不强制**（Kasten 控制平面已把公钥注入到 FRS spec，公钥指纹差异只会让 SFTP 在步骤 4 失败时被捕获）。
