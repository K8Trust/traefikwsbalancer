//go:build yaegi
// +build yaegi

package traefikwsbalancer

// Dummy implementations of Prometheus metric types for Yaegi.

type dummyGaugeVec struct{}
type dummyHistogramVec struct{}
type dummyCounterVec struct{}

func (dummyGaugeVec) WithLabelValues(vals ...string) dummyGauge { return dummyGauge{} }
type dummyGauge struct{}
func (dummyGauge) Set(val float64) {}

func (dummyHistogramVec) WithLabelValues(vals ...string) dummyHistogram { return dummyHistogram{} }
type dummyHistogram struct{}
func (dummyHistogram) Observe(val float64) {}

func (dummyCounterVec) WithLabelValues(vals ...string) dummyCounter { return dummyCounter{} }
type dummyCounter struct{}
func (dummyCounter) Inc() {}

// MustRegister is a dummy no-op.
func MustRegister(collectors ...interface{}) {}

// Export dummy metrics variables.
var (
	activeConnections  dummyGaugeVec   = dummyGaugeVec{}
	connectionDuration dummyHistogramVec = dummyHistogramVec{}
	connectionErrors   dummyCounterVec   = dummyCounterVec{}
)
