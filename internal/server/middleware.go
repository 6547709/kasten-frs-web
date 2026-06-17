// Package server wires http.Server, middleware chain, and routes.
package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/liguoqiang/kasten-frs-web/internal/logging"
)

// SecurityHeaders injects the standard response headers (CSP, HSTS, etc).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		next.ServeHTTP(w, r)
	})
}

// Recoverer catches panics, logs them, and returns 500.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// Use the request-scoped logger so the panic line
				// carries the same request_id as the access log,
				// and emit structured JSON consistent with the rest
				// of the app instead of a bare fmt.Printf.
				logging.FromContext(r.Context(), slog.Default()).Error("panic.recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
