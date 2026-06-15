# Deployment Guide

## Pre-flight

1. OCP >= 4.11
2. Kasten K10 with `filerecoverysessions.datamover.kio.kasten.io` CRD installed
3. Generate a single keypair that will be used for all FRS:

   ```bash
   ssh-keygen -t ed25519 -f k10-frs.key -N "" -C "k10-frs"
   ```

4. Create the private-key Secret in the `kasten-io` namespace:

   ```bash
   oc create secret generic kasten-frs-helper-private-key \
       --namespace=kasten-io \
       --type=kubernetes.io/ssh-auth \
       --from-file=ssh-privatekey=k10-frs.key
   ```

5. Generate Helper credentials and cookie secret (each >= 16 bytes):

   ```bash
   PW=$(openssl rand -base64 24)
   CS=$(openssl rand -base64 32)
   ```

6. Create `kasten-frs-web-helper-credentials` Secret with the three values
   (`HELPER_USERNAME`, `HELPER_PASSWORD`, `HELPER_COOKIE_SECRET`).

## Apply manifests

```bash
oc apply -k deploy/
```

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