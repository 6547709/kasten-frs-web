package sftpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
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
	timeout  time.Duration
}

// NewClient validates config. HostKeySig is accepted (and validated if
// provided) for early failure detection, but Dial does not use it; the
// per-FRS host key is passed to Dial directly.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Signer == nil {
		return nil, errors.New("signer required")
	}
	if cfg.Username == "" {
		return nil, errors.New("username required")
	}
	if cfg.HostKeySig != "" {
		if _, err := ParseHostKeySignature(cfg.HostKeySig); err != nil {
			return nil, fmt.Errorf("parse host key: %w", err)
		}
	}
	return &Client{
		username: cfg.Username,
		signer:   cfg.Signer,
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
	if s == nil {
		return nil
	}
	if s.sftp != nil {
		_ = s.sftp.Close()
	}
	if s.ssh != nil {
		return s.ssh.Close()
	}
	return nil
}

// ListDir lists entries at path.
//
// On Windows FRS restores (NTFS / ReFS volumes exposed via the
// K10 datamover's SFTP server), the kernel presents Windows
// directory junctions (reparse points) — Documents and Settings,
// All Users, Default User, etc. — as symlinks. pkg/sftp's Lstat
// path reflects that, so each junction comes back as
// os.ModeSymlink rather than os.ModeDir, and a naive IsDir()
// check shows the user "this is a file" — they can't browse
// into the linked directory.
//
// The fix: after ReadDir, do a follow-stat for any entry whose
// mode includes os.ModeSymlink. If the target is a directory,
// wrap the FileInfo so IsDir() reports true (and the Mode
// bit matches). The rest of the stack — Open(), the
// download-zip walker, the path validation — all use the
// wrapped FileInfo consistently, so the rest of the user
// flow (clicking into the junction, downloading from it) just
// works.
//
// Cost: one extra SFTP round trip per junction. On a typical
// FRS-restore top level (a few junctions, mostly real dirs),
// that's < 100ms total — well under the user's perception
// threshold. For deeply-nested FRS restores, the cost is
// bounded by the number of junctions in each listed dir, not
// the total entry count.
func (s *Session) ListDir(path string) ([]os.FileInfo, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	infos, err := s.sftp.ReadDir(path)
	if err != nil {
		slog.Warn("sftp.listdir.failed", "path", path, "err", err)
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(infos))
	for _, info := range infos {
		// Most entries are regular files or directories; the
		// symlink-follow is a no-op for those. Skip the
		// Stat() call unless the mode says it's a symlink —
		// saves a round trip per entry in the common case.
		if info.Mode()&os.ModeSymlink == 0 {
			out = append(out, info)
			continue
		}
		// Follow the symlink (Stat, not Lstat) so we can see
		// the target's real type. Errors here mean the link is
		// broken or otherwise unreadable — fall back to the
		// original symlink FileInfo so the user at least sees
		// the entry.
		targetInfo, statErr := s.sftp.Stat(filepath.Join(path, info.Name()))
		if statErr != nil {
			slog.Warn("sftp.listdir.symlink_stat_failed",
				"path", path, "name", info.Name(), "err", statErr)
			out = append(out, info)
			continue
		}
		if targetInfo.IsDir() {
			out = append(out, &fileInfoWithDir{FileInfo: info, isDir: true})
			slog.Debug("sftp.listdir.symlink_resolved_to_dir",
				"path", path, "name", info.Name())
		} else {
			out = append(out, info)
		}
	}
	slog.Info("sftp.listdir", "path", path, "count", len(out))
	return out, nil
}

// fileInfoWithDir wraps an os.FileInfo and overrides IsDir() /
// Mode() so an entry that the SFTP server reported as a symlink
// can be presented as a directory to the rest of the stack.
// Embedding the original FileInfo means every other method
// (Name, Size, ModTime, Sys, etc.) is forwarded unchanged.
type fileInfoWithDir struct {
	os.FileInfo
	isDir bool
}

func (f *fileInfoWithDir) IsDir() bool { return f.isDir }

// Mode returns the underlying mode with the directory bit
// set or cleared to match f.isDir. Some downstream code
// (e.g. tar.FileInfoHeader, path validation) reads Mode()
// directly instead of going through IsDir(); keeping the
// bit in sync means those code paths see the same answer.
func (f *fileInfoWithDir) Mode() os.FileMode {
	m := f.FileInfo.Mode()
	if f.isDir {
		return m | os.ModeDir
	}
	return m &^ os.ModeDir
}

// Open returns a ReadCloser for a file at path.
func (s *Session) Open(path string) (io.ReadCloser, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	f, err := s.sftp.Open(path)
	if err != nil {
		slog.Warn("sftp.open.failed", "path", path, "err", err)
		return nil, err
	}
	slog.Info("sftp.open", "path", path)
	return f, nil
}

// Stat returns info for a path.
func (s *Session) Stat(path string) (os.FileInfo, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	return s.sftp.Stat(path)
}

// Dial establishes a new SFTP session to the given tcp address. The host
// key signature is supplied per-call so a single Client can serve dials to
// FRSs with different host keys without locking.
func (c *Client) Dial(ctx context.Context, addr, hostKeySig string) (*Session, error) {
	// Log the dial target with a truncated key fingerprint so
	// operators can correlate "i/o timeout" with the FRS+key
	// pair in the cluster without leaking the full secret.
	slog.Info("sftp.dial.start",
		"user", c.username, "addr", addr,
		"host_key_sig", shortHostKeySig(hostKeySig),
	)
	hostKey, err := ParseHostKeySignature(hostKeySig)
	if err != nil {
		slog.Warn("sftp.dial.parse-host-key",
			"addr", addr,
			"host_key_sig", shortHostKeySig(hostKeySig),
			"err", err,
		)
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            c.username,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(c.signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         c.timeout,
	}
	sshConn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		slog.Warn("sftp.dial.failed",
			"addr", addr,
			"host_key_sig", shortHostKeySig(hostKeySig),
			"err", err,
		)
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	sc, err := sftp.NewClient(sshConn)
	if err != nil {
		_ = sshConn.Close()
		slog.Warn("sftp.client.failed", "addr", addr, "err", err)
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	slog.Info("sftp.dial.ready", "addr", addr, "user", c.username)
	return &Session{sftp: sc, ssh: sshConn}, nil
}

// shortHostKeySig returns the host:port prefix of a Kasten-style
// signature for log correlation. We deliberately don't log the full
// signature so that logs don't leak long-lived key material.
func shortHostKeySig(sig string) string {
	if sig == "" {
		return ""
	}
	// Format is "[host:port] alg base64"; take the bracketed host
	// spec verbatim.
	end := strings.Index(sig, "]")
	if end < 0 || end+1 >= len(sig) {
		if len(sig) > 64 {
			return sig[:64] + "…"
		}
		return sig
	}
	return sig[:end+1]
}

// hostKeySigRe parses "[host]:port alg base64..." (the format Kasten
// uses for non-default ports like :2222) or "[host:port] alg base64..."
// (the format ssh-keygen -l uses on output; appears in test fixtures).
// Both forms are accepted because real FRS CRs emit the former and
// unit tests hand-roll the latter.
var hostKeySigRe = regexp.MustCompile(`^\[([^\]]+)\](?::(\d+))?\s+(\S+)\s+(\S+)\s*$`)

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
	if m[2] != "" {
		// "[host]:port" form: append port for net.SplitHostPort.
		hostPort = hostPort + ":" + m[2]
	}
	_, portStr, splitErr := netSplitHostPort(hostPort)
	if splitErr != nil {
		return nil, fmt.Errorf("malformed host:port %q: %w", hostPort, splitErr)
	}
	_ = portStr
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(m[3] + " " + m[4]))
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

// validatePath rejects any path that is not a clean absolute path
// rooted at "/". It explicitly rejects any ".." path SEGMENT (rather
// than the old strings.Contains(p, "..") substring check, which both
// rejected legitimate names like "/data/file..name" and could be
// fooled). Rejecting traversal segments outright — instead of relying
// on path.Clean to silently resolve them — is the safer choice: we
// never want to second-guess what a "/a/../../etc" the caller
// "really meant"; we refuse it.
func validatePath(p string) error {
	if p == "" {
		return errors.New("invalid path: empty")
	}
	// Must be absolute: the FRS SFTP root is "/", and all browse/
	// download handlers build absolute paths.
	if !strings.HasPrefix(p, "/") {
		return errors.New("invalid path: must be absolute")
	}
	// Reject any ".." that appears as a whole segment. Splitting on
	// "/" means "file..name" (a name that merely contains dots) is
	// allowed, while "/a/../b" (a traversal segment) is refused.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New("invalid path: contains parent-directory segment")
		}
	}
	// Defence in depth: the normalised form must still be absolute.
	if !strings.HasPrefix(path.Clean(p), "/") {
		return errors.New("invalid path: not rooted after normalisation")
	}
	return nil
}
