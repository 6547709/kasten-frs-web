package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Run starts the HTTP server and blocks until ctx is cancelled. Performs
// a graceful shutdown with a 10s timeout.
func Run(ctx context.Context, srv *http.Server, l net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(l) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	return nil
}
