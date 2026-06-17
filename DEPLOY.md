# Deployment Guide

## Pre-flight

1. OCP >= 4.11
2. Kasten K10 with `filerecoverysessions.datamover.kio.kasten.io` CRD installed
3. Generate Helper credentials and cookie secret (each >= 16 bytes):
   ```bash
   PW=$(openssl rand -base64 24)
   CS=$(openssl rand -base64 32)
   ```
4. Create `kasten-frs-web-helper-credentials` Secret with the three values
   (`HELPER_USERNAME`, `HELPER_PASSWORD`, `HELPER_COOKIE_SECRET`).

The helper will auto-generate and persist the SSH keypair on first start.
The public key is embedded in every FRS the wizard creates; the private
key never leaves the helper pod.

## Apply manifests

```bash
oc apply -k deploy/
```

## Required NetworkPolicy for FRS dial

The K10 datamover controller creates a per-FRS `NetworkPolicy` whose
ingress source is the namespace where the app lives (e.g. `default`).
Because the helper pod runs in `kasten-io`, those policies block the
SFTP dial to FRS port 2222 and the browser hangs with an
`i/o timeout` after creating an FRS.

The bundle ships `deploy/55-networkpolicy-helper-access-frs.yaml`
which widens ingress on every K10 generation-1 FRS pod to also
accept the helper pod. `oc apply -k deploy/` applies it. If you
deploy without the kustomize bundle, copy the YAML and apply it
explicitly:

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

Verification: after creating a wizard FRS, click through to the
directory tree. If the dial hangs with `i/o timeout` on the FRS
service, this policy is missing — apply it and retry.

## OpenShift SecurityContextConstraints

The K10 datamover mounts FRS data via a privileged container
(securityContext.privileged=true + hostPath + capabilities.add
SYS_ADMIN). OCP's restricted-v2 SCC rejects this by default. The
helper bundle cannot grant SCCs itself, but you need to make
sure the K10 controller can run its FRS mounter pod.

Two options:

1. **Label the kasten-io namespace privileged** (simplest; matches
   the upstream K10 docs recommendation):

   ```bash
   oc label namespace kasten-io \
       pod-security.kubernetes.io/enforce=privileged \
       pod-security.kubernetes.io/audit=privileged \
       pod-security.kubernetes.io/warn=privileged \
       --overwrite
   ```

   Without this, every wizard / k10tools-created FRS will end up
   in `state=Failed` with `CreatedPod: violates PodSecurity
   "restricted:latest"` and no SFTP dial will succeed.

2. **Or grant the privileged SCC to the K10 datamover SA** in the
   namespaces where FRSes are created (typically `default`):

   ```bash
   oc adm policy add-scc-to-user privileged -n default -z default
   ```

   Choose option 1 unless your security team requires the more
   granular option 2.

The `scripts/deploy-test.sh` step5_netpol already verifies
NetworkPolicy connectivity; the SCC check above is documented
here only — apply it during pre-flight.

## Post-flight verification

```bash
HELPER_POD=$(oc get pod -n kasten-io -l app=kasten-frs-web-helper -o jsonpath='{.items[0].metadata.name}')
oc wait --for=condition=Ready pod/$HELPER_POD -n kasten-io --timeout=60s

# NetworkPolicy checks
oc exec -n kasten-io $HELPER_POD -- nslookup kubernetes.default
oc exec -n kasten-io $HELPER_POD -- curl -sk https://kubernetes.default.svc/api
# (replace frs-xxx with an actual FRS service name)
oc exec -n kasten-io $HELPER_POD -- bash -c "timeout 3 bash -c '</dev/tcp/frs-xxx.kasten-io.svc.cluster.local/2222'"

# Or run the convenience script:
./bin/check-netpol.sh frs-xxx kasten-io
```

## Troubleshooting

See section 19 of the design spec.

## Wizard smoke

After the helper pod is Ready, log in via the Route and navigate to
`/wizard`. You should see at least one VM (assuming K10 has a
`virtualMachine`-labelled RestorePoint). Pick a VM, then a Bound RP,
then any volume, and click **Create FRS**. You should be redirected
to `/browse` showing the FRS directory tree within 30 seconds.