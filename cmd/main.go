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

	supervisor := crocochrome.New(logger, crocochrome.Options{
		ChromiumPath: "chromium",
		UserGroup:    65534, // Id for nobody user and group on alpine.
	})

	server := crocohttp.New(logger, supervisor)

	const address = ":8080"
	logger.Info("Starting HTTP server", "address", address)
	err := http.ListenAndServe(address, server)
	if err != nil {
		logger.Error("Setting up HTTP listener", "err", err)
	}
}
