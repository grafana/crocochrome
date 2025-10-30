package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/grafana/crocochrome"
	crocohttp "github.com/grafana/crocochrome/http"
	"github.com/grafana/crocochrome/internal/version"

	"github.com/grafana/crocochrome/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	UserGroup int
	TempDir   string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	config := &Config{}
	flag.StringVar(&config.TempDir, "temp-dir", "/chromium-tmp", "Directory for chromiumium instances to write their data to")
	flag.IntVar(&config.UserGroup, "user-group", 65534, "Default user to run as. For local development, set this flag to 0")

	flag.Parse()
	if err := run(logger, config); err != nil {
		logger.Error("run failed to execute",
			slog.String("msg", err.Error()))
	}
}

func run(logger *slog.Logger, config *Config) error {
	logger.Info("Starting crocochrome supervisor",
		slog.String("version", version.Short()),
		slog.String("commit", version.Commit()),
		slog.String("timestamp", version.Buildstamp()),
	)

	mux := http.NewServeMux()

	registry := prometheus.NewRegistry()

	supervisor := crocochrome.New(logger, crocochrome.Options{
		ChromiumPath: "chromium",
		// Id for nobody user and group on alpine.
		UserGroup: config.UserGroup,
		// In production we mount an emptyDir here, as opposed to /tmp, and configure chromium to write everything in
		// /chromium-tmp instead. We do this to make sure we are not accidentally allowing things we don't know about
		// to be written, as it is safe to assume that anything writing here (the only writable path) is doing so
		// because we told it to.
		TempDir:      config.TempDir,
		Registry:     registry,
		ExtraUATerms: "GrafanaSyntheticMonitoring",
	})

	err := supervisor.ComputeUserAgent(context.Background())
	if err != nil {
		return fmt.Errorf("could not compute user agent: %w", err)
	}

	server := crocohttp.New(logger, supervisor)
	instrumentedServer := metrics.InstrumentHTTP(registry, server)
	// This adds the bits, but doesn't get the bytes
	registry.MustRegister(collectors.NewBuildInfoCollector())

	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	mux.Handle("/", instrumentedServer)

	const address = ":8080"
	logger.Info("Starting HTTP server", "address", address)

	err = http.ListenAndServe(address, mux)
	if err != nil {
		return fmt.Errorf("could not set up HTTP listener: %w", err)
	}
	return nil
}
