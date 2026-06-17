# Deployment Guide

This document walks an operator through installing the
`kasten-frs-web-helper` onto an OpenShift cluster that already has
Kasten K10 with the `filerecoverysessions.datamover.kio.kasten.io`
CRD installed. A Chinese sibling lives at `DEPLOY_cn.md`; the two
files describe the same steps — keep them in sync when you change
either.

A Chinese translation of this guide is available at
[`DEPLOY_cn.md`](DEPLOY_cn.md). Both documents describe the same
deployment procedure; please keep them in sync if you change one.

---

## Table of contents

1. [Pre-flight checklist](#pre-flight-checklist)
2. [Apply manifests](#apply-manifests)
3. [Where the SSH keypair lives](#where-the-ssh-keypair-lives)
4. [Required NetworkPolicy for FRS dial](#required-networkpolicy-for-frs-dial)
5. [OpenShift SecurityContextConstraints](#openshift-securitycontextconstraints)
6. [Post-flight verification](#post-flight-verification)
7. [Wizard smoke test](#wizard-smoke-test)
8. [Cleaning up Failed / leftover FRSes](#cleaning-up-failed--leftover-frses)
9. [Troubleshooting](#troubleshooting)
10. [Upgrading / changing the image tag](#upgrading--changing-the-image-tag)
11. [Customising the namespace allow-list](#customising-the-namespace-allow-list)

Throughout this guide, items are tagged:

| Tag | Meaning |
| --- | --- |
| 🔴 **Required** | The deployment **will not work** if you skip this. Do it. |
| 🟡 **Strongly recommended** | The deployment will technically run, but you should still do this for a usable, secure install. |
| 🟢 **Optional** | Only needed for non-default behaviour. Skip on a first install; come back if you need to change it later. |

---

## 1. Pre-flight checklist

| # | Step | Tag |
| - | ---- | --- |
| 1 | OCP ≥ 4.11 | 🔴 Required |
| 2 | Kasten K10 with the `filerecoverysessions.datamover.kio.kasten.io` CRD installed (`kubectl api-resources \| grep filerecovery`) | 🔴 Required |
| 3 | A namespace for the helper — default is `kasten-io` (matches K10). If you use a different namespace, you must update `deploy/kustomization.yaml`'s `namespace:` field **and** the `HELPER_PRIVATE_KEY_SECRET_NAMESPACE` env var. | 🔴 Required |
| 4 | Generate the three credentials and put them in a Secret called `kasten-frs-web-helper-credentials` in the helper namespace. Each value must be **≥ 16 bytes**. | 🔴 Required |
| 5 | Pick a release tag from <https://github.com/6547709/kasten-frs-web/pkgs/container/kasten-frs-web> (the latest is recommended). You can pin it after the first apply; for the first install the default in `deploy/kustomization.yaml` is fine. | 🟢 Optional |
| 6 | Decide whether you want the helper to be able to see FRSes cluster-wide or only in a few namespaces. (Default = cluster-wide.) | 🟢 Optional |

### Generating credentials

🔴 **Required.** Run once, store the three values safely:

```bash
PW=$(openssl rand -base64 24)   # HELPER_PASSWORD
US=$(openssl rand -base64 16)   # HELPER_USERNAME (or pick your own)
CS=$(openssl rand -base64 32)   # HELPER_COOKIE_SECRET
```

Save them in your password manager. The helper will reject startup
if any value is shorter than 16 bytes, or if
`HELPER_COOKIE_SECRET` is empty.

### Creating the credentials Secret

🔴 **Required.** Two equivalent options:

**Option A — from the literal values:**

```bash
oc -n kasten-io create secret generic kasten-frs-web-helper-credentials \
  --from-literal=username="$US" \
  --from-literal=password="$PW" \
  --from-literal=cookie-secret="$CS"
```

**Option B — from a YAML file** (use this if you manage secrets
in git/SOPS/SealedSecrets):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kasten-frs-web-helper-credentials
  namespace: kasten-io
type: Opaque
stringData:
  username: "REPLACE_ME_USERNAME"
  password: "REPLACE_ME_PASSWORD"
  cookie-secret: "REPLACE_ME_COOKIE_SECRET_BASE64_32B"
```

The Deployment manifest references this Secret by name; if you
rename it, update `envFrom.secretRef.name` in
`deploy/20-deployment.yaml`.

---

## 2. Apply manifests

🔴 **Required.** From the repository root:

```bash
oc apply -k deploy/
```

This applies, in order:

1. `05-serviceaccount.yaml` — `kasten-frs-web-helper` SA
2. `06-rbac.yaml` — Role + RoleBinding for Secrets, ConfigMaps, Pods, FRSes
3. `20-deployment.yaml` — the helper Deployment (1 replica)
4. `30-service.yaml` — ClusterIP service on port 8080
5. `40-route.yaml` — the externally-reachable edge-TLS Route
6. `50-networkpolicy.yaml` — `kasten-io` egress to the API server
7. `55-networkpolicy-helper-access-frs.yaml` — ingress policy that
   lets the helper dial FRS port 2222 (see §4)

> **RBAC note (first boot):** the helper auto-creates its SSH-key
> Secret on first start (see §3). Kubernetes does not honour
> `resourceNames` for the `create` verb (the object name doesn't
> exist yet at admission), so the `create` grant on Secrets in
> `06-rbac.yaml` is intentionally namespace-scoped and unscoped by
> name. `get/update/patch` remain restricted to the single
> `kasten-frs-helper-private-key` Secret. Do **not** re-add
> `resourceNames` to the `create` rule or the helper will crash-loop
> with `secrets is forbidden: cannot create`.

### What the bundle does NOT do

🟢 **Optional.** The bundle intentionally leaves these out so it
plays nicely with K10's own helm-managed `kasten-io` namespace:

- It does **not** create the `kasten-io` namespace. K10's chart
  owns that. On a brand-new cluster without K10, run
  `oc create namespace kasten-io` first.
- It does **not** create the credentials Secret. See §1.
- It does **not** label the `kasten-io` namespace for
  privileged pod security. See §5.
- It does **not** push the SSH keypair Secret. See §3.

---

## 3. Where the SSH keypair lives

🔴 **Required reading** (no action needed — the helper manages it).

The helper uses an SSH keypair to authenticate to the FRS SFTP
transport. The public half is embedded in every FileRecoverySession
the wizard creates; the private half is needed by the helper to
dial back into the FRS pod.

The keypair is **generated automatically on the helper's first
start** and persisted as a Kubernetes Secret so the same key
survives pod restarts. You do not need to run `ssh-keygen` or
mount a key in.

| Property | Value |
| -------- | ----- |
| Secret name | `kasten-frs-helper-private-key` |
| Secret namespace | `kasten-io` (configurable via `HELPER_PRIVATE_KEY_SECRET_NAMESPACE`) |
| Secret type | `kubernetes.io/ssh-auth` |
| Data field `ssh-privatekey` | PEM-encoded ed25519 private key (no passphrase) |
| Data field `ssh-publickey` | `ssh-ed25519 AAAA…` authorized_keys line |

The defaults live in `internal/config/config.go`:

| Env var | Default | Purpose |
| ------- | ------- | ------- |
| `HELPER_PRIVATE_KEY_SECRET_NAME` | `kasten-frs-helper-private-key` | Override if you need a different name. |
| `HELPER_PRIVATE_KEY_SECRET_NAMESPACE` | `kasten-io` | Override if the helper runs in a non-default namespace. |
| `HELPER_PRIVATE_KEY_SECRET_FIELD` | `ssh-privatekey` | Override the data field name. (Advanced.) |
| `HELPER_USERNAME_FIELD` | `username` | Legacy, unused for SFTP login. |

### What happens at first boot

1. Helper reads Secret `kasten-io/kasten-frs-helper-private-key`.
2. Not found → generates an ed25519 keypair, writes both halves
   to the Secret with `type: kubernetes.io/ssh-auth`, and starts
   serving.
3. Found with both fields → loads them and starts serving.
4. Found with only the public key → **refuses to start** ("refusing
   to operate"). Delete the Secret and let the helper re-generate,
   or restore the private key from backup.
5. Found with only the private key → derives the public key and
   patches the Secret (idempotent).

### Where to find the public key (to register on a non-managed FRS)

If you need to add the helper's public key to a pre-existing FRS
manually (the wizard handles this automatically):

```bash
oc -n kasten-io get secret kasten-frs-helper-private-key \
  -o jsonpath='{.data.ssh-publickey}' | base64 -d
# ssh-ed25519 AAAA… kasten-frs-web-helper
```

### Where to find the private key (rarely needed)

The private key never leaves the helper pod in normal operation.
If you must read it (e.g. for disaster-recovery testing):

```bash
oc -n kasten-io get secret kasten-frs-helper-private-key \
  -o jsonpath='{.data.ssh-privatekey}' | base64 -d
```

**Treat this output as sensitive.** It is the SSH key for every
FRS the helper can talk to.

### Backing up / restoring the keypair

```bash
# Backup
oc -n kasten-io get secret kasten-frs-helper-private-key -o yaml > ssh-key.yaml

# Restore (the helper will use it on next start)
oc -n kasten-io apply -f ssh-key.yaml
```

If you delete the Secret, the helper generates a new keypair on
the next start. **All in-flight FRSes will lose SFTP access** —
the FRSes still exist but the helper can't auth to them. Delete
the stale FRSes and recreate via the wizard.

---

## 4. Required NetworkPolicy for FRS dial

🔴 **Required.** Already in the bundle at
`55-networkpolicy-helper-access-frs.yaml`; this section explains
why it's there.

K10's datamover controller creates a per-FRS `NetworkPolicy` whose
ingress source is the namespace where the app lives (e.g.
`default`). Because the helper pod runs in `kasten-io`, those
policies block the SFTP dial to FRS port 2222 and the browser
hangs with an `i/o timeout` after creating an FRS.

The bundle ships a policy that widens ingress on every K10
generation-1 FRS pod to also accept the helper pod. `oc apply -k
deploy/` applies it. If you deploy without the kustomize bundle,
copy the YAML and apply it explicitly:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kasten-frs-web-helper-allow-all-frs
  namespace: kasten-io
spec:
  podSelector:
    matchLabels:
      k10.kasten.io/frs-generation: "1"
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: kasten-frs-web-helper
    ports:
    - port: 2222
      protocol: TCP
  policyTypes:
  - Ingress
```

**Verification:** after creating a wizard FRS, click through to
the directory tree. If the dial hangs with `i/o timeout` on the
FRS service, this policy is missing — apply it and retry.

---

## 5. OpenShift SecurityContextConstraints

🟡 **Strongly recommended.**

Two different pods are involved; only one needs a privileged SCC.

### 5.1 Helper pod (no SCC needed)

The `kasten-frs-web-helper` Deployment ships with the standard
restricted-v2-friendly securityContext:

- `runAsNonRoot: true`
- `allowPrivilegeEscalation: false`
- `capabilities.drop: [ALL]`
- `readOnlyRootFilesystem: true`
- no hostPath, no privileged container, no special volumes

OCP's default `restricted-v2` SCC accepts this verbatim. The
helper SA `kasten-frs-web-helper` in the `kasten-io` namespace
inherits that SCC automatically through the `system:authenticated`
group, so **no extra SCC grant is needed for the helper**.

### 5.2 FRS mounter pod (K10 datamover — needs privileged)

The K10 datamover mounts FRS data via a privileged container
(`securityContext.privileged=true` + hostPath +
`capabilities.add: SYS_ADMIN`). OCP's `restricted-v2` SCC rejects
this by default. The helper bundle cannot grant SCCs itself, but
you need to make sure the K10 controller can run its FRS mounter
pod.

Two options:

**Option A — Label the `kasten-io` namespace privileged**
(simplest; matches the upstream K10 docs recommendation):

```bash
oc label namespace kasten-io \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/audit=privileged \
    pod-security.kubernetes.io/warn=privileged \
    --overwrite
```

Without this, every wizard / k10tools-created FRS will end up in
`state=Failed` with `CreatedPod: violates PodSecurity
"restricted:latest"` and no SFTP dial will succeed.

**Option B — Grant the privileged SCC to the K10 datamover SA**
in the namespaces where FRSes are created (typically `default`):

```bash
oc adm policy add-scc-to-user privileged -n default -z default
```

Choose Option A unless your security team requires the more
granular Option B.

---

## 6. Post-flight verification

🔴 **Required** before going to production.

```bash
HELPER_POD=$(oc get pod -n kasten-io -l app=kasten-frs-web-helper -o jsonpath='{.items[0].metadata.name}')
oc wait --for=condition=Ready pod/$HELPER_POD -n kasten-io --timeout=60s

# 1. API reachability
oc exec -n kasten-io $HELPER_POD -- nslookup kubernetes.default
oc exec -n kasten-io $HELPER_POD -- curl -sk https://kubernetes.default.svc/api

# 2. FRS dial (replace frs-xxx with an actual FRS service name)
oc exec -n kasten-io $HELPER_POD -- bash -c "timeout 3 bash -c '</dev/tcp/frs-xxx.kasten-io.svc.cluster.local/2222'"

# 3. (or use the convenience script)
./bin/check-netpol.sh frs-xxx kasten-io
```

The `/healthz` and `/readyz` endpoints can be hit directly:

```bash
ROUTE=$(oc get route -n kasten-io -l app=kasten-frs-web-helper -o jsonpath='{.items[0].spec.host}')
curl -sk https://$ROUTE/healthz   # 200
curl -sk https://$ROUTE/readyz    # 200
curl -sk https://$ROUTE/login     # 200, HTML body
```

The login page footer should show `Kasten FRS Web Helper · vX.Y.Z`.
If it shows `v0.0.0` or `dev`, the `VERSION` build-arg was not
injected at build time (see §10).

---

## 7. Wizard smoke test

🔴 **Required** before declaring the deployment done.

After the helper pod is Ready, log in via the Route and navigate
to `/wizard`. You should see at least one VM (assuming K10 has a
`virtualMachine`-labelled RestorePoint). Pick a VM, then a Bound
RP, then any volume, and click **Create FRS** (now anchored to
the right side of the form, matching the left-to-right wizard
flow). You should be redirected to `/browse` showing the FRS
directory tree within 120 seconds (the default
`HELPER_FRS_WAIT_TIMEOUT`).

If the prepare page shows `Failed` or `Timeout` after 120s, see
§9.

---

## 8. Cleaning up Failed / leftover FRSes

🟡 **Strongly recommended** as part of routine maintenance.

The Sessions page lists **every** FRS in the cluster (subject to
the optional `HELPER_FRS_NAMESPACES` allow-list), including ones
in a terminal state. When an FRS times out it transitions to
`Failed` and K10 tears down its `frs-xxx` pod, but the FRS
custom resource lingers. These rows show a disabled
**Unavailable** button and an enabled **Delete** button — click
Delete to remove the leftover CR. The ClusterRole already grants
`delete` on `filerecoverysessions`, so no extra RBAC is needed.
Confirmation now uses an in-app dialog (no browser
`window.confirm` popup).

---

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
| ------- | ------------ | --- |
| Helper pod `CrashLoopBackOff` with `secrets is forbidden: cannot create` | `resourceNames` was re-added to the Secret `create` grant | See RBAC note in §2. |
| Helper pod `CrashLoopBackOff` with `secrets "kasten-frs-helper-private-key" has public key but no private key` | A previous operator put only the public key in the Secret | `oc -n kasten-io delete secret kasten-frs-helper-private-key` and let the helper re-generate. |
| Browser hangs at `i/o timeout` after clicking Create FRS | §4 NetworkPolicy missing | `oc apply -k deploy/` (re-applies) or apply the YAML from §4 directly. |
| Every wizard FRS lands in `Failed` immediately | §5.2 SCC not granted | Apply Option A or B from §5.2. |
| `Wait longer` button not present | The preparing page renders before the timeout fires; this is normal | Wait for timeout, then the button appears. |
| Login page footer shows `dev` | `VERSION` build-arg not propagated | Re-build the image with `--build-arg VERSION=vX.Y.Z`; see §10. |

For anything else, see the design spec (`docs/superpowers/specs/`)
and the implementation plan (`docs/implementation_plan.md`).

---

## 10. Upgrading / changing the image tag

🟢 **Optional** (only when upgrading).

The image tag is pinned centrally in `deploy/kustomization.yaml`
under `images:`. To roll out a specific release:

1. Edit `deploy/kustomization.yaml`:

   ```yaml
   images:
   - name: ghcr.io/6547709/kasten-frs-web
     newTag: v0.3.29        # <-- bump here
     # newName: registry.internal/mirror/kasten-frs-web   # optional mirror
   ```

2. (Optional) Edit `deploy/20-deployment.yaml` so the fallback
   `image:` line matches. This is the value used by a plain
   `oc apply -f deploy/20-deployment.yaml` without kustomize; the
   kustomize path always wins on `oc apply -k deploy/`.

3. Apply: `oc apply -k deploy/`. The Deployment controller
   performs a rolling update; the helper is up the whole time.

The `image:` line in `20-deployment.yaml` is **only** the
fallback for a non-kustomize apply. **Always prefer
`oc apply -k deploy/`.**

### Building your own image (advanced)

🟢 **Optional.**

```bash
docker build --build-arg VERSION=v0.3.29 -t registry.internal/kasten-frs-web:v0.3.29 .
docker push registry.internal/kasten-frs-web:v0.3.29
```

Then either:

- Mirror to `ghcr.io` and re-tag, or
- Set `newName:` in `deploy/kustomization.yaml` to your private
  registry.

---

## 11. Customising the namespace allow-list

🟢 **Optional.**

By default, the helper lists FileRecoverySessions cluster-wide
(minus `kube-system` and friends). To restrict it to a known
set, set `HELPER_FRS_NAMESPACES`:

```bash
oc -n kasten-io set env deploy/kasten-frs-web-helper \
  HELPER_FRS_NAMESPACES=default,kasten-io,vm-apps
```

The Sessions page will only show FRSes from those namespaces.
The list is comma-separated, no spaces. Restart is automatic
(env-var change triggers a rolling update).

---

*Last reviewed: 2026-06-17. If you change a Required step, also
update `DEPLOY_cn.md` to match.*
