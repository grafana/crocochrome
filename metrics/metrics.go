package metrics

import (
	"net/http"
	"time"

	"github.com/grafana/crocochrome/internal/version"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	metricNs                   = "sm"
	metricSubsystemCrocochrome = "crocochrome"

	ExecutionState         = "state"
	ExecutionStateFinished = "finished"
	ExecutionStateFailed   = "failed"

	Resource    = "resource"
	ResourceRSS = "rss"
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

func AddVersionMetrics(reg prometheus.Registerer) {
	info := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "sm",
			Subsystem: "crocochrome",
			Name:      "info",
			Help:      "Crocochrome Info",
			ConstLabels: prometheus.Labels{
				"version":   version.Short(),
				"commit":    version.Commit(),
				"timestamp": version.Buildstamp(),
			},
		},
	)

	// make sure the value is always one
	info.Set(1)

	reg.MustRegister(info)

	// Add the standard go_build_info gauge too.
	reg.MustRegister(collectors.NewBuildInfoCollector())
}

// SupervisorMetrics contains metrics used by the crocochrome supervisor.
type SupervisorMetrics struct {
	SessionDuration    prometheus.Histogram
	ChromiumExecutions *prometheus.CounterVec
	ChromiumResources  *prometheus.HistogramVec
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
		ChromiumExecutions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNs,
				Subsystem: metricSubsystemCrocochrome,
				Name:      "chromium_executions_total",
				Help: "Total number of executions, labeled by \"state\". " +
					"\"finished\" means the execution terminated normally as part of the session cancellation. " +
					"\"failed\" means chromium existed with an unexpected error.",
			},
			[]string{ExecutionState},
		),
		ChromiumResources: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricNs,
				Subsystem: metricSubsystemCrocochrome,
				Name:      "chromium_resource_usage",
				Help: "Resources used by chromium when the execution ends." +
					"Memory resources are expressed in bytes.",
				Buckets:                         prometheus.LinearBuckets(0, 64<<20, 16), // 64Mi*16=1024Mi
				NativeHistogramBucketFactor:     1.2,
				NativeHistogramMaxBucketNumber:  32,
				NativeHistogramMinResetDuration: 1 * time.Hour,
			},
			[]string{Resource},
		),
	}

	reg.MustRegister(m.SessionDuration)
	reg.MustRegister(m.ChromiumExecutions)
	reg.MustRegister(m.ChromiumResources)

	return m
}
