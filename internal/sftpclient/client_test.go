package sftpclient

import (
	"context"
	"io"
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
