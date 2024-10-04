package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/grafana/crocochrome"
	crocohttp "github.com/grafana/crocochrome/http"
	"github.com/grafana/crocochrome/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	mux := http.NewServeMux()

	registry := prometheus.NewRegistry()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tp, err := tracerProvider(ctx)
	if err != nil {
		logger.Error("could not enable tracing", "err", err)
		tp = noop.NewTracerProvider()
	}

	supervisor := crocochrome.New(logger, crocochrome.Options{
		ChromiumPath: "chromium",
		// Id for nobody user and group on alpine.
		UserGroup: 65534,
		// In production we mount an emptyDir here, as opposed to /tmp, and configure chromium to write everything in
		// /chromium-tmp instead. We do this to make sure we are not accidentally allowing things we don't know about
		// to be written, as it is safe to assume that anything writing here (the only writable path) is doing so
		// because we told it to.
		TempDir:      "/chromium-tmp",
		Registry:     registry,
		ExtraUATerms: "GrafanaSyntheticMonitoring",
	})

	err = supervisor.ComputeUserAgent(context.Background())
	if err != nil {
		logger.Error("Computing user agent", "err", err)
		os.Exit(1)
		return
	}

	var handler http.Handler = crocohttp.New(logger, supervisor)
	handler = metrics.InstrumentHTTP(registry, handler)
	handler = otelhttp.NewHandler(
		// Instead of using a span name formatter, this middleware sets the span name using the mux pattern.
		spanNameFromPattern(handler),
		"http",
		otelhttp.WithTracerProvider(tp),
		// Consider all endpoints private. This enables propagating traceIDs from clients.
		otelhttp.WithPublicEndpointFn(func(r *http.Request) bool { return false }),
		otelhttp.WithPropagators(propagation.TraceContext{}),
	)

	mux.Handle("/", handler)

	const address = ":8080"
	logger.Info("Starting HTTP server", "address", address)

	err = http.ListenAndServe(address, mux)
	if err != nil {
		logger.Error("Setting up HTTP listener", "err", err)
	}
}

func tracerProvider(ctx context.Context) (trace.TracerProvider, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		// Otel is not configured, do not enable tracing.
		return noop.NewTracerProvider(), nil
	}

	te, err := otlptracehttp.New(ctx) // This reads OTEL_EXPORTER_OTLP_ENDPOINT from env.
	if err != nil {
		return nil, fmt.Errorf("starting otel exporter: %w", err)
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("crocochrome"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating otel resources: %w", err)
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(te),
		sdktrace.WithResource(res),
	), nil
}

// spanNameFromPattern is a simple middleware that sets the name of the span in the request context to the pattern used
// to match this request.
// This has to be done _after_ http.ServeMux has figured out the route.
func spanNameFromPattern(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Call handler first, so http.ServeMux can populate r.Pattern
		next.ServeHTTP(w, r)
		// Set span name after the fact. As long as this middleware is used within otelhttp.Handler, the span should
		// still be open and thus renameable.
		trace.SpanFromContext(r.Context()).SetName(r.Pattern)
	})
}
