#!/usr/bin/env bash
# Wizard e2e using the in-process fake K8s clientset.
# Usage: scripts/wizard-test.sh
# The full e2e coverage lives in the Go test files; this shell script
# is a placeholder that documents the entry point.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE/.."

echo "→ wizard e2e is covered by go test ./... — run:"
echo "  go test ./internal/handlers/ -run Wizard -v"
