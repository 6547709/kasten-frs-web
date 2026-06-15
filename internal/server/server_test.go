package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRun_GracefulShutdown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NewServeMux()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, l) }()
	time.Sleep(50 * time.Millisecond)

	// Confirm it's serving
	resp, err := http.Get("http://" + l.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		// 404 is fine — handler not registered
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
