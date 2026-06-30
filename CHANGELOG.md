# Changelog

## 0.3.46 (2026-06-30)

- sftp: junction resolution gains chroot-stripping, the actual
  fix for the Windows FRS restore layout that v0.3.45's ancestor
  walk only partially addressed. v0.3.45's heuristic ("target
  basename matches some ancestor's basename") works for the
  simple junctions (Documents and Settings, Users/All Users,
  Users/Default User) but fails for every junction whose
  destination basename doesn't appear anywhere in the parent
  chain — which is most of them on a real Windows FRS volume.
  Live-cluster inspection of the FRS mounter pod showed that
  ProgramData/{Documents, Templates, 「开始」菜单, 桌面},
  Users/<name>/{Application Data, My Documents, Cookies, ...},
  and ProgramData/Application Data (a self-loop) all had
  unresolvable targets under v0.3.45.
  Fix: probe 2 now strips the SFTP chroot prefix from absolute
  ReadLink targets. K10 datamover returns absolute targets
  with the datamover's internal mount path prepended
  ("/mnt/export/<job>/<vol>/<rest>" on this cluster, but
  unspecified in general). The SFTP client can't see past
  that prefix, so we discover how many leading components
  belong to the chroot by trying ReadDir at each possible
  chroot depth (1..5) and using the first one that produces
  a valid SFTP-relative path. The discovered depth is cached
  on the Session struct — every subsequent junction on the
  same connection costs only one ReadDir. After discovery, the
  helper resolves every junction shape on the live volume
  correctly:
    ProgramData/Documents         → /Users/Public/Documents
    ProgramData/Templates         → /ProgramData/Microsoft/Windows/Templates
    ProgramData/「开始」菜单   → /ProgramData/Microsoft/Windows/Start Menu
    ProgramData/桌面               → /Users/Public/Desktop
    ProgramData/Application Data  → /ProgramData (self-loop, NTFS-correct)
    Users/Alice/My Documents      → /Users/Alice/Documents
    Users/Alice/Application Data  → /Users/Alice/AppData/Roaming
    Users/Alice/Cookies           → /Users/Alice/AppData/Local/Microsoft/Windows/INetCookies
    Users/Alice/Templates         → /Users/Alice/AppData/Roaming/Microsoft/Windows/Templates
  v0.3.45's ancestor walk is kept as a fallback for the rare
  case where chroot-stripping can't derive a depth (e.g. all
  candidates fail ReadDir, which shouldn't happen on a real
  datamover but defends against test/edge cases).
- sftp: tests — `TestClient_ListDir_ChrootStrip_TypicalK10Datamover`
  covers a Documents-style junction with the production-shaped
  target. `..._SelfLoop` covers Application Data pointing back
  at its parent. `..._CachedDepth` verifies the cache is
  populated by the first junction and reused for the second.
  `..._DeeplyNested` covers a 4-suffix-component target
  (Templates-style). All four assert both the resolved
  SFTP-relative path AND `sess.chrootDepth == 2` (the
  discovery value for a 2-component "/mnt/export" chroot).

## 0.3.45 (2026-06-27)

- sftp: replace the hardcoded Windows-junction probe with a
  depth-bounded ancestor walk. v0.3.42's probe 3 carried a
  tiny table of well-known junctions (Documents and Settings
  → Users, All Users → ProgramData, Default User → Default)
  that worked only at depth 0 — it joined the table's target
  onto the junction's own parent, so for `/Users/All Users`
  it probed `/Users/ProgramData` (missing — ProgramData lives
  at `/ProgramData`, the volume root) instead of the real
  target. The new resolver drops the table entirely and uses
  the NTFS invariant "a junction's target basename equals
  the destination directory's name": we ReadLink the
  junction, take the basename, then walk up the parent chain
  probing `<ancestor>/<basename>` at each level up to depth
  4. For `/Users/All Users` (target basename `ProgramData`):
    depth 0: /Users/ProgramData (miss)
    depth 1: /ProgramData      (hit)
  For `/Users/Alice/Documents/Profile` (target basename
  `UserProfile`):
    depth 0: /Users/Alice/Documents/UserProfile (miss)
    depth 1: /Users/Alice/UserProfile            (miss)
    depth 2: /Users/UserProfile                  (miss)
    depth 3: /UserProfile                        (hit)
  Absolute ReadLink targets (K10 datamover's chroot-
  prefixed strings like `/mnt/export/<job>/ProgramData`)
  work the same way — we keep only the basename before
  walking. The hardcoded `windowsJunctionTarget` function
  and its four-entry table are deleted; the resolver is now
  fully generic.
- sftp: ancestor walk capped at `maxJunctionDepth = 4` —
  covers any practical Windows restore layout. Each failed
  probe is one ReadDir round trip; worst case is 4 round
  trips per unresolved symlink (typical FRS top level has
  a handful of junctions, mostly resolved in 1-2 hops).
- sftp: tests for the old hardcode-map fallback removed;
  replaced with `TestClient_ListDir_AncestorWalk_Depth1`,
  `..._Depth2`, `..._Depth3` (nested junctions at increasing
  depths) and `..._AbsoluteTarget` (K10-style chroot-
  prefixed absolute symlink target). All four assert
  IsDir=true and ResolvedPath pointing at the real
  destination directory, NOT at the junction itself.

## 0.3.44 (2026-06-26)

- handlers: fix the regression where /browse rendered an
  empty `<tbody>` (visible as "no contents" in the browser).
  Cause: v0.3.42's browse template referenced `.ResolvedPath`
  on each entry — but Go's html/template looks up fields/
  methods on the CONCRETE type (e.g. pkg/sftp's *fs.fileInfo),
  not the os.FileInfo interface. The wrapper's ResolvedPath()
  method exists on fileInfoWithDir but not on the concrete
  *fs.fileInfo that most entries actually are, so the
  template panicked at execution time. The handler logged
  "render browse: ...can't evaluate field ResolvedPath in
  type fs.FileInfo" but the response went out with the
  partial HTML that had been streamed before the panic.
  Fix: introduce a template-friendly viewmodel struct
  (`browseEntry`) with the fields the template uses, and
  populate it in the handler before ExecuteTemplate. The
  template now reads `.ClickPath` (a pre-computed string)
  instead of calling .ResolvedPath. The wrapper still
  attaches ResolvedPath to resolved entries; the handler
  reads it via resolvedPathOf() and writes the result into
  the viewmodel. No behaviour change for non-symlink
  entries.
- handlers: test for newBrowseEntry locks the viewmodel
  contract — regular dir gets ClickPath=parent/name,
  resolved symlink gets ClickPath=resolved.

## 0.3.43 (2026-06-26)

- sftp: probe 2 (ReadLink) now handles ABSOLUTE symlink targets
  returned by K10 datamover. NTFS junctions on the datamover
  are stored with absolute targets that include the SFTP
  chroot prefix (e.g. `/mnt/export/scheduled-.../Users`),
  which is invisible from the SFTP client — passing the
  absolute path through to ReadDir fails with "file does
  not exist". Fix: when ReadLink returns an absolute target,
  take just its basename and resolve it relative to the
  parent directory. For the typical junction case the
  basename IS the directory name (`Users`, `ProgramData`,
  `Default`), so this lands on the navigable path.
- deploy: drop `commonLabels: {app: kasten-frs-web-helper}`
  from kustomization.yaml. Kustomize injects commonLabels
  into EVERY resource's `spec.podSelector.matchLabels`,
  including the NetworkPolicy `55-*.yaml` whose podSelector
  is supposed to match FRS pods by `k10.kasten.io/frs-
  generation=1` only. The injected `app=kasten-frs-web-
  helper` label didn't match FRS pods (they don't carry
  it), so the policy selected nothing and `default-deny`
  blocked helper→FRS:2222 traffic silently. `oc apply -k
  deploy/` re-injected the label on every rollout, masking
  the issue between `oc delete` + `oc apply -f` manual
  fixes. Removing commonLabels (and adding the few labels
  in-line in each resource file) makes the policy stable
  across rollouts.

## 0.3.42 (2026-06-26)

- sftp: junction resolution gains a two-tier fallback after
  v0.3.41's OPENDIR probe also failed on the live cluster
  (`sftp.listdir.symlink_probe_failed` for Documents and
  Settings / All Users / Default User). K10's datamover SFTP
  server returns "file does not exist" for SSH_FXP_STAT,
  SSH_FXP_OPENDIR, AND SSH_FXP_OPEN on NTFS junctions — its
  mount layer doesn't translate reparse points at the SFTP
  level. New probes added after OPENDIR fails:
    1. `SSH_FXP_READLINK` on the link path. Readlink is a
       metadata read — the server can serve it without
       resolving at the mount layer (it's just bytes from
       the reparse-point metadata block). We then resolve
       the target ourselves (relative target → absolute
       against the parent dir) and probe the resolved
       path with OPENDIR.
    2. Hardcoded Windows junction map. Covers the four
       NTFS junctions that show up on every Windows FRS
       restore (Documents and Settings → Users, All Users
       → ProgramData, Default User → Default, plus the
       nested "Documents and Settings/All Users" form).
       Used as a last-resort fallback when even Readlink
       can't be served.
  When probe 2 or 3 succeeds, we record the RESOLVED path
  on the wrapped FileInfo (`ResolvedPath()`), and the
  browse template + download-zip walker navigate to the
  target instead of the (broken) link.
- ui: browse template uses `entry.ResolvedPath()` (when
  non-empty) as the click target. The link name still
  displays ("Documents and Settings"), but clicking
  goes to /Users — the actual navigable directory.
- handlers(zip): download-zip walker follows `ResolvedPath()`
  on each junction entry, so a zip of a volume root contains
  the contents of /Users, /ProgramData, /Default rather
  than three unreadable 0-byte files. Archive entry NAMES
  keep the original junction name (the user sees
  "Documents and Settings/" in the tar, not "Users/").
- sftp: testserver fs now implements `ReadlinkFileLister`
  so test ReadLink calls return real symlink targets
  (production-shape behaviour). `WithBrokenOpenDir` lets
  a test simulate "datamover doesn't follow NTFS junctions
  at OPENDIR" so probe 2 and 3 can be exercised in isolation.

## 0.3.41 (2026-06-26)

- sftp: switch the junction probe from `Stat` to `OPENDIR`
  (via `ReadDir` on the joined path). v0.3.40's `Stat`-based
  probe was wrong: K10's datamover SFTP server returns
  "file does not exist" for SSH_FXP_STAT on Windows NTFS
  junctions (essentially LSTAT semantics for both LSTAT
  and STAT), so the probe always failed and the entry
  stayed as a file. OPENDIR is mandatory server semantics
  (the server can't list the root without it) and ntfs-3g
  follows reparse points at OPENDIR time, so a successful
  OPENDIR is a reliable tell that the junction is
  navigable. On success we wrap the FileInfo (IsDir=true,
  ModeDir bit set); on failure we keep the original
  symlink FileInfo so the user at least sees the row.
  Cost: one extra OPENDIR+CLOSE per junction (a few
  hundred ms in practice). The `sftp.listdir.symlink_
  stat_failed` log line from v0.3.40 is replaced with
  `sftp.listdir.symlink_probe_failed` /
  `sftp.listdir.symlink_resolved_to_dir`.
- sftp: add `TestClient_ListDir_BrokenSymlinkStaysFile`
  to lock in the fallback path (broken symlink stays as
  file rather than being silently dropped or misreported
  as a directory).
- sftp: testserver `Filelist.Stat` now uses `os.Lstat`
  to mirror K10's datamover LSTAT-on-STAT behaviour, so
  the symlink tests run against a server shape that's
  closer to production.

## 0.3.40 (2026-06-26)

- sftp: ListDir now follows symlinks so Windows directory
  junctions (Documents and Settings, All Users, Default
  User, etc.) are correctly reported as directories instead
  of files. The K10 datamover's SFTP server exposes NTFS
  junctions as symlinks, and pkg/sftp's Lstat returns
  os.ModeSymlink rather than os.ModeDir for those entries,
  so a naive IsDir() check showed them as files. We now
  follow each symlink via Stat; if the target is a
  directory, we wrap the FileInfo so IsDir() reports true
  and the Mode() bit matches. The rest of the stack
  (download, tar walker, path validation) sees a normal
  directory entry. Cost: one SFTP round trip per junction;
  unaffected dirs/files pay nothing.

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