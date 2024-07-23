package http

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"github.com/grafana/crocochrome"
	"github.com/koding/websocketproxy"
)

type Server struct {
	logger     *slog.Logger
	supervisor *crocochrome.Supervisor
	mux        *http.ServeMux
}

func New(logger *slog.Logger, supervisor *crocochrome.Supervisor) *Server {
	mux := http.NewServeMux()

	api := &Server{
		logger:     logger,
		supervisor: supervisor,
		mux:        mux,
	}

	mux.HandleFunc("GET /sessions", api.List)
	mux.HandleFunc("POST /sessions", api.Create)
	mux.HandleFunc("DELETE /sessions/{id}", api.Delete)
	mux.HandleFunc("/proxy/{id}", api.Proxy)

	return api
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	s.logger.Debug("handling request", "method", r.Method, "path", r.URL.Path)
	s.mux.ServeHTTP(rw, r)
}

func (s *Server) List(rw http.ResponseWriter, r *http.Request) {
	list := s.supervisor.Sessions()

	rw.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(rw).Encode(list)
}

func (s *Server) Create(rw http.ResponseWriter, r *http.Request) {
	session, err := s.supervisor.Create()
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	// Chromium is guaranteed to run on the same host as this server, and we make it listen in 0.0.0.0. However,
	// chromium always returns `localhost` as the URL host. Here we replace `localhost` in that URL with the host being
	// used to reach this service.
	newURL, err := replaceHost(session.ChromiumVersion.WebSocketDebuggerURL, r.Host)
	if err != nil {
		s.logger.Error("replacing chromium url with host header", "err", err)
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}

	session.ChromiumVersion.WebSocketDebuggerURL = newURL

	rw.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(rw).Encode(session)
}

func (s *Server) Delete(rw http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	found := s.supervisor.Delete(sessionID)
	if !found {
		rw.WriteHeader(http.StatusNotFound)
		return
	}
}

// Proxy checks an open session for the given session ID (from path) and proxies the request to the URL present in that
// session.
// This is needed as recent versions of chromium do not support listening in addresses other than localhost, so to make
// chromium reachable from the outside we need to proxy it.
func (s *Server) Proxy(rw http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	sessionInfo := s.supervisor.Session(sessionID)
	if sessionInfo == nil {
		s.logger.Warn("sessionID not found", "sessionID", sessionID)
		rw.WriteHeader(http.StatusNotFound)
		return
	}

	rawUrl := sessionInfo.ChromiumVersion.WebSocketDebuggerURL
	chromiumURL, err := url.Parse(rawUrl)
	if err != nil {
		s.logger.Warn("could not parse ws URL form chromium response", "sessionID", sessionID, "url", rawUrl, "err", err)
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}

	s.logger.Debug("Proxying WS connection", "sessionID", sessionID, "chromiumURL", rawUrl)

	wsp := websocketproxy.WebsocketProxy{
		Backend: func(r *http.Request) *url.URL {
			return chromiumURL
		},
	}
	wsp.ServeHTTP(rw, r)
}

// replaceHost returns a new url with its hostname replaced with host. The port is kept as it is.
func replaceHost(urlStr, host string) (string, error) {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}

	_, port, err := net.SplitHostPort(parsedURL.Host)
	if err != nil {
		return "", err
	}

	// Get rid of the port if a port is present in host.
	host, _, err = net.SplitHostPort(host)
	if err != nil {
		return "", err
	}

	parsedURL.Host = net.JoinHostPort(host, port)

	return parsedURL.String(), nil
}
