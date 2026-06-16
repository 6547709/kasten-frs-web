# Kasten FRS Web Helper

A web UI for browsing and downloading Kasten FileRecoverySession (FRS) recovery
data over HTTPS, in environments where OpenShift Router cannot expose raw SFTP.

## Features

- Web-based browse + download of FRS contents (no SFTP client needed)
- SSO-friendly: one Helper account, HMAC-signed session cookies
- Cluster-wide FRS listing, namespace-scoped Secret read (least-privilege)
- Veeam-style UI (Chinese localization)
- Prometheus metrics, structured logging
- NetworkPolicy-aware: explicit egress allowlist for DNS, K8s API, FRS :2222

## Wizard (v0.3.0+)

`/wizard` provides a single-page recovery flow:

1. Pick a VM (filtered by `k10.kasten.io/appType=virtualMachine` label)
2. Pick a Bound RestorePoint
3. Pick the PVCs to include
4. Click **Create FRS**

The helper:
- generates or derives the SSH keypair on first start (no operator
  `ssh-keygen` step required)
- creates the FileRecoverySession via the K8s dynamic client
- polls the FRS state in-memory; the user sees a "preparing" page
  until state=Ready, then is redirected to the directory listing

## Quick start (local)

```bash
# Prereqs: Go 1.22+
make fetch-htmx
go test ./...
go build -o bin/helper ./cmd/helper
# Out-of-cluster: provide a kubeconfig and disable in-cluster mode
HELPER_USERNAME=admin \
HELPER_PASSWORD=secret \
HELPER_COOKIE_SECRET=$(openssl rand -hex 16) \
HELPER_K8S_INCLUSTER=false \
KUBECONFIG=$HOME/.kube/config \
./bin/helper
```

Then open http://localhost:8080/login.

## OpenShift deployment

See [DEPLOY.md](DEPLOY.md).

## Architecture

See [docs/superpowers/specs/2026-06-15-kasten-frs-web-design.md](docs/superpowers/specs/2026-06-15-kasten-frs-web-design.md).

## License

Apache 2.0