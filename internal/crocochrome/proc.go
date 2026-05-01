package crocochrome

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/procfs"
)

// processInfo holds OS-level information for a single process in the Chromium process tree.
type processInfo struct {
	PID     int
	Type    string // "browser", "renderer", "gpu-process", "network-service", "utility", "unknown"
	RSS     int64  // current resident set size in bytes (point-in-time at collection)
	PeakRSS int64  // peak resident set size since process start (VmHWM), in bytes
}

// cgroupProcsPath derives the cgroup.procs file path from the memory events file path.
// Both cgroupsv1 and cgroupsv2 keep cgroup.procs alongside the memory event files.
func cgroupProcsPath(eventsPath string) string {
	return filepath.Join(filepath.Dir(eventsPath), "cgroup.procs")
}

// cgroupMemoryCurrentPath derives the total memory usage file path from the events file path.
// For cgroupsv2 (memory.events) this is memory.current; for cgroupsv1 (memory.oom_control)
// this is memory.usage_in_bytes.
func cgroupMemoryCurrentPath(eventsPath string) string {
	dir := filepath.Dir(eventsPath)
	if filepath.Base(eventsPath) == "memory.events" {
		return filepath.Join(dir, "memory.current")
	}
	return filepath.Join(dir, "memory.usage_in_bytes")
}

// chromiumProcessType reads /proc/<pid>/cmdline and returns the Chromium process type.
// Returns "browser" only for the main Chromium browser process (no --type= flag and no
// other known binary signature). Returns "unknown" if cmdline cannot be read.
//
// Chromium uses two distinct cmdline formats on Linux:
//
//   - Processes launched via exec (browser, crashpad): argv entries are null-byte separated,
//     matching the standard Linux /proc/<pid>/cmdline layout.
//
//   - Processes spawned via the Zygote (renderers, GPU, utility): after fork, Chromium calls
//     SetProcessTitleFromCommandLine() which rewrites the argv region as a single
//     space-separated string with only a trailing null byte. Null-splitting these produces
//     one giant token, so --type= is found as a raw substring instead.
//
// Processes without a --type= flag are further classified by their binary/cmdline content
// to distinguish the Chromium browser process from co-resident processes (tini,
// crocochrome, crashpad handlers) that also lack a --type= argument.
// Note: tini's cmdline includes "crocochrome" as an argument, so it must be checked first.
func chromiumProcessType(pid int, procRoot string) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "unknown"
	}

	raw := string(data)

	// Find --type= as a raw substring and extract the value terminated by the next
	// space, null byte, or end of string.
	if idx := strings.Index(raw, "--type="); idx != -1 {
		rest := raw[idx+len("--type="):]
		var processType string
		if end := strings.IndexAny(rest, " \x00"); end != -1 {
			processType = rest[:end]
		} else {
			processType = rest
		}

		// Distinguish the network service from generic utility processes.
		if processType == "utility" && strings.Contains(raw, "network.mojom.NetworkService") {
			return "network-service"
		}
		return processType
	}

	// No --type= flag. Classify by the basename of the executable (the first
	// argument, terminated by the first null byte or space) to separate the
	// Chromium browser process from other no-flag co-residents.
	//
	// We use filepath.Base rather than a prefix check because tini may be
	// invoked with a full path (/sbin/tini, /usr/bin/tini, etc.). A prefix
	// check would miss those cases and fall through to the "crocochrome"
	// branch — incorrectly — because tini's cmdline contains "crocochrome"
	// as an argument.
	firstArgEnd := strings.IndexAny(raw, "\x00 ")
	var execBase string
	if firstArgEnd == -1 {
		execBase = filepath.Base(raw)
	} else {
		execBase = filepath.Base(raw[:firstArgEnd])
	}

	switch {
	case strings.Contains(raw, "chrome_crashpad"):
		return "crashpad"
	case execBase == "tini":
		return "tini"
	case strings.Contains(raw, "crocochrome"):
		return "crocochrome"
	default:
		return "browser"
	}
}

// processMemoryStats returns the current RSS (VmRSS) and peak RSS (VmHWM) in bytes for
// the given pid, parsed from /proc/<pid>/status via prometheus/procfs.
//
// VmHWM (high-water mark) is the peak RSS since the process started. Since Chromium
// subprocesses are spawned fresh per session, VmHWM is effectively the session peak.
func processMemoryStats(pid int, procRoot string) (rss, peakRSS int64, _ error) {
	fs, err := procfs.NewFS(procRoot)
	if err != nil {
		return 0, 0, fmt.Errorf("opening procfs at %q: %w", procRoot, err)
	}

	p, err := fs.Proc(pid)
	if err != nil {
		return 0, 0, err
	}

	st, err := p.NewStatus()
	if err != nil {
		return 0, 0, err
	}

	if st.VmRSS == 0 && st.VmHWM == 0 {
		return 0, 0, fmt.Errorf("VmRSS/VmHWM not found in /proc/%d/status", pid)
	}

	return int64(st.VmRSS), int64(st.VmHWM), nil
}

// collectProcessMetrics walks cgroup.procs and returns per-process info plus the total
// cgroup RSS from memory.current (cgroupsv2) or memory.usage_in_bytes (cgroupsv1).
// Processes that exit between enumeration and reading are silently skipped — this is
// an expected race condition when collecting at session teardown.
func collectProcessMetrics(cgroupEventsPath, procRoot string) (processes []processInfo, cgroupRSS int64, _ error) {
	procsData, err := os.ReadFile(cgroupProcsPath(cgroupEventsPath))
	if err != nil {
		return nil, 0, fmt.Errorf("reading cgroup.procs: %w", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(procsData)), "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		rss, peakRSS, err := processMemoryStats(pid, procRoot)
		if err != nil {
			continue // process likely exited between enumeration and read; skip silently
		}
		processes = append(processes, processInfo{
			PID:     pid,
			Type:    chromiumProcessType(pid, procRoot),
			RSS:     rss,
			PeakRSS: peakRSS,
		})
	}

	// Read the kernel-maintained total. More accurate than summing per-process VmRSS:
	// the cgroup counter includes kernel memory and avoids shared-page double-counting.
	currentData, err := os.ReadFile(cgroupMemoryCurrentPath(cgroupEventsPath))
	if err != nil {
		return processes, 0, fmt.Errorf("reading cgroup memory current: %w", err)
	}
	cgroupRSS, err = strconv.ParseInt(strings.TrimSpace(string(currentData)), 10, 64)
	if err != nil {
		return processes, 0, fmt.Errorf("parsing cgroup memory current: %w", err)
	}

	return processes, cgroupRSS, nil
}
