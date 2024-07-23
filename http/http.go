package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"path"

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

	// Replace chromium's listen url with our proxy's for this session.
	// Copy request's url to use the same scheme, host and port the client used to connect to us, as it is guaranteed
	// that's correct.
	proxyUrl := url.URL{
		Scheme: "ws",
		Host:   r.Host, // r.Host includes port if non-standard.
		Path:   path.Join("proxy", session.ID),
	}
	session.ChromiumVersion.WebSocketDebuggerURL = proxyUrl.String()

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
