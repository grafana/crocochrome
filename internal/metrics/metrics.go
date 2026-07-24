package metrics

import (
	"context"
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

	TerminationReason         = "reason"
	TerminationReasonDeleted  = "deleted"
	TerminationReasonTimeout  = "timeout"
	TerminationReasonReplaced = "replaced"
)

// routeCtxKey is the context key under which the route label is stored for promhttp to pick up.
type routeCtxKey struct{}

// InstrumentHTTP uses promhttp to instrument a handler with total and duration of requests, labeled by status code,
// method, and route. The route function must map a request to a bounded set of route labels (e.g. mux patterns, never
// raw paths) to keep metric cardinality bounded.
func InstrumentHTTP(reg prometheus.Registerer, handler http.Handler, route func(*http.Request) string) http.Handler {
	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNs,
			Subsystem: metricSubsystemCrocochrome,
			Name:      "requests_total",
			Help:      "Total number of requests received",
		},
		[]string{"code", "method", "route"},
	)

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricNs,
			Subsystem: metricSubsystemCrocochrome,
			Name:      "request_duration_seconds",
			Help:      "Duration of requests",
			Buckets:   prometheus.ExponentialBucketsRange(0.5, 60, 16),
		},
		[]string{"code", "method", "route"},
	)

	reg.MustRegister(requests)
	reg.MustRegister(duration)

	routeLabel := promhttp.WithLabelFromCtx("route", func(ctx context.Context) string {
		if r, ok := ctx.Value(routeCtxKey{}).(string); ok {
			return r
		}
		return "unknown"
	})

	handler = promhttp.InstrumentHandlerCounter(requests, handler, routeLabel)
	handler = promhttp.InstrumentHandlerDuration(duration, handler, routeLabel)

	instrumented := handler

	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), routeCtxKey{}, route(r))
		instrumented.ServeHTTP(rw, r.WithContext(ctx))
	})
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
	// SessionActive is 1 when a session is active and 0 otherwise. It is deliberately a boolean rather than a
	// session count: fleet busy-ratio is avg() of this gauge across instances, with no assumption about
	// per-instance capacity baked into the queries.
	SessionActive prometheus.Gauge
	// SessionsCreated counts sessions successfully created.
	SessionsCreated prometheus.Counter
	// SessionsTerminated counts sessions terminated, labeled by "reason": "deleted" (explicit delete), "timeout"
	// (session timeout fired), or "replaced" (killed by a new session). A sustained rate of "timeout" terminations
	// means clients are not releasing their sessions.
	SessionsTerminated *prometheus.CounterVec
	// OOMKills counts the number of times the kernel OOM-killer fired within the container's
	// cgroup during a Chromium session. A non-zero value indicates that Chromium's multi-process
	// tree (renderer, GPU process, etc.) exceeded the container memory limit and had one or more
	// processes killed, even if crocochrome itself survived.
	OOMKills prometheus.Counter
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
		SessionActive: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: metricNs,
				Subsystem: metricSubsystemCrocochrome,
				Name:      "session_active",
				Help:      "Set to 1 when a session is active, 0 otherwise.",
			},
		),
		SessionsCreated: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNs,
				Subsystem: metricSubsystemCrocochrome,
				Name:      "sessions_created_total",
				Help:      "Total number of sessions created.",
			},
		),
		SessionsTerminated: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNs,
				Subsystem: metricSubsystemCrocochrome,
				Name:      "sessions_terminated_total",
				Help: "Total number of sessions terminated, labeled by \"reason\". " +
					"\"deleted\" means the session was explicitly deleted by a client. " +
					"\"timeout\" means the session timeout fired. " +
					"\"replaced\" means the session was killed by the creation of a new one.",
			},
			[]string{TerminationReason},
		),
		OOMKills: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: metricNs,
				Subsystem: metricSubsystemCrocochrome,
				Name:      "chromium_oom_kills_total",
				Help: "Total number of times the kernel OOM-killer fired within the container cgroup " +
					"during a Chromium session. Incremented when the oom_kill counter in the cgroup " +
					"memory events file increases between session start and session end.",
			},
		),
	}

	reg.MustRegister(m.SessionDuration)
	reg.MustRegister(m.ChromiumExecutions)
	reg.MustRegister(m.ChromiumResources)
	reg.MustRegister(m.SessionActive)
	reg.MustRegister(m.SessionsCreated)
	reg.MustRegister(m.SessionsTerminated)
	reg.MustRegister(m.OOMKills)

	return m
}
