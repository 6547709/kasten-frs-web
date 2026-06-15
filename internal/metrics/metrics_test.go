package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistry_ExposesMetrics(t *testing.T) {
	reg := NewRegistry()
	// Touch at least one label combo on each *Vec metric so the
	// Prometheus client emits HELP/TYPE/sample lines for it.
	HTTPRequestsTotal.WithLabelValues("GET", "/", "200").Inc()
	LoginAttemptsTotal.WithLabelValues("ok").Inc()
	DownloadBytesTotal.WithLabelValues("ns", "n").Add(0)
	rr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, m := range []string{
		"kfrs_http_requests_total",
		"kfrs_login_attempts_total",
		"kfrs_sftp_connections_active",
		"kfrs_download_bytes_total",
	} {
		if !strings.Contains(body, m) {
			t.Errorf("metric %s missing", m)
		}
	}
}
