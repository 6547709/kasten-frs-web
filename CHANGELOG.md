# Changelog

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