package crocochrome

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

const cdpCollectTimeout = 300 * time.Millisecond

// cdpRequest is a JSON-RPC message sent to the CDP WebSocket.
type cdpRequest struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// cdpResponse is a JSON-RPC response from the CDP WebSocket.
// CDP may also send unsolicited event messages (no ID), which we skip.
type cdpResponse struct {
	ID     int `json:"id"`
	Result struct {
		Metrics []struct {
			Name  string  `json:"name"`
			Value float64 `json:"value"`
		} `json:"metrics"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// chromiumTarget represents a single Chromium DevTools target as returned by /json/list.
type chromiumTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// rendererMetrics holds the CDP Performance.getMetrics results for a single renderer target.
type rendererMetrics struct {
	TargetURL string
	Attrs     []slog.Attr
}

// chromiumTargets fetches all active DevTools targets from Chromium's /json/list endpoint.
// wsURL must be the browser-level WebSocket URL (from /json/version); its host is reused
// to construct the HTTP GET /json/list request.
func chromiumTargets(ctx context.Context, wsURL string) ([]chromiumTarget, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parsing WebSocket URL: %w", err)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return nil, fmt.Errorf("unexpected scheme %q in WebSocket URL", u.Scheme)
	}
	u.Path = "/json/list"
	u.RawQuery = ""
	u.Fragment = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building /json/list request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching /json/list: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var targets []chromiumTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("decoding /json/list response: %w", err)
	}

	return targets, nil
}

// cdpCollectRendererMetrics enumerates all page targets via /json/list and calls
// Performance.getMetrics on each one. All target connections share ctx's deadline so the
// total collection is bounded by a single timeout regardless of how many renderers exist.
// Renderers that fail to connect or respond are silently skipped (best-effort).
func cdpCollectRendererMetrics(ctx context.Context, wsURL string) ([]rendererMetrics, error) {
	targets, err := chromiumTargets(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("listing CDP targets: %w", err)
	}

	var results []rendererMetrics
	for _, target := range targets {
		if target.Type != "page" || target.WebSocketDebuggerURL == "" {
			continue
		}
		attrs, err := cdpPerformanceMetrics(ctx, target.WebSocketDebuggerURL)
		if err != nil {
			continue // best-effort: skip this renderer, continue with others
		}
		results = append(results, rendererMetrics{
			TargetURL: target.URL,
			Attrs:     attrs,
		})
	}

	return results, nil
}

// cdpPerformanceMetrics connects to a single CDP target and collects Performance.getMetrics.
// ctx governs both the WebSocket dial and all subsequent reads — its deadline is propagated
// to the connection's read deadline so ReadMessage is also bounded, not just DialContext.
//
// The call is best-effort: any error (connection refused, Chromium already dead, deadline
// exceeded) is returned so the caller can log at debug level and continue with teardown.
func cdpPerformanceMetrics(ctx context.Context, wsURL string) ([]slog.Attr, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dialing chromium DevTools WebSocket: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort close

	// Propagate the context deadline to the connection so that ReadMessage calls
	// are also bounded. DialContext clears the deadline after the handshake, so
	// we must re-apply it explicitly.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("setting read deadline: %w", err)
		}
	}

	// CDP messages must be matched by ID. We use IDs 1 and 2 and skip any
	// unsolicited events (which have no "id" field or id==0) we receive in between.
	const (
		idEnable     = 1
		idGetMetrics = 2
	)

	// Step 1: enable the Performance domain.
	if err := conn.WriteJSON(cdpRequest{
		ID:     idEnable,
		Method: "Performance.enable",
		Params: map[string]any{"timeDomain": "timeTicks"},
	}); err != nil {
		return nil, fmt.Errorf("sending Performance.enable: %w", err)
	}

	if err := readCDPResponse(conn, idEnable); err != nil {
		return nil, fmt.Errorf("reading Performance.enable response: %w", err)
	}

	// Step 2: collect metrics.
	if err := conn.WriteJSON(cdpRequest{ID: idGetMetrics, Method: "Performance.getMetrics"}); err != nil {
		return nil, fmt.Errorf("sending Performance.getMetrics: %w", err)
	}

	var resp cdpResponse
	if err := readCDPResponseInto(conn, idGetMetrics, &resp); err != nil {
		return nil, fmt.Errorf("reading Performance.getMetrics response: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("CDP error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	attrs := make([]slog.Attr, 0, len(resp.Result.Metrics))
	for _, m := range resp.Result.Metrics {
		attrs = append(attrs, slog.Attr{Key: m.Name, Value: slog.Float64Value(m.Value)})
	}

	return attrs, nil
}

// readCDPResponse reads CDP messages from conn until it sees one with the expected ID,
// discarding unsolicited event messages (id == 0 or missing). Returns an error if the
// response contains a CDP-level error.
func readCDPResponse(conn *websocket.Conn, expectedID int) error {
	return readCDPResponseInto(conn, expectedID, nil)
}

// readCDPResponseInto is like readCDPResponse but unmarshals the matched message into dst
// when dst is non-nil.
func readCDPResponseInto(conn *websocket.Conn, expectedID int, dst *cdpResponse) error {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("reading WebSocket message: %w", err)
		}

		// Peek at the ID without fully decoding yet.
		var peek struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			continue // malformed message; skip
		}

		if peek.ID == 0 {
			continue // unsolicited CDP event; skip
		}

		if peek.ID != expectedID {
			continue // response to a different pending command; skip
		}

		if dst != nil {
			if err := json.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("unmarshalling CDP response: %w", err)
			}
		}

		return nil
	}
}
