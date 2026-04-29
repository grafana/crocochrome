package crocochrome_test

import (
	"bytes"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
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

	t.Run("logs per-renderer and per-process metrics on Delete", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		hb := testutil.NewHeartbeat(t)
		cdpURL := testutil.CDPServer(t)

		// Two fake page targets pointing to the same CDP server.
		// StartChromiumWithTargets wires /json/version and /json/list on the same server,
		// matching real Chromium's behaviour so chromiumTargets() resolves /json/list correctly.
		targets := []testutil.CDPTargetInfo{
			{URL: "https://example.com", WebSocketDebuggerURL: cdpURL},
			{URL: "https://other.com", WebSocketDebuggerURL: cdpURL},
		}
		port := testutil.StartChromiumWithTargets(t, targets)

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
		writeFile(procRoot+"/9001/cmdline", "/usr/bin/chromium\x00--no-sandbox\x00")
		writeFile(procRoot+"/9001/status", "VmHWM:\t 250000 kB\nVmRSS:\t 200000 kB\n")

		cc := crocochrome.New(logger, crocochrome.Options{
			ChromiumPath:           hb.Path,
			ChromiumPort:           port,
			EnableProcessMetrics:   true,
			EnableCDPMetrics:       true,
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

		// Per-renderer: metric names and target URLs must appear.
		for name := range testutil.CDPPerformanceMetrics {
			if !strings.Contains(logs, name) {
				t.Errorf("expected CDP metric %q in logs\nLogs:\n%s", name, logs)
			}
		}
		for _, want := range []string{
			"chromium renderer metrics",
			"targetURL=https://example.com",
			"targetURL=https://other.com",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected %q in logs\nLogs:\n%s", want, logs)
			}
		}

		// Per-process: the fake browser process entry must appear, including peakRSS.
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

		// Summary entry must appear with the cgroup total.
		for _, want := range []string{
			"chromium session memory",
			"cgroupRSS=314572800",
			"rendererCount=2",
			"processCount=1",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected %q in logs\nLogs:\n%s", want, logs)
			}
		}
	})

	t.Run("logs renderer count with only CDP metrics enabled", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		hb := testutil.NewHeartbeat(t)
		cdpURL := testutil.CDPServer(t)
		targets := []testutil.CDPTargetInfo{
			{URL: "https://example.com", WebSocketDebuggerURL: cdpURL},
			{URL: "https://other.com", WebSocketDebuggerURL: cdpURL},
		}
		port := testutil.StartChromiumWithTargets(t, targets)

		cc := crocochrome.New(logger, crocochrome.Options{
			ChromiumPath:     hb.Path,
			ChromiumPort:     port,
			EnableCDPMetrics: true,
		})

		sess, err := cc.Create(crocochrome.CheckInfo{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		cc.Delete(sess.ID)
		cc.Wait()

		logs := buf.String()
		for _, want := range []string{
			"chromium session memory",
			"rendererCount=2",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("expected %q in logs\nLogs:\n%s", want, logs)
			}
		}
		for _, forbidden := range []string{"cgroupRSS=", "processCount="} {
			if strings.Contains(logs, forbidden) {
				t.Errorf("unexpected %q in logs\nLogs:\n%s", forbidden, logs)
			}
		}
		hb.AssertAliveDead(0, 1)
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

		// Collect the counter value via GatherAndCount is only a presence check.
		// Use CollectAndCompare for the actual value.
		const wantMetric = `# HELP sm_crocochrome_chromium_oom_kills_total Total number of times the kernel OOM-killer fired within the container cgroup during a Chromium session. Incremented when the oom_kill counter in the cgroup memory events file increases between session start and session end.
# TYPE sm_crocochrome_chromium_oom_kills_total counter
sm_crocochrome_chromium_oom_kills_total 1
`
		if err := promtestutil.GatherAndCompare(reg, strings.NewReader(wantMetric),
			"sm_crocochrome_chromium_oom_kills_total"); err != nil {
			t.Errorf("OOM kill counter mismatch: %v", err)
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
}
