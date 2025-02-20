//go:build !yaegi
// +build !yaegi

package traefikwsbalancer

import (
    "github.com/prometheus/client_golang/prometheus"
)

// Prometheus implementations
type promGaugeVec struct {
    *prometheus.GaugeVec
}

func (v *promGaugeVec) WithLabelValues(lvs ...string) Gauge {
    return v.GaugeVec.WithLabelValues(lvs...)
}

type promHistogramVec struct {
    *prometheus.HistogramVec
}

func (v *promHistogramVec) WithLabelValues(lvs ...string) Histogram {
    return v.HistogramVec.WithLabelValues(lvs...)
}

type promCounterVec struct {
    *prometheus.CounterVec
}

func (v *promCounterVec) WithLabelValues(lvs ...string) Counter {
    return v.CounterVec.WithLabelValues(lvs...)
}

func init() {
    activeConnections = &promGaugeVec{
        prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Name: "wsbalancer_active_connections",
                Help: "Number of active WebSocket connections per service",
            },
            []string{"service"},
        ),
    }

    connectionDuration = &promHistogramVec{
        prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "wsbalancer_connection_duration_seconds",
                Help:    "Duration of WebSocket connections",
                Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
            },
            []string{"service"},
        ),
    }

    connectionErrors = &promCounterVec{
        prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "wsbalancer_connection_errors_total",
                Help: "Total number of WebSocket connection errors",
            },
            []string{"service", "error_type"},
        ),
    }

    MustRegister = func(collectors ...interface{}) {
        // Convert interface{} to prometheus.Collector
        promCollectors := make([]prometheus.Collector, 0, len(collectors))
        for _, c := range collectors {
            if pc, ok := c.(prometheus.Collector); ok {
                promCollectors = append(promCollectors, pc)
            }
        }
        prometheus.MustRegister(promCollectors...)
    }
}