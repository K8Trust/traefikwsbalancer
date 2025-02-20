// +build !yaegi

package traefikwsbalancer

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	activeConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wsbalancer_active_connections",
			Help: "Number of active WebSocket connections per service",
		},
		[]string{"service"},
	)

	connectionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wsbalancer_connection_duration_seconds",
			Help:    "Duration of WebSocket connections",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		},
		[]string{"service"},
	)

	connectionErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wsbalancer_connection_errors_total",
			Help: "Total number of WebSocket connection errors",
		},
		[]string{"service", "error_type"},
	)
)

// MustRegister is just a proxy to Prometheus' MustRegister.
func MustRegister(collectors ...prometheus.Collector) {
	prometheus.MustRegister(collectors...)
}
