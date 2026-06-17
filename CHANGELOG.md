# Changelog

## 0.3.26 (2026-06-17)

Security, observability, and UX hardening from the full code review
(`docs/implementation_plan.md`):

- security: eliminate XSS in the wizard volume picker (DOM API instead
  of innerHTML)
- security: enforce session expiry server-side via an HMAC-signed issue
  timestamp (client can no longer extend a session by editing Max-Age)
- security: stateless HMAC CSRF tokens on all authenticated POST forms
- security: harden SFTP path validation to reject `..` segments while
  allowing legitimate dotted names
- security/download: RFC 5987-encoded Content-Disposition filenames
- k8s: use `rest.HTTPClientFor` so the RestorePoint /details call works
  with BearerTokenFile (K8s 1.21+) and rotated tokens
- k8s: `Get` instead of `List`+scan in LookupFRSSource; server-side
  `appType=virtualMachine` label selector for VM listings
- ui: in-flight (Pending) FRS sessions are now listed with a disabled
  Browse button instead of silently disappearing
- observability: per-request `request_id` correlation (context logger +
  `X-Request-Id` header), structured panic logging, startup config
  summary, and real Prometheus metric wiring
- ui: viewport meta, deferred scripts, responsive layout (<=768px),
  human-friendly file sizes
- reliability: watch-map background sweeper prevents unbounded growth

## 0.3.0 (2026-06-16)

- Web-based recovery wizard (VM → restore point → volume → FRS)
- SSH keypair auto-managed by the helper (no operator `ssh-keygen`)
- FRS ready polling is async (no HTTP-blocking waits)
- Sessions page shows expiry countdown + color (warn < 1h, crit < 15m)
- "End and delete" button on sessions and browse pages
- Deploy-experience doc captures SCC, NetworkPolicy, ghcr, and
  FRS state-machine lessons

## 0.1.0 (2026-06-15)

- Initial release
- Web UI for browsing/downloading FRS via SFTP
- Veeam-style Chinese UI
- SSH public-key auth with cached private key
- Cluster-wide FRS list, namespace-scoped Secret read
- Prometheus metrics, structured logging
- Multi-stage UBI 9 Minimal container
- GitHub Actions CI with ghcr.io + cosign + trivy