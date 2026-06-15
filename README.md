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