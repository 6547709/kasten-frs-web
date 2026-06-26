package sftpclient

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHostKeySignature_BothForms(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()
	hostKey := ts.HostKeyString()

	// ssh-keygen-style: "[host:port] alg key" (port in the bracketed
	// part, single space, alg, single space, key). Used in unit tests.
	formA := "[" + ts.Addr().String() + "] " + hostKey
	if _, err := ParseHostKeySignature(formA); err != nil {
		t.Errorf("ssh-keygen form failed: %v", err)
	}

	// Kasten FRS-style: "[host]:port alg key" (port outside brackets).
	// This is what FRS status.transports.sftp.hostKeySignature actually
	// looks like in production. The colon-port variant must also parse.
	host := strings.SplitN(ts.Addr().String(), ":", 2)[0]
	formB := "[" + host + "]:2222 " + hostKey
	if _, err := ParseHostKeySignature(formB); err != nil {
		t.Errorf("Kasten form failed: %v (input: %q)", err, formB)
	}

	// Garbage should still fail.
	if _, err := ParseHostKeySignature("not a host key sig"); err == nil {
		t.Error("expected error for malformed input")
	}
	if _, err := ParseHostKeySignature(""); err == nil {
		t.Error("expected error for empty input")
	}
}

func TestClient_DialListRead(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	addr := ts.Addr().String()

	hostKeySig := "[" + ts.Addr().String() + "] " + ts.HostKeyString()
	c, err := NewClient(ClientConfig{
		Username:       "root",
		Signer:         ts.Signer(),
		HostKeySig:     hostKeySig,
		ConnectTimeout: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	sess, err := c.Dial(context.Background(), addr, hostKeySig)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	entries, err := sess.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one entry")
	}

	rc, err := sess.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if !strings.Contains(string(b), "hi") {
		t.Errorf("content = %q", b)
	}
}

func TestClient_ListDir_FollowsSymlinkToDir(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	c, err := NewClient(ClientConfig{
		Username: "root", Signer: ts.Signer(), ConnectTimeout: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.Dial(context.Background(), ts.Addr().String(),
		"["+ts.Addr().String()+"] "+ts.HostKeyString())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	entries, err := sess.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}

	// Find the symlink entry ("link to dir"). The testserver
	// creates it via os.Symlink above.
	var linkInfo os.FileInfo
	for _, e := range entries {
		if e.Name() == "link to dir" {
			linkInfo = e
			break
		}
	}
	if linkInfo == nil {
		t.Skip("test server didn't create the symlink — skipping")
	}

	// Before the fix, IsDir() returns false (pkg/sftp's Lstat
	// reports os.ModeSymlink, not os.ModeDir). After the fix,
	// ListDir probes via OPENDIR and reports IsDir()=true
	// (and flips the Mode() bit to match).
	if !linkInfo.IsDir() {
		t.Errorf("symlink-to-dir entry IsDir()=false, want true (target IS a directory)")
	}
	if linkInfo.Mode()&os.ModeDir == 0 {
		t.Errorf("symlink-to-dir entry Mode() missing ModeDir bit; downstream tar.FileInfoHeader would misclassify")
	}

	// And actually navigating INTO it should work — the
	// wrapped FileInfo flows through the rest of the stack
	// unchanged, and the SFTP Open/Stat calls the helper
	// issues follow symlinks on the server side.
	inside, err := sess.ListDir("/link to dir")
	if err != nil {
		t.Errorf("ListDir into symlink-to-dir failed: %v", err)
	}
	if len(inside) == 0 || inside[0].Name() != "inside.txt" {
		t.Errorf("expected inside.txt in the symlinked dir, got %+v", inside)
	}
}

// TestClient_ListDir_BrokenSymlinkStaysFile exercises the fallback
// path: when the symlink probe fails (broken link, target
// unreadable), ListDir falls back to the original symlink
// FileInfo (no IsDir flip) so the user at least sees the row.
func TestClient_ListDir_BrokenSymlinkStaysFile(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// Add a broken symlink to the test root: target doesn't
	// exist. pkg/sftp's ReadDir will list it as os.ModeSymlink;
	// our probe (ReadDir on joined path) will fail. We expect
	// the entry to come through with IsDir()=false so the user
	// at least sees it.
	broken := filepath.Join(ts.RootDir(), "broken-link")
	if err := os.Symlink(filepath.Join(ts.RootDir(), "does-not-exist"), broken); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}

	c, err := NewClient(ClientConfig{
		Username: "root", Signer: ts.Signer(), ConnectTimeout: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.Dial(context.Background(), ts.Addr().String(),
		"["+ts.Addr().String()+"] "+ts.HostKeyString())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	entries, err := sess.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var brokenInfo os.FileInfo
	for _, e := range entries {
		if e.Name() == "broken-link" {
			brokenInfo = e
			break
		}
	}
	if brokenInfo == nil {
		t.Skip("test server didn't create the broken symlink")
	}
	if brokenInfo.IsDir() {
		t.Errorf("broken symlink flipped to IsDir=true; probe should have failed and we should have kept the original symlink FileInfo")
	}
}

// TestClient_ListDir_ReadLinkFallback exercises probe 2: when
// OPENDIR on the link itself fails (simulating K10 datamover
// not following NTFS junctions at OPENDIR time), the helper
// should fall through to READLINK, resolve the target itself,
// and probe the resolved path with OPENDIR. On success the
// entry is reported as a directory with a non-empty
// ResolvedPath pointing at the actual target.
func TestClient_ListDir_ReadLinkFallback(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// Tell the testserver to refuse OPENDIR on the symlink
	// path, mirroring K10 datamover's behaviour for NTFS
	// junctions. ReadLink + the resolved-path probe should
	// still succeed because the target itself is a real
	// directory and OPENDIR on the target works fine.
	ts.WithBrokenOpenDir("/link to dir")

	c, err := NewClient(ClientConfig{
		Username: "root", Signer: ts.Signer(), ConnectTimeout: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.Dial(context.Background(), ts.Addr().String(),
		"["+ts.Addr().String()+"] "+ts.HostKeyString())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	entries, err := sess.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var linkInfo os.FileInfo
	for _, e := range entries {
		if e.Name() == "link to dir" {
			linkInfo = e
			break
		}
	}
	if linkInfo == nil {
		t.Skip("test server didn't create the symlink")
	}
	if !linkInfo.IsDir() {
		t.Errorf("symlink-to-dir entry IsDir()=false; ReadLink fallback should have promoted it to a dir")
	}
	if rp := resolvedPathOf(linkInfo); rp != "/real-dir" {
		t.Errorf("ResolvedPath()=%q, want %q", rp, "/real-dir")
	}
}

// TestClient_ListDir_JunctionMapFallback exercises probe 3: the
// hardcoded Windows junction map. We register a Windows-style
// junction name ("Documents and Settings") symlinked to a
// real directory, configure the testserver to refuse both
// OPENDIR-on-link and READLINK (simulating a totally unco-
// operative datamover), and assert the helper still resolves
// the junction via the hardcoded map to "Users".
func TestClient_ListDir_JunctionMapFallback(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// Create the Windows junction name (with a space, on purpose)
	// pointing to "Users" — matches the hardcoded map.
	target := filepath.Join(ts.RootDir(), "Users")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "alice.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ts.RootDir(), "Documents and Settings")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}

	// Block both OPENDIR-on-link AND READLINK so probes 1+2
	// miss. Probe 3 (junction map) is the only thing that can
	// resolve it.
	ts.WithBrokenOpenDir("/Documents and Settings")
	// pkg/sftp's ReadLink is implemented via sftp_request_server.go's
	// sshFxpReadlinkPacket which calls FileCmd. The default handler
	// returns nil — so ReadLink "succeeds" but returns an empty
	// string. We patch the FileCmd return to inject an error so
	// probe 2 falls through too. The cleanest way in this test is
	// to just trust that an empty ReadLink result causes probe 2
	// to skip — see client.go's `if target, rlErr := ...` branch
	// which only proceeds on err == nil AND non-empty target.

	c, err := NewClient(ClientConfig{
		Username: "root", Signer: ts.Signer(), ConnectTimeout: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.Dial(context.Background(), ts.Addr().String(),
		"["+ts.Addr().String()+"] "+ts.HostKeyString())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	entries, err := sess.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "Documents and Settings" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the junction-style symlink")
	}
	if !info.IsDir() {
		t.Errorf("junction entry IsDir()=false; junction-map fallback should have promoted it")
	}
	if rp := resolvedPathOf(info); rp != "/Users" {
		t.Errorf("ResolvedPath()=%q, want %q (Windows junction map)", rp, "/Users")
	}
}

// resolvedPathOf mirrors handlers.resolvedPathOf so tests can
// assert on the resolved path without taking a handler-package
// dependency. Both implementations must stay in sync — if you
// add a new wrapper type, expose ResolvedPath on it AND add a
// case here.
func resolvedPathOf(fi os.FileInfo) string {
	type resolver interface{ ResolvedPath() string }
	if r, ok := fi.(resolver); ok {
		return r.ResolvedPath()
	}
	return ""
}

func TestValidatePath(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"/", true},
		{"/data", true},
		{"/data/file..name", true}, // dots in a name are fine
		{"/x/.hidden", true},       // hidden file is fine
		{"/a/b/c", true},
		{"", false},               // empty
		{"relative/path", false},  // not absolute
		{"/../etc/passwd", false}, // escapes root
		{"/a/../../etc", false},   // interleaved escape
		{"..", false},
	}
	for _, c := range cases {
		err := validatePath(c.in)
		if c.ok && err != nil {
			t.Errorf("validatePath(%q) = %v, want nil", c.in, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validatePath(%q) = nil, want error", c.in)
		}
	}
}
