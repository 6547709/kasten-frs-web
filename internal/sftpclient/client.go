package sftpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ClientConfig configures an SFTP Client.
type ClientConfig struct {
	Username       string
	Signer         ssh.Signer
	HostKeySig     string // FRS status.transports.sftp.hostKeySignature
	ConnectTimeout time.Duration
}

// Client dials and parses SFTP sessions.
type Client struct {
	username string
	signer   ssh.Signer
	hostKey  ssh.PublicKey
	timeout  time.Duration
}

// NewClient parses the host key signature and validates config.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Signer == nil {
		return nil, errors.New("signer required")
	}
	hostKey, err := ParseHostKeySignature(cfg.HostKeySig)
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	if cfg.Username == "" {
		return nil, errors.New("username required")
	}
	return &Client{
		username: cfg.Username,
		signer:   cfg.Signer,
		hostKey:  hostKey,
		timeout:  cfg.ConnectTimeout,
	}, nil
}

// Session is a live SFTP session.
type Session struct {
	sftp *sftp.Client
	ssh  *ssh.Client
}

// Close terminates the SFTP and underlying SSH connection.
func (s *Session) Close() error {
	_ = s.sftp.Close()
	return s.ssh.Close()
}

// ListDir lists entries at path.
func (s *Session) ListDir(path string) ([]os.FileInfo, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	return s.sftp.ReadDir(path)
}

// Open returns a ReadCloser for a file at path.
func (s *Session) Open(path string) (io.ReadCloser, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	f, err := s.sftp.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Stat returns info for a path.
func (s *Session) Stat(path string) (os.FileInfo, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	return s.sftp.Stat(path)
}

// Dial establishes a new SFTP session to the given tcp address.
func (c *Client) Dial(ctx context.Context, addr string) (*Session, error) {
	cfg := &ssh.ClientConfig{
		User:            c.username,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(c.signer)},
		HostKeyCallback: ssh.FixedHostKey(c.hostKey),
		Timeout:         c.timeout,
	}
	sshConn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	sc, err := sftp.NewClient(sshConn)
	if err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	return &Session{sftp: sc, ssh: sshConn}, nil
}

// hostKeySigRe parses "[host:port] alg base64..." into host/port/key.
var hostKeySigRe = regexp.MustCompile(`^\[([^\]]+)\]\s+(\S+)\s+(\S+)\s*$`)

// ParseHostKeySignature parses a Kasten-style host key signature.
func ParseHostKeySignature(sig string) (ssh.PublicKey, error) {
	if sig == "" {
		return nil, errors.New("host key signature empty")
	}
	m := hostKeySigRe.FindStringSubmatch(strings.TrimSpace(sig))
	if m == nil {
		return nil, fmt.Errorf("malformed host key signature: %q", sig)
	}
	hostPort := m[1]
	_, portStr, splitErr := netSplitHostPort(hostPort)
	if splitErr != nil {
		return nil, fmt.Errorf("malformed host:port %q: %w", hostPort, splitErr)
	}
	_ = portStr
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(m[2] + " " + m[3]))
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	return key, nil
}

// netSplitHostPort is a tiny stdlib-free helper. Go 1.22 has net.SplitHostPort;
// we wrap it to keep imports tidy.
func netSplitHostPort(s string) (string, string, error) {
	// find last colon
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", &netAddrError{s: s}
	}
	host := s[:i]
	port := s[i+1:]
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", &netAddrError{s: s}
	}
	return host, port, nil
}

type netAddrError struct{ s string }

func (e *netAddrError) Error() string { return "invalid address: " + e.s }

func validatePath(p string) error {
	if strings.Contains(p, "..") {
		return errors.New("invalid path")
	}
	return nil
}
