package crocochrome

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grafana/crocochrome/internal/chromium"
	"github.com/grafana/crocochrome/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// ErrSessionExists is returned by CreateIfFree when a session is already active.
	ErrSessionExists = errors.New("a session already exists")

	// ErrDraining is returned by Create and CreateIfFree after Drain has been called.
	ErrDraining = errors.New("shutting down, not accepting new sessions")
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
	sessions    map[string]session
	sessionsMtx sync.Mutex
	metrics     *metrics.SupervisorMetrics
	// userAgent stores a "patched" user agent derived from the default UA for the installed chromium. We "patch" the
	// default user agent so chromium does not identify itself as headless. In order to do that, we have to run chromium
	// once, get its user agent, and then make modifications. After doing that process, we store the result here.
	// ComputeUserAgent() performs this process.
	userAgent string
	// wg stores a WaitGroup. This WaitGroup is used to track the number of open sessions, and Supervisor.Wait relies on
	// it to work.
	wg *sync.WaitGroup
	// draining, when set, makes session creation fail with ErrDraining. Existing sessions are unaffected. It is set by
	// Drain during graceful shutdown and never unset.
	draining atomic.Bool
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
	// TempDir is the path to a writable directory where folders for chromium processes will be stored.
	TempDir string
	// Registry is a prometheus registerer for telemetry.
	Registry prometheus.Registerer
	// ExtraUATerms is appended, after a space, to the chromium user agent. It can be used to add vendor-specific
	// information to it, such as the name of the product using chromium to perform requests.
	ExtraUATerms string
	// CgroupMemoryEventsPath is the path to the cgroup memory events file used to detect OOM kills.
	// If empty, the path is auto-detected (cgroupsv2: /sys/fs/cgroup/memory.events,
	// cgroupsv1: /sys/fs/cgroup/memory/<hierarchy>/memory.oom_control).
	// Set to a non-existent path to disable OOM detection (e.g. in tests that do not need it).
	CgroupMemoryEventsPath string
	// EnableProcessMetrics enables per-process RSS collection at session teardown.
	// When true, Delete() walks cgroup.procs and reads VmRSS and VmHWM from
	// /proc/<pid>/status for each process, emitting "chromium process memory" log entries
	// and a "chromium session memory" summary with the cgroup-level total.
	// This is fast (file reads only, no network) and adds negligible overhead to DELETE.
	// Disabled by default; enable via the -process-metrics flag.
	EnableProcessMetrics bool
	// ProcFSRoot is the root of the proc filesystem used for per-process RSS and cmdline reads.
	// Defaults to "/proc". Override in tests to point at a temp directory.
	ProcFSRoot string
}

const (
	defaultChromiumPort   = "5222"
	defaultSessionTimeout = 5 * time.Minute
)

// allowedLabels are the metadata keys we enrich session logs with. tenantID and (check) id are regionally unique;
// the combination of the three labels allows us to connect logs to the specific organization responsible for the check.
// Note: the casing of regionID and tenantID does not match the protobuf definition (regionId, tenantId) because
// CheckInfoFromSM in the synthetic-monitoring-agent encodes them this way:
// https://github.com/grafana/synthetic-monitoring-agent/blob/main/internal/k6runner/k6runner.go#L76-L77
var allowedLabels = []string{"regionID", "tenantID", "id"}

func (o Options) withDefaults() Options {
	if o.ChromiumPort == "" {
		o.ChromiumPort = defaultChromiumPort
	}

	if o.SessionTimeout == 0 {
		o.SessionTimeout = defaultSessionTimeout
	}

	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}

	if o.Registry == nil {
		o.Registry = prometheus.NewRegistry() // Empty, unused.
	}

	if o.ProcFSRoot == "" {
		o.ProcFSRoot = "/proc"
	}

	if o.CgroupMemoryEventsPath == "" {
		o.CgroupMemoryEventsPath = detectCgroupMemoryEventsPath(o.ProcFSRoot)
	}

	return o
}

// CheckInfo holds information about the SM check that triggered this session.
// This mirrors the CheckInfo type in sm-k6-runner.
type CheckInfo struct {
	Type     string         `json:"type"`
	Metadata map[string]any `json:"metadata"`
}

// SessionInfo contains the ID and chromium info for a session.
type SessionInfo struct {
	ID              string               `json:"id"`
	ChromiumVersion chromium.VersionInfo `json:"chromiumVersion"`
}

// session contains the session data associated with a session ID.
type session struct {
	// info is returned to clients when they provide an ID.
	info *SessionInfo
	// cancel is the CancelFunc for this session, called on Delete and when the session times out.
	cancel context.CancelFunc
	// logger is the session-scoped logger enriched with sessionID, regionID, tenantID, and check id.
	// Constructed in Create and used in goroutines that outlive it (e.g. Delete).
	logger *slog.Logger
}

func New(logger *slog.Logger, opts Options) *Supervisor {
	opts = opts.withDefaults()

	return &Supervisor{
		opts:     opts,
		logger:   logger,
		cclient:  chromium.NewClient(),
		sessions: map[string]session{},
		metrics:  metrics.Supervisor(opts.Registry),
		wg:       &sync.WaitGroup{},
	}
}

// Session returns an existing session with the given ID. If the session does not exist, either because it has expired
// or because it has not been created, Session returns nil.
func (s *Supervisor) Session(id string) *SessionInfo {
	s.sessionsMtx.Lock()
	defer s.sessionsMtx.Unlock()

	if sess, found := s.sessions[id]; found {
		return sess.info
	}

	return nil
}

// Sessions returns a list of active session IDs.
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

// Create creates a new browser session, and returns its information.
// Currently, creating a new session will terminate other existing ones, if present. Clients should not rely on this
// behavior and should delete their sessions when they finish. If a session has to be terminated when a new one is
// created, an error is logged.
func (s *Supervisor) Create(checkInfo CheckInfo) (SessionInfo, error) {
	return s.create(checkInfo, false)
}

// CreateIfFree creates a new browser session only if no session is currently active, returning ErrSessionExists
// otherwise. Unlike Create, it never terminates an existing session.
func (s *Supervisor) CreateIfFree(checkInfo CheckInfo) (SessionInfo, error) {
	return s.create(checkInfo, true)
}

// create creates a new browser session. If ifFree is true and a session already exists, it returns ErrSessionExists;
// otherwise existing sessions are terminated before creating the new one.
func (s *Supervisor) create(checkInfo CheckInfo, ifFree bool) (SessionInfo, error) {
	s.sessionsMtx.Lock()
	defer s.sessionsMtx.Unlock()

	if s.draining.Load() {
		return SessionInfo{}, ErrDraining
	}

	if ifFree && len(s.sessions) > 0 {
		return SessionInfo{}, ErrSessionExists
	}

	s.killExisting()

	id := randString()
	logger := s.logger.With("sessionID", id)

	for k, v := range checkInfo.Metadata {
		if slices.Contains(allowedLabels, k) {
			logger = logger.With(k, v)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.opts.SessionTimeout)

	// Sample the cgroup OOM kill counter before launching Chromium so we
	// can detect any OOM kills that occur during this session (as opposed
	// to prior sessions). This is done synchronously, before the launch
	// goroutine starts, so the baseline is guaranteed to reflect the state
	// at the moment Create is called rather than racing with the goroutine
	// being scheduled.
	oomBefore, oomBeforeErr := readOOMKillCount(s.opts.CgroupMemoryEventsPath)
	if oomBeforeErr != nil {
		logger.Warn("could not read cgroup OOM kill count before session", "err", oomBeforeErr)
	}

	s.wg.Add(1)

	// Register a function that removes the session from the map when the context is cancelled.
	// It is okay to register this before adding the session to the map as s.Delete is a no-op if the session does not
	// exist.
	context.AfterFunc(ctx, func() {
		// The session context may be cancelled by calling s.Delete, but may also timeout naturally. This function calls
		// s.Delete to ensure we remove the session from the map on the natural timeout case, which means that s.Delete
		// will be called a second time by this function if called manually. This is fine, as s.Delete is a no-op if the
		// session has already been removed.
		logger.Debug("context cancelled, removing session from the map")
		s.Delete(id) // AfterFunc runs on a separate goroutine, so we want the mutex-locking version.
	})

	// Launch chromium and wait for it to finish asynchronously.
	//
	// We do not wait for errors, as we probe chromium below. If something
	// went wrong, we error out there.
	go func() {
		defer s.wg.Done()
		err := s.launch(ctx, logger, oomBefore, oomBeforeErr)
		if err != nil {
			logger.Error("launching chromium", "err", err)
		}
	}()

	versionCtx, versionCancel := context.WithTimeout(ctx, 2*time.Second)
	defer versionCancel()

	version, err := s.cclient.Version(versionCtx, net.JoinHostPort("localhost", s.opts.ChromiumPort))
	if err != nil {
		// We were not able to connect to chrome, the session is toast.
		logger.Error("could not get chromium info, cancelling session", "err", err)
		cancel()

		return SessionInfo{}, err
	}

	si := SessionInfo{
		ID:              id,
		ChromiumVersion: *version,
	}

	s.sessions[id] = session{
		cancel: cancel,
		info:   &si,
		logger: logger,
	}

	s.setSessionActive()

	return si, nil
}

// Delete collects teardown observability data and cancels the session context (sending
// SIGKILL to Chromium).
//
// Concurrency: takeSession atomically removes the session from the map AND cancels its
// context under the same lock. This eliminates the race window where a concurrent Create
// could find an empty map (no session to kill) and attempt to start a new Chromium on
// the same port while the old one was still alive. Any concurrent caller sees either the
// session in the map (and kills it via killExisting) or the session gone with SIGKILL
// already in flight. No intermediate state is observable.
//
// Observability collection happens after cancel. SIGKILL is non-blocking: the kernel
// schedules process cleanup but does not wait for it, so Chromium and its subprocesses
// remain present in /proc and cgroup.procs for a brief window after the signal is sent.
// emitTeardownObservability reads during this window and treats ENOENT (process exited
// before read) as an expected race, skipping silently.
func (s *Supervisor) Delete(sessionID string) bool {
	sess, found := s.takeSession(sessionID)
	if !found {
		return false
	}

	s.emitTeardownObservability(sess)

	return true
}

// takeSession atomically removes the session from the map, cancels its context (sending
// SIGKILL to Chromium), and returns the session. Both operations happen under the same
// lock so no caller can observe the session gone from the map without SIGKILL already
// having been sent. Returns (session{}, false) if no session with that ID exists.
func (s *Supervisor) takeSession(sessionID string) (session, bool) {
	s.sessionsMtx.Lock()
	defer s.sessionsMtx.Unlock()

	sess, found := s.sessions[sessionID]
	if !found {
		return session{}, false
	}

	delete(s.sessions, sessionID)
	s.setSessionInactive()
	sess.cancel()

	return sess, true
}

// emitTeardownObservability collects and emits process metrics for the given session.
// Called after takeSession (SIGKILL already sent) so the window in which process data is
// readable is brief. ENOENT on any /proc read is treated as the process having already
// exited and is skipped silently.
// All collection is best-effort; errors are logged at Debug and do not affect teardown.
func (s *Supervisor) emitTeardownObservability(sess session) {
	if !s.opts.EnableProcessMetrics {
		return
	}

	processes, cgroupRSS, procErr := collectProcessMetrics(s.opts.CgroupMemoryEventsPath, s.opts.ProcFSRoot)
	if procErr != nil {
		sess.logger.Debug("could not collect process metrics", "err", procErr)
	}

	// context.Background() is intentional: the session context is already cancelled
	// (sess.cancel was called in takeSession) and some slog handler implementations
	// suppress output when the provided context is done.
	for _, p := range processes {
		sess.logger.LogAttrs(context.Background(), slog.LevelInfo, "chromium process memory",
			slog.Int("pid", p.PID),
			slog.String("processType", p.Type),
			slog.Int64("rss", p.RSS),
			slog.Int64("peakRSS", p.PeakRSS),
		)
	}

	sess.logger.LogAttrs(context.Background(), slog.LevelInfo, "chromium session memory",
		slog.Int64("cgroupRSS", cgroupRSS),
		slog.Int("processCount", len(processes)),
	)
}

// Drain makes all subsequent session creations fail with ErrDraining. Existing sessions are unaffected: they can
// still be deleted, proxied to, and will time out normally. Once draining, the session count can only decrease, so
// Wait is guaranteed to return within the session timeout.
func (s *Supervisor) Drain() {
	s.draining.Store(true)
}

// Wait blocks until there are no sessions running.
func (s *Supervisor) Wait() {
	s.wg.Wait()
}

// SessionTimeout returns the maximum time a session is allowed to run, after which it is killed unconditionally.
func (s *Supervisor) SessionTimeout() time.Duration {
	return s.opts.SessionTimeout
}

// delete cancels a session's context and removes it from the map, without locking the mutex.
// It must be used only inside functions that already grab the lock.
func (s *Supervisor) delete(sessionID string) bool {
	if sess, found := s.sessions[sessionID]; found {
		s.logger.Debug("cancelling context and deleting session", "sessionID", sessionID)
		sess.cancel()
		delete(s.sessions, sessionID)
		s.setSessionInactive()
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

// setSessionActive records that a session is now active.
// Under the one-session model, session creation and removal map directly to
// the active/inactive transitions.
func (s *Supervisor) setSessionActive() {
	s.metrics.SessionActive.Set(1)
}

// setSessionInactive records that no session is active.
func (s *Supervisor) setSessionInactive() {
	s.metrics.SessionActive.Set(0)
}

// launch prepares the requires directories and launches chromium, blocking
// until it exits. Cancelling the context kills chromium. The sessionID is used
// only for logging.
func (s *Supervisor) launch(ctx context.Context, logger *slog.Logger, oomBefore uint64, oomBeforeErr error) error {
	logger.Debug("starting session")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	tmpDir, err := s.mkdirTemp()
	if err != nil {
		return fmt.Errorf("creating temporary directory: %w", err)
	}

	defer func() {
		// Clean up files after chromium exits.
		err := os.RemoveAll(tmpDir)
		if err != nil {
			panic(fmt.Errorf("deleting tmpdir, bug or sandbox compromised: %w", err))
		}
	}()

	args := []string{
		// The following flags have been tested to be required:
		"--headless",
		"--remote-debugging-address=0.0.0.0",
		"--remote-debugging-port=" + s.opts.ChromiumPort,
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
	}

	if s.userAgent != "" {
		args = append(
			args,
			"--user-agent="+s.userAgent,
		)
	}

	cmd := exec.CommandContext(ctx,
		s.opts.ChromiumPath,
		args...,
	)
	cmd.Env = []string{
		// Chromium uses this env var to figure where the temporary directory is. We want that to be the directory
		// we created for this session, because /tmp is read-only in production.
		// https://github.com/chromium/chromium/blob/7c4f56ca9dba3a884212ef3a71c8db5d3633f0a6/base/files/file_util_posix.cc#L764
		"TMPDIR=" + tmpDir,
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if s.opts.UserGroup != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(s.opts.UserGroup),
				Gid: uint32(s.opts.UserGroup),
			},
		}
	}

	created := time.Now()
	defer func() {
		s.metrics.SessionDuration.Observe(time.Since(created).Seconds())
	}()

	err = cmd.Run()

	// Check whether the OOM killer fired during this session. Skip the comparison if the
	// baseline read failed — incrementing against an unknown baseline (implicitly 0) would
	// produce false positives. Also guard against counter wraparound/reset which would
	// manifest as oomAfter < oomBefore.
	if oomBeforeErr == nil {
		if oomAfter, oomErr := readOOMKillCount(s.opts.CgroupMemoryEventsPath); oomErr != nil {
			logger.Warn("could not read cgroup OOM kill count after session", "err", oomErr)
		} else if oomAfter < oomBefore {
			logger.Warn("cgroup OOM kill counter decreased unexpectedly (counter reset or cgroup recreated?)",
				"before", oomBefore, "after", oomAfter)
		} else if delta := oomAfter - oomBefore; delta > 0 {
			logger.Warn("OOM kill(s) detected during session", "oomKills", delta)
			s.metrics.OOMKills.Add(float64(delta))
		}
	}

	attrs := make([]slog.Attr, 0, 9)

	if err != nil {
		attrs = append(attrs, slog.Attr{Key: "err", Value: slog.AnyValue(err)})
	}

	if cmd.ProcessState != nil {
		if rUsage, isSyscallRusage := cmd.ProcessState.SysUsage().(*syscall.Rusage); rUsage != nil && isSyscallRusage {
			s.metrics.ChromiumResources.With(map[string]string{
				metrics.Resource: metrics.ResourceRSS,
			}).Observe(float64(rUsage.Maxrss * 1024)) // Convert from KiB to Bytes, as it is conventional in metrics.
		}

		attrs = append(attrs,
			slog.Attr{Key: "pid", Value: slog.IntValue(cmd.ProcessState.Pid())},
			slog.Attr{Key: "exitCode", Value: slog.IntValue(cmd.ProcessState.ExitCode())},
			slog.Attr{Key: "processState", Value: slog.StringValue(cmd.ProcessState.String())},
			slog.Attr{Key: "systemTime", Value: slog.DurationValue(cmd.ProcessState.SystemTime())},
			slog.Attr{Key: "userTime", Value: slog.DurationValue(cmd.ProcessState.UserTime())},
			slog.Attr{Key: "sysUsage", Value: slog.AnyValue(cmd.ProcessState.SysUsage())},
		)
	}

	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		s.metrics.ChromiumExecutions.With(map[string]string{
			metrics.ExecutionState: metrics.ExecutionStateFailed,
		}).Inc()

		// Append stdout and stderr to the log if there was an error,
		// as the log message may be useful for debugging what happened.
		attrs = append(attrs,
			slog.Attr{Key: "stdout", Value: slog.StringValue(stdout.String())},
			slog.Attr{Key: "stderr", Value: slog.StringValue(stderr.String())},
		)

		logger.LogAttrs(ctx, slog.LevelError, "chromium process finished", attrs...)

		return err
	}

	s.metrics.ChromiumExecutions.With(map[string]string{
		metrics.ExecutionState: metrics.ExecutionStateFinished,
	}).Inc()

	// Avoid spamming the logs with chromium's output, unless the logging level is debug.
	if logger.Enabled(ctx, slog.LevelDebug) {
		attrs = append(attrs,
			slog.Attr{Key: "stdout", Value: slog.StringValue(stdout.String())},
			slog.Attr{Key: "stderr", Value: slog.StringValue(stderr.String())},
		)
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "chromium process finished", attrs...)

	return nil
}

func (s *Supervisor) mkdirTemp() (string, error) {
	_, err := os.Stat(s.opts.TempDir)
	if errors.Is(err, fs.ErrNotExist) {
		s.logger.Warn(
			"Specified TempDir does not exist, is it mounted? Falling back to creating it.",
			"TempDir", s.opts.TempDir,
		)
		err = os.MkdirAll(s.opts.TempDir, 0o755) // 700 would not allow other users to descend into subdirectories.
		if err != nil {
			return "", fmt.Errorf("tmpdir does not exist and couldn't be created: %w", err)
		}
	}

	tmpDir, err := os.MkdirTemp(s.opts.TempDir, "")
	if err != nil {
		return "", err
	}

	if s.opts.UserGroup == 0 {
		// No chowning necessary.
		return tmpDir, nil
	}

	err = os.Chown(tmpDir, s.opts.UserGroup, s.opts.UserGroup)
	if err != nil {
		return "", fmt.Errorf("chowning temporary dir: %w", err)
	}

	return tmpDir, nil
}

// ComputeUserAgent runs chromium once, retrieves its default user agent, and stores a patched version so it can be used
// in all subsequent calls.
func (s *Supervisor) ComputeUserAgent(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	logger := s.logger.With("sessionID", "compute-user-agent")
	skipOOMSampling := errors.New("skip OOM sampling")

	go func() {
		err := s.launch(ctx, logger, 0, skipOOMSampling)
		if err != nil {
			s.logger.Error("launching chromium", "err", err)
		}
	}()

	versionCtx, versionCancel := context.WithTimeout(ctx, 2*time.Second)
	defer versionCancel()

	version, err := s.cclient.Version(versionCtx, net.JoinHostPort("localhost", s.opts.ChromiumPort))
	if err != nil {
		return fmt.Errorf("contacting chromium: %w", err)
	}

	s.logger.Debug("Found default user agent", "defaultUA", version.UserAgent)

	patchedUA := strings.ReplaceAll(version.UserAgent, "Headless", "")
	if s.opts.ExtraUATerms != "" {
		patchedUA += " " + s.opts.ExtraUATerms
	}

	s.userAgent = patchedUA

	s.logger.Info("Computed new user agent", "UA", s.userAgent)

	return nil
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
