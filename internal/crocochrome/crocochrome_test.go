package crocochrome_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/crocochrome/internal/crocochrome"
	"github.com/grafana/crocochrome/internal/testutil"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCrocochrome(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping long tests")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))

	t.Run("creates a session", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		session, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		t.Run("returns valid session info", func(t *testing.T) {
			t.Parallel()

			u, err := url.Parse(session.ChromiumVersion.WebSocketDebuggerURL)
			if err != nil {
				t.Fatalf("invalid wsurl %q: %v", session.ChromiumVersion.WebSocketDebuggerURL, err)
			}

			if u.Scheme != "ws" {
				t.Fatalf("unexpected scheme %q", u.Scheme)
			}
			if u.Port() != "9222" { // As returned by testutil.ChromiumVersion()
				t.Fatalf("unexpected port %q", u.Port())
			}
		})

		t.Run("returns session ID in list", func(t *testing.T) {
			if list := cc.Sessions(); !slices.Contains(list, session.ID) {
				t.Fatalf("session ID %q not found in sessions list %v", session.ID, list)
			}
		})
	})

	t.Run("returns an error and kills chromium", func(t *testing.T) {
		t.Parallel()

		t.Run("when chromium returns 500", func(t *testing.T) {
			t.Parallel()

			hb := testutil.NewHeartbeat(t)
			port := testutil.HTTPInfo(t, testutil.InternalServerErrorHandler)
			cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

			_, err := cc.Create(crocochrome.CheckInfo{})
			if err == nil {
				t.Fatalf("expected an error, got: %v", err)
			}

			hb.AssertAliveDead(0, 1)
		})

		t.Run("when chromium is not reachable", func(t *testing.T) {
			t.Parallel()

			hb := testutil.NewHeartbeat(t)
			cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: "0"})

			_, err := cc.Create(crocochrome.CheckInfo{})
			if err == nil {
				t.Fatalf("expected an error, got: %v", err)
			}

			hb.AssertAliveDead(0, 1)
		})
	})

	t.Run("terminates a session when asked", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		sess, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)
		cc.Delete(sess.ID)
		hb.AssertAliveDead(0, 1)

		t.Run("session is removed from list", func(t *testing.T) {
			if list := cc.Sessions(); len(list) > 0 {
				t.Fatalf("expected sessions list to be empty, not %v", list)
			}
		})
	})

	t.Run("terminates a session when another is created", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		sess1, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		_, err = cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating second session: %v", err)
		}

		hb.AssertAliveDead(1, 1)

		t.Run("session is removed from list", func(t *testing.T) {
			if list := cc.Sessions(); slices.Contains(list, sess1.ID) {
				t.Fatalf("session list %v should not contain terminated session %q", list, sess1.ID)
			}
		})
	})

	t.Run("CreateIfFree creates a session when free", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		session, err := cc.CreateIfFree(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		if list := cc.Sessions(); !slices.Contains(list, session.ID) {
			t.Fatalf("session ID %q not found in sessions list %v", session.ID, list)
		}
	})

	t.Run("CreateIfFree does not terminate an existing session", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		sess1, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		_, err = cc.CreateIfFree(crocochrome.CheckInfo{})
		if !errors.Is(err, crocochrome.ErrSessionExists) {
			t.Fatalf("expected ErrSessionExists, got: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		if list := cc.Sessions(); len(list) != 1 || !slices.Contains(list, sess1.ID) {
			t.Fatalf("expected sessions list to contain only %q, got %v", sess1.ID, list)
		}
	})

	t.Run("CreateIfFree succeeds after the session is deleted", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		sess, err := cc.CreateIfFree(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		cc.Delete(sess.ID)

		_, err = cc.CreateIfFree(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session after delete: %v", err)
		}

		hb.AssertAliveDead(1, 1)
	})

	t.Run("concurrent CreateIfFree yields exactly one session", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		const concurrency = 10

		errs := make([]error, concurrency)
		var wg sync.WaitGroup
		for i := range concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = cc.CreateIfFree(crocochrome.CheckInfo{})
			}()
		}
		wg.Wait()

		var successes, conflicts int
		for _, err := range errs {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, crocochrome.ErrSessionExists):
				conflicts++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}

		if successes != 1 || conflicts != concurrency-1 {
			t.Fatalf("expected 1 success and %d conflicts, got %d and %d", concurrency-1, successes, conflicts)
		}

		if list := cc.Sessions(); len(list) != 1 {
			t.Fatalf("expected exactly one session, got %v", list)
		}

		hb.AssertAliveDead(1, 0)
	})

	t.Run("Drain rejects new sessions but preserves the existing one", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		sess, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		cc.Drain()

		if _, err := cc.Create(crocochrome.CheckInfo{}); !errors.Is(err, crocochrome.ErrDraining) {
			t.Fatalf("expected ErrDraining from Create, got: %v", err)
		}

		if _, err := cc.CreateIfFree(crocochrome.CheckInfo{}); !errors.Is(err, crocochrome.ErrDraining) {
			t.Fatalf("expected ErrDraining from CreateIfFree, got: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		if !cc.Delete(sess.ID) {
			t.Fatalf("expected session %q to be deletable while draining", sess.ID)
		}

		hb.AssertAliveDead(0, 1)
	})

	t.Run("tracks active sessions in a gauge", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)

		reg := prometheus.NewRegistry()
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port, Registry: reg})

		assertSessionActive(t, reg, 0)

		sess, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		assertSessionActive(t, reg, 1)

		// Replacing the session via kill-existing keeps the gauge at 1.
		sess, err = cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating second session: %v", err)
		}

		assertSessionActive(t, reg, 1)

		cc.Delete(sess.ID)

		assertSessionActive(t, reg, 0)
	})

	t.Run("clears the active sessions gauge when a session times out", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)

		reg := prometheus.NewRegistry()
		cc := crocochrome.New(logger, crocochrome.Options{
			ChromiumPath:   hb.Path,
			ChromiumPort:   port,
			SessionTimeout: 3 * time.Second,
			Registry:       reg,
		})

		_, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		assertSessionActive(t, reg, 1)

		time.Sleep(4 * time.Second)

		assertSessionActive(t, reg, 0)
	})

	t.Run("SessionTimeout returns the resolved timeout", func(t *testing.T) {
		t.Parallel()

		cc := crocochrome.New(logger, crocochrome.Options{SessionTimeout: 42 * time.Second})
		if got := cc.SessionTimeout(); got != 42*time.Second {
			t.Fatalf("expected configured timeout 42s, got %v", got)
		}

		cc = crocochrome.New(logger, crocochrome.Options{})
		if got := cc.SessionTimeout(); got != 5*time.Minute {
			t.Fatalf("expected default timeout 5m, got %v", got)
		}
	})

	t.Run("creates a session with nil metadata", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		_, err := cc.Create(crocochrome.CheckInfo{Metadata: nil})
		if err != nil {
			t.Fatalf("creating session with nil metadata: %v", err)
		}
	})

	t.Run("enriches session logger with allowed metadata", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		// Keys match what synthetic-monitoring-agent sends via sm-k6-runner.
		sess, err := cc.Create(crocochrome.CheckInfo{
			Metadata: map[string]any{
				"id":       int64(69),
				"tenantID": int64(1234),
				"regionID": 4,
				"created":  1.0,
				"modified": 2.0,
			},
		})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		cc.Delete(sess.ID)
		cc.Wait()

		logs := buf.String()

		// Allowed keys should appear in logs.
		for _, expected := range []string{"id=69", "tenantID=1234", "regionID=4"} {
			if !strings.Contains(logs, expected) {
				t.Errorf("expected %q in logs, not found.\nLogs:\n%s", expected, logs)
			}
		}

		// Disallowed keys should not appear.
		for _, forbidden := range []string{"created=", "modified="} {
			if strings.Contains(logs, forbidden) {
				t.Errorf("unexpected %q in logs.\nLogs:\n%s", forbidden, logs)
			}
		}
	})

	t.Run("terminates a session after timeout", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port, SessionTimeout: 3 * time.Second})

		_, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		time.Sleep(4 * time.Second)

		hb.AssertAliveDead(0, 1)

		t.Run("session is removed from list", func(t *testing.T) {
			if list := cc.Sessions(); len(list) > 0 {
				t.Fatalf("expected sessions list to be empty, not %v", list)
			}
		})
	})

	t.Run("increments OOM kill counter when cgroup reports a kill during session", func(t *testing.T) {
		t.Parallel()

		// Write a cgroup file starting at oom_kill=0. We'll bump it to 1 while the session
		// is live to simulate the OOM killer firing during the session.
		cgroupFile := writeTempFile(t, "oom_kill 0\n")

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)

		reg := prometheus.NewRegistry()
		cc := crocochrome.New(
			slog.New(slog.NewTextHandler(os.Stderr, nil)),
			crocochrome.Options{
				ChromiumPath:           hb.Path,
				ChromiumPort:           port,
				CgroupMemoryEventsPath: cgroupFile,
				Registry:               reg,
			},
		)

		sess, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		// Simulate OOM kill by incrementing the cgroup counter while the session is running.
		if err := os.WriteFile(cgroupFile, []byte("oom_kill 1\n"), 0o600); err != nil {
			t.Fatalf("updating cgroup file: %v", err)
		}

		cc.Delete(sess.ID)
		cc.Wait()

		got, err := promtestutil.GatherAndCount(reg, "sm_crocochrome_chromium_oom_kills_total")
		if err != nil {
			t.Fatalf("gathering metrics: %v", err)
		}

		if got == 0 {
			t.Fatal("expected sm_crocochrome_chromium_oom_kills_total to be present in registry")
		}

		const wantMetric = `# HELP sm_crocochrome_chromium_oom_kills_total Total number of times the kernel OOM-killer fired within the container cgroup during a Chromium session. Incremented when the oom_kill counter in the cgroup memory events file increases between session start and session end.
# TYPE sm_crocochrome_chromium_oom_kills_total counter
sm_crocochrome_chromium_oom_kills_total 1
`
		if err := promtestutil.GatherAndCompare(reg, strings.NewReader(wantMetric),
			"sm_crocochrome_chromium_oom_kills_total"); err != nil {
			t.Errorf("OOM kill counter mismatch: %v", err)
		}
	})

	t.Run("logs per-process metrics and session summary on Delete", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)

		// Set up a fake cgroup dir and proc dir so process metrics can be collected.
		cgroupDir := t.TempDir()
		procRoot := t.TempDir()

		writeFile := func(path, content string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}

		eventsPath := cgroupDir + "/memory.events"
		writeFile(eventsPath, "oom_kill 0\n")
		writeFile(cgroupDir+"/cgroup.procs", "9001\n")
		writeFile(cgroupDir+"/memory.current", "314572800\n") // 300 MiB

		if err := os.MkdirAll(procRoot+"/9001", 0755); err != nil {
			t.Fatal(err)
		}
		writeFile(procRoot+"/9001/cmdline", "/usr/lib/chromium/chromium\x00--no-sandbox\x00")
		writeFile(procRoot+"/9001/status", "VmHWM:\t 250000 kB\nVmRSS:\t 200000 kB\n")

		cc := crocochrome.New(logger, crocochrome.Options{
			ChromiumPath:           hb.Path,
			ChromiumPort:           port,
			EnableProcessMetrics:   true,
			CgroupMemoryEventsPath: eventsPath,
			ProcFSRoot:             procRoot,
		})

		sess, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		cc.Delete(sess.ID)
		cc.Wait()

		logs := buf.String()

		for _, want := range []string{
			"chromium process memory",
			"pid=9001",
			"processType=browser",
			"peakRSS=256000000", // 250000 kB * 1024
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected %q in logs\nLogs:\n%s", want, logs)
			}
		}

		for _, want := range []string{
			"chromium session memory",
			"cgroupRSS=314572800",
			"processCount=1",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected %q in logs\nLogs:\n%s", want, logs)
			}
		}
	})
}

// assertSessionActive checks that the session active gauge in reg has the given value.
func assertSessionActive(t *testing.T, reg *prometheus.Registry, want float64) {
	t.Helper()

	wantMetric := fmt.Sprintf(`# HELP sm_crocochrome_session_active Set to 1 when a session is active, 0 otherwise.
# TYPE sm_crocochrome_session_active gauge
sm_crocochrome_session_active %g
`, want)
	if err := promtestutil.GatherAndCompare(reg, strings.NewReader(wantMetric),
		"sm_crocochrome_session_active"); err != nil {
		t.Errorf("session active gauge mismatch: %v", err)
	}
}
