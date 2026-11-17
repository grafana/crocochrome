package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFlag_String(t *testing.T) {
	f := Flag(slog.LevelWarn)

	if got, want := f.String(), slog.LevelWarn.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestFlag_Value(t *testing.T) {
	f := Flag(slog.LevelError)

	if got := f.Value(); got != slog.LevelError {
		t.Errorf("Value() = %v, want %v", got, slog.LevelError)
	}
}

func TestFlag_Set(t *testing.T) {
	t.Run("parses a valid level and updates the value", func(t *testing.T) {
		f := Flag(slog.LevelInfo)

		if err := f.Set("warn"); err != nil {
			t.Fatalf("Set() returned an error: %v", err)
		}

		if got, want := f.Value(), slog.LevelWarn; got != want {
			t.Errorf("Value() = %v, want %v", got, want)
		}
	})

	t.Run("returns an error for an invalid level", func(t *testing.T) {
		f := Flag(slog.LevelInfo)

		if err := f.Set("not-a-level"); err == nil {
			t.Error("Set() returned no error, want one")
		}
	})

	t.Run("defaults to info when never set", func(t *testing.T) {
		var f Flag

		if got, want := f.Value(), slog.LevelInfo; got != want {
			t.Errorf("Value() = %v, want %v", got, want)
		}
	})
}

func TestNewHandler(t *testing.T) {
	t.Run("adds source for every record when the level is debug or lower", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, &HandlerOptions{Level: slog.LevelDebug}))

		logger.Debug("hello")
		logger.Warn("world")

		lines := decodeLines(t, buf.Bytes())
		if len(lines) != 2 {
			t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
		}

		for _, line := range lines {
			if _, ok := line["source"]; !ok {
				t.Errorf("expected a source attribute, got %v", line)
			}
		}
	})

	t.Run("omits source for levels below error when the level is above debug", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo}))

		logger.Info("hello")
		logger.Warn("world")

		lines := decodeLines(t, buf.Bytes())
		if len(lines) != 2 {
			t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
		}

		for _, line := range lines {
			if _, ok := line["source"]; ok {
				t.Errorf("expected no source attribute, got %v", line)
			}
		}
	})

	t.Run("adds source for error records when the level is above debug", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo}))

		logger.Error("boom")

		line := decodeLines(t, buf.Bytes())[0]

		src, ok := line["source"].(map[string]any)
		if !ok {
			t.Fatalf("expected a source attribute, got %v", line)
		}

		if src["file"] == "" || src["line"] == 0.0 {
			t.Errorf("expected a populated source, got %v", src)
		}
	})

	t.Run("respects the configured level", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, &HandlerOptions{Level: slog.LevelWarn}))

		logger.Info("hello")

		if buf.Len() != 0 {
			t.Errorf("expected nothing to be written, got %q", buf.String())
		}
	})

	t.Run("treats a nil options struct like the zero value", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, nil))

		logger.Error("boom")

		line := decodeLines(t, buf.Bytes())[0]
		if _, ok := line["source"]; !ok {
			t.Errorf("expected a source attribute, got %v", line)
		}
	})

	t.Run("does not add a source attribute for a record without a program counter", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo})

		r := slog.NewRecord(time.Now(), slog.LevelError, "manual", 0)
		if err := h.Handle(context.Background(), r); err != nil {
			t.Fatalf("Handle() returned an error: %v", err)
		}

		line := decodeLines(t, buf.Bytes())[0]
		if _, ok := line["source"]; ok {
			t.Errorf("expected no source attribute, got %v", line)
		}
	})

	t.Run("still adds source for errors after With", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo}))

		logger.With("sessionID", "abc").Error("boom")

		line := decodeLines(t, buf.Bytes())[0]
		if _, ok := line["source"]; !ok {
			t.Errorf("expected a source attribute, got %v", line)
		}
		if line["sessionID"] != "abc" {
			t.Errorf("sessionID = %v, want %q", line["sessionID"], "abc")
		}
	})

	t.Run("still adds source for errors after WithGroup", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo}))

		logger.WithGroup("session").Error("boom")

		line := decodeLines(t, buf.Bytes())[0]

		// Like any attribute added to the record, the manually-added source
		// attribute is nested under the active group.
		group, ok := line["session"].(map[string]any)
		if !ok {
			t.Fatalf("expected a session group, got %v", line)
		}
		if _, ok := group["source"]; !ok {
			t.Errorf("expected a source attribute, got %v", group)
		}
	})
}

func TestHandler_ServeHTTP(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, nil)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/log-level", nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}

		resp := decodeErrorResponse(t, rec.Body.Bytes())
		if want := `invalid request method "GET"`; resp.Msg != want {
			t.Errorf("msg = %q, want %q", resp.Msg, want)
		}
		if resp.Error == "" {
			t.Error("expected a non-empty error message")
		}

		assertJSONHeaders(t, rec.Header())
	})

	t.Run("rejects a malformed request body", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, nil)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader("not json"))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}

		resp := decodeErrorResponse(t, rec.Body.Bytes())
		if resp.Msg != "invalid request body" {
			t.Errorf("msg = %q, want %q", resp.Msg, "invalid request body")
		}
		if resp.Error == "" {
			t.Error("expected a non-empty error message")
		}

		assertJSONHeaders(t, rec.Header())
	})

	t.Run("rejects an unknown level name", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, nil)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader(`{"level":"bogus"}`))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("rejects a request without a level field", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, &HandlerOptions{Level: slog.LevelWarn})
		logger := slog.New(h)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader(`{}`))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}

		resp := decodeErrorResponse(t, rec.Body.Bytes())
		if want := "request does not contain a level field"; resp.Msg != want {
			t.Errorf("msg = %q, want %q", resp.Msg, want)
		}
		if resp.Error == "" {
			t.Error("expected a non-empty error message")
		}

		buf.Reset()
		logger.Info("still filtered")

		if lines := decodeLines(t, buf.Bytes()); len(lines) != 0 {
			t.Errorf("expected the level to be untouched, got %q", buf.String())
		}
	})

	t.Run("updates the level and echoes it back", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo})
		logger := slog.New(h)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader(`{"level":"WARN"}`))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		if ct, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; ct != want {
			t.Errorf("Content-Type = %q, want %q", ct, want)
		}

		var got struct {
			Level slog.Level `json:"level"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding response body: %v\nbody: %s", err, rec.Body.Bytes())
		}
		if got.Level != slog.LevelWarn {
			t.Errorf("Level = %v, want %v", got.Level, slog.LevelWarn)
		}

		buf.Reset()
		logger.Info("filtered")
		logger.Warn("kept")

		lines := decodeLines(t, buf.Bytes())
		if len(lines) != 1 {
			t.Fatalf("expected 1 line after raising the level, got %d: %q", len(lines), buf.String())
		}
		if lines[0]["msg"] != "kept" {
			t.Errorf("msg = %v, want %q", lines[0]["msg"], "kept")
		}
	})

	t.Run("raising the level to debug enables source immediately", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, &HandlerOptions{Level: slog.LevelInfo})
		logger := slog.New(h)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader(`{"level":"DEBUG"}`))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		buf.Reset()
		logger.Debug("hello")

		line := decodeLines(t, buf.Bytes())[0]
		if _, ok := line["source"]; !ok {
			t.Errorf("expected a source attribute, got %v", line)
		}
	})

	t.Run("leaves the level untouched when the request is rejected", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, &HandlerOptions{Level: slog.LevelWarn})
		logger := slog.New(h)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader("not json"))
		h.ServeHTTP(rec, req)

		buf.Reset()
		logger.Info("still filtered")

		if lines := decodeLines(t, buf.Bytes()); len(lines) != 0 {
			t.Errorf("expected no output, got %q", buf.String())
		}
	})

	t.Run("logs an error through the wrapped handler when encoding the response fails", func(t *testing.T) {
		var buf bytes.Buffer
		h := NewHandler(&buf, nil)

		req := httptest.NewRequest(http.MethodPost, "/log-level", strings.NewReader(`{"level":"WARN"}`))
		w := newFailingWriter()
		h.ServeHTTP(w, req)

		if w.status != http.StatusOK {
			t.Errorf("status = %d, want %d", w.status, http.StatusOK)
		}

		lines := decodeLines(t, buf.Bytes())
		if len(lines) != 1 {
			t.Fatalf("expected 1 log line, got %d: %q", len(lines), buf.String())
		}

		line := lines[0]
		if line["msg"] != "encoding HTTP response" {
			t.Errorf("msg = %v, want %q", line["msg"], "encoding HTTP response")
		}
		if line["method"] != http.MethodPost {
			t.Errorf("method = %v, want %q", line["method"], http.MethodPost)
		}
		// r.Pattern is only populated by http.ServeMux when a request is
		// routed through it; calling ServeHTTP directly, as this test does,
		// leaves it empty.
		if _, ok := line["pattern"]; !ok {
			t.Errorf("expected a pattern attribute, got %v", line)
		}
		if s, _ := line["err"].(string); s == "" {
			t.Errorf("expected a non-empty err attribute, got %v", line["err"])
		}
	})
}

type errorResponse struct {
	Msg   string `json:"msg"`
	Error string `json:"error,omitempty"`
	Code  int    `json:"code"`
}

func decodeErrorResponse(t *testing.T, b []byte) errorResponse {
	t.Helper()

	var resp errorResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("decoding error response: %v\nbody: %s", err, b)
	}

	return resp
}

func assertJSONHeaders(t *testing.T, h http.Header) {
	t.Helper()

	if got, want := h.Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := h.Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
}

// failingWriter is an http.ResponseWriter whose Write always fails, used to
// exercise ServeHTTP's fallback logging when the response can't be encoded.
type failingWriter struct {
	header http.Header
	status int
}

func newFailingWriter() *failingWriter {
	return &failingWriter{header: make(http.Header)}
}

func (w *failingWriter) Header() http.Header { return w.header }

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func (w *failingWriter) WriteHeader(status int) {
	w.status = status
}

// decodeLines parses each newline-delimited JSON object in b, in order.
func decodeLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()

	var lines []map[string]any

	for _, l := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		if len(l) == 0 {
			continue
		}

		var line map[string]any
		if err := json.Unmarshal(l, &line); err != nil {
			t.Fatalf("decoding output line: %v\ninput: %s", err, l)
		}

		lines = append(lines, line)
	}

	return lines
}
