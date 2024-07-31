package crocochrome

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/grafana/crocochrome/chromium"
)

type Supervisor struct {
	opts    Options
	logger  *slog.Logger
	cclient *chromium.Client
	// sessions is a map from session ID to a cancel function that kills the session when called.
	// Currently, crocochrome ensures that only one session is active at a time, by killing all other active sessions
	// when a new one is creating (by traversing this map).
	// Despite this decision of allowing only one session at a time, we wanted the design of Supervisor and its HTTP
	// API to not be strongly coupled to this decision, hence the use of a map here instead of a single CancelFunc.
	sessions    map[string]context.CancelFunc
	sessionsMtx sync.Mutex
}

type Options struct {
	// ChromiumPath is the path to the chromium executable.
	// Must be specified.
	ChromiumPath string
	// ChromiumPort is the port where chromium will be instructed to listen.
	// Defaults to 5222.
	ChromiumPort string
	// Maximum time a browser is allowed to be running, after which it will be killed unconditionally.
	// Defaults to 5m.
	SessionTimeout time.Duration
	// UserGroup allows running chromium as a different user and group. The same value will be used for both.
	// If UserGroup is 0, chromium will be run as the current user.
	UserGroup int
}

const (
	defaultChromiumPort   = "5222"
	defaultSessionTimeout = 5 * time.Minute
)

const (
	nobodyUIDAlpine = 65534
	nobodyGIDAlpine = 65534
)

func (o Options) withDefaults() Options {
	if o.ChromiumPort == "" {
		o.ChromiumPort = defaultChromiumPort
	}

	if o.SessionTimeout == 0 {
		o.SessionTimeout = defaultSessionTimeout
	}

	return o
}

type SessionInfo struct {
	ID              string                `json:"id"`
	ChromiumVersion *chromium.VersionInfo `json:"chromiumVersion"`
}

func New(logger *slog.Logger, opts Options) *Supervisor {
	return &Supervisor{
		opts:     opts.withDefaults(),
		logger:   logger,
		cclient:  chromium.NewClient(),
		sessions: map[string]context.CancelFunc{},
	}
}

// List returns a list of active session IDs.
// Crocochrome is currently wired to allow only one session at a time, by means of terminating all others when a new one
// is created, but the design of its API try to be agnostic to this decision.
func (s *Supervisor) Sessions() []string {
	s.sessionsMtx.Lock()
	defer s.sessionsMtx.Unlock()

	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}

	return ids
}

// Session creates a new browser session, and returns its information.
// Currently, creating a new session will terminate other existing ones, if present. Clients should not rely on this
// behavior and should delete their sessions when they finish. If a session has to be terminated when a new one is
// created, an error is logged.
func (s *Supervisor) Session() (SessionInfo, error) {
	s.sessionsMtx.Lock()
	defer s.sessionsMtx.Unlock()

	s.killExisting()

	id := randString()
	logger := s.logger.With("sessionID", id)

	ctx, cancel := context.WithTimeout(context.Background(), s.opts.SessionTimeout)
	s.sessions[id] = cancel

	context.AfterFunc(ctx, func() {
		// The session context may be cancelled by calling s.Delete, but may also timeout naturally. Here we call
		// s.Delete to ensure that the session is removed from the map.
		// If the session is deleted by s.Delete, then it will be called again by this function, but that is okay.
		logger.Debug("context cancelled, deleting session")
		s.Delete(id) // AfterFunc runs on a separate goroutine, so we want the mutex-locking version.
	})

	// Launch chromium and wait for it to finish asynchronously.
	go func() {
		logger.Debug("starting session")
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}

		cmd := exec.CommandContext(ctx,
			s.opts.ChromiumPath,
			// The following flags have been tested to be required:
			"--headless",
			"--remote-debugging-address=0.0.0.0",
			"--remote-debugging-port="+s.opts.ChromiumPort,
			"--no-sandbox", // We run a single instance as nobody:nobody, making this redundant.
			// Containers often have a small /dev/shm, causing crashes if chromium uses it.
			// http://crbug.com/715363
			"--disable-dev-shm-usage",

			// The following flags have been added here because they _seemed_ beneficial, but haven't been proved to be
			// needed:
			"--disable-breakpad", "--disable-crash-reporter", // Disable crash reporting.
			"--disable-3d-apis", // Disable webGL and the likes.
			"--disable-audio-input", "--disable-audio-output",
			"--disable-default-apps", // Disables installation of default apps on first run.
			"--disable-extensions",
			"--disable-file-system",
			"--disable-first-run-ui",
			"--disable-notifications",
			"--disable-smooth-scrolling", // No need to burn CPU on this.
		)
		cmd.Env = []string{}
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if s.opts.UserGroup != 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{
					Uid: nobodyUIDAlpine,
					Gid: nobodyGIDAlpine,
				},
			}
		}

		err := cmd.Run()
		if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
			logger.Error("running chromium", "err", err)
			logger.Error("chromium output", "stdout", stdout.String())
			logger.Error("chromium output", "stderr", stderr.String())
			return
		}

		logger.Debug("chromium output", "stdout", stdout.String())
		logger.Debug("chromium output", "stderr", stderr.String())
	}()

	versionCtx, versionCancel := context.WithTimeout(ctx, 2*time.Second)
	defer versionCancel()

	version, err := s.cclient.Version(versionCtx, net.JoinHostPort("localhost", s.opts.ChromiumPort))
	if err != nil {
		logger.Error("could not get chromium info", "err", err)
		s.delete(id) // We were not able to connect to chrome, the session is borked.
		return SessionInfo{}, err
	}

	return SessionInfo{
		ID:              id,
		ChromiumVersion: version,
	}, nil
}

// Delete cancels a session's context and removes it from the map.
func (s *Supervisor) Delete(sessionID string) bool {
	s.sessionsMtx.Lock()
	defer s.sessionsMtx.Unlock()

	return s.delete(sessionID)
}

// delete cancels a session's context and removes it from the map, without locking the mutex.
// It must be used only inside functions that already grab the lock.
func (s *Supervisor) delete(sessionID string) bool {
	if cancelSession, found := s.sessions[sessionID]; found {
		s.logger.Debug("cancelling context and deleting session", "sessionID", sessionID)
		cancelSession()
		delete(s.sessions, sessionID)
		return true
	}

	return false
}

// killExisting cancels all sessions present in the map.
// If a session is cancelled this way, an error is logged.
func (s *Supervisor) killExisting() {
	for id := range s.sessions {
		s.logger.Error("existing session found, killing", "sessionID", id)
		s.delete(id)
	}
}

// randString returns 12 random hex characters.
func randString() string {
	const IDLen = 12
	idBytes := make([]byte, IDLen/2)
	_, err := rand.Read(idBytes)
	if err != nil {
		panic(fmt.Errorf("error reading random bytes, %w", err))
	}

	return hex.EncodeToString(idBytes)
}
