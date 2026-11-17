package log

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

type Flag slog.Level

func (f *Flag) String() string {
	return slog.Level(*f).String()
}

func (f *Flag) Set(s string) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return err
	}

	*f = Flag(level)

	return nil
}

func (f *Flag) Value() slog.Level {
	return slog.Level(*f)
}

// Handler wraps a JSON handler, dynamically adding source code location to
// every log entry while the level is slog.LevelDebug, and to entries at
// level slog.LevelError or higher otherwise. The level can change at
// runtime, and this decision is re-evaluated on every call to Handle.
type Handler struct {
	slog.Handler
	level *slog.LevelVar
}

type HandlerOptions struct {
	Level slog.Level
}

func NewHandler(w io.Writer, opts *HandlerOptions) *Handler {
	level := new(slog.LevelVar)

	if opts != nil {
		level.Set(opts.Level)
	}

	return &Handler{
		Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level: level,
		}),
		level: level,
	}
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) {
		return nil
	}

	if h.level.Level() <= slog.LevelDebug || r.Level >= slog.LevelError {
		if src := r.Source(); src != nil {
			r.AddAttrs(slog.Any(slog.SourceKey, src))
		}
	}

	// Forward the record to the underlying handler
	return h.Handler.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		Handler: h.Handler.WithAttrs(attrs),
		level:   h.level,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		Handler: h.Handler.WithGroup(name),
		level:   h.level,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := slog.New(h.Handler).With(
		slog.String("method", r.Method),
		slog.String("pattern", r.Pattern),
	)

	switch r.Method {
	case http.MethodPost:
		var settings struct {
			Level *slog.Level `json:"level"`
		}

		if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
			httpError(w, "invalid request body", err, http.StatusBadRequest, logger)

			return
		}

		if settings.Level == nil {
			msg := "request does not contain a level field"
			err := errors.New("invalid request body")
			httpError(w, msg, err, http.StatusBadRequest, logger)

			return
		}

		h.level.Set(*settings.Level)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		w.WriteHeader(http.StatusOK)

		if err := json.NewEncoder(w).Encode(&settings); err != nil {
			logger.Error("encoding HTTP response", slog.String("err", err.Error()))

			return
		}

	default:
		msg := fmt.Sprintf("invalid request method %q", r.Method)
		err := errors.New("invalid request method")
		httpError(w, msg, err, http.StatusMethodNotAllowed, logger)
	}
}

func httpError(w http.ResponseWriter, msg string, err error, code int, logger *slog.Logger) {
	resp := struct {
		Msg   string `json:"msg"`
		Error string `json:"error,omitempty"`
		Code  int    `json:"code"`
	}{
		Msg:   msg,
		Code:  code,
		Error: "unknown",
	}

	if err != nil {
		resp.Error = err.Error()
	}

	logger.Warn(msg, slog.String("error", resp.Error), slog.Int("code", resp.Code))

	h := w.Header()

	h.Del("Content-Length") // Recompute the content length based on this response.
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		logger.Warn("failed to encode error response",
			slog.String("err", err.Error()),
			slog.Any("resp", resp),
		)
	}
}
