# Kasten FRS Web 部署测试脚本 — 实施 Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `kasten-frs-web` 仓库里实现一个可重放的端到端部署测试脚本 `scripts/deploy-test.sh`，把凭据注入、镜像部署（GHCR `:main`）、Pod 探活、NetworkPolicy 连通性、Route 端到端访问 8 个 step 串起来。

**Architecture:** 单一 bash 脚本（~400 行），`set -euo pipefail`，用 trap + 步骤编号退出来定位失败。Secret 写入和 Kustomize 镜像覆盖都是幂等的（`oc apply`，不 `oc create`），临时 overlay 用 `mktemp -d` 落盘到 `/tmp`、结束时不主动清理（除非 `--cleanup`）。任何 step 失败都打印 `oc describe pod` + `oc logs` + 最近 30 行 `deploy-test.log`。

**Tech Stack:**
- bash ≥ 4.0（用 `[[ ]]`、数组、process substitution）
- `oc` / `kubectl`（已通过 KUBECONFIG 验证）
- `curl`（系统自带）
- 可选 `shellcheck`（验证时使用）
- 可选 `jq`（解析 oc 输出；可被 oc jsonpath 替代）

**前置条件（实施前确认）：**
- `KUBECONFIG` 已指向 OCP 4 集群，`oc whoami` 不为空
- `k10_frs`（SSH 私钥，mode 0600）在 `kasten-frs-web/` 项目根
- 用户已提供 `HELPER_USERNAME=admin`、`HELPER_PASSWORD=VMware1!`
  - 长度 9 < 默认 16，调用前需 `export HELPER_PASSWORD_MIN=8`
- FRS `my-frs-2` 在 `default` 命名空间 Ready（已确认）
- 镜像 `ghcr.io/6547709/kasten-frs-web:main` 在 GHCR 可达（已确认 200）

**约定：**
- 所有 step 内的 `oc` 命令在容器外跑；需要进 pod 的命令用 `oc -n kasten-io exec POD -- ...`
- 每完成一个 Task 做一次 `git commit`
- 临时文件统一在 `mkdir -p /tmp/kfrs-test && cd /tmp/kfrs-test` 下创建

---

## File Structure

| 文件 | 行为 | 状态 |
| --- | --- | --- |
| `scripts/deploy-test.sh` | 入口脚本，包含所有 8 个 step | Create |
| `docs/superpowers/specs/2026-06-15-kasten-frs-web-deploy-test-design.md` | 已有的设计 spec | Exists（不修改） |
| `deploy/20-deployment.yaml` | 已有（不修改；通过 kustomize image override 改 tag） | Exists（不修改） |

单文件脚本。约 400 行。函数粒度：每个 step 一个 `step_<N>_<name>` 函数；公共 `log/die/require/curl_pretty` 在脚本顶部。

---

## Task 1: 脚本骨架 + helpers + 参数解析

**Files:**
- Create: `scripts/deploy-test.sh`

- [ ] **Step 1.1: 创建空文件并加 shebang + 严格模式**

```bash
mkdir -p /home/liguoqiang/kasten-frs-web/scripts
cd /home/liguoqiang/kasten-frs-web

cat > scripts/deploy-test.sh <<'HEADER'
#!/usr/bin/env bash
# deploy-test.sh — end-to-end deployment test for kasten-frs-web.
# See docs/superpowers/specs/2026-06-15-kasten-frs-web-deploy-test-design.md.

set -euo pipefail
HEADER
chmod +x scripts/deploy-test.sh
```

- [ ] **Step 1.2: 跑语法检查**

Run: `bash -n scripts/deploy-test.sh && echo OK`
Expected: prints `OK`, exit 0.

- [ ] **Step 1.3: 提交骨架**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: add deploy-test.sh skeleton with strict mode"
```

- [ ] **Step 1.4: 追加 helpers（log / die / require / curl_pretty）**

把以下内容追加到 `scripts/deploy-test.sh` 末尾：

```bash

# ---- logging ----
LOG_FILE="${LOG_FILE:-/tmp/kfrs-test/deploy-test.log}"
mkdir -p "$(dirname "$LOG_FILE")"
: > "$LOG_FILE"

_color() { printf '\033[%sm%s\033[0m' "$1" "$2"; }
log()   { printf '%s %s\n' "$(_color 36 '>>>')" "$*"; }
ok()    { printf '%s %s\n' "$(_color 32 ' OK')" "$*"; }
warn()  { printf '%s %s\n' "$(_color 33 'WARN')" "$*" >&2; }
err()   { printf '%s %s\n' "$(_color 31 'ERR ')" "$*" >&2; }
die()   { err "$*"; exit 1; }

step() {
    STEP_NAME="$*"
    log "[step $STEP_NUM] $STEP_NAME"
    printf '=== STEP %s :: %s ===\n' "$STEP_NUM" "$STEP_NAME" >> "$LOG_FILE"
}

require() {
    command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

# curl wrapper: fail-fast, silent, follow redirects, capture body & status.
# Usage: curl_pretty URL OUT_VAR STATUS_VAR
curl_pretty() {
    local url="$1" out_var="$2" status_var="$3"
    local body status
    body=$(curl -sS -L -k -o /dev/null -w '%{http_code}' "$url" 2>>"$LOG_FILE") \
        || { err "curl failed for $url (see $LOG_FILE)"; return 1; }
    status="$body"
    body=$(curl -sS -L -k "$url")
    printf -v "$out_var" '%s' "$body"
    printf -v "$status_var" '%s' "$status"
}

trap 'on_err $? $LINENO' ERR

on_err() {
    local rc=$1 line=$2
    err "FAIL at step ${STEP_NUM:-?}: ${STEP_NAME:-?} (line $line, rc=$rc)"
    echo "--- last 30 lines of $LOG_FILE ---" >&2
    tail -n 30 "$LOG_FILE" >&2 || true
    if [ -n "${POD:-}" ]; then
        echo "--- oc describe pod -n kasten-io $POD ---" >&2
        oc -n kasten-io describe pod "$POD" >&2 || true
        echo "--- oc logs -n kasten-io $POD --previous ---" >&2
        oc -n kasten-io logs --previous "$POD" >&2 || true
    fi
    exit "$rc"
}
```

- [ ] **Step 1.5: 语法 + 必需命令检查**

Run:
```bash
bash -n scripts/deploy-test.sh && echo SYNTAX_OK
bash -c 'source <(sed -n "/^trap/,/^}/p" scripts/deploy-test.sh); echo HELPERS_LOADED'
```
Expected: `SYNTAX_OK` 然后 `HELPERS_LOADED`.

- [ ] **Step 1.6: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: add log/die/curl_pretty helpers and ERR trap"
```

---

## Task 2: 参数解析 + 环境变量校验 (preflight 1/2)

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 2.1: 追加 `parse_args` + 全局变量声明**

```bash

# ---- globals & config ----
IMAGE_REPO="ghcr.io/6547709/kasten-frs-web"
IMAGE_TAG="${IMAGE_TAG:-main}"
FRS_NAME="${FRS_NAME:-my-frs-2}"
FRS_NAMESPACE="${FRS_NAMESPACE:-default}"
NS="kasten-io"
LOGIN_USER_KEY="HELPER_USERNAME"
LOGIN_PASS_KEY="HELPER_PASSWORD"
COOKIE_SECRET_KEY="HELPER_COOKIE_SECRET"
HELPER_PASSWORD_MIN="${HELPER_PASSWORD_MIN:-16}"
DEPLOY_LABEL="app=kasten-frs-web-helper"
CLEANUP="false"
SKIP_E2E="false"
POD=""
ROUTE_HOST=""
BASE=""
COOKIE_JAR=""

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --cleanup)  CLEANUP=true; shift ;;
            --skip-e2e) SKIP_E2E=true; shift ;;
            --tag)      IMAGE_TAG="$2"; shift 2 ;;
            --frs)      FRS_NAME="$2"; shift 2 ;;
            -h|--help)
                cat <<USAGE
Usage: $0 [--cleanup] [--skip-e2e] [--tag <tag>] [--frs <name>]
Env:   HELPER_USERNAME  HELPER_PASSWORD  HELPER_COOKIE_SECRET
       HELPER_PASSWORD_MIN  (default 16)  LOG_FILE  (default /tmp/kfrs-test/deploy-test.log)
USAGE
                exit 0 ;;
            *) die "unknown arg: $1" ;;
        esac
    done
}
```

- [ ] **Step 2.2: 追加 `require_env` (env 长度校验)**

```bash

require_env() {
    for k in "$LOGIN_USER_KEY" "$LOGIN_PASS_KEY" "$COOKIE_SECRET_KEY"; do
        [ -n "${!k:-}" ] || die "env $k is required"
    done
    local min_pw="$HELPER_PASSWORD_MIN"
    if [ "${#HELPER_PASSWORD}" -lt "$min_pw" ]; then
        warn "HELPER_PASSWORD length ${#HELPER_PASSWORD} < $min_pw (test-env override)"
    fi
    if [ "${#HELPER_COOKIE_SECRET}" -lt 16 ]; then
        die "HELPER_COOKIE_SECRET length ${#HELPER_COOKIE_SECRET} < 16"
    fi
}
```

- [ ] **Step 2.3: 语法 + `--help` 烟雾测试**

Run:
```bash
bash -n scripts/deploy-test.sh
bash scripts/deploy-test.sh --help
```
Expected: 第一行 exit 0；第二行打印 Usage 段（到 `USAGE`），exit 0。

- [ ] **Step 2.4: 缺 env 应报错**

Run:
```bash
env -i bash -c 'unset HELPER_USERNAME HELPER_PASSWORD HELPER_COOKIE_SECRET
                source <(grep -A1 "^require_env" scripts/deploy-test.sh)
                require_env' 2>&1 | tail -3
```
Expected: 包含 `HELPER_USERNAME is required`。

- [ ] **Step 2.5: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: add arg parsing and env validation helpers"
```

---

## Task 3: preflight — 集群、文件、FRS、镜像可达性

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 3.1: 追加 `step1_preflight`**

```bash

step1_preflight() {
    STEP_NUM=1
    step "preflight: env / oc / FRS / ghcr.io"
    require_env
    require oc
    require curl
    oc whoami >/dev/null || die "oc not logged in"
    [ -f ./k10_frs ] || die "./k10_frs (SSH private key) not found in cwd"
    chmod 600 ./k10_frs || true

    local frs_active
    frs_active=$(oc get frs "$FRS_NAME" -n "$FRS_NAMESPACE" \
        -o jsonpath='{.status.conditions[?(@.type=="IsActive")].status}')
    [ "$frs_active" = "True" ] || die "FRS $FRS_NAMESPACE/$FRS_NAME IsActive != True (got '$frs_active')"

    local svc_count
    svc_count=$(oc get svc -n "$NS" -l "k10.kasten.io/frs-name=$FRS_NAME" -o name | wc -l)
    [ "$svc_count" -ge 1 ] || die "no Service in $NS for FRS $FRS_NAME"

    local code
    code=$(curl -sS -o /dev/null -w '%{http_code}' \
        "https://ghcr.io/v2/6547709/kasten-frs-web/manifests/${IMAGE_TAG}")
    [ "$code" = "200" ] || die "ghcr.io manifest for ${IMAGE_TAG} returned $code (expected 200)"
    ok "preflight ok: image ${IMAGE_REPO}:${IMAGE_TAG} reachable, FRS active"
}
```

- [ ] **Step 3.2: 追加 `main()` 入口骨架（先只跑 preflight）**

```bash

main() {
    parse_args "$@"
    step1_preflight
    if [ "$SKIP_E2E" = "true" ]; then
        log "skip-e2e set; stopping after preflight"
        exit 0
    fi
    log "(steps 2-8 not yet implemented; this is a checkpoint run)"
    exit 0
}

main "$@"
```

- [ ] **Step 3.3: 跑 preflight 真实集群**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
export HELPER_USERNAME=admin
export HELPER_PASSWORD=VMware1!
export HELPER_PASSWORD_MIN=8
export HELPER_COOKIE_SECRET="$(openssl rand -base64 32)"
bash scripts/deploy-test.sh
```
Expected: 打印 `>>> [step 1] preflight ...` 然后 `OK preflight ok: ...` 然后 `skip-e2e` 提示前退出，exit 0。

- [ ] **Step 3.4: 镜像 tag 错时预校验失败**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh --tag this-tag-does-not-exist 2>&1 | tail -5
```
Expected: 包含 `ghcr.io manifest for this-tag-does-not-exist returned 404`。

- [ ] **Step 3.5: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: implement preflight (env/oc/FRS/ghcr.io checks)"
```

---

## Task 4: secrets 步骤（凭证 + 私钥）

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 4.1: 追加 `step2_secrets`**

```bash

step2_secrets() {
    STEP_NUM=2
    step "secrets: credentials + ssh private key"
    oc -n "$NS" create secret generic kasten-frs-web-helper-credentials \
        --from-literal="HELPER_USERNAME=$HELPER_USERNAME" \
        --from-literal="HELPER_PASSWORD=$HELPER_PASSWORD" \
        --from-literal="HELPER_COOKIE_SECRET=$HELPER_COOKIE_SECRET" \
        --dry-run=client -o yaml | oc apply -f - >>"$LOG_FILE" 2>&1

    oc -n "$NS" create secret generic kasten-frs-helper-private-key \
        --type=kubernetes.io/ssh-auth \
        --from-file=ssh-privatekey=./k10_frs \
        --dry-run=client -o yaml | oc apply -f - >>"$LOG_FILE" 2>&1

    oc -n "$NS" get secret kasten-frs-web-helper-credentials \
        -o jsonpath='{.data.HELPER_USERNAME}' | base64 -d >/dev/null \
        || die "credentials secret missing HELPER_USERNAME"
    oc -n "$NS" get secret kasten-frs-helper-private-key \
        -o jsonpath='{.data.ssh-privatekey}' | base64 -d | head -1 \
        | grep -q -- '-----BEGIN' \
        || die "private-key secret does not look like a PEM key"
    ok "secrets applied (idempotent)"
}
```

- [ ] **Step 4.2: 在 `main()` 中插入 step2 调用**

Replace the `(steps 2-8 not yet implemented...)` block in `main()` with:

```bash
    step2_secrets
    log "(steps 3-8 not yet implemented; this is a checkpoint run)"
    exit 0
```

- [ ] **Step 4.3: 跑 step 1+2**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh
```
Expected: step 1 `OK preflight ok...` → step 2 `OK secrets applied...` → exit 0。

- [ ] **Step 4.4: 幂等性 — 再跑一次**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh
```
Expected: 同样 PASS（不出现 `AlreadyExists` 错误，因为用 `oc apply`）。

- [ ] **Step 4.5: 验证 Secret 真在集群里**

Run:
```bash
oc -n kasten-io get secret kasten-frs-web-helper-credentials kasten-frs-helper-private-key
```
Expected: 两行，TYPE 分别 `Opaque` 和 `kubernetes.io/ssh-auth`。

- [ ] **Step 4.6: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: apply credentials and ssh private-key secrets"
```

---

## Task 5: overlay 生成 + oc apply

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 5.1: 追加 `step3_overlay_apply`**

```bash

OVERLAY_DIR=""

step3_overlay_apply() {
    STEP_NUM=3
    step "overlay: kustomize image override + apply"
    OVERLAY_DIR=$(mktemp -d -t kfrs-overlay-XXXXXX)
    cat > "$OVERLAY_DIR/kustomization.yaml" <<YAML
namespace: $NS
resources:
  - ../../deploy/
images:
  - name: ghcr.io/liguoqiang/kasten-frs-web
    newName: $IMAGE_REPO
    newTag: $IMAGE_TAG
YAML
    oc apply -k "$OVERLAY_DIR/" >>"$LOG_FILE" 2>&1
    oc -n "$NS" get deploy kasten-frs-web-helper \
        -o jsonpath='{.spec.template.spec.containers[0].image}' >/dev/null \
        || die "deployment kasten-frs-web-helper not created"
    local actual
    actual=$(oc -n "$NS" get deploy kasten-frs-web-helper \
        -o jsonpath='{.spec.template.spec.containers[0].image}')
    [ "$actual" = "${IMAGE_REPO}:${IMAGE_TAG}" ] \
        || die "image override failed: got '$actual' expected '${IMAGE_REPO}:${IMAGE_TAG}'"
    ok "applied overlay; image is $actual"
}
```

- [ ] **Step 5.2: 在 main 替换 checkpoint 块**

Replace `(steps 3-8 not yet implemented...)` with:
```bash
    step3_overlay_apply
    log "(steps 4-8 not yet implemented; this is a checkpoint run)"
    exit 0
```

- [ ] **Step 5.3: 跑 step 1+2+3**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh
```
Expected: 三步都 OK，Deployment 用 `:main` 镜像创建。

- [ ] **Step 5.4: 验证 deployment 用 `:main` 镜像**

Run:
```bash
oc -n kasten-io get deploy kasten-frs-web-helper \
    -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```
Expected: `ghcr.io/6547709/kasten-frs-web:main`

- [ ] **Step 5.5: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: generate kustomize overlay with image override and apply"
```

---

## Task 6: 等待 Pod Ready + 探活 (/healthz, /readyz)

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 6.1: 追加 `step4_wait_probe`**

```bash

step4_wait_probe() {
    STEP_NUM=4
    step "wait + probe: pod Ready, /healthz, /readyz"
    oc -n "$NS" wait --for=condition=Ready pod -l "$DEPLOY_LABEL" \
        --timeout=180s >>"$LOG_FILE" 2>&1 \
        || die "pod did not become Ready within 180s"
    POD=$(oc -n "$NS" get pod -l "$DEPLOY_LABEL" \
        -o jsonpath='{.items[0].metadata.name}')
    [ -n "$POD" ] || die "no pod found for label $DEPLOY_LABEL"
    oc -n "$NS" exec "$POD" -- curl -fsS http://127.0.0.1:8080/healthz >>"$LOG_FILE" 2>&1 \
        || die "/healthz probe failed"
    oc -n "$NS" exec "$POD" -- curl -fsS http://127.0.0.1:8080/readyz >>"$LOG_FILE" 2>&1 \
        || die "/readyz probe failed"
    ok "pod $POD ready; /healthz and /readyz OK"
}
```

- [ ] **Step 6.2: 在 main 中插入**

Replace `(steps 4-8 not yet implemented...)` with:
```bash
    step4_wait_probe
    log "(steps 5-8 not yet implemented; this is a checkpoint run)"
    exit 0
```

- [ ] **Step 6.3: 跑 step 1-4**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh
```
Expected: 4 步都 OK，退出 0。

> 镜像首次拉取可能慢；`--timeout=180s` 足够。若 ContainerCreating 报 `ImagePullBackOff`，先 `oc -n kasten-io describe pod`。

- [ ] **Step 6.4: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: wait for pod Ready and probe /healthz, /readyz"
```

---

## Task 7: NetworkPolicy — DNS / K8s API / FRS :2222

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 7.1: 追加 `step5_netpol`**

```bash

step5_netpol() {
    STEP_NUM=5
    step "netpol: DNS, K8s API, FRS:2222"
    oc -n "$NS" exec "$POD" -- nslookup kubernetes.default >>"$LOG_FILE" 2>&1 \
        || die "DNS lookup kubernetes.default failed"

    local api_code
    api_code=$(oc -n "$NS" exec "$POD" -- \
        curl -sS -k -o /dev/null -w '%{http_code}' \
        https://kubernetes.default.svc/api)
    [ "$api_code" = "200" ] || die "K8s API returned $api_code (expected 200)"

    local frs_svc
    frs_svc=$(oc -n "$NS" get svc -l "k10.kasten.io/frs-name=$FRS_NAME" \
        -o jsonpath='{.items[0].metadata.name}')
    [ -n "$frs_svc" ] || die "FRS service for $FRS_NAME not found in $NS"

    oc -n "$NS" exec "$POD" -- \
        bash -c "timeout 3 bash -c '</dev/tcp/${frs_svc}.${NS}.svc.cluster.local/2222' && echo OK" \
        >>"$LOG_FILE" 2>&1 \
        || die "TCP connect to FRS :2222 failed (svc=$frs_svc)"
    ok "netpol: DNS, API, FRS:2222 all reachable"
}
```

- [ ] **Step 7.2: 在 main 中插入**

Replace `(steps 5-8 not yet implemented...)` with:
```bash
    step5_netpol
    log "(steps 6-8 not yet implemented; this is a checkpoint run)"
    exit 0
```

- [ ] **Step 7.3: 跑 step 1-5**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh
```
Expected: 5 步都 OK。

- [ ] **Step 7.4: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: verify NetworkPolicy egress (DNS, API, FRS:2222)"
```

---

## Task 8: e2e — Route 端到端 (login → /sessions → connect → /browse)

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 8.1: 追加 `step6_e2e`**

```bash

step6_e2e() {
    STEP_NUM=6
    step "e2e: Route, login, /sessions, connect, /browse"
    ROUTE_HOST=$(oc -n "$NS" get route kasten-frs-web-helper \
        -o jsonpath='{.spec.host}')
    [ -n "$ROUTE_HOST" ] || die "Route kasten-frs-web-helper has no host"
    BASE="https://${ROUTE_HOST}"
    COOKIE_JAR=$(mktemp -t kfrs-cookies-XXXXXX)

    log "BASE=$BASE"
    local code
    code=$(curl -sS -k -L -o /dev/null -w '%{http_code}' "$BASE/login")
    [ "$code" = "200" ] || die "/login returned $code (expected 200)"

    local login_code
    login_code=$(curl -sS -k -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
        -o /dev/null -w '%{http_code}' \
        --data-urlencode "username=$HELPER_USERNAME" \
        --data-urlencode "password=$HELPER_PASSWORD" \
        "$BASE/login")
    [ "$login_code" = "303" ] || die "POST /login returned $login_code (expected 303)"
    grep -q 'kfrs_sid' "$COOKIE_JAR" \
        || die "no kfrs_sid cookie issued (see $COOKIE_JAR)"

    local sessions_html
    sessions_html=$(curl -sS -k -b "$COOKIE_JAR" "$BASE/sessions")
    echo "$sessions_html" | grep -q "$FRS_NAME" \
        || die "/sessions does not list $FRS_NAME"
    echo "$sessions_html" | grep -qiE '<table' \
        || die "/sessions HTML does not contain <table"

    local connect_code
    connect_code=$(curl -sS -k -L -b "$COOKIE_JAR" -c "$COOKIE_JAR" \
        -o /dev/null -w '%{http_code}' -X POST \
        "$BASE/sessions/${FRS_NAMESPACE}/${FRS_NAME}/connect")
    [ "$connect_code" = "303" ] || die "POST /sessions/.../connect returned $connect_code (expected 303)"

    local browse_html
    browse_html=$(curl -sS -k -L -b "$COOKIE_JAR" \
        "$BASE/browse?frs=${FRS_NAMESPACE}/${FRS_NAME}&path=/")
    echo "$browse_html" | grep -qiE '<tr|<td' \
        || die "/browse HTML does not look like a directory listing"
    echo "$browse_html" | grep -q "$FRS_NAME" \
        || die "/browse does not mention $FRS_NAME"
    ok "e2e passed: login → sessions → connect → browse all 200/303"
}
```

- [ ] **Step 8.2: 在 main 中插入**

Replace `(steps 6-8 not yet implemented...)` with:
```bash
    step6_e2e
    log "(steps 7-8 not yet implemented; this is a checkpoint run)"
    exit 0
```

- [ ] **Step 8.3: 跑 step 1-6**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh
```
Expected: 6 步都 OK；最后打印 `e2e passed: login → sessions → connect → browse all 200/303`。

- [ ] **Step 8.4: 故意错密码看是否拦得住**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=WRONG_PASSWORD_xxxxxxxx \
HELPER_PASSWORD_MIN=8 HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh 2>&1 | tail -8
```
Expected: 走到 step 6 e2e 时 `die "POST /login returned 401 (expected 303)"`（或 401，匹配 `!= 303`）。

- [ ] **Step 8.5: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: add end-to-end Route/login/sessions/connect/browse flow"
```

---

## Task 9: summary + `--cleanup` 收尾

**Files:**
- Modify: `scripts/deploy-test.sh`（追加）

- [ ] **Step 9.1: 追加 `step7_summary` 和 `cleanup`**

```bash

step7_summary() {
    STEP_NUM=7
    step "summary"
    cat <<SUMMARY
=== DEPLOY TEST SUMMARY ===
image:    ${IMAGE_REPO}:${IMAGE_TAG}
pod:      ${NS}/${POD}
route:    ${BASE}
frs:      ${FRS_NAMESPACE}/${FRS_NAME}
log:      ${LOG_FILE}
overall:  PASS
SUMMARY
}

cleanup() {
    log "cleanup: removing test resources from $NS"
    oc -n "$NS" delete deploy,svc,route -l "$DEPLOY_LABEL" \
        --ignore-not-found >>"$LOG_FILE" 2>&1 || true
    oc -n "$NS" delete sa kasten-frs-web-helper \
        --ignore-not-found >>"$LOG_FILE" 2>&1 || true
    oc -n "$NS" delete rolebinding,role,clusterrolebinding,clusterrole \
        -l "$DEPLOY_LABEL" --ignore-not-found >>"$LOG_FILE" 2>&1 || true
    oc -n "$NS" delete networkpolicy -l "$DEPLOY_LABEL" \
        --ignore-not-found >>"$LOG_FILE" 2>&1 || true
    oc -n "$NS" delete secret kasten-frs-web-helper-credentials \
        kasten-frs-helper-private-key --ignore-not-found >>"$LOG_FILE" 2>&1 || true
    [ -n "$OVERLAY_DIR" ] && [ -d "$OVERLAY_DIR" ] && rm -rf "$OVERLAY_DIR"
    [ -n "$COOKIE_JAR" ] && [ -f "$COOKIE_JAR" ] && rm -f "$COOKIE_JAR"
    ok "cleanup done"
}
```

- [ ] **Step 9.2: 在 main 末尾插入 summary + 可选 cleanup**

Replace `(steps 7-8 not yet implemented...)` with:
```bash
    step7_summary
    [ "$CLEANUP" = "true" ] && cleanup
    exit 0
```

- [ ] **Step 9.3: 跑完整流程（无 cleanup）**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh 2>&1 | tail -10
```
Expected: 全部 7 步 OK，最后打印 summary 段，`overall: PASS`；exit 0。集群里资源保留。

- [ ] **Step 9.4: 资源仍在**

Run:
```bash
oc -n kasten-io get deploy,svc,route,sa,secret -l app=kasten-frs-web-helper
```
Expected: 至少看到 `deploy/kasten-frs-web-helper`、`svc/kasten-frs-web-helper-svc`、`route/kasten-frs-web-helper`、`sa/kasten-frs-web-helper`、两个 secret。

- [ ] **Step 9.5: 跑 `--cleanup`**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh --cleanup 2>&1 | tail -8
```
Expected: 7 步 OK + `cleanup done`。

- [ ] **Step 9.6: 资源已清**

Run:
```bash
oc -n kasten-io get deploy,svc,route,sa,secret -l app=kasten-frs-web-helper
```
Expected: 头行为 `No resources found in kasten-io namespace.`（Pod 删除时间可能滞后几秒，可等 5s 再查）。

- [ ] **Step 9.7: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: add summary and --cleanup support"
```

---

## Task 10: shellcheck + README 注释 + 最终全量 run

**Files:**
- Modify: `scripts/deploy-test.sh`（追加 docstring）
- Create: `docs/superpowers/plans/2026-06-15-kasten-frs-web-deploy-test-design.md`（plan 自身可不入库；如要保留则 commit）

- [ ] **Step 10.1: 跑 shellcheck（如有）**

Run:
```bash
command -v shellcheck >/dev/null && shellcheck scripts/deploy-test.sh || echo "shellcheck not installed, skipping"
```
Expected: 若有 shellcheck 应输出 0 issues；否则打印 `skipping`。
如果有 issue，按建议修；典型会出现的：`SC2086`（未引号变量）、`SC2155`（声明+赋值合并）。逐个修。

- [ ] **Step 10.2: 在脚本顶部 Usage 段加 docstring**

Replace the first 3 行：

```bash
#!/usr/bin/env bash
# deploy-test.sh — end-to-end deployment test for kasten-frs-web.
# See docs/superpowers/specs/2026-06-15-kasten-frs-web-deploy-test-design.md.
```

with:

```bash
#!/usr/bin/env bash
#
# deploy-test.sh — end-to-end deployment test for kasten-frs-web.
#
# Drives the kasten-frs-web Helper image (ghcr.io/6547709/kasten-frs-web)
# through preflight → secrets → kustomize apply → wait/probe → netpol
# → Route e2e, on an OpenShift 4 cluster with KUBECONFIG set.
#
# Usage:
#   HELPER_USERNAME=admin HELPER_PASSWORD=... HELPER_COOKIE_SECRET=... \
#     bash scripts/deploy-test.sh [--cleanup] [--skip-e2e] [--tag <tag>]
#
# Design: docs/superpowers/specs/2026-06-15-kasten-frs-web-deploy-test-design.md
```

- [ ] **Step 10.3: 跑完整流程 + cleanup，验证全绿**

Run:
```bash
cd /home/liguoqiang/kasten-frs-web
HELPER_USERNAME=admin HELPER_PASSWORD=VMware1! HELPER_PASSWORD_MIN=8 \
HELPER_COOKIE_SECRET="$(openssl rand -base64 32)" \
bash scripts/deploy-test.sh --cleanup
```
Expected: 7 步全部 OK，summary `overall: PASS`，`cleanup done`，exit 0。

- [ ] **Step 10.4: 提交**

```bash
cd /home/liguoqiang/kasten-frs-web
git add scripts/deploy-test.sh
git commit -m "scripts: document deploy-test.sh and pass shellcheck"
```

---

## Self-Review Checklist（执行完后自检）

- [ ] `bash -n scripts/deploy-test.sh` exit 0
- [ ] `shellcheck scripts/deploy-test.sh` 0 issues（若已安装）
- [ ] `git log --oneline | head -10` 看到 10 个新 commit
- [ ] 跑 `bash scripts/deploy-test.sh --cleanup` 全程 PASS，集群资源清干净
- [ ] `k10_frs` 和 `k10_frs.pub` 仍 untracked（脚本不会 commit 它们）
