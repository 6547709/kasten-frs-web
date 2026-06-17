# Changelog

## 0.3.25 (2026-06-17)

- Removed the last remaining Chinese strings on the FRS pages
  (sessions table headers, error back-link, app.js countdown and
  wizard placeholders, layout `lang="en"`). The deploy/00-namespace
  yaml and the helper-only SCC note from 0.3.24 are kept.
- Fixed the screen-in-screen regression on the FRS preparing page.
  `handlePartialReady` now returns just the preparing fragment on
  terminal Failed/Timeout (via a new `renderPreparingBody` helper)
  instead of the full layout, so htmx no longer wraps a full
  `<html>` inside the wrapper on every poll.
- `handlePartialReady` now also detects plain-browser navigation
  to `?partial=ready` (no `HX-Request` header) and falls back to
  the full preparing page render, so the back/forward button and
  the address bar no longer land on a blank 204 page.
- Lengthened the FRS ready-wait window: `HELPER_FRS_WAIT_TIMEOUT`
  default is now `120s` (was 30s), the "Wait longer" button passes
  `sec=60`, and `handleBrowseExtend` calls a new
  `watchFRSCreatedWithTimeout` so the button label matches the
  actual wait window. Poll interval 2s -> 3s.
- Ignored `k10_frs` / `k10_frs.pub` in `.gitignore` (local SSH
  keys for manual K10 testing).

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