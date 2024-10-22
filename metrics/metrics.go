package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	metricNs                   = "sm"
	metricSubsystemCrocochrome = "crocochrome"
)

// InstrumentHTTP uses promhttp to instrument a handler with total, duration, and in-flight requests.
func InstrumentHTTP(reg prometheus.Registerer, handler http.Handler) http.Handler {
	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNs,
			Subsystem: metricSubsystemCrocochrome,
			Name:      "requests_total",
			Help:      "Total number of requests received",
		},
		[]string{"code"},
	)

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricNs,
			Subsystem: metricSubsystemCrocochrome,
			Name:      "request_duration_seconds",
			Help:      "Duration of requests",
			Buckets:   prometheus.ExponentialBucketsRange(0.5, 60, 16),
		},
		[]string{"code"},
	)

	reg.MustRegister(requests)
	reg.MustRegister(duration)

	handler = promhttp.InstrumentHandlerCounter(requests, handler)
	handler = promhttp.InstrumentHandlerDuration(duration, handler)

	return handler
}

// SupervisorMetrics contains metrics used by the crocochrome supervisor.
type SupervisorMetrics struct {
	SessionDuration prometheus.Histogram
}

// Supervisor registers and returns handlers for metrics used by the supervisor.
func Supervisor(reg prometheus.Registerer) *SupervisorMetrics {
	m := &SupervisorMetrics{
		SessionDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Namespace:                       metricNs,
				Subsystem:                       metricSubsystemCrocochrome,
				Name:                            "session_duration_seconds",
				Help:                            "Lifespan of a chromium session.",
				Buckets:                         prometheus.ExponentialBucketsRange(0.5, 120, 16),
				NativeHistogramBucketFactor:     1.2,
				NativeHistogramMaxBucketNumber:  32,
				NativeHistogramMinResetDuration: 1 * time.Hour,
			},
		),
	}

	reg.MustRegister(m.SessionDuration)

	return m
}