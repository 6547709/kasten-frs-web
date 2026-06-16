package sftpclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestStartSFTPTestServer(t *testing.T) {
	ts, cleanup := StartSFTPTestServer(t)
	defer cleanup()

	addr := ts.Addr().String()
	host, port, _ := net.SplitHostPort(addr)

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(ts.Signer())},
		HostKeyCallback: ssh.FixedHostKey(ts.HostKey()),
	}
	conn, err := ssh.Dial("tcp", net.JoinHostPort(host, port), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.NewSession(); err != nil {
		t.Fatalf("open session: %v", err)
	}
	_ = context.Background
}

// helper to make sure rand is used
var _ = rand.Reader

// silence unused import for ed25519 in case testserver adapts
var _ = ed25519.PrivateKey(nil)
