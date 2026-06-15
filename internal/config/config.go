// Package config loads configuration from environment variables and CLI flags.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the helper.
type Config struct {
	Listen                    string
	Port                      int
	Username                  string
	Password                  string
	CookieSecret              []byte
	SessionCookieName         string
	SessionTTL                time.Duration
	SFTPPoolTTL               time.Duration
	SFTPConnectTimeout        time.Duration
	FRSPort                   int
	FRSNamespaceWhitelist     []string
	FRSDefaultUsername        string
	PrivateKeySecretName      string
	PrivateKeySecretNamespace string
	PrivateKeyField           string
	UsernameField             string // legacy, v0.6 unused for SFTP login
	K8sInCluster              bool
	LogLevel                  string
}

// Load reads configuration from environment variables. Returns an error if
// any required value is missing or invalid.
func Load() (Config, error) {
	c := Config{
		Listen:                    getenv("HELPER_LISTEN", "0.0.0.0"),
		SessionCookieName:         getenv("HELPER_SESSION_COOKIE", "kfrs_sid"),
		PrivateKeySecretName:      getenv("HELPER_PRIVATE_KEY_SECRET_NAME", "kasten-frs-helper-private-key"),
		PrivateKeySecretNamespace: getenv("HELPER_PRIVATE_KEY_SECRET_NAMESPACE", "kasten-io"),
		PrivateKeyField:           getenv("HELPER_PRIVATE_KEY_SECRET_FIELD", "ssh-privatekey"),
		UsernameField:             getenv("HELPER_USERNAME_FIELD", "username"),
		FRSDefaultUsername:        getenv("HELPER_FRS_DEFAULT_USERNAME", "root"),
		LogLevel:                  getenv("HELPER_LOG_LEVEL", "info"),
	}

	c.Username = os.Getenv("HELPER_USERNAME")
	c.Password = os.Getenv("HELPER_PASSWORD")

	cookieSecret := os.Getenv("HELPER_COOKIE_SECRET")
	c.CookieSecret = []byte(cookieSecret)

	port, err := strconv.Atoi(getenv("HELPER_PORT", "8080"))
	if err != nil {
		return Config{}, fmt.Errorf("HELPER_PORT: %w", err)
	}
	c.Port = port

	sessionTTL, err := time.ParseDuration(getenv("HELPER_SESSION_TTL", "8h"))
	if err != nil {
		return Config{}, fmt.Errorf("HELPER_SESSION_TTL: %w", err)
	}
	c.SessionTTL = sessionTTL

	poolTTL, err := time.ParseDuration(getenv("HELPER_SFTP_TTL", "30m"))
	if err != nil {
		return Config{}, fmt.Errorf("HELPER_SFTP_TTL: %w", err)
	}
	c.SFTPPoolTTL = poolTTL

	connectTimeout, err := time.ParseDuration(getenv("HELPER_SFTP_TIMEOUT", "10s"))
	if err != nil {
		return Config{}, fmt.Errorf("HELPER_SFTP_TIMEOUT: %w", err)
	}
	c.SFTPConnectTimeout = connectTimeout

	frsPort, err := strconv.Atoi(getenv("HELPER_FRS_PORT", "2222"))
	if err != nil {
		return Config{}, fmt.Errorf("HELPER_FRS_PORT: %w", err)
	}
	c.FRSPort = frsPort

	if ns := strings.TrimSpace(getenv("HELPER_FRS_NAMESPACES", "")); ns != "" {
		c.FRSNamespaceWhitelist = strings.Split(ns, ",")
	}

	switch getenv("HELPER_K8S_INCLUSTER", "true") {
	case "true", "1", "yes":
		c.K8sInCluster = true
	default:
		c.K8sInCluster = false
	}

	if c.Username == "" {
		return Config{}, fmt.Errorf("HELPER_USERNAME is required")
	}
	if c.Password == "" {
		return Config{}, fmt.Errorf("HELPER_PASSWORD is required")
	}
	if len(c.CookieSecret) < 16 {
		return Config{}, fmt.Errorf("HELPER_COOKIE_SECRET must be at least 16 bytes")
	}

	return c, nil
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
