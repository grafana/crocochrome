package testutil

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var heartbeatSh = `#!/usr/bin/env bash

# heartbeat.sh is a script that writes the current timestamp to a file named after its PID.

set -e

DIR="$(realpath "$(dirname "${BASH_SOURCE[0]}")")"

while true; do
	date '+%s' > "$DIR/canary-$$"
	sleep 1
done
`

type Heartbeat struct {
	Path string
	t    *testing.T
}

// Heartbeat returns a script that, when executed, will write the current unix timestamp to a file until killed.
// As the second value, it returns the file this particular heartbeat instance writes to.
// Heartbeat does not run the script.
func NewHeartbeat(t *testing.T) Heartbeat {
	t.Helper()

	// Lock ForkLock whenever we are writing to a file that will execute shortly after, to prevent its FD from leaking
	// into a forked process and thus making exec fail with ETXBSY.
	// https://github.com/golang/go/issues/22315
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	dir := t.TempDir()
	scriptFile, err := os.OpenFile(filepath.Join(dir, "heartbeat.sh"), os.O_CREATE|os.O_TRUNC|os.O_RDWR, os.FileMode(0o700))
	if err != nil {
		t.Fatalf("creating heartbeat script: %v", err)
	}
	defer scriptFile.Close()

	_, err = scriptFile.WriteString(heartbeatSh)
	if err != nil {
		t.Fatalf("writing heartbeat script contents: %v", err)
	}

	return Heartbeat{
		Path: scriptFile.Name(),
		t:    t,
	}
}

// AssertAlive ensures the script writing to hbFile is still alive.
// It does this by waiting a couple seconds, and then ensure a timestamp was written to the file in that time.
func (h Heartbeat) AssertAliveDead(alive, dead int) {
	h.t.Helper()

	time.Sleep(2 * time.Second)
	actualAlive, actualDead := h.aliveDead()
	total := actualAlive + actualDead

	if alive != actualAlive {
		h.t.Fatalf("Wanted %d alive processes, found %d/%d", alive, actualAlive, total)
	}

	if dead != actualDead {
		h.t.Fatalf("Wanted %d dead processes, found %d/%d", dead, actualDead, total)
	}
}

func (h Heartbeat) aliveDead() (alive int, dead int) {
	h.t.Helper()

	err := filepath.WalkDir(filepath.Dir(h.Path), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if !strings.HasPrefix(filepath.Base(path), "canary-") {
			return nil
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			h.t.Fatalf("reading hbFile: %v", err)
		}

		strTimestamp := string(bytes.TrimSpace(contents))
		timestamp, err := strconv.ParseInt(strTimestamp, 10, 64)
		if err != nil {
			h.t.Fatalf("parsing timestamp from hbFile: %v", err)
		}

		if time.Since(time.Unix(timestamp, 0)) < 2*time.Second {
			alive++
		} else {
			dead++
		}

		return nil
	})
	if err != nil {
		h.t.Fatalf("walking canary files: %v", err)
	}

	return
}
