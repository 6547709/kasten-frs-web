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

set -euo pipefail

# ---- logging ----
LOG_FILE="${LOG_FILE:-/tmp/kfrs-test/deploy-test.log}"
mkdir -p "$(dirname "$LOG_FILE")"
: > "$LOG_FILE"
chmod 600 "$LOG_FILE"

_color() { printf '\033[%sm%s\033[0m' "$1" "$2"; }
log()   { printf '%s %s\n' "$(_color 36 '>>>')" "$*"; }
ok()    { printf '%s %s\n' "$(_color 32 ' OK')" "$*"; }
warn()  { printf '%s %s\n' "$(_color 33 'WARN')" "$*" >&2; }
err()   { printf '%s %s\n' "$(_color 31 'ERR ')" "$*" >&2; }
die()   { err "$*"; exit 1; }

# Tracks the last-entered step's name and number so the ERR trap can
# report which step failed even if a step's body aborts before we get
# to log(). Reset to empty at the start of each step.
LAST_STEP_NUM=""
LAST_STEP_NAME=""

# step_start <n> <name> — record the current step and log the header.
step_start() {
    LAST_STEP_NUM="$1"
    LAST_STEP_NAME="$2"
    log "[step $LAST_STEP_NUM] $LAST_STEP_NAME"
    printf '=== STEP %s :: %s ===\n' "$LAST_STEP_NUM" "$LAST_STEP_NAME" >> "$LOG_FILE"
}

require() {
    command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

trap 'on_err $? $LINENO' ERR

on_err() {
    local rc=$1 line=$2
    err "FAIL at step ${LAST_STEP_NUM:-?}: ${LAST_STEP_NAME:-?} (line $line, rc=$rc)"
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
OVERLAY_DIR=""

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

step1_preflight() {
    step_start 1 "preflight: env / oc / FRS / ghcr.io"
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

    local code token
    # GHCR's manifest endpoint requires a bearer token even for public
    # packages, and the multi-arch images are OCI image indexes. Fetch
    # an anonymous pull-scoped token first, then GET with the right
    # Accept header (Docker manifest headers return 404 for OCI indexes).
    token=$(curl -sS \
        "https://ghcr.io/token?service=ghcr.io&scope=repository:6547709/kasten-frs-web:pull" \
        | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    [ -n "$token" ] || die "failed to get ghcr.io anonymous token"
    code=$(curl -sS -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $token" \
        -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json" \
        "https://ghcr.io/v2/6547709/kasten-frs-web/manifests/${IMAGE_TAG}")
    [ "$code" = "200" ] || die "ghcr.io manifest for ${IMAGE_TAG} returned $code (expected 200)"
    ok "preflight ok: image ${IMAGE_REPO}:${IMAGE_TAG} reachable, FRS active"
}

step2_secrets() {
    step_start 2 "secrets: credentials + ssh private key"
    oc -n "$NS" create secret generic kasten-frs-web-helper-credentials \
        --from-literal="HELPER_USERNAME=$HELPER_USERNAME" \
        --from-literal="HELPER_PASSWORD=$HELPER_PASSWORD" \
        --from-literal="HELPER_COOKIE_SECRET=$HELPER_COOKIE_SECRET" \
        --dry-run=client -o yaml | oc apply -f - >>"$LOG_FILE" 2>&1

    oc -n "$NS" create secret generic kasten-frs-helper-private-key \
        --type=kubernetes.io/ssh-auth \
        --from-file=ssh-privatekey=./k10_frs \
        --dry-run=client -o yaml | oc apply -f - >>"$LOG_FILE" 2>&1

    local b64 pem
    b64=$(oc -n "$NS" get secret kasten-frs-web-helper-credentials \
        -o jsonpath='{.data.HELPER_USERNAME}')
    [ -n "$b64" ] || die "credentials secret missing HELPER_USERNAME"
    pem=$(oc -n "$NS" get secret kasten-frs-helper-private-key \
        -o jsonpath='{.data.ssh-privatekey}')
    [ -n "$pem" ] || die "private-key secret missing ssh-privatekey"
    echo "$pem" | base64 -d | grep -qE -- '-----BEGIN .* PRIVATE KEY-----' \
        || die "private-key secret does not look like a PEM key"
    ok "secrets applied (idempotent)"
}

step3_overlay_apply() {
    step_start 3 "overlay: kustomize image override + apply"
    # Resolve script's own location to an absolute path. BASH_SOURCE[0] is
    # relative when the script is invoked as `bash scripts/deploy-test.sh`,
    # so we must absolutize before the inner `cd`, otherwise the path
    # resolution follows $PWD (which may be unrelated) and fails silently.
    local src="${BASH_SOURCE[0]}"
    [ "${src:0:1}" = "/" ] || src="$PWD/$src"
    local DEPLOY_DIR
    DEPLOY_DIR="$(cd "$(dirname "$src")/.." && pwd)/deploy"
    [ -d "$DEPLOY_DIR" ] || die "deploy/ dir not found at $DEPLOY_DIR (script must be at scripts/deploy-test.sh)"
    local PROJECT_ROOT
    PROJECT_ROOT="$(dirname "$DEPLOY_DIR")"
    # Read-only source trees (CI sandboxes, read-only bind mounts) cannot
    # host the overlay. Reject explicitly rather than producing a cryptic
    # mktemp error and (worse) a kustomize path that won't resolve.
    if ! ( : >> "$PROJECT_ROOT/.write-test" ) 2>/dev/null; then
        die "PROJECT_ROOT $PROJECT_ROOT is read-only; cannot create overlay"
    fi
    rm -f "$PROJECT_ROOT/.write-test"
    OVERLAY_DIR=$(mktemp -d -p "$PROJECT_ROOT" -t .kfrs-overlay-XXXXXX)
    cat > "$OVERLAY_DIR/kustomization.yaml" <<YAML
namespace: $NS
resources:
  - ../deploy
images:
  - name: ghcr.io/liguoqiang/kasten-frs-web
    newName: $IMAGE_REPO
    newTag: $IMAGE_TAG
# OpenShift's restricted-v2 SCC has runAsUser.type=MustRunAsRange and
# validates against the namespace's allocated UID range (in this cluster
# 1001100000/10000). deploy/20-deployment.yaml pins runAsUser: 1001 which
# falls outside that range and is rejected on admission. Strip the explicit
# runAsUser so the SCC picks one from the namespace range; set fsGroup
# to the same range so the emptyDir volumes (/tmp, /app/.cache) are writable
# by the container's kernel UID.
patches:
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: kasten-frs-web-helper
    patch: |-
      - op: remove
        path: /spec/template/spec/securityContext/runAsUser
      - op: add
        path: /spec/template/spec/securityContext/fsGroup
        value: 1001100000
  - target:
      group: networking.k8s.io
      version: v1
      kind: NetworkPolicy
      name: kasten-frs-web-helper
    patch: |-
      # K10's default-deny netpol (selector={}, policyTypes=[Ingress]) blocks
      # all ingress to kasten-io pods. Our netpol only allows ingress on 8080
      # from openshift-ingress, so the helper cannot receive responses from
      # the kube-apiserver (secret GET at startup) or the FRS pod (SFTP).
      # Add ingress allow rules for the apiserver namespace and the FRS
      # namespace (default).
      - op: add
        path: /spec/ingress/-
        value:
          from:
            - namespaceSelector:
                matchLabels:
                  kubernetes.io/metadata.name: openshift-apiserver
          ports:
            - protocol: TCP
              port: 443
      - op: add
        path: /spec/ingress/-
        value:
          from:
            - namespaceSelector:
                matchLabels:
                  kubernetes.io/metadata.name: default
          ports:
            - protocol: TCP
              port: 2222
YAML
    oc apply -k "$OVERLAY_DIR/" >>"$LOG_FILE" 2>&1
    local actual
    actual=$(oc -n "$NS" get deploy kasten-frs-web-helper \
        -o jsonpath='{.spec.template.spec.containers[0].image}') \
        || die "deployment kasten-frs-web-helper not created"
    [ "$actual" = "${IMAGE_REPO}:${IMAGE_TAG}" ] \
        || die "image override failed: got '$actual' expected '${IMAGE_REPO}:${IMAGE_TAG}'"
    ok "applied overlay; image is $actual"
}

step4_wait_probe() {
    step_start 4 "wait + probe: pod Ready, /healthz, /readyz"
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

step5_netpol() {
    step_start 5 "netpol: DNS, K8s API, FRS:2222"
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

step6_e2e() {
    step_start 6 "e2e: Route, login, /sessions, connect, /browse"
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

step7_summary() {
    step_start 7 "summary"
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

main() {
    parse_args "$@"
    step1_preflight
    step2_secrets
    step3_overlay_apply
    step4_wait_probe
    step5_netpol
    # SKIP_E2E is checked *before* step6_e2e so the flag's name matches
    # its behavior: a user who passes --skip-e2e to "verify the deploy
    # without hitting the public Route" gets exactly that.
    if [ "$SKIP_E2E" = "true" ]; then
        log "skip-e2e set; stopping after netpol"
        exit 0
    fi
    step6_e2e
    step7_summary
    [ "$CLEANUP" = "true" ] && cleanup
    exit 0
}

main "$@"
