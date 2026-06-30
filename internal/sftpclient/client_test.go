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

// TestClient_ListDir_ReadLinkAbsoluteFallback exercises the
// case where the SFTP server returns an ABSOLUTE symlink target
// (typical for K10 datamover exposing NTFS junctions — the
// raw target string includes the SFTP chroot prefix like
// "/mnt/export/.../Users", which is invisible from the SFTP
// client). Probe 2 should treat the absolute target as a
// basename-relative reference and resolve against the parent
// directory.
func TestClient_ListDir_ReadLinkAbsoluteFallback(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// Build an absolute target pointing at a dir we control
	// (real-dir). The testserver's fs.real() map this to a
	// local path — but the helper only sees the absolute
	// string and shouldn't trust it; it should fall back to
	// "join(parent, basename(target))" = "/real-dir" and find
	// that real dir under the SFTP root.
	absTarget := filepath.Join(ts.RootDir(), "real-dir")
	if err := os.Symlink(absTarget, filepath.Join(ts.RootDir(), "abs-link")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}

	// Force probe 1 (OPENDIR on the link path) to fail so we
	// exercise probe 2 (ReadLink + resolve).
	ts.WithBrokenOpenDir("/abs-link")

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
		if e.Name() == "abs-link" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the abs-link symlink")
	}
	if !info.IsDir() {
		t.Errorf("abs-target symlink IsDir()=false; ReadLink + basename-resolve fallback should have promoted it")
	}
	if rp := resolvedPathOf(info); rp != "/real-dir" {
		t.Errorf("ResolvedPath()=%q, want %q (basename of absolute target joined to parent)", rp, "/real-dir")
	}
}
// ancestor-walk junction resolver at depth 1 (Users/All Users → /ProgramData),
// depth 2 (Users/Alice/AppData → /Application Data), and depth 3
// (Users/Alice/Documents/Profile → /UserProfile). These mirror the
// real Windows FRS junction shapes — the target lives in an ancestor
// directory of the junction, not next to it. The helper walks up
// the parent chain looking for `<ancestor>/<target basename>`.
//
// All three tests follow the same pattern:
//   - build a real target directory at the destination depth
//   - build a symlink with a target whose basename matches the
//     destination directory's name
//   - block OPENDIR on the link path so probe 1 fails (matches
//     K10 datamover behaviour for NTFS junctions)
//   - assert IsDir()=true and ResolvedPath() points at the real
//     target, NOT at the junction itself
func TestClient_ListDir_AncestorWalk_Depth1(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// /Users/All Users is a junction whose target basename is
	// "ProgramData". The real /ProgramData directory lives at
	// the volume ROOT, not inside Users/. The walk must go up
	// one level (/Users → /) to find it.
	target := filepath.Join(ts.RootDir(), "ProgramData")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "marker.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	usersDir := filepath.Join(ts.RootDir(), "Users")
	if err := os.MkdirAll(usersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(usersDir, "All Users")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	ts.WithBrokenOpenDir("/Users/All Users")

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

	entries, err := sess.ListDir("/Users")
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "All Users" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the depth-1 junction")
	}
	if !info.IsDir() {
		t.Errorf("depth-1 junction IsDir()=false; ancestor walk should have promoted it")
	}
	if rp := resolvedPathOf(info); rp != "/ProgramData" {
		t.Errorf("ResolvedPath()=%q, want %q (depth-1 ancestor walk: /Users/ProgramData miss → /ProgramData hit)", rp, "/ProgramData")
	}
}

func TestClient_ListDir_AncestorWalk_Depth2(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// /Users/Alice/AppData is a junction whose target basename
	// is "Application Data". The real /Application Data
	// directory lives at the volume ROOT, two ancestors up.
	target := filepath.Join(ts.RootDir(), "Application Data")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	aliceDir := filepath.Join(ts.RootDir(), "Users", "Alice")
	if err := os.MkdirAll(aliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(aliceDir, "AppData")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	ts.WithBrokenOpenDir("/Users/Alice/AppData")

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

	entries, err := sess.ListDir("/Users/Alice")
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "AppData" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the depth-2 junction")
	}
	if !info.IsDir() {
		t.Errorf("depth-2 junction IsDir()=false; ancestor walk should have promoted it")
	}
	if rp := resolvedPathOf(info); rp != "/Application Data" {
		t.Errorf("ResolvedPath()=%q, want %q (depth-2 ancestor walk: /Users/Alice/Application Data miss → /Users/Application Data miss → /Application Data hit)", rp, "/Application Data")
	}
}

func TestClient_ListDir_AncestorWalk_Depth3(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// /Users/Alice/Documents/Profile is a junction whose target
	// basename is "UserProfile". The real /UserProfile
	// directory lives at the volume ROOT, three ancestors up.
	target := filepath.Join(ts.RootDir(), "UserProfile")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	docDir := filepath.Join(ts.RootDir(), "Users", "Alice", "Documents")
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(docDir, "Profile")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	ts.WithBrokenOpenDir("/Users/Alice/Documents/Profile")

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

	entries, err := sess.ListDir("/Users/Alice/Documents")
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "Profile" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the depth-3 junction")
	}
	if !info.IsDir() {
		t.Errorf("depth-3 junction IsDir()=false; ancestor walk should have promoted it")
	}
	if rp := resolvedPathOf(info); rp != "/UserProfile" {
		t.Errorf("ResolvedPath()=%q, want %q (depth-3 ancestor walk)", rp, "/UserProfile")
	}
}

// TestClient_ListDir_AncestorWalk_AbsoluteTarget covers the
// K10 datamover case where ReadLink returns an absolute path
// containing the SFTP chroot prefix. The ancestor walk keeps
// only the basename, so the chroot prefix is dropped and the
// walk proceeds normally — same answer as a relative target
// with the same basename.
func TestClient_ListDir_AncestorWalk_AbsoluteTarget(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// Real target at the volume root. Symlink stores an
	// absolute target string that doesn't match anything on
	// disk (the testserver's fs.real() would map the absolute
	// path through the chroot, but the helper must not trust
	// it — it should fall back to basename-only).
	target := filepath.Join(ts.RootDir(), "ProgramData")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	usersDir := filepath.Join(ts.RootDir(), "Users")
	if err := os.MkdirAll(usersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Point the symlink at an absolute path that LOOKS like a
	// K10 chroot-prefixed target — the helper should keep only
	// the basename ("ProgramData") and walk from there.
	chrootTarget := "/mnt/export/scheduled-xyz/ProgramData"
	if err := os.Symlink(chrootTarget, filepath.Join(usersDir, "All Users")); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	ts.WithBrokenOpenDir("/Users/All Users")

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

	entries, err := sess.ListDir("/Users")
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "All Users" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the abs-target junction")
	}
	if !info.IsDir() {
		t.Errorf("abs-target junction IsDir()=false; ancestor walk should have promoted it")
	}
	if rp := resolvedPathOf(info); rp != "/ProgramData" {
		t.Errorf("ResolvedPath()=%q, want %q (basename of %q joined to ancestor /)", rp, "/ProgramData", chrootTarget)
	}
}

// Chroot-stripping tests (v0.3.46).
//
// These mirror the live cluster's junction shapes: every NTFS
// junction target returned by K10 datamover is absolute and
// carries the datamover's internal mount prefix (typically
// "/mnt/export/<job-id>/<volume>/<rest>"). The SFTP chroot is
// the mount path; the SFTP-relative equivalent is just the
// target minus the chroot's leading components.
//
// The helper discovers the chroot depth lazily on the first
// absolute target seen per session, then reuses it for every
// subsequent junction. Tests below create junctions with the
// production-shaped targets and verify the helper resolves
// them to the correct SFTP-relative path.

// TestClient_ListDir_ChrootStrip_TypicalK10Datamover covers the
// canonical case: a ProgramData/Documents junction whose target
// is "/mnt/export/<job>/<vol>/Users/Public/Documents". The
// helper should strip the 2-component chroot ("/mnt/export")
// and resolve to the SFTP-relative path
// "/<job>/<vol>/Users/Public/Documents".
func TestClient_ListDir_ChrootStrip_TypicalK10Datamover(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	// Build the production-shaped directory tree:
	//   /scheduled-xyz/win2025-uefi01-volume/
	//     ProgramData/
	//       Documents (junction -> /mnt/export/.../Users/Public/Documents)
	//     Users/Public/Documents/   (real dir, the destination)
	const jobID = "scheduled-xyz"
	const volName = "win2025-uefi01-volume"
	target := filepath.Join(ts.RootDir(), jobID, volName, "Users", "Public", "Documents")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "marker.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	programData := filepath.Join(ts.RootDir(), jobID, volName, "ProgramData")
	if err := os.MkdirAll(programData, 0o755); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(programData, "Documents")
	absTarget := "/mnt/export/" + jobID + "/" + volName + "/Users/Public/Documents"
	if err := os.Symlink(absTarget, junction); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	// Force probe 1 (OPENDIR-on-link) to fail so probe 2 runs.
	junctionInSFTP := "/" + jobID + "/" + volName + "/ProgramData/Documents"
	ts.WithBrokenOpenDir(junctionInSFTP)

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

	programDataSFTP := "/" + jobID + "/" + volName + "/ProgramData"
	entries, err := sess.ListDir(programDataSFTP)
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "Documents" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the Documents junction")
	}
	if !info.IsDir() {
		t.Errorf("Documents junction IsDir()=false; chroot-strip should have promoted it")
	}
	wantResolved := "/" + jobID + "/" + volName + "/Users/Public/Documents"
	if rp := resolvedPathOf(info); rp != wantResolved {
		t.Errorf("ResolvedPath()=%q, want %q (chroot-stripped SFTP-relative path)", rp, wantResolved)
	}
	if sess.chrootDepth != 2 {
		t.Errorf("sess.chrootDepth = %d, want 2 (chroot = /mnt/export = 2 components)", sess.chrootDepth)
	}
}

// TestClient_ListDir_ChrootStrip_SelfLoop covers the
// NTFS-internal self-referential junction:
//   ProgramData/Application Data -> /mnt/export/.../ProgramData
// The target's basename IS the parent's name; the only
// "destination" is the parent directory itself. The chroot
// stripper should resolve this to the parent's SFTP path,
// which on a real Windows system would re-show ProgramData's
// contents (the loop is by design — legacy compatibility).
func TestClient_ListDir_ChrootStrip_SelfLoop(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	const jobID = "scheduled-xyz"
	const volName = "win2025-uefi01-volume"
	programData := filepath.Join(ts.RootDir(), jobID, volName, "ProgramData")
	if err := os.MkdirAll(programData, 0o755); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(programData, "Application Data")
	// Target points BACK at ProgramData — the NTFS self-loop.
	absTarget := "/mnt/export/" + jobID + "/" + volName + "/ProgramData"
	if err := os.Symlink(absTarget, junction); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	junctionInSFTP := "/" + jobID + "/" + volName + "/ProgramData/Application Data"
	ts.WithBrokenOpenDir(junctionInSFTP)

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

	programDataSFTP := "/" + jobID + "/" + volName + "/ProgramData"
	entries, err := sess.ListDir(programDataSFTP)
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "Application Data" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the Application Data junction")
	}
	if !info.IsDir() {
		t.Errorf("Application Data junction IsDir()=false; chroot-strip should have promoted it")
	}
	// ResolvedPath == parent path == the SFTP-relative ProgramData.
	wantResolved := programDataSFTP
	if rp := resolvedPathOf(info); rp != wantResolved {
		t.Errorf("ResolvedPath()=%q, want %q (self-loop resolves to parent)", rp, wantResolved)
	}
	if sess.chrootDepth != 2 {
		t.Errorf("sess.chrootDepth = %d, want 2", sess.chrootDepth)
	}
}

// TestClient_ListDir_ChrootStrip_CachedDepth verifies the cache
// behaviour: two absolute targets on the same session — only the
// FIRST triggers discovery (N = 1..5 round trips); the SECOND
// uses the cached depth (single ReadDir). The test checks
// sess.chrootDepth is set after the first junction and that
// both junctions resolve correctly.
func TestClient_ListDir_ChrootStrip_CachedDepth(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	const jobID = "scheduled-xyz"
	const volName = "win2025-uefi01-volume"
	volRoot := filepath.Join(ts.RootDir(), jobID, volName)
	if err := os.MkdirAll(volRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// Two junctions with different subtrees, both with the
	// 2-component chroot prefix "/mnt/export".
	targets := []struct{ junction, absTarget, wantResolved string }{
		{
			junction:     filepath.Join(volRoot, "One"),
			absTarget:   "/mnt/export/" + jobID + "/" + volName + "/Users/Public/Documents",
			wantResolved: "/" + jobID + "/" + volName + "/Users/Public/Documents",
		},
		{
			junction:     filepath.Join(volRoot, "Two"),
			absTarget:   "/mnt/export/" + jobID + "/" + volName + "/ProgramData/Microsoft/Windows/Templates",
			wantResolved: "/" + jobID + "/" + volName + "/ProgramData/Microsoft/Windows/Templates",
		},
	}
	for _, tgt := range targets {
		// Build the SFTP-server-side path that mirrors the absolute
		// target with the chroot stripped. The testserver's fs.real()
		// strips the leading "/" and joins with rootDir, so a path
		// like "/scheduled-xyz/.../Users/Public/Documents" maps to
		// "<rootDir>/scheduled-xyz/.../Users/Public/Documents" on disk.
		sftpMirrored := tgt.absTarget[len("/mnt/export"):]
		fullSftpMirrored := filepath.Join(ts.RootDir(), sftpMirrored)
		if err := os.MkdirAll(fullSftpMirrored, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tgt.absTarget, tgt.junction); err != nil {
			t.Skipf("symlink unsupported in test env: %v", err)
		}
		// Force probe 1 (OPENDIR-on-link) to fail so probe 2
		// (chroot-strip) is exercised. Mirrors K10 datamover's
		// "doesn't follow NTFS junctions at OPENDIR" behaviour.
		linkInSFTP := "/" + jobID + "/" + volName + "/" + filepath.Base(tgt.junction)
		ts.WithBrokenOpenDir(linkInSFTP)
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

	volRootSFTP := "/" + jobID + "/" + volName
	entries, err := sess.ListDir(volRootSFTP)
	if err != nil {
		t.Fatal(err)
	}
	// Both junctions should be promoted to dirs with the right
	// resolved paths.
	found := map[string]string{}
	for _, e := range entries {
		for _, tgt := range targets {
			if e.Name() == filepath.Base(tgt.junction) {
				found[tgt.wantResolved] = resolvedPathOf(e)
			}
		}
	}
	for want, got := range found {
		if got != want {
			t.Errorf("ResolvedPath = %q, want %q", got, want)
		}
	}
	if sess.chrootDepth != 2 {
		t.Errorf("sess.chrootDepth = %d, want 2 (cached after first junction)", sess.chrootDepth)
	}
}

// TestClient_ListDir_ChrootStrip_DeeplyNested covers a junction
// whose target suffix is many components deep. Mirrors the live
// ProgramData/Templates junction whose target is
// "/mnt/export/<job>/<vol>/ProgramData/Microsoft/Windows/Templates"
// (4 suffix components after stripping the 2-component chroot).
func TestClient_ListDir_ChrootStrip_DeeplyNested(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	const jobID = "scheduled-xyz"
	const volName = "win2025-uefi01-volume"
	deepPath := filepath.Join(ts.RootDir(), jobID, volName,
		"ProgramData", "Microsoft", "Windows", "Templates")
	if err := os.MkdirAll(deepPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deepPath, "readme.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	programData := filepath.Join(ts.RootDir(), jobID, volName, "ProgramData")
	if err := os.MkdirAll(programData, 0o755); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(programData, "Templates")
	absTarget := "/mnt/export/" + jobID + "/" + volName +
		"/ProgramData/Microsoft/Windows/Templates"
	if err := os.Symlink(absTarget, junction); err != nil {
		t.Skipf("symlink unsupported in test env: %v", err)
	}
	junctionInSFTP := "/" + jobID + "/" + volName + "/ProgramData/Templates"
	ts.WithBrokenOpenDir(junctionInSFTP)

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

	programDataSFTP := "/" + jobID + "/" + volName + "/ProgramData"
	entries, err := sess.ListDir(programDataSFTP)
	if err != nil {
		t.Fatal(err)
	}
	var info os.FileInfo
	for _, e := range entries {
		if e.Name() == "Templates" {
			info = e
			break
		}
	}
	if info == nil {
		t.Skip("test server didn't create the Templates junction")
	}
	if !info.IsDir() {
		t.Errorf("Templates junction IsDir()=false; chroot-strip should have promoted it")
	}
	wantResolved := "/" + jobID + "/" + volName +
		"/ProgramData/Microsoft/Windows/Templates"
	if rp := resolvedPathOf(info); rp != wantResolved {
		t.Errorf("ResolvedPath()=%q, want %q (deeply-nested chroot-stripped path)", rp, wantResolved)
	}
	if sess.chrootDepth != 2 {
		t.Errorf("sess.chrootDepth = %d, want 2", sess.chrootDepth)
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
