package crocochrome

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/procfs"
)

const (
	// cgroupV2MemoryEvents is the unified-hierarchy memory events file present on cgroups v2.
	// It contains a line "oom_kill <N>" that increments each time the kernel OOM-killer fires
	// within this cgroup.
	cgroupV2MemoryEvents = "/sys/fs/cgroup/memory.events"

	// cgroupV1OOMControl is the path template for the cgroups v1 memory OOM control file.
	// The actual path is constructed by reading /proc/self/cgroup.
	cgroupV1OOMControl = "/sys/fs/cgroup/memory%s/memory.oom_control"
)

// detectCgroupMemoryEventsPath returns the path to the file that contains the cgroup OOM kill
// counter for the current process, or an empty string if neither cgroups v2 nor v1 can be
// located.
//
// Priority:
//  1. cgroupsv2: /sys/fs/cgroup/memory.events
//  2. cgroupsv1: /sys/fs/cgroup/memory/<hierarchy-path>/memory.oom_control (derived from
//     /proc/self/cgroup)
func detectCgroupMemoryEventsPath(procRoot string) string {
	if _, err := os.Stat(cgroupV2MemoryEvents); err == nil {
		return cgroupV2MemoryEvents
	}

	return cgroupV1MemoryOOMControlPath(procRoot)
}

// cgroupV1MemoryOOMControlPath resolves the calling process's cgroupv1 memory hierarchy
// via prometheus/procfs and returns the memory.oom_control path for that hierarchy, or
// "" if the memory controller is not found or procfs cannot be opened.
func cgroupV1MemoryOOMControlPath(procRoot string) string {
	fs, err := procfs.NewFS(procRoot)
	if err != nil {
		return ""
	}

	self, err := fs.Self()
	if err != nil {
		return ""
	}

	cgs, err := self.Cgroups()
	if err != nil {
		return ""
	}

	for _, cg := range cgs {
		for _, c := range cg.Controllers {
			if c == "memory" {
				return fmt.Sprintf(cgroupV1OOMControl, cg.Path)
			}
		}
	}

	return ""
}

// readOOMKillCount reads the oom_kill counter from the given memory events file.
// It handles both cgroups v2 (memory.events) and v1 (memory.oom_control) formats, as both
// use a "oom_kill <N>" line (space-delimited; tabs are not used by any known kernel version).
// Returns 0 and no error when the file is empty or the counter line is absent.
// Returns an error only on I/O failure.
func readOOMKillCount(path string) (uint64, error) {
	if path == "" {
		return 0, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // file absent in test environments; treat as zero
		}

		return 0, fmt.Errorf("opening cgroup memory events file %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "oom_kill ") {
			continue
		}

		val, err := strconv.ParseUint(strings.TrimPrefix(line, "oom_kill "), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing oom_kill value from %q: %w", filepath.Base(path), err)
		}

		return val, nil
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scanning cgroup memory events file %q: %w", path, err)
	}

	return 0, nil
}
