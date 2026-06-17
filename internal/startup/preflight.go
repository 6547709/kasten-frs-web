// Package startup runs a preflight diagnostic before the helper
// fully starts. The helper's first real API call is the keymgr
// Secret Get, and if that fails (typically with a TCP-level
// "i/o timeout" because a NetworkPolicy blocks egress, or a 403
// because RBAC is missing) the helper exits before listening on
// :8080, which makes the readiness/liveness probes fail and the
// pod CrashLoop. Pinning the failure mode down used to require
// reading the wrapped error and guessing — preflight emits each
// step at startup so the operator can read the pod log top-down
// and see exactly which check failed.
//
// All checks are non-fatal: a failure is reported with an
// actionable hint, but the function still returns nil so the
// caller can proceed (and the original error from keymgr will
// surface if the underlying problem is real). The intent is
// observability, not policy.
package startup

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Options control which checks the preflight runs and how it
// logs them.
type Options struct {
	// Logger receives one structured event per check. Required.
	Logger *slog.Logger
	// Clientset is the in-cluster clientset. Required for the
	// RBAC and discovery checks; if nil, those checks are
	// skipped with a log line (the earlier checks still run).
	Clientset kubernetes.Interface
	// Namespace is the namespace the helper runs in (and the
	// namespace the keymgr Secret lives in). Required for the
	// RBAC check.
	Namespace string
	// SecretName is the keypair Secret. Logged for context and
	// used by the RBAC check.
	SecretName string
	// ServiceAccountName is the helper's SA; logged for context.
	// Optional — if empty, preflight reads the projected SA
	// token to figure it out.
	ServiceAccountName string
	// DialTimeout caps the TCP reachability check. Defaults to
	// 5s — long enough to cross a slow NAT, short enough to
	// keep the pod log useful.
	DialTimeout time.Duration
	// OverallTimeout caps the preflight block as a whole.
	// Defaults to 30s. The preflight is a best-effort check;
	// if it hangs the helper's overall startup budget is
	// governed by the kubelet, not by us.
	OverallTimeout time.Duration
}

// Run executes each preflight check in order and logs the
// outcome. Returns nil on any individual failure (a failure is
// informational; the caller will see the real error from
// keymgr). Returns a non-nil error only if the preflight itself
// is misconfigured.
func Run(ctx context.Context, opts Options) error {
	if opts.Logger == nil {
		return fmt.Errorf("startup: Logger is required")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 5 * time.Second
	}
	if opts.OverallTimeout == 0 {
		opts.OverallTimeout = 30 * time.Second
	}
	if opts.Namespace == "" {
		opts.Namespace = "kasten-io"
	}
	if opts.SecretName == "" {
		opts.SecretName = "kasten-frs-helper-private-key"
	}
	if opts.ServiceAccountName == "" {
		opts.ServiceAccountName = readSANameFromToken()
	}

	log := opts.Logger.With("phase", "preflight")
	log.Info("preflight.start",
		"helper_namespace", opts.Namespace,
		"secret_name", opts.SecretName,
		"service_account", opts.ServiceAccountName,
	)

	// 1. config: where did the API host come from?
	apiHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	apiPort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if apiHost == "" {
		log.Warn("preflight.config.no_in_cluster_env",
			"hint", "KUBERNETES_SERVICE_HOST is empty — the helper is NOT running in-cluster, or the projected service account token volume was not mounted")
	} else {
		log.Info("preflight.config.api_host",
			"host", apiHost,
			"port", apiPort,
			"hint_if_wrong", "if this IP is wrong, check that the pod was scheduled into the intended namespace and the projected SA token is mounted",
		)
	}

	// 2. DNS: resolve kubernetes.default.svc
	dnsCtx, dnsCancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer dnsCancel()
	addrs, dnsErr := net.DefaultResolver.LookupHost(dnsCtx, "kubernetes.default.svc")
	if dnsErr != nil {
		log.Error("preflight.dns.failed",
			"host", "kubernetes.default.svc",
			"err", dnsErr.Error(),
			"hint", "DNS resolution failed — likely a NetworkPolicy blocking UDP/53 egress to the openshift-dns namespace, or a misconfigured CoreDNS",
		)
	} else {
		log.Info("preflight.dns.ok", "host", "kubernetes.default.svc", "addrs", addrs)
	}

	// 3. TCP reachability: dial the API server (not via the K8s
	//    client, so this isolates a network failure from an
	//    auth/RBAC failure). Use a short timeout so the user
	//    doesn't wait 30s to see a fail.
	tcpCtx, tcpCancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer tcpCancel()
	addr := net.JoinHostPort(apiHost, apiPort)
	if apiHost == "" && len(addrs) > 0 {
		// Fall back to the first resolved address from the DNS check.
		addr = net.JoinHostPort(addrs[0], apiPort)
	}
	if addr == ":" {
		log.Error("preflight.tcp.skipped",
			"hint", "no API host to dial — KUBERNETES_SERVICE_HOST is empty and DNS did not return any addresses",
		)
	} else {
		conn, dialErr := (&net.Dialer{}).DialContext(tcpCtx, "tcp", addr)
		if dialErr != nil {
			log.Error("preflight.tcp.failed",
				"addr", addr,
				"timeout", opts.DialTimeout.String(),
				"err", dialErr.Error(),
				"hint", "TCP dial to the API server timed out — most likely an egress NetworkPolicy in the helper namespace. Apply a policy that allows egress from the helper pod to port 443 on any namespace (see deploy/50-networkpolicy.yaml), or delete any default-deny-all / default-deny-egress policy in kasten-io",
			)
		} else {
			log.Info("preflight.tcp.ok", "addr", addr, "local", conn.LocalAddr().String())
			_ = conn.Close()
		}
	}

	// 4. clientset-backed checks (RBAC, discovery). Skipped if
	//    we don't have a clientset, which can happen in tests.
	if opts.Clientset == nil {
		log.Warn("preflight.clientset.skipped", "hint", "no clientset supplied; RBAC and discovery checks were not run")
		return nil
	}

	// 4a. Discovery: server version. This is a real API call,
	//     so it exercises the full client (DNS + TCP + TLS +
	//     bearer token + authz). If it works, the network and
	//     auth plumbing are sound; if it fails, the error tells
	//     you which layer is broken.
	discCtx, discCancel := context.WithTimeout(ctx, opts.OverallTimeout)
	_ = discCtx
	defer discCancel()
	if v, vErr := opts.Clientset.Discovery().ServerVersion(); vErr != nil {
		log.Error("preflight.discovery.failed",
			"err", vErr.Error(),
			"hint", classifyAPICallError(vErr),
		)
	} else {
		log.Info("preflight.discovery.ok", "kubernetes_version", v.GitVersion)
	}

	// 4b. RBAC self-check: ask the API server "can I get / create
	//     this Secret?". Runs as a SelfSubjectAccessReview, so it
	//     evaluates the actual RBAC chain for the helper's SA.
	for _, verb := range []string{"get", "create"} {
		sarCtx, sarCancel := context.WithTimeout(ctx, opts.OverallTimeout)
		sar, sarErr := opts.Clientset.AuthorizationV1().
			SelfSubjectAccessReviews().
			Create(sarCtx, &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: opts.Namespace,
						Group:     "",
						Resource:  "secrets",
						Name:      opts.SecretName,
						Verb:      verb,
					},
				},
			}, metav1.CreateOptions{})
		sarCancel()
		if sarErr != nil {
			log.Error("preflight.rbac.sar_failed",
				"verb", verb,
				"err", sarErr.Error(),
				"hint", "the SelfSubjectAccessReview call itself failed; the network or auth path is broken before the RBAC check can run",
			)
			continue
		}
		if sar.Status.Allowed {
			log.Info("preflight.rbac.allowed", "verb", verb, "resource", "secrets/"+opts.SecretName)
		} else {
			log.Warn("preflight.rbac.denied",
				"verb", verb,
				"resource", "secrets/"+opts.SecretName,
				"reason", string(sar.Status.Reason),
				"hint", "RBAC does not grant this verb to the helper SA. Re-apply deploy/06-rbac.yaml. (create cannot be restricted by resourceNames; this is by design — see DEPLOY.md §2.)",
			)
		}
	}

	log.Info("preflight.done")
	return nil
}

// classifyAPICallError turns a discovery-call error into a one-line
// hint. The Go net package and K8s apierrors package both have
// fairly specific error messages; we look for the most common
// patterns and emit an actionable hint per case.
func classifyAPICallError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "i/o timeout"):
		return "TCP-level timeout — egress NetworkPolicy is the most likely cause; see preflight.tcp.failed hint"
	case strings.Contains(s, "connection refused"):
		return "TCP refused — the API server is not listening on the expected port, or a sidecar is intercepting"
	case strings.Contains(s, "forbidden") || strings.Contains(s, "Forbidden"):
		return "RBAC denied the call; the SA's RBAC binding is missing or mis-scoped"
	case strings.Contains(s, "Unauthorized") || strings.Contains(s, "401"):
		return "auth failure — the bearer token (or its projected-token file) was rejected; check that the SA token volume is mounted"
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate"):
		return "TLS error — the cluster CA bundle is missing, rotated, or the API server presents an unexpected cert"
	case strings.Contains(s, "no such host"):
		return "DNS failure — kubernetes.default.svc did not resolve; check CoreDNS and the pod's /etc/resolv.conf"
	default:
		return "unclassified — copy the full err field into a search to triage"
	}
}

// readSANameFromToken peeks at the projected SA token's
// kubernetes.io/serviceaccount.name claim. This is best-effort:
// if the file is missing or the claim isn't there we return
// "unknown" and let the user figure out the SA from the cluster.
func readSANameFromToken() string {
	const tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "unknown"
	}
	// JWT format: header.payload.signature — base64url-encoded
	parts := strings.SplitN(string(data), ".", 3)
	if len(parts) < 2 {
		return "unknown"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "unknown"
	}
	const key = `"kubernetes.io/serviceaccount.name":"`
	if i := strings.Index(string(payload), key); i >= 0 {
		rest := string(payload)[i+len(key):]
		if j := strings.IndexByte(rest, '"'); j > 0 {
			return rest[:j]
		}
	}
	return "unknown"
}
