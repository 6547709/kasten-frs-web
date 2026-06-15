// Package logging provides structured logging via log/slog.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
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
