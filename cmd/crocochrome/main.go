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

	"github.com/grafana/crocochrome/internal/crocochrome"
	crocohttp "github.com/grafana/crocochrome/internal/http"
	"github.com/grafana/crocochrome/internal/metrics"
	"github.com/grafana/crocochrome/internal/version"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	UserGroup            int
	TempDir              string
	EnableProcessMetrics bool
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	config := &Config{}
	flag.StringVar(&config.TempDir, "temp-dir", "/chromium-tmp", "Directory for chromiumium instances to write their data to")
	flag.IntVar(&config.UserGroup, "user-group", 65534, "Default user to run as. For local development, set this flag to 0")
	flag.BoolVar(&config.EnableProcessMetrics, "process-metrics", false, "Enable per-process RSS collection at session teardown. Adds negligible overhead.")

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
		TempDir:              config.TempDir,
		Registry:             registry,
		ExtraUATerms:         "GrafanaSyntheticMonitoring",
		EnableProcessMetrics: config.EnableProcessMetrics,
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
		// graceTime bounds how long we wait for the active session after draining. Once draining, a session's
		// remaining lifetime is capped by its own timeout, so SessionTimeout plus a teardown margin is sufficient
		// by construction.
		// This value has implications outside the crocochrome implementation itself: the environment must allow
		// the process at least graceTime + httpShutdownGraceTime to shut down after SIGTERM. On Kubernetes, that
		// means terminationGracePeriodSeconds >= graceTime + httpShutdownGraceTime (~360s with the default 5m
		// session timeout); the k8s default of 30s would SIGKILL the process long before this grace elapses.
		graceTime := supervisor.SessionTimeout() + 30*time.Second
		// httpShutdownGraceTime bounds flushing in-flight HTTP requests after all sessions have ended, which is
		// unrelated to session lifetime.
		const httpShutdownGraceTime = 10 * time.Second

		// Wait for the main context to get canceled. This will typically happen when we receive a signal.
		<-ctx.Done()

		// Stop accepting new sessions before waiting for existing ones, so the session count can only decrease
		// from this point on.
		supervisor.Drain()

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

		// Shut down the HTTP server _after_ all sessions are terminated, and not before, so clients keep the
		// ability to terminate them during the drain.

		logger.Warn("Existing sessions terminated, shutting down HTTP server", "httpShutdownGrace", httpShutdownGraceTime)

		shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), httpShutdownGraceTime)
		defer cancelShutdown()

		return server.Shutdown(shutdownCtx)
	})

	return eg.Wait()
}
