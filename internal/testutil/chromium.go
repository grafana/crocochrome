package testutil

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HTTPInfo starts an http server and returns the port where it is listening. The supplied handler will be used to
// handle requests to /json/version, the endpoint Chromium uses to expose version and WS information.
// The server is automatically closed on t.Cleanup.
func HTTPInfo(t *testing.T, chromiumHandler http.HandlerFunc) string {
	mux := http.NewServeMux()
	mux.Handle("GET /json/version", chromiumHandler)

	server := httptest.NewServer(mux)

	t.Cleanup(server.Close)

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("retrieving fakeInfo port: %v", err)
	}

	return port
}

// ChromiumVersionHandler returns what chromium does.
func ChromiumVersionHandler(rw http.ResponseWriter, r *http.Request) {
	_, _ = rw.Write([]byte(`{
   "Browser": "HeadlessChrome/124.0.6367.207",
   "Protocol-Version": "1.3",
   "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/124.0.6367.207 Safari/537.36",
   "V8-Version": "12.4.254.15",
   "WebKit-Version": "537.36 (@a9001a6e39fbaa559510ca866052950457dd4e6b)",
   "webSocketDebuggerUrl": "ws://foobar:9222/devtools/browser/a279f03c-3afc-4acc-92ba-7134f618fedf"
}`))
}

// InternalServerErrorHandler returns 500 and no body.
func InternalServerErrorHandler(rw http.ResponseWriter, r *http.Request) {
	rw.WriteHeader(http.StatusInternalServerError)
}
