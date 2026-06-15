package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_MissingRequired(t *testing.T) {
	t.Setenv("HELPER_USERNAME", "")
	t.Setenv("HELPER_PASSWORD", "")
	t.Setenv("HELPER_COOKIE_SECRET", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when required envs missing, got nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("HELPER_USERNAME", "admin")
	t.Setenv("HELPER_PASSWORD", "secret123")
	t.Setenv("HELPER_COOKIE_SECRET", "this-is-a-32byte-cookie-secret-value")
	t.Setenv("HELPER_PORT", "")
	t.Setenv("HELPER_SESSION_TTL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.SessionTTL != 8*time.Hour {
		t.Errorf("SessionTTL = %v, want 8h", cfg.SessionTTL)
	}
	if cfg.FRSDefaultUsername != "root" {
		t.Errorf("FRSDefaultUsername = %q, want root", cfg.FRSDefaultUsername)
	}
	if cfg.PrivateKeySecretName != "kasten-frs-helper-private-key" {
		t.Errorf("PrivateKeySecretName = %q, want kasten-frs-helper-private-key",
			cfg.PrivateKeySecretName)
	}
	if cfg.PrivateKeySecretNamespace != "kasten-io" {
		t.Errorf("PrivateKeySecretNamespace = %q, want kasten-io",
			cfg.PrivateKeySecretNamespace)
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("HELPER_USERNAME", "admin")
	t.Setenv("HELPER_PASSWORD", "secret123")
	t.Setenv("HELPER_COOKIE_SECRET", "sixteen-byte-secret")
	t.Setenv("HELPER_PORT", "9090")
	t.Setenv("HELPER_LISTEN", "127.0.0.1")
	t.Setenv("HELPER_SESSION_TTL", "1h")
	t.Setenv("HELPER_FRS_DEFAULT_USERNAME", "svc")
	t.Setenv("HELPER_FRS_PORT", "2222")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.Listen != "127.0.0.1" {
		t.Errorf("Listen = %q, want 127.0.0.1", cfg.Listen)
	}
	if cfg.SessionTTL != time.Hour {
		t.Errorf("SessionTTL = %v, want 1h", cfg.SessionTTL)
	}
	if cfg.FRSDefaultUsername != "svc" {
		t.Errorf("FRSDefaultUsername = %q, want svc", cfg.FRSDefaultUsername)
	}
	if cfg.FRSPort != 2222 {
		t.Errorf("FRSPort = %d, want 2222", cfg.FRSPort)
	}
}

func TestLoad_ShortCookieSecret(t *testing.T) {
	t.Setenv("HELPER_USERNAME", "admin")
	t.Setenv("HELPER_PASSWORD", "secret123")
	t.Setenv("HELPER_COOKIE_SECRET", "short") // 5 bytes, below 16-byte minimum
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for short cookie secret")
	}
	if !strings.Contains(err.Error(), "HELPER_COOKIE_SECRET") {
		t.Errorf("error should name HELPER_COOKIE_SECRET, got: %v", err)
	}
}
