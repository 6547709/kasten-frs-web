#!/usr/bin/env bash
# deploy-test.sh — end-to-end deployment test for kasten-frs-web.
# See docs/superpowers/specs/2026-06-15-kasten-frs-web-deploy-test-design.md.

set -euo pipefail

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

main() {
    parse_args "$@"
    step1_preflight
    step2_secrets
    if [ "$SKIP_E2E" = "true" ]; then
        log "skip-e2e set; stopping after preflight"
        exit 0
    fi
    log "(steps 3-8 not yet implemented; this is a checkpoint run)"
    exit 0
}

main "$@"
