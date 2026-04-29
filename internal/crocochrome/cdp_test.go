package crocochrome

// White-box unit tests for the CDP collection functions. readCDPResponseInto tests cover
// the message-skipping and deadline paths; chromiumTargets and cdpCollectRendererMetrics
// tests cover target enumeration and per-renderer collection.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grafana/crocochrome/internal/testutil"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// cdpTestServer starts an httptest.Server that accepts a single WebSocket connection,
// invokes fn with the server-side conn, and then closes. It returns the client-side conn.
func cdpTestServer(t *testing.T, fn func(serverConn *websocket.Conn)) *websocket.Conn {
	t.Helper()

	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade error: %v", err)
			return
		}
		close(ready)
		fn(conn)
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	wsURL := "ws" + server.URL[len("http"):]
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	<-ready // wait for the handler to have upgraded before returning

	return clientConn
}

func TestReadCDPResponseInto_skipsUnsolicitedEvents(t *testing.T) {
	// Unsolicited CDP events have id==0 (absent "id" field). They should be skipped
	// and the loop should continue until the response with the expected ID arrives.

	clientConn := cdpTestServer(t, func(serverConn *websocket.Conn) {
		// Unsolicited event (no id field).
		raw, _ := json.Marshal(map[string]any{
			"method": "Network.requestWillBeSent",
			"params": map[string]any{},
		})
		_ = serverConn.WriteMessage(websocket.TextMessage, raw)

		// Real response with id=2.
		_ = serverConn.WriteJSON(map[string]any{
			"id": 2,
			"result": map[string]any{"metrics": []any{
				map[string]any{"name": "Nodes", "value": 42.0},
			}},
		})
	})

	var resp cdpResponse
	if err := readCDPResponseInto(clientConn, 2, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Result.Metrics) != 1 || resp.Result.Metrics[0].Name != "Nodes" {
		t.Fatalf("expected Nodes metric, got %+v", resp.Result.Metrics)
	}
}

func TestReadCDPResponseInto_skipsWrongIDMessages(t *testing.T) {
	// Responses with a different ID should be skipped; the loop waits for the expected one.

	clientConn := cdpTestServer(t, func(serverConn *websocket.Conn) {
		// Wrong-ID response first.
		_ = serverConn.WriteJSON(map[string]any{"id": 99, "result": map[string]any{}})
		// Correct ID response second.
		_ = serverConn.WriteJSON(map[string]any{"id": 1, "result": map[string]any{}})
	})

	if err := readCDPResponseInto(clientConn, 1, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadCDPResponse_returnsCDPError(t *testing.T) {
	clientConn := cdpTestServer(t, func(serverConn *websocket.Conn) {
		_ = serverConn.WriteJSON(map[string]any{
			"id": 1,
			"error": map[string]any{
				"code":    -32000,
				"message": "Performance domain unavailable",
			},
		})
	})

	err := readCDPResponse(clientConn, 1)
	if err == nil {
		t.Fatal("expected CDP error, got nil")
	}
	if !strings.Contains(err.Error(), "Performance domain unavailable") {
		t.Fatalf("expected error message to include CDP error, got %v", err)
	}
}

func TestReadCDPResponseInto_skipsMalformedJSON(t *testing.T) {
	// Malformed JSON frames should be skipped without returning an error.

	clientConn := cdpTestServer(t, func(serverConn *websocket.Conn) {
		_ = serverConn.WriteMessage(websocket.TextMessage, []byte("not-json{{{{"))
		_ = serverConn.WriteJSON(map[string]any{"id": 1, "result": map[string]any{}})
	})

	if err := readCDPResponseInto(clientConn, 1, nil); err != nil {
		t.Fatalf("unexpected error after malformed frame: %v", err)
	}
}

func TestReadCDPResponseInto_timesOutOnInfiniteWrongIDs(t *testing.T) {
	// A misbehaving server that only sends wrong-ID messages must not block forever.
	// The connection deadline (set externally, as cdpPerformanceMetrics does via its
	// context timeout) is the safety net; verify readCDPResponseInto returns an error
	// when the deadline fires rather than looping indefinitely.

	clientConn := cdpTestServer(t, func(serverConn *websocket.Conn) {
		for {
			if err := serverConn.WriteJSON(map[string]any{"id": 99}); err != nil {
				return
			}
		}
	})

	// Simulate the context-driven deadline that cdpPerformanceMetrics applies.
	_ = clientConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))

	err := readCDPResponseInto(clientConn, 1, nil)
	if err == nil {
		t.Fatal("expected deadline error for infinite wrong-ID stream, got nil")
	}
}

// -- chromiumTargets tests --

// browserWSURLForServer returns a ws:// URL that chromiumTargets will use to derive the
// /json/list HTTP URL for the given httptest.Server.
func browserWSURLForServer(server *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(server.URL, "http://") + "/devtools/browser/test"
}

func TestChromiumTargets_returnsAllTargets(t *testing.T) {
	t.Parallel()

	cdpURL := testutil.CDPServer(t)
	targets := []testutil.CDPTargetInfo{
		{URL: "https://example.com", WebSocketDebuggerURL: cdpURL},
		{URL: "https://other.com", WebSocketDebuggerURL: cdpURL},
	}
	server := httptest.NewServer(testutil.ChromiumHandlerWithCDPTargets(cdpURL, targets))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := chromiumTargets(ctx, browserWSURLForServer(server))
	if err != nil {
		t.Fatalf("chromiumTargets() error: %v", err)
	}

	// ChromiumHandlerWithCDPTargets adds one browser-type target, so total = len(targets)+1.
	if len(got) != len(targets)+1 {
		t.Errorf("got %d targets, want %d", len(got), len(targets)+1)
	}
}

func TestChromiumTargets_errorsOnUnreachableHost(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := chromiumTargets(ctx, "ws://127.0.0.1:1/devtools/browser/test")
	if err == nil {
		t.Fatal("expected error for unreachable host, got nil")
	}
}

// -- cdpCollectRendererMetrics tests --

func TestCdpCollectRendererMetrics_collectsFromPageTargetsOnly(t *testing.T) {
	t.Parallel()

	cdpURL := testutil.CDPServer(t)
	pageTargets := []testutil.CDPTargetInfo{
		{URL: "https://example.com", WebSocketDebuggerURL: cdpURL},
		{URL: "https://other.com", WebSocketDebuggerURL: cdpURL},
	}
	server := httptest.NewServer(testutil.ChromiumHandlerWithCDPTargets(cdpURL, pageTargets))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results, err := cdpCollectRendererMetrics(ctx, browserWSURLForServer(server))
	if err != nil {
		t.Fatalf("cdpCollectRendererMetrics() error: %v", err)
	}

	// Only page targets are collected; the browser-type target is filtered out.
	if len(results) != len(pageTargets) {
		t.Fatalf("got %d results, want %d", len(results), len(pageTargets))
	}

	// Verify target URLs and that metrics were collected.
	for i, r := range results {
		if r.TargetURL != pageTargets[i].URL {
			t.Errorf("results[%d].TargetURL = %q, want %q", i, r.TargetURL, pageTargets[i].URL)
		}
		if len(r.Attrs) == 0 {
			t.Errorf("results[%d].Attrs is empty", i)
		}
		// Verify known metric names appear.
		attrKeys := make(map[string]bool)
		for _, a := range r.Attrs {
			attrKeys[a.Key] = true
		}
		for name := range testutil.CDPPerformanceMetrics {
			if !attrKeys[name] {
				t.Errorf("results[%d] missing expected metric %q", i, name)
			}
		}
	}
}

func TestCdpCollectRendererMetrics_continuesOnSingleFailure(t *testing.T) {
	t.Parallel()

	cdpURL := testutil.CDPServer(t)
	// Second target has an unreachable WebSocket URL — should be skipped, not fatal.
	pageTargets := []testutil.CDPTargetInfo{
		{URL: "https://example.com", WebSocketDebuggerURL: cdpURL},
		{URL: "https://dead.com", WebSocketDebuggerURL: "ws://127.0.0.1:1/devtools/page/dead"},
	}
	server := httptest.NewServer(testutil.ChromiumHandlerWithCDPTargets(cdpURL, pageTargets))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results, err := cdpCollectRendererMetrics(ctx, browserWSURLForServer(server))
	if err != nil {
		t.Fatalf("cdpCollectRendererMetrics() returned unexpected error: %v", err)
	}

	// Only the reachable target should appear in results.
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].TargetURL != "https://example.com" {
		t.Errorf("results[0].TargetURL = %q, want %q", results[0].TargetURL, "https://example.com")
	}
}

func TestCdpCollectRendererMetrics_returnsEmptyOnNoPageTargets(t *testing.T) {
	t.Parallel()

	cdpURL := testutil.CDPServer(t)
	// No page targets — just the browser-type target added by ChromiumHandlerWithCDPTargets.
	server := httptest.NewServer(testutil.ChromiumHandlerWithCDPTargets(cdpURL, nil))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	results, err := cdpCollectRendererMetrics(ctx, browserWSURLForServer(server))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}
