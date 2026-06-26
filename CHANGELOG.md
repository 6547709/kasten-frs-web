# Changelog

## 0.3.39 (2026-06-26)

- handlers(wizard): the "Missing parameters" 400 from
  /wizard/create now lists the SPECIFIC empty fields and
  what they mean in plain English ("rpName (the chosen
  RestorePoint — not set means no RP row was clicked)"),
  plus an end-of-message hint pointing at the browser
  dev console. The previous "vmNs, vmName, rpName,
  pvcNames are required" left you guessing which step
  of the wizard didn't populate.
- handlers(wizard): on the same 400, log a structured
  wizard.create.missing_params event with the full
  received form (vmNs/vmName/rpName/pvcCount + the
  raw list of form keys) so post-mortem doesn't have
  to guess whether the request came from the wizard
  UI, dev tools, or curl.

## 0.3.38 (2026-06-22)

- ui(wizard): show ✓ Export / ⚠ Snapshot badge on every
  RestorePoint in the wizard's RP picker. K10's
  FileRecoverySession only accepts RPs whose data has been
  pushed to a configured export target (object store or
  NFS/SMB file share). The "snapshot not found... or not
  an exported RestorePoint" error from the datamover is
  misleading — the "or" suggests snapshots are also
  accepted, but per the K10 docs they aren't. So picking a
  pure-snapshot RP and clicking Create FRS just burns
  ~30s of wizard->K10 round trip for a guaranteed
  failure. The new badge lets the user pick an export RP
  on the first try.
- k8s: k8s.RestorePoint gets a Type field ("export" /
  "snapshot") populated from the K10 labels — presence
  of k10.kasten.io/exportProfile ⇒ "export", else
  "snapshot".

## 0.3.37 (2026-06-18)

- ui: drop the "Wait 60s more" button on the preparing page
  and replace it with a "Back to sessions" link. The button
  called /browse/extend → frsGet, which 404'd once K10 had
  cleaned up the (now-Terminal) FRS — surfacing as
  "Failed to query FRS" the moment the user tried to extend
  the wait. The extending-the-wait behaviour was also
  misleading: it unconditionally overwrote the existing
  watch-map entry with Pending, silently restarting a wait
  that K10 had already given up on. Removing the button
  makes the preparing page a clear two-action surface:
  "Cancel and delete FRS" (destructive) or "Back to
  sessions" (read-only exit). The /browse/extend route
  and handleBrowseExtend are removed; the bundle no
  longer ships the dead code.

## 0.3.30 (2026-06-17)

- ui: right-align the wizard "Create FRS" button (text-align on
  .wizard-create) so it reads as the terminal step of the
  left-to-right wizard flow (Step 0 → 1 → 2 → 3 → Create).
- docs: DEPLOY.md rewritten with a Required / Strongly recommended /
  Optional legend, a new "Where the SSH keypair lives" section
  (Secret name, namespace, fields, backup/restore), an upgrade
  section, and a troubleshooting table. New DEPLOY_cn.md is a
  complete Chinese sibling; the two files must stay in sync.

## 0.3.28 (2026-06-17)

UX + deployment hardening:

- ui: replace browser-native `window.confirm` with an in-app, theme-
  matched confirmation modal (animated, focus-managed, Esc=cancel /
  Enter=confirm, backdrop-click cancels). Applies to all
  `confirm-delete` forms (sessions + browse delete).
- deploy: **fix image repo** — manifests pointed at the stale
  `ghcr.io/liguoqiang/...:v0.1.0`; CI publishes to
  `ghcr.io/6547709/kasten-frs-web`. Pinned to `v0.3.27` centrally via
  kustomize `images:` and in the deployment fallback. (Was a guaranteed
  ImagePullBackOff / stale-version deploy.)
- deploy: **fix RBAC** — the Secret `create` grant was scoped by
  `resourceNames`, which Kubernetes ignores for `create`, so the
  helper's first-boot SSH-key Secret creation was unauthorized. Split
  into an unscoped `create` rule + name-scoped `get/update/patch`.
- docs: DEPLOY.md documents the image pin, the RBAC subtlety, and the
  Failed-FRS cleanup flow.

## 0.3.27 (2026-06-17)

- sessions: list ALL FileRecoverySessions, including Failed / Succeeded
  / Terminated and expired ones. Previously a timed-out FRS (which goes
  Failed and has its frs-xxx pod torn down by K10) disappeared from the
  UI, leaving operators unable to clean up the accumulating CR objects.
  Non-Ready rows now render with only a Delete action; the expiry
  countdown is suppressed for terminal/expired sessions.
  - k8s: new `ListAllFRS`; `FRSView.Terminal` flag
  - ui: state-aware action column + badge styling

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