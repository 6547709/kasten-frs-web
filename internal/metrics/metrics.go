// Package metrics defines Prometheus metrics for the helper.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTPRequestsTotal counts HTTP requests by method/path/status.
var HTTPRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_http_requests_total", Help: "HTTP requests."},
	[]string{"method", "path", "status"},
)

// HTTPRequestDuration tracks request latency.
var HTTPRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{Name: "kfrs_http_request_duration_seconds", Help: "Latency."},
	[]string{"method", "path"},
)

// LoginAttemptsTotal counts login attempts.
var LoginAttemptsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_login_attempts_total", Help: "Login attempts."},
	[]string{"result"},
)

// FRSListTotal counts FRS list calls.
var FRSListTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_frs_list_total", Help: "FRS list calls."},
	[]string{"result"},
)

// FRSListSize tracks number of FRS returned.
var FRSListSize = prometheus.NewGauge(
	prometheus.GaugeOpts{Name: "kfrs_frs_list_size", Help: "FRS list size."},
)

// SFTPConnectionsActive is the current pool size.
var SFTPConnectionsActive = prometheus.NewGauge(
	prometheus.GaugeOpts{Name: "kfrs_sftp_connections_active", Help: "Active SFTP connections."},
)

// SFTPConnectTotal counts SFTP connect attempts.
var SFTPConnectTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_sftp_connect_total", Help: "SFTP connect attempts."},
	[]string{"result"},
)

// DownloadBytesTotal counts bytes downloaded.
var DownloadBytesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_download_bytes_total", Help: "Bytes downloaded."},
	[]string{"frs_ns", "frs_name"},
)

// DownloadFilesTotal counts files downloaded.
var DownloadFilesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_download_files_total", Help: "Files downloaded."},
	[]string{"frs_ns", "frs_name"},
)

// K8sAPIErrorsTotal counts K8s API errors.
var K8sAPIErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{Name: "kfrs_k8s_api_errors_total", Help: "K8s API errors."},
	[]string{"op", "code"},
)

// Registry bundles a Prometheus registry and the HTTP handler.
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry creates a registry and registers all metrics.
func NewRegistry() *Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		HTTPRequestsTotal, HTTPRequestDuration, LoginAttemptsTotal,
		FRSListTotal, FRSListSize, SFTPConnectionsActive,
		SFTPConnectTotal, DownloadBytesTotal, DownloadFilesTotal, K8sAPIErrorsTotal,
	)
	return &Registry{reg: r}
}

// Handler returns the /metrics HTTP handler.
func (r *Registry) Handler() http.Handler { return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{}) }
