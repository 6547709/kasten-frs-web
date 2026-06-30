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
	sftp        *sftp.Client
	ssh         *ssh.Client
	chrootDepth int // # of leading components of an absolute symlink
	//            target that are the SFTP chroot prefix (i.e.
	//            invisible from this client). 0 = not yet
	//            discovered; lazily learned from the first
	//            absolute-target ReadLink on this session and
	//            reused for every subsequent junction. See
	//            resolveAbsoluteTarget.
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
// History of attempts:
//
//   - v0.3.40: SSH_FXP_STAT on the junction path. K10 datamover
//     returns "file does not exist" — server's STAT doesn't
//     follow symlinks.
//   - v0.3.41: SSH_FXP_OPENDIR via sftp.ReadDir(joined). Same
//     not-exist error — datamover's OPENDIR also doesn't follow
//     reparse points. The mount layer underneath the SFTP server
//     doesn't translate NTFS junctions at all.
//
// Current strategy (v0.3.46): probe with two methods, in order:
//
//  1. OPENDIR on the joined path. Works for any SFTP server that
//     follows symlinks at OPENDIR time (the standard behaviour).
//  2. SSH_FXP_READLINK + chroot-strip. K10 datamover returns
//     absolute targets with an internal mount prefix the SFTP
//     client can't see (e.g. "/mnt/export/<job>/<vol>/Users/
//     Public/Documents"). Strip the leading N components
//     (N = the chroot depth) and the result is the SFTP-
//     relative path. N is unknown up-front, so we discover it
//     lazily: try N = 1, 2, 3, ... up to 5; the first one that
//     yields a valid ReadDir is the answer, cached on the
//     session for all subsequent junctions. This handles every
//     Windows junction shape on a real FRS restore — including
//     the ones the v0.3.45 ancestor walk got wrong (Application
//     Data, Templates, 「开始」菜单, 桌面, etc.). Falls back to
//     the v0.3.45 ancestor walk if no depth works or the target
//     is relative (rare).
//
// On any successful probe, we wrap the FileInfo with
// fileInfoWithDir so IsDir() reports true and the Mode() bit
// matches. If probe 2 succeeded, we also record the resolved
// absolute path so the rest of the stack (the browse template's
// click URL, the download handler, the zip walker) navigates
// to the TARGET instead of the link itself — the link's path
// is unreachable on this server, but the target's path is.
//
// On total failure, fall back to the original symlink FileInfo
// so the user at least sees the entry; clicking will return a
// clear 404 from Open() rather than the helper silently hiding
// the row.
//
// Cost: 1-2 extra SFTP round trips per junction. Worst case
// (junction target unreachable AND no map hit) is 3 probes
// per junction. On a typical FRS-restore top level (a few
// junctions, mostly real dirs), that's still a few hundred ms
// total — well under the user's perception threshold.
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
		// symlink probes are a no-op for those. Skip unless
		// the mode says it's a symlink — saves a round trip
		// per entry in the common case.
		if info.Mode()&os.ModeSymlink == 0 {
			out = append(out, info)
			continue
		}
		joined := filepath.Join(path, info.Name())

		// Probe 1: OPENDIR on the joined path.
		if _, err := s.sftp.ReadDir(joined); err == nil {
			out = append(out, &fileInfoWithDir{FileInfo: info, isDir: true})
			slog.Info("sftp.listdir.symlink_resolved_via_opendir",
				"path", path, "name", info.Name())
			continue
		}

		// Probe 2: READLINK + chroot-strip + ancestor-walk fallback.
		//
		// ReadLink returns the symlink's textual target. K10
		// datamover serves absolute targets that include the SFTP
		// chroot prefix (e.g. "/mnt/export/<job>/<vol>/Users/Public/
		// Documents" for a ProgramData/Documents junction). The
		// SFTP client can't see past the chroot, so the absolute
		// path is unreachable as-is — but stripping the chroot
		// prefix gives the SFTP-relative path, which IS reachable.
		//
		// We don't know the chroot prefix up-front (it's an
		// internal K10 datamover mount path that varies across
		// installs), so we discover it lazily: for the first
		// absolute target on a given session, try stripping 1, 2,
		// 3, ... leading path components and see which one
		// produces a valid SFTP-relative path on the first
		// ReadDir. The depth that succeeds is the chroot prefix's
		// component count; cache it on the session and reuse for
		// every subsequent junction on this connection. Subsequent
		// junctions cost one ReadDir round trip.
		//
		// This handles every Windows junction shape we observed
		// on the live cluster — including ones the v0.3.45
		// ancestor walk got wrong (Application Data, Templates,
		// 「开始」菜单, 桌面, etc.). The ancestor walk is kept as
		// a fallback for the cases chroot-stripping can't handle
		// (relative targets, target shapes that match no
		// reasonable chroot depth).
		if target, rlErr := s.sftp.ReadLink(joined); rlErr == nil && target != "" {
			if filepath.IsAbs(target) {
				if resolved := s.resolveAbsoluteTarget(target); resolved != "" {
					out = append(out, &fileInfoWithDir{
						FileInfo:     info,
						isDir:        true,
						resolvedPath: resolved,
					})
					slog.Info("sftp.listdir.symlink_resolved_via_chroot_strip",
						"path", path, "name", info.Name(),
						"target", target, "resolved", resolved,
						"chroot_depth", s.chrootDepth)
					continue
				}
			}
			// Relative target, OR absolute target whose chroot
			// couldn't be derived. Fall back to the v0.3.45
			// ancestor walk — handles junctions where the target
			// basename happens to coincide with a directory at
			// some ancestor level.
			base := filepath.Base(target)
			if base != "" && base != "." && base != "/" && base != ".." {
				if resolved, depth := ancestorWalkResolve(s.sftp, path, base); resolved != "" {
					out = append(out, &fileInfoWithDir{
						FileInfo:     info,
						isDir:        true,
						resolvedPath: resolved,
					})
					slog.Info("sftp.listdir.symlink_resolved_via_ancestor_walk",
						"path", path, "name", info.Name(),
						"target", target, "resolved", resolved, "depth", depth)
					continue
				}
			}
		}

		// All probes failed: junction target is unreachable from
		// this SFTP server. Fall back to the symlink entry so the
		// user at least sees the row; clicking will return a
		// clear 404 from Open() rather than the helper silently
		// dropping it.
		slog.Warn("sftp.listdir.symlink_unresolvable",
			"path", path, "name", info.Name())
		out = append(out, info)
	}
	slog.Info("sftp.listdir", "path", path, "count", len(out))
	return out, nil
}

// ancestorWalkResolve probes ReadDir at each ancestor + basename
// combination, walking up the parent chain. Returns the first
// existing directory and the depth at which it was found, or
// ("", 0) if no ancestor up to maxJunctionDepth contains a
// matching directory.
//
// The NTFS invariant that makes this work: a junction's target
// basename equals the destination directory's name. The
// destination may live in the junction's parent OR in any
// ancestor (e.g. Users/All Users → /ProgramData, where the
// destination lives at the volume root, not at Users/). We
// walk up the parent chain so we find it regardless of how
// deeply nested the junction is.
//
// maxJunctionDepth = 4 covers the practical Windows restore
// layout (depth 1 for root-level junctions, depth 2 for
// Users/Alice junctions, depth 3-4 for pathological cases).
// Each failed probe is a single ReadDir round trip; total cost
// is bounded by 4 ReadDir calls per unresolved symlink.
func ancestorWalkResolve(client *sftp.Client, parent, base string) (string, int) {
	const maxJunctionDepth = 4
	candidate := parent
	for depth := 0; depth <= maxJunctionDepth; depth++ {
		resolved := filepath.Join(candidate, base)
		if _, err := client.ReadDir(resolved); err == nil {
			return resolved, depth
		}
		if candidate == "/" || candidate == "." {
			break
		}
		next := filepath.Dir(candidate)
		if next == candidate {
			// filepath.Dir collapses on the root segment, e.g.
			// filepath.Dir("/Users") = "/Users" — stop here.
			break
		}
		candidate = next
	}
	return "", 0
}

// resolveAbsoluteTarget maps an absolute ReadLink target to its
// SFTP-relative path by stripping the chroot prefix, and caches the
// discovered chroot depth on the Session so subsequent junctions on
// the same connection cost only one ReadDir round trip.
//
// K10 datamover's absolute targets look like
// "/mnt/export/<job-id>/<volume>/<rest>" (the leading components
// are the datamover's internal mount path, invisible from the SFTP
// client). The SFTP-relative equivalent is "/<job-id>/<volume>/<rest>"
// — exactly the target with the first N components removed, where N
// is the chroot prefix's component count.
//
// We don't know N up-front. For the first absolute target we see on
// a session, we try N = 1, 2, 3, ... up to maxChrootDepth; the first
// N whose ReadDir succeeds is the answer, cached on the session.
// Subsequent calls use the cached N for a single ReadDir. If the
// cached N stops working (rare — would mean a second chroot mount
// appeared mid-session), we transparently fall back to re-discovery.
//
// Returns the resolved SFTP-relative path, or "" if no depth
// produced a valid directory.
func (s *Session) resolveAbsoluteTarget(target string) string {
	const maxChrootDepth = 5
	parts := strings.Split(filepath.Clean(target), "/")
	// parts[0] is the empty string produced by Split on a
	// leading-slash path; meaningful components start at parts[1].
	// chrootDepth = N means strip the first N meaningful components,
	// so the kept suffix is parts[N+1:].

	if s.chrootDepth > 0 {
		// Cached fast path: single ReadDir.
		if s.chrootDepth+1 < len(parts) {
			suffix := strings.Join(parts[s.chrootDepth+1:], "/")
			if suffix != "" {
				sftpPath := "/" + suffix
				if _, err := s.sftp.ReadDir(sftpPath); err == nil {
					return sftpPath
				}
			}
		}
		// Cached depth stopped working — invalidate and
		// re-discover. Could happen if the SFTP root changed
		// mid-session (different FRS pool entry, container
		// restart, etc.). Cheap to retry.
		slog.Warn("sftp.chroot_depth_invalidated",
			"previous_depth", s.chrootDepth, "target", target)
		s.chrootDepth = 0
	}

	// Discovery: try depths 1..maxChrootDepth.
	for depth := 1; depth <= maxChrootDepth; depth++ {
		if depth+1 >= len(parts) {
			break
		}
		suffix := strings.Join(parts[depth+1:], "/")
		if suffix == "" {
			continue
		}
		sftpPath := "/" + suffix
		if _, err := s.sftp.ReadDir(sftpPath); err == nil {
			s.chrootDepth = depth
			slog.Info("sftp.chroot_depth_discovered",
				"depth", depth, "sample_target", target,
				"sample_resolved", sftpPath)
			return sftpPath
		}
	}
	return ""
}

// fileInfoWithDir wraps an os.FileInfo and overrides IsDir() /
// Mode() so an entry that the SFTP server reported as a symlink
// can be presented as a directory to the rest of the stack.
// Embedding the original FileInfo means every other method
// (Name, Size, ModTime, Sys, etc.) is forwarded unchanged.
//
// If resolvedPath is non-empty, the browse/handlers code should
// navigate to that path instead of the literal Name (the link
// itself may be unreachable on the SFTP server even though its
// target is fine — typical of K10 datamover + NTFS junctions).
type fileInfoWithDir struct {
	os.FileInfo
	isDir        bool
	resolvedPath string
}

func (f *fileInfoWithDir) IsDir() bool { return f.isDir }

// ResolvedPath returns the absolute path this entry actually
// points to (after symlink / junction resolution), or "" if
// the entry should be navigated by its literal name. Used by
// the browse template's click URLs and the download/zip walker
// to skip the (possibly broken) link and go straight to the
// target.
func (f *fileInfoWithDir) ResolvedPath() string { return f.resolvedPath }

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
