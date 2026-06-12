package crocochrome_test

// Self-exec fake Chromium for the teardown-ordering test.
//
// The supervisor launches Options.ChromiumPath as a subprocess and SIGKILLs it when the
// session context is cancelled. To exercise the real Create -> live session -> Delete
// (collect -> cancel) sequence, this file re-executes the test binary itself as a fake
// Chromium that serves /json/version, /json/list and the per-target CDP WebSocket on the
// debug port. Because the CDP endpoints live *inside* that process, SIGKILL tears them down
// with it: if CDP collection runs while Chromium is alive (before cancel) the dials succeed
// and metrics are logged; if a regression moves collection after cancel, the dials are
// refused and the assertions fail. This is what the decoupled httptest.Server fakes could
// not catch.
//
// The supervisor wipes the child environment (only TMPDIR survives), so the fake mode is
// detected from the launch arguments rather than an env var.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/grafana/crocochrome/internal/testutil"
)

// Page target URLs served by the fake Chromium's /json/list. Tests assert these appear in
// the per-renderer log lines.
const (
	fakeChromiumPage0URL = "https://example.com"
	fakeChromiumPage1URL = "https://other.com"
)

// TestMain re-enters as the fake Chromium when launched with Chromium's debug-port flag,
// otherwise runs the test suite normally.
func TestMain(m *testing.M) {
	if port, ok := fakeChromiumPort(os.Args); ok {
		runFakeChromium(port)
		return // unreachable: runFakeChromium blocks until killed
	}

	os.Exit(m.Run())
}

// fakeChromiumPort extracts the value of --remote-debugging-port=NNNN from the launch
// arguments. Its presence is what distinguishes a supervisor-launched invocation from a
// normal `go test` run.
func fakeChromiumPort(args []string) (string, bool) {
	const flag = "--remote-debugging-port="
	for _, a := range args {
		if port, ok := strings.CutPrefix(a, flag); ok {
			return port, true
		}
	}
	return "", false
}

var fakeChromiumUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// runFakeChromium serves a minimal Chromium debug API on 127.0.0.1:port and blocks until the
// process is killed. It mirrors real Chromium closely enough for chromiumTargets() and the
// CDP Performance collection to succeed.
func runFakeChromium(port string) {
	addr := net.JoinHostPort("127.0.0.1", port)
	base := "ws://" + addr

	mux := http.NewServeMux()

	mux.HandleFunc("/json/version", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]string{
			"Browser":              "HeadlessChrome/124.0.6367.207",
			"Protocol-Version":     "1.3",
			"User-Agent":           "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
			"V8-Version":           "12.4.254.15",
			"WebKit-Version":       "537.36",
			"webSocketDebuggerUrl": base + "/devtools/browser/fake",
		})
	})

	mux.HandleFunc("/json/list", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode([]map[string]string{
			{"id": "page-0", "type": "page", "url": fakeChromiumPage0URL, "webSocketDebuggerUrl": base + "/devtools/page/0"},
			{"id": "page-1", "type": "page", "url": fakeChromiumPage1URL, "webSocketDebuggerUrl": base + "/devtools/page/1"},
			// A browser-type target to verify callers filter to page targets only.
			{"id": "browser", "type": "browser", "url": ""},
		})
	})

	mux.HandleFunc("/devtools/", func(rw http.ResponseWriter, r *http.Request) {
		conn, err := fakeChromiumUpgrader.Upgrade(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		serveFakeCDPSession(conn)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake chromium: listening on %s: %v\n", addr, err)
		os.Exit(1)
	}

	// Serve blocks until the process is killed; that is the whole point.
	_ = http.Serve(ln, mux)
}

// serveFakeCDPSession answers Performance.enable and Performance.getMetrics on a single CDP
// WebSocket connection, returning the metrics from testutil.CDPPerformanceMetrics.
func serveFakeCDPSession(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return // connection closed
		}

		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}

		switch req.Method {
		case "Performance.getMetrics":
			metrics := make([]map[string]any, 0, len(testutil.CDPPerformanceMetrics))
			for name, value := range testutil.CDPPerformanceMetrics {
				metrics = append(metrics, map[string]any{"name": name, "value": value})
			}
			_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": map[string]any{"metrics": metrics}})
		default:
			// Performance.enable and anything else: empty result.
			_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
		}
	}
}

// freeTCPPort returns a port that was free at the moment of the call. There is an inherent
// race between releasing it and the fake Chromium binding it, but it is negligible in tests
// and mirrors how the supervisor is handed a fixed debug port.
func freeTCPPort(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting host/port: %v", err)
	}
	return port
}
