package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/grafana/crocochrome"
	crocohttp "github.com/grafana/crocochrome/http"
	"github.com/grafana/crocochrome/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	mux := http.NewServeMux()

	registry := prometheus.NewRegistry()

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

	err := supervisor.ComputeUserAgent(context.Background())
	if err != nil {
		logger.Error("Computing user agent", "err", err)
		os.Exit(1)
		return
	}

	server := crocohttp.New(logger, supervisor)
	instrumentedServer := metrics.InstrumentHTTP(registry, server)

	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.Handle("/", instrumentedServer)

	const address = ":8080"
	logger.Info("Starting HTTP server", "address", address)

	err = http.ListenAndServe(address, mux)
	if err != nil {
		logger.Error("Setting up HTTP listener", "err", err)
	}
}
