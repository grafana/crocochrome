package http_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/grafana/crocochrome"
	crocohttp "github.com/grafana/crocochrome/http"
	"github.com/grafana/crocochrome/testutil"
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

		defer resp.Body.Close()

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
}
