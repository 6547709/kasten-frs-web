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
