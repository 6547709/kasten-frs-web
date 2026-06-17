// Package logging provides structured logging via log/slog.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keySessionID
)

// New builds a JSON slog.Logger at the given level writing to w.
func New(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// WithRequestID returns a context carrying the given request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// WithSessionID returns a context carrying the given session ID.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keySessionID, id)
}

// FromContext returns a logger with request_id and session_id attrs derived
// from ctx, falling back to the base logger.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	l := base
	if v, ok := ctx.Value(keyRequestID).(string); ok && v != "" {
		l = l.With("request_id", v)
	}
	if v, ok := ctx.Value(keySessionID).(string); ok && v != "" {
		l = l.With("session_id", v)
	}
	return l
}

// newRequestID returns a short random hex id used to correlate all
// log lines emitted while handling a single request.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a timestamp-derived id; correlation is
		// best-effort, never a hard failure.
		return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	}
	return hex.EncodeToString(b[:])
}

// AccessLog returns a middleware that emits one INFO line per request
// with method, path, status, bytes_written, duration, and remote_addr.
// It also mints a request_id, stitches it into the request context so
// downstream handlers can pull a correlated logger via FromContext,
// and echoes it back as the X-Request-Id response header.
// Health/readiness probes are filtered so a flapping kubelet doesn't
// drown out useful traffic in customer-deployed environments.
func AccessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip probes. They run every few seconds and would dwarf
		// any real user activity in the log stream.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		// Honour an inbound X-Request-Id (e.g. from a fronting
		// proxy / Route) so the id is consistent end-to-end;
		// otherwise mint a fresh one.
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		ctx := WithRequestID(r.Context(), reqID)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", reqID)

		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		log.Info("http",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", ww.status,
			"bytes", ww.written,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
			"ua", r.UserAgent(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}
