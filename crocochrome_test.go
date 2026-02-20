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

	"github.com/grafana/crocochrome"
	"github.com/grafana/crocochrome/testutil"
)

func TestCrocochrome(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))

	t.Run("creates a session", func(t *testing.T) {
		t.Parallel()

		hb := testutil.NewHeartbeat(t)
		port := testutil.HTTPInfo(t, testutil.ChromiumVersionHandler)
		cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: port})

		session, err := cc.Create(crocochrome.CreateRequest{})
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

			_, err := cc.Create(crocochrome.CreateRequest{})
			if err == nil {
				t.Fatalf("expected an error, got: %v", err)
			}

			hb.AssertAliveDead(0, 1)
		})

		t.Run("when chromium is not reachable", func(t *testing.T) {
			t.Parallel()

			hb := testutil.NewHeartbeat(t)
			cc := crocochrome.New(logger, crocochrome.Options{ChromiumPath: hb.Path, ChromiumPort: "0"})

			_, err := cc.Create(crocochrome.CreateRequest{})
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

		sess, err := cc.Create(crocochrome.CreateRequest{})
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

		sess1, err := cc.Create(crocochrome.CreateRequest{})
		if err != nil {
			t.Fatalf("creating session: %v", err)
		}

		hb.AssertAliveDead(1, 0)

		_, err = cc.Create(crocochrome.CreateRequest{})
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

		_, err := cc.Create(crocochrome.CreateRequest{Metadata: nil})
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
		sess, err := cc.Create(crocochrome.CreateRequest{
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

		_, err := cc.Create(crocochrome.CreateRequest{})
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
