//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/grafana/crocochrome/internal/crocochrome"
)

// TestHeapExporter builds crocochrome with a fast heap probe interval, starts a session carrying
// SM metadata (check id, tenantID, regionID), opens a page target via CDP so the prober has data,
// and asserts that /metrics exposes the V8 + DOM gauges with the expected labels.
func TestHeapExporter(t *testing.T) {
	if testing.Short() {
		t.Skipf("Skipping integration test due to -short")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	image, err := buildImage("..", "crocochrome-heap-exporter")
	if err != nil {
		t.Fatalf("building image: %v", err)
	}

	cc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"8080/tcp"},
			WaitingFor:   wait.ForExposedPort(),
			Cmd:          []string{"--heap-probe-interval=2s"},
			Mounts:       testcontainers.Mounts(testcontainers.VolumeMount("chromium-tmp-heap-exporter", "/chromium-tmp")),
		},
	})
	testcontainers.CleanupContainer(t, cc)
	if err != nil {
		t.Fatalf("starting container: %v", err)
	}

	endpoint, err := cc.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatalf("getting endpoint: %v", err)
	}

	const (
		wantCheckID   = "check-12345"
		wantTenantID  = "98765"
		wantRegionID  = "prod-us-east"
	)

	session, err := createSessionWithMetadata(endpoint, map[string]any{
		"id":       wantCheckID,
		"tenantID": wantTenantID,
		"regionID": wantRegionID,
	})
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { _ = deleteSession(endpoint, session.ID) })

	wsURL := fmt.Sprintf("ws://%s/proxy/%s", strings.TrimPrefix(endpoint, "http://"), session.ID)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		t.Fatalf("dialing CDP: %v", err)
	}
	defer ws.Close()

	// Create a page target so the heap prober has something to measure — otherwise it'd see zero page targets.
	if err := ws.WriteJSON(map[string]any{
		"id":     1,
		"method": "Target.createTarget",
		"params": map[string]any{"url": "about:blank"},
	}); err != nil {
		t.Fatalf("Target.createTarget: %v", err)
	}
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("reading createTarget response: %v", err)
		}
		var resp struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(raw, &resp); err == nil && resp.ID == 1 {
			break
		}
	}

	// Wait for at least two probe cycles so the metric has surely refreshed.
	time.Sleep(5 * time.Second)

	body := fetchMetrics(t, endpoint)

	for _, check := range []struct {
		metric string
		// value must match this regex when found on the metric line.
		valueRegex string
	}{
		{"sm_crocochrome_v8_heap_used_bytes", `[1-9]\d*`},
		{"sm_crocochrome_v8_heap_total_bytes", `[1-9]\d*`},
		{"sm_crocochrome_v8_heap_limit_bytes", `[1-9]\d*`},
		{"sm_crocochrome_v8_embedder_heap_bytes", `\d+`},
		{"sm_crocochrome_v8_backing_store_bytes", `\d+`},
		{"sm_crocochrome_dom_nodes", `\d+`},
		{"sm_crocochrome_dom_documents", `[1-9]\d*`},
		{"sm_crocochrome_dom_event_listeners", `\d+`},
		{"sm_crocochrome_heap_probe_last_success_timestamp_seconds", `[1-9]\d*`},
	} {
		check := check
		t.Run(check.metric, func(t *testing.T) {
			labelClause := fmt.Sprintf(
				`check_id="%s",region_id="%s",tenant_id="%s"`,
				wantCheckID, wantRegionID, wantTenantID,
			)
			// Prometheus sorts labels alphabetically in exposition: check_id,region_id,tenant_id.
			pattern := regexp.QuoteMeta(check.metric) + `\{` + regexp.QuoteMeta(labelClause) + `\} ` + check.valueRegex
			if !regexp.MustCompile(pattern).MatchString(body) {
				matchingLines := findLines(body, check.metric)
				t.Fatalf("metric line matching /%s/ not found.\nLines for this metric:\n%s", pattern, matchingLines)
			}
		})
	}
}

// createSessionWithMetadata posts a CheckInfo body so the session picks up the SM metadata labels.
func createSessionWithMetadata(endpoint string, metadata map[string]any) (*crocochrome.SessionInfo, error) {
	body, err := json.Marshal(crocochrome.CheckInfo{
		Type:     "browser",
		Metadata: metadata,
	})
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(endpoint+"/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("got unexpected status %d", resp.StatusCode)
	}

	var session crocochrome.SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, err
	}
	return &session, nil
}

func fetchMetrics(t *testing.T, endpoint string) string {
	t.Helper()

	resp, err := http.Get(endpoint + "/metrics")
	if err != nil {
		t.Fatalf("requesting /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics returned non-200 status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}
	return string(raw)
}

func findLines(body, prefix string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "(no lines found)"
	}
	return strings.Join(out, "\n")
}
