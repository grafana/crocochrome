package crocochrome

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// cgroupV2MemoryEvents is the unified-hierarchy memory events file present on cgroups v2.
	// It contains a line "oom_kill <N>" that increments each time the kernel OOM-killer fires
	// within this cgroup.
	cgroupV2MemoryEvents = "/sys/fs/cgroup/memory.events"

	// cgroupV1OOMControl is the path template for the cgroups v1 memory OOM control file.
	// The actual path is constructed by reading /proc/self/cgroup.
	cgroupV1OOMControl = "/sys/fs/cgroup/memory%s/memory.oom_control"

	// procSelfCgroup is the standard path for the calling process's cgroup memberships.
	procSelfCgroup = "/proc/self/cgroup"
)

// detectCgroupMemoryEventsPath returns the path to the file that contains the cgroup OOM kill
// counter for the current process, or an empty string if neither cgroups v2 nor v1 can be
// located.
//
// Priority:
//  1. cgroupsv2: /sys/fs/cgroup/memory.events
//  2. cgroupsv1: /sys/fs/cgroup/memory/<hierarchy-path>/memory.oom_control (derived from
//     /proc/self/cgroup)
func detectCgroupMemoryEventsPath() string {
	if _, err := os.Stat(cgroupV2MemoryEvents); err == nil {
		return cgroupV2MemoryEvents
	}

	return cgroupV1MemoryOOMControlPath(procSelfCgroup)
}

// cgroupV1MemoryOOMControlPath reads procSelfCgroupPath (/proc/self/cgroup) and returns the
// memory.oom_control path for the memory controller hierarchy, or "" if it cannot be found.
func cgroupV1MemoryOOMControlPath(procSelfCgroupPath string) string {
	f, err := os.Open(procSelfCgroupPath)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Format: <hierarchy-id>:<controller-list>:<cgroup-path>
		// e.g.   12:memory:/kubepods/besteffort/pod.../container-id
		parts := strings.SplitN(scanner.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}

		controllers := strings.Split(parts[1], ",")
		for _, c := range controllers {
			if c == "memory" {
				return fmt.Sprintf(cgroupV1OOMControl, parts[2])
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
