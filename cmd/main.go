package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/grafana/crocochrome"
	crocohttp "github.com/grafana/crocochrome/http"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	chromiumVersion, err := os.ReadFile("/etc/chromium-version")
	if err != nil {
		logger.Error("reading chromium version")
		return
	}

	logger.Info("Starting crocochrome", "chromiumVersion", string(chromiumVersion))

	supervisor := crocochrome.New(logger, crocochrome.Options{
		ChromiumPath: "chromium",
		// Id for nobody user and group on alpine.
		UserGroup: 65534,
		// In production we mount an emptyDir here, as opposed to /tmp, and configure chromium to write everything in
		// /chromium-tmp instead. We do this to make sure we are not accidentally allowing things we don't know about
		// to be written, as it is safe to assume that anything writing here (the only writable path) is doing so
		// because we told it to.
		TempDir: "/chromium-tmp",
	})

	server := crocohttp.New(logger, supervisor)

	const address = ":8080"
	logger.Info("Starting HTTP server", "address", address)
	err = http.ListenAndServe(address, server)
	if err != nil {
		logger.Error("Setting up HTTP listener", "err", err)
	}
}
