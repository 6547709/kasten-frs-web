#!/usr/bin/env bash
# bin/check-netpol.sh - validate network paths from inside the helper pod.
set -euo pipefail
NS=kasten-io
POD=$(oc get pod -n "$NS" -l app=kasten-frs-web-helper -o jsonpath='{.items[0].metadata.name}')

echo "=== 1. DNS ==="
oc exec -n "$NS" "$POD" -- nslookup kubernetes.default.svc.cluster.local >/dev/null

echo "=== 2. K8s API ==="
TOKEN=$(oc exec -n "$NS" "$POD" -- cat /var/run/secrets/kubernetes.io/serviceaccount/token)
oc exec -n "$NS" "$POD" -- curl -sk -H "Authorization: Bearer $TOKEN" \
    https://kubernetes.default.svc/api >/dev/null

echo "=== 3. FRS Service ==="
FRS_SVC="${1:?usage: $0 <frs-service-name> <frs-namespace>}"
FRS_NS="${2:-kasten-io}"
oc exec -n "$NS" "$POD" -- bash -c \
    "timeout 3 bash -c '</dev/tcp/$FRS_SVC.$FRS_NS.svc.cluster.local/2222'"

echo "all network paths verified"
