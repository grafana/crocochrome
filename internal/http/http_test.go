package http_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/grafana/crocochrome/internal/crocochrome"
	crocohttp "github.com/grafana/crocochrome/internal/http"
	"github.com/grafana/crocochrome/internal/metrics"
	"github.com/grafana/crocochrome/internal/testutil"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHTTP(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))

	t.Run("creates a session", func(t *testing.T) {
		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})
		api := crocohttp.New(logger, cc)

		server := httptest.NewServer(api)
		t.Cleanup(server.Close)

		resp, err := http.Post(server.URL+"/sessions", "", nil)
		if err != nil {
			t.Fatalf("making request: %v", err)
		}

		defer resp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		var response struct {
			ID              string `json:"ID"`
			ChromiumVersion struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			} `json:"chromiumVersion"`
		}

		err = json.NewDecoder(resp.Body).Decode(&response)
		if err != nil {
			t.Fatalf("decoding response: %v", err)
		}

		if response.ID == "" {
			t.Fatalf("session ID is empty")
		}

		if response.ChromiumVersion.WebSocketDebuggerURL == "" {
			t.Fatalf("webSocketDebuggerUrl is unexpectedly empty")
		}

		parsedURL, err := url.Parse(response.ChromiumVersion.WebSocketDebuggerURL)
		if err != nil {
			t.Fatalf("parsing response url: %v", err)
		}

		if parsedURL.Hostname() != "127.0.0.1" {
			t.Fatalf("expected returned url to have 127.0.0.1 as host, got %q", response.ChromiumVersion.WebSocketDebuggerURL)
		}

		if !strings.HasPrefix(parsedURL.Path, "/proxy/") {
			t.Fatalf("expected returned url to be replaced to /proxy, got %q", parsedURL.String())
		}
	})

	t.Run("acquire creates a session when free", func(t *testing.T) {
		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})
		api := crocohttp.New(logger, cc)

		server := httptest.NewServer(api)
		t.Cleanup(server.Close)

		resp, err := http.Post(server.URL+"/sessions/acquire", "", nil)
		if err != nil {
			t.Fatalf("making request: %v", err)
		}

		defer resp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}

		var response struct {
			ID              string `json:"ID"`
			ChromiumVersion struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			} `json:"chromiumVersion"`
		}

		err = json.NewDecoder(resp.Body).Decode(&response)
		if err != nil {
			t.Fatalf("decoding response: %v", err)
		}

		if response.ID == "" {
			t.Fatalf("session ID is empty")
		}

		if response.ChromiumVersion.WebSocketDebuggerURL == "" {
			t.Fatalf("webSocketDebuggerUrl is unexpectedly empty")
		}
	})

	t.Run("acquire returns 409 when busy and keeps the existing session", func(t *testing.T) {
		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})
		api := crocohttp.New(logger, cc)

		server := httptest.NewServer(api)
		t.Cleanup(server.Close)

		resp, err := http.Post(server.URL+"/sessions", "", nil)
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		var session struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
			t.Fatalf("decoding session: %v", err)
		}

		acquireResp, err := http.Post(server.URL+"/sessions/acquire", "", nil)
		if err != nil {
			t.Fatalf("making acquire request: %v", err)
		}
		defer acquireResp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		if acquireResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected status %d, got %d", http.StatusConflict, acquireResp.StatusCode)
		}

		if list := cc.Sessions(); len(list) != 1 || list[0] != session.ID {
			t.Fatalf("expected sessions list to contain only %q, got %v", session.ID, list)
		}
	})

	t.Run("returns 503 while draining but allows deleting the existing session", func(t *testing.T) {
		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})
		api := crocohttp.New(logger, cc)

		server := httptest.NewServer(api)
		t.Cleanup(server.Close)

		resp, err := http.Post(server.URL+"/sessions", "", nil)
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		var session struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
			t.Fatalf("decoding session: %v", err)
		}

		cc.Drain()

		for _, path := range []string{"/sessions", "/sessions/acquire"} {
			createResp, err := http.Post(server.URL+path, "", nil)
			if err != nil {
				t.Fatalf("making request to %s: %v", path, err)
			}
			createResp.Body.Close() //nolint:errcheck // We can safely ignore this error.

			if createResp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("expected status %d from %s, got %d", http.StatusServiceUnavailable, path, createResp.StatusCode)
			}
		}

		req, err := http.NewRequest(http.MethodDelete, server.URL+"/sessions/"+session.ID, nil)
		if err != nil {
			t.Fatalf("building delete request: %v", err)
		}

		deleteResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("deleting session: %v", err)
		}
		deleteResp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		if deleteResp.StatusCode != http.StatusOK {
			t.Fatalf("expected status %d deleting session while draining, got %d", http.StatusOK, deleteResp.StatusCode)
		}
	})

	t.Run("labels request metrics by code, method and route", func(t *testing.T) {
		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})
		api := crocohttp.New(logger, cc)

		reg := prometheus.NewRegistry()
		server := httptest.NewServer(metrics.InstrumentHTTP(reg, api, crocohttp.Route))
		t.Cleanup(server.Close)

		resp, err := http.Post(server.URL+"/sessions", "", nil)
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		var session struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
			t.Fatalf("decoding session: %v", err)
		}

		req, err := http.NewRequest(http.MethodDelete, server.URL+"/sessions/"+session.ID, nil)
		if err != nil {
			t.Fatalf("building delete request: %v", err)
		}

		deleteResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("deleting session: %v", err)
		}
		deleteResp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		// The delete request must be labeled with the route pattern, not the raw path containing the session ID.
		wantMetric := `# HELP sm_crocochrome_requests_total Total number of requests received
# TYPE sm_crocochrome_requests_total counter
sm_crocochrome_requests_total{code="200",method="delete",route="/sessions/{id}"} 1
sm_crocochrome_requests_total{code="200",method="post",route="/sessions"} 1
`
		if err := promtestutil.GatherAndCompare(reg, strings.NewReader(wantMetric),
			"sm_crocochrome_requests_total"); err != nil {
			t.Errorf("requests counter mismatch: %v", err)
		}
	})

	// Regression test for https://github.com/grafana/crocochrome/issues/519.
	// Chromium 149+ rejects a WS upgrade whose Host header is not an IP or localhost
	// (DevTools DNS-rebinding protection). The proxy must dial Chromium with the
	// backend's Host (127.0.0.1:<port>), not forward the client's Host.
	t.Run("proxy dials chromium with backend Host, not the client's", func(t *testing.T) {
		// Stand up a mock that plays Chromium: it serves /json/version (pointing the
		// debugger URL at itself over an IP host) and a WS endpoint that mimics
		// Chromium 149 by rejecting any non-IP, non-localhost Host.
		var recordedHost atomic.Pointer[string]
		upgrader := websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		}

		mux := http.NewServeMux()
		mux.HandleFunc("/devtools/browser/", func(rw http.ResponseWriter, r *http.Request) {
			host := r.Host
			recordedHost.Store(&host)

			// Mimic Chromium 149's DNS-rebinding protection.
			if !isIPOrLocalhost(host) {
				rw.WriteHeader(http.StatusInternalServerError)
				_, _ = rw.Write([]byte("Host header is specified and is not an IP address or localhost."))
				return
			}

			conn, err := upgrader.Upgrade(rw, r, nil)
			if err != nil {
				return
			}
			_ = conn.Close()
		})

		var wsHost string
		mux.HandleFunc("GET /json/version", func(rw http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(rw, `{"webSocketDebuggerUrl": "ws://%s/devtools/browser/abc"}`, wsHost)
		})

		chromium := httptest.NewServer(mux)
		t.Cleanup(chromium.Close)
		wsHost = chromium.Listener.Addr().String() // 127.0.0.1:<port>

		_, port, err := net.SplitHostPort(wsHost)
		if err != nil {
			t.Fatalf("splitting mock chromium address: %v", err)
		}

		hb := testutil.NewHeartbeat(t)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})
		api := crocohttp.New(logger, cc)

		server := httptest.NewServer(api)
		t.Cleanup(server.Close)

		resp, err := http.Post(server.URL+"/sessions", "", nil)
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // We can safely ignore this error.

		var session struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
			t.Fatalf("decoding session: %v", err)
		}
		if session.ID == "" {
			t.Fatalf("session ID is empty")
		}

		// Connect to the proxy exactly like a CDP client reaching us over a
		// service/DNS name: a non-localhost Host and no Origin.
		proxyURL := "ws://" + server.Listener.Addr().String() + "/proxy/" + session.ID
		header := http.Header{}
		header.Set("Host", "crocochrome-abc:8080")

		conn, wsResp, err := websocket.DefaultDialer.Dial(proxyURL, header)
		if err != nil {
			status := 0
			if wsResp != nil {
				status = wsResp.StatusCode
			}
			t.Fatalf("proxy handshake failed (status %d): %v", status, err)
		}
		_ = conn.Close()

		got := recordedHost.Load()
		if got == nil {
			t.Fatalf("mock chromium never received a connection")
		}
		if *got != wsHost {
			t.Fatalf("chromium received Host %q, want backend host %q", *got, wsHost)
		}
	})
}

// isIPOrLocalhost reports whether the host part of a host[:port] string is an IP
// address or "localhost", mirroring Chromium 149's DevTools Host check.
func isIPOrLocalhost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return host == "localhost" || net.ParseIP(host) != nil
}
