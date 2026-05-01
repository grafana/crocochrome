package crocochrome_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/crocochrome/internal/crocochrome"
)

func TestReadOOMKillCount(t *testing.T) {
	t.Parallel()

	t.Run("returns zero for empty path", func(t *testing.T) {
		t.Parallel()

		count, err := crocochrome.ReadOOMKillCount("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if count != 0 {
			t.Fatalf("expected 0, got %d", count)
		}
	})

	t.Run("returns zero for non-existent file", func(t *testing.T) {
		t.Parallel()

		count, err := crocochrome.ReadOOMKillCount("/does/not/exist/memory.events")
		if err != nil {
			t.Fatalf("unexpected error for missing file: %v", err)
		}

		if count != 0 {
			t.Fatalf("expected 0, got %d", count)
		}
	})

	t.Run("parses cgroupsv2 memory.events format", func(t *testing.T) {
		t.Parallel()

		f := writeTempFile(t, `low 0
high 0
max 5
oom 2
oom_kill 3
oom_group_kill 0
`)
		count, err := crocochrome.ReadOOMKillCount(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if count != 3 {
			t.Fatalf("expected oom_kill=3, got %d", count)
		}
	})

	t.Run("parses cgroupsv1 memory.oom_control format", func(t *testing.T) {
		t.Parallel()

		f := writeTempFile(t, `oom_kill_disable 0
under_oom 0
oom_kill 7
`)
		count, err := crocochrome.ReadOOMKillCount(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if count != 7 {
			t.Fatalf("expected oom_kill=7, got %d", count)
		}
	})

	t.Run("returns zero when oom_kill line is absent", func(t *testing.T) {
		t.Parallel()

		f := writeTempFile(t, `low 0
high 0
max 0
`)
		count, err := crocochrome.ReadOOMKillCount(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if count != 0 {
			t.Fatalf("expected 0, got %d", count)
		}
	})

	t.Run("returns error on malformed oom_kill value", func(t *testing.T) {
		t.Parallel()

		f := writeTempFile(t, "oom_kill notanumber\n")

		_, err := crocochrome.ReadOOMKillCount(f)
		if err == nil {
			t.Fatal("expected error for malformed oom_kill value, got nil")
		}
	})
}

func TestCgroupV1MemoryOOMControlPath(t *testing.T) {
	t.Parallel()

	t.Run("extracts memory controller path", func(t *testing.T) {
		t.Parallel()

		root := newFakeProcFS(t, `11:blkio:/kubepods
12:memory:/kubepods/besteffort/podabc123/container456
13:cpu,cpuacct:/kubepods
`)
		got := crocochrome.CgroupV1MemoryOOMControlPath(root)
		want := "/sys/fs/cgroup/memory/kubepods/besteffort/podabc123/container456/memory.oom_control"

		if got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	})

	t.Run("returns empty string when memory controller is absent", func(t *testing.T) {
		t.Parallel()

		root := newFakeProcFS(t, `11:blkio:/kubepods
13:cpu,cpuacct:/kubepods
`)
		if got := crocochrome.CgroupV1MemoryOOMControlPath(root); got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})

	t.Run("returns empty string for non-existent procfs root", func(t *testing.T) {
		t.Parallel()

		if got := crocochrome.CgroupV1MemoryOOMControlPath("/does/not/exist"); got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})
}

// newFakeProcFS creates a tempdir that procfs.NewFS will accept as a procfs root,
// containing a single pid directory with the given /proc/<pid>/cgroup contents and
// a "self" symlink pointing at it. Returns the root path.
func newFakeProcFS(t *testing.T, cgroupContents string) string {
	t.Helper()

	root := t.TempDir()
	const pid = "1234"

	pidDir := filepath.Join(root, pid)
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("creating pid dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(cgroupContents), 0o600); err != nil {
		t.Fatalf("writing cgroup file: %v", err)
	}
	if err := os.Symlink(pid, filepath.Join(root, "self")); err != nil {
		t.Fatalf("creating self symlink: %v", err)
	}

	return root
}

// writeTempFile writes content to a uniquely-named temp file and returns its path.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test-cgroup-events")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	return path
}
