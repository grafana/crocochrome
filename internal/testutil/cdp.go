package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

// CDPTargetInfo describes a fake page target for use with ChromiumHandlerWithCDPTargets.
type CDPTargetInfo struct {
	// URL is the page URL reported in the /json/list response (e.g. "https://example.com").
	URL string
	// WebSocketDebuggerURL is the ws:// endpoint for this target's CDP session.
	// Typically a URL returned by CDPServer.
	WebSocketDebuggerURL string
}

// ChromiumHandlerWithCDPTargets returns an http.HandlerFunc that serves both
// /json/version (with browserWsURL as the browser endpoint) and /json/list (with the
// provided page targets). It also includes a browser-type target in /json/list to verify
// that callers correctly filter to page targets only.
//
// Use CDPServer to create the WebSocket endpoints for the targets, and HTTPInfo to start
// the HTTP server.
func ChromiumHandlerWithCDPTargets(browserWsURL string, pageTargets []CDPTargetInfo) http.HandlerFunc {
	type targetJSON struct {
		Description          string `json:"description"`
		ID                   string `json:"id"`
		Title                string `json:"title"`
		Type                 string `json:"type"`
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl,omitempty"`
	}

	allTargets := make([]targetJSON, 0, len(pageTargets)+1)
	for i, pt := range pageTargets {
		allTargets = append(allTargets, targetJSON{
			ID:                   fmt.Sprintf("fake-page-target-%d", i),
			Type:                 "page",
			Title:                pt.URL,
			URL:                  pt.URL,
			WebSocketDebuggerURL: pt.WebSocketDebuggerURL,
		})
	}
	// Include a browser-type target to verify callers filter to page targets only.
	allTargets = append(allTargets, targetJSON{
		ID:   "fake-browser-target",
		Type: "browser",
		URL:  "",
	})

	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/json/version":
			_ = json.NewEncoder(rw).Encode(map[string]string{
				"Browser":              "HeadlessChrome/124.0.6367.207",
				"Protocol-Version":     "1.3",
				"User-Agent":           "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
				"webSocketDebuggerUrl": browserWsURL,
			})
		case "/json/list", "/json":
			_ = json.NewEncoder(rw).Encode(allTargets)
		default:
			http.NotFound(rw, r)
		}
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// CDPPerformanceMetrics is the set of metrics the fake CDP server returns, exported so
// tests can assert against them.
var CDPPerformanceMetrics = map[string]float64{
	"JSHeapUsedSize":  float64(10 * 1024 * 1024), // 10 MiB
	"JSHeapTotalSize": float64(20 * 1024 * 1024), // 20 MiB
	"Nodes":           127,
	"Documents":       3,
}

// ChromiumVersionHandlerWithCDP returns an http.HandlerFunc that serves /json/version
// responses with webSocketDebuggerUrl pointing to wsURL.
// Use this together with CDPServer to wire up a complete fake Chromium for tests that
// exercise the CDP performance metrics collection path.
func ChromiumVersionHandlerWithCDP(wsURL string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		resp := map[string]string{
			"Browser":              "HeadlessChrome/124.0.6367.207",
			"Protocol-Version":     "1.3",
			"User-Agent":           "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
			"V8-Version":           "12.4.254.15",
			"WebKit-Version":       "537.36",
			"webSocketDebuggerUrl": wsURL,
		}

		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(resp)
	}
}

// CDPServer starts a fake Chromium DevTools Protocol WebSocket server that handles
// Performance.enable and Performance.getMetrics commands. It returns the ws:// URL of the
// server. The server is automatically shut down on t.Cleanup.
//
// The fake server responds to all CDP requests with well-formed responses and emits the
// metric values from CDPPerformanceMetrics.
func CDPServer(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(rw, r, nil)
		if err != nil {
			t.Logf("CDPServer: WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close() //nolint:errcheck

		serveCDPSession(t, conn)
	}))

	t.Cleanup(server.Close)

	return "ws://" + server.Listener.Addr().String() + "/devtools/browser/test-session"
}

// serveCDPSession handles one WebSocket connection, responding to CDP commands.
func serveCDPSession(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return // connection closed; normal teardown
		}

		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Logf("CDPServer: malformed request: %v", err)
			continue
		}

		switch req.Method {
		case "Performance.enable":
			writeJSON(t, conn, map[string]any{"id": req.ID, "result": map[string]any{}})

		case "Performance.getMetrics":
			metrics := make([]map[string]any, 0, len(CDPPerformanceMetrics))
			for name, value := range CDPPerformanceMetrics {
				metrics = append(metrics, map[string]any{"name": name, "value": value})
			}

			writeJSON(t, conn, map[string]any{
				"id":     req.ID,
				"result": map[string]any{"metrics": metrics},
			})

		default:
			// Return an empty result for unknown methods rather than leaving the
			// caller blocked waiting for a response.
			writeJSON(t, conn, map[string]any{"id": req.ID, "result": map[string]any{}})
		}
	}
}

func writeJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()

	if err := conn.WriteJSON(v); err != nil {
		t.Logf("CDPServer: write error: %v", err)
	}
}
