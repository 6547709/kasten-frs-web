package sftpclient

import (
	"context"
	"io"
	"strings"
	"testing"
)

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

	sess, err := c.Dial(context.Background(), addr)
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
