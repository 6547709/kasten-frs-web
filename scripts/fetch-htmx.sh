#!/usr/bin/env bash
# scripts/fetch-htmx.sh - download HTMX 1.9.10 for embedding
set -euo pipefail
DEST="${1:-web/static/htmx.min.js}"
URL="https://github.com/bigskysoftware/htmx/releases/download/v1.9.10/htmx.min.js"
mkdir -p "$(dirname "$DEST")"
curl -sSL -o "$DEST" "$URL"
test $(stat -c%s "$DEST") -gt 10000 || { echo "download too small, aborting" >&2; exit 1; }
echo "downloaded $DEST ($(stat -c%s "$DEST") bytes)"
