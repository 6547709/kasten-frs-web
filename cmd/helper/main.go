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
	"github.com/liguoqiang/kasten-frs-web/internal/logging"
	"github.com/liguoqiang/kasten-frs-web/internal/metrics"
	"github.com/liguoqiang/kasten-frs-web/internal/server"
	"github.com/liguoqiang/kasten-frs-web/internal/sftpclient"
	"golang.org/x/crypto/ssh"
)

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

	// Load private key once at startup
	creds, err := kc.LoadPrivateKey(context.Background(), k8s.CredentialsConfig{
		Namespace: cfg.PrivateKeySecretNamespace,
		Name:      cfg.PrivateKeySecretName,
		Field:     cfg.PrivateKeyField,
	})
	if err != nil {
		return fmt.Errorf("load private key: %w", err)
	}
	signer, err := parseSigner(creds.PrivateKey)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	sftpClient, err := sftpclient.NewClient(sftpclient.ClientConfig{
		Username:       creds.Username,
		Signer:         signer,
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

	hs := handlers.New(sessions, pool, kc, creds.Username, cfg.FRSPort, cfg.FRSNamespaceWhitelist)
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

	logger.Info("helper starting", "addr", l.Addr().String())
	return server.Run(ctx, srv, l)
}

func parseSigner(pem []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return signer, nil
}
