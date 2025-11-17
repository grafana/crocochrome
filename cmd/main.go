package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/grafana/crocochrome"
	crocohttp "github.com/grafana/crocochrome/http"
	"github.com/grafana/crocochrome/internal/version"

	"github.com/grafana/crocochrome/metrics"
	"github.com/prometheus/client_golang/prometheus"
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

	// Add build info metrics, both custom and the standard `go_build_info`.
	metrics.AddVersionMetrics(registry)

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

	crocoHandler := crocohttp.New(logger, supervisor)
	instrumentedHandler := metrics.InstrumentHTTP(registry, crocoHandler)

	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.Handle("/", instrumentedHandler)

	const address = ":8080"
	server := &http.Server{
		Addr:    address,
		Handler: mux,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		logger.Info("Starting HTTP server", "address", address)

		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			logger.Warn("HTTP server shut down")
			return nil // Expected error.
		}

		return err
	})

	eg.Go(func() error {
		const graceTime = 2 * time.Minute

		// Wait for the main context to get canceled. This will typically happen when we receive a signal.
		<-ctx.Done()

		graceCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), graceTime)
		defer cancelShutdown()

		logger.Info("Context cancelled, waiting for existing sessions to finish", "graceTime", graceTime)

		waitCh := make(chan struct{})
		go func() {
			supervisor.Wait()
			close(waitCh)
		}()

		select {
		case <-graceCtx.Done():
			logger.Warn("Sessions did not finish within graceTime", "graceTime", graceTime)
		case <-waitCh:
		}

		// Shut down the HTTP server _after_ all sessions are terminated, and not before. By doing it in this order,
		// there's a chance of a new session being created just before we stop the server, but if we did it the other
		// way around, we wouldn't give clients the ability to terminate them altogether.

		logger.Warn("Existing sessions terminated, shutting down HTTP server", "graceTime", graceTime)

		shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), graceTime)
		defer cancelShutdown()

		return server.Shutdown(shutdownCtx)
	})

	return eg.Wait()
}
