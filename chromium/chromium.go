package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

type VersionInfo struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	V8Version            string `json:"V8-Version"`
	WebKitVersion        string `json:"WebKit-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"` // /!\ This one is lowercase in JSON.
}

type Client struct{}

// Version pokes chromium's /json/version endpoint, and returns its response.
// It will retry with exponential backoff until timeout is reached.
func (c *Client) Version(ctx context.Context, address string) (versionInfo *VersionInfo, err error) {
	ctx, versionSpan := trace.SpanFromContext(ctx).TracerProvider().Tracer("").Start(ctx, "fetch chromium version")
	defer versionSpan.End()

	delay := 100 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("retries exhausted trying to reach chromium: %w", ctx.Err())
		default:
		}

		versionInfo, err = c.version(ctx, address)
		if err == nil {
			return // All good
		}

		time.Sleep(delay)
		delay += delay / 2 // Integer version of *1.5.
	}
}

// version pokes /json/version endpoint once.
func (c *Client) version(ctx context.Context, address string) (*VersionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	url := fmt.Sprintf("http://%s/json/version", address)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient(ctx).Do(req)
	if err != nil {
		return nil, fmt.Errorf("making request: %w", err)
	}

	defer resp.Body.Close()

	versionInfo := VersionInfo{}
	err = json.NewDecoder(resp.Body).Decode(&versionInfo)
	if err != nil {
		return nil, fmt.Errorf("decoding response body: %w", err)
	}

	return &versionInfo, nil
}

func (c *Client) httpClient(ctx context.Context) *http.Client {
	return &http.Client{
		Transport: otelhttp.NewTransport(
			http.DefaultTransport,
			otelhttp.WithTracerProvider(trace.SpanFromContext(ctx).TracerProvider()),
			// Span names do not include method and path by default to avoid cardinality explosion with paths containing
			// IDs. As this is not the case with this endpoint, we use a custom formatter that includes both.
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return fmt.Sprintf("%s %s", r.Method, r.URL.Path)
			}),
			// No propagator, as there is no need to send TraceIDs in outgoing requests.
		),
	}
}
