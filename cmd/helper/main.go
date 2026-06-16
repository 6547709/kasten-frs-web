// Command helper is the entry point for the Kasten FRS Web Helper.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/liguoqiang/kasten-frs-web/internal/auth"
	"github.com/liguoqiang/kasten-frs-web/internal/config"
	"github.com/liguoqiang/kasten-frs-web/internal/handlers"
	"github.com/liguoqiang/kasten-frs-web/internal/k8s"
	"github.com/liguoqiang/kasten-frs-web/internal/keymgr"
	"github.com/liguoqiang/kasten-frs-web/internal/logging"
	"github.com/liguoqiang/kasten-frs-web/internal/metrics"
	"github.com/liguoqiang/kasten-frs-web/internal/server"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
)

// version is the build version surfaced in the UI footer. Three
// precedence levels:
//   1. ldflags -X main.version=... (set at image build time by CI)
//   2. VERSION env var (lets operators override at pod deploy time)
//   3. "dev" (fallback for `go run` / local builds)
var version = func() string {
	if v := os.Getenv("VERSION"); v != "" {
		return v
	}
	return "dev"
}()

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger := logging.New(os.Stderr, cfg.LogLevel)
	slog.SetDefault(logger)

	kc, err := k8s.NewClient(k8s.ClientOptions{InCluster: cfg.K8sInCluster})
	if err != nil {
		return fmt.Errorf("k8s client: %w", err)
	}

	// Load (or generate) the SSH keypair used for FRS SFTP auth.
	km, err := keymgr.LoadOrGenerate(context.Background(), kc.Core(), cfg.PrivateKeySecretNamespace, cfg.PrivateKeySecretName)
	if err != nil {
		return fmt.Errorf("load/generate SSH key: %w", err)
	}

	sftpClient, err := sftpclient.NewClient(sftpclient.ClientConfig{
		Username:       cfg.FRSDefaultUsername,
		Signer:         km.Signer,
		ConnectTimeout: cfg.SFTPConnectTimeout,
		// HostKeySig is per-FRS; supplied at Dial time in handleConnect.
	})
	if err != nil {
		return fmt.Errorf("sftp client: %w", err)
	}
	pool := sftpclient.NewPool(sftpClient, cfg.SFTPPoolTTL)

	sessions := auth.NewAuthenticator(cfg.Username, cfg.Password,
		auth.NewSessionStore(cfg.CookieSecret, cfg.SessionTTL), "kfrs_sid")
	registry := metrics.NewRegistry()

	hs := handlers.New(sessions, pool, kc, cfg.FRSDefaultUsername, string(km.PubKeyPEM), cfg.FRSPort, cfg.FRSNamespaceWhitelist, cfg.FRSWaitTimeout, version)
	mux := http.NewServeMux()
	mux.Handle("/metrics", registry.Handler())
	mux.Handle("/", server.SecurityHeaders(server.Recoverer(hs.Router())))

	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Listen, cfg.Port))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("helper starting", "addr", l.Addr().String(), "version", version)
	return server.Run(ctx, srv, l)
}
