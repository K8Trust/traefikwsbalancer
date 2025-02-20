//go:build yaegi
// +build yaegi

package traefikwsbalancer

// Dummy implementations
type dummyGaugeVec struct{}

func (v dummyGaugeVec) WithLabelValues(lvs ...string) Gauge {
    return dummyGauge{}
}

type dummyGauge struct{}

func (g dummyGauge) Set(float64) {}

type dummyHistogramVec struct{}

func (v dummyHistogramVec) WithLabelValues(lvs ...string) Histogram {
    return dummyHistogram{}
}

type dummyHistogram struct{}

func (h dummyHistogram) Observe(float64) {}

type dummyCounterVec struct{}

func (v dummyCounterVec) WithLabelValues(lvs ...string) Counter {
    return dummyCounter{}
}

type dummyCounter struct{}

func (c dummyCounter) Inc() {}

func init() {
    activeConnections = dummyGaugeVec{}
    connectionDuration = dummyHistogramVec{}
    connectionErrors = dummyCounterVec{}
    
    MustRegister = func(collectors ...interface{}) {
        // No-op for yaegi build
    }
}