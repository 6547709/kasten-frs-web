// Package sftpclient provides SFTP client connections backed by a per-FRS pool.
package sftpclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TestServer is an in-process SFTP server bound to a temp dir.
type TestServer struct {
	listener net.Listener
	signer   ssh.Signer
	hostKey  ssh.PublicKey
	rootDir  string
}

// Addr returns the listener address.
func (ts *TestServer) Addr() net.Addr { return ts.listener.Addr() }

// Signer returns the server's user signer (use as client key).
func (ts *TestServer) Signer() ssh.Signer { return ts.signer }

// HostKey returns the server's host public key (use in FixedHostKey).
func (ts *TestServer) HostKey() ssh.PublicKey { return ts.hostKey }

// RootDir is the temp directory served over SFTP.
func (ts *TestServer) RootDir() string { return ts.rootDir }

// HostKeyString returns the host key in authorized_keys format.
func (ts *TestServer) HostKeyString() string {
	return string(ssh.MarshalAuthorizedKey(ts.hostKey))
}

// StartSFTPTestServer launches an in-process SFTP server rooted at t.TempDir().
// Returns the server and a cleanup func. The signer is the matching "user"
// key; the host key is the server's identity.
func StartSFTPTestServer(t *testing.T) (*TestServer, func()) {
	t.Helper()

	root := t.TempDir()
	// Touch a sample file so listdir has content.
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Host key
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	// User key (the "client" identity)
	_, userPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userPriv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if key.Type() != userSigner.PublicKey().Type() {
				return nil, fmt.Errorf("key type mismatch")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &TestServer{
		listener: listener,
		signer:   userSigner,
		hostKey:  hostSigner.PublicKey(),
		rootDir:  root,
	}
	go func() {
		for {
			nconn, err := listener.Accept()
			if err != nil {
				return
			}
			go srv.handle(nconn, cfg)
		}
	}()

	cleanup := func() { _ = listener.Close() }
	return srv, cleanup
}

// fs is a minimal in-process SFTP filesystem rooted at ts.rootDir.
// Only Filelist is implemented to satisfy the FileLister interface.
// File ops beyond listing are not exercised by the smoke test.
type fs struct {
	root string
}

func (f *fs) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		infos, err := os.ReadDir(filepath.Join(f.root, r.Filepath))
		if err != nil {
			return nil, err
		}
		// Convert to []os.FileInfo via interface type
		entries := make([]os.FileInfo, 0, len(infos))
		for _, e := range infos {
			info, err := e.Info()
			if err != nil {
				return nil, err
			}
			entries = append(entries, info)
		}
		return listerAt(entries), nil
	case "Stat":
		info, err := os.Stat(filepath.Join(f.root, r.Filepath))
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{info}), nil
	}
	return nil, fmt.Errorf("unsupported method %q", r.Method)
}

func (f *fs) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	return os.Open(filepath.Join(f.root, r.Filepath))
}

func (f *fs) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	fp := filepath.Join(f.root, r.Filepath)
	return os.OpenFile(fp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

func (f *fs) Filecmd(r *sftp.Request) error {
	return nil
}

// listerAt is a minimal sftp.ListerAt over a fixed slice.
type listerAt []os.FileInfo

func (l listerAt) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

func (ts *TestServer) handle(nconn net.Conn, cfg *ssh.ServerConfig) {
	defer nconn.Close()
	conn, chans, reqs, err := ssh.NewServerConn(nconn, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "unknown")
			continue
		}
		ch, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go func(in <-chan *ssh.Request) {
			for req := range in {
				switch req.Type {
				case "subsystem":
					if string(req.Payload[4:]) == "sftp" {
						_ = req.Reply(true, nil)
						server := sftp.NewRequestServer(ch, sftp.Handlers{
							FileGet:  &fs{root: ts.rootDir},
							FilePut:  &fs{root: ts.rootDir},
							FileCmd:  &fs{root: ts.rootDir},
							FileList: &fs{root: ts.rootDir},
						})
						_ = server.Serve()
					}
				default:
					_ = req.Reply(false, nil)
				}
			}
		}(requests)
	}
}
