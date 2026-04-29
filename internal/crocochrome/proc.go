package crocochrome

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	// No --type= flag. Classify by binary name to separate the Chromium browser
	// process from other no-flag co-residents.
	switch {
	case strings.Contains(raw, "chrome_crashpad"):
		return "crashpad"
	case strings.HasPrefix(raw, "tini"):
		// tini's cmdline includes its arguments (e.g. "-- /usr/local/bin/crocochrome"),
		// so it must be matched before the "crocochrome" check below.
		return "tini"
	case strings.Contains(raw, "crocochrome"):
		return "crocochrome"
	default:
		return "browser"
	}
}

// processMemoryStats reads /proc/<pid>/status and returns the current RSS (VmRSS) and
// peak RSS (VmHWM) in bytes. Both fields are parsed in a single pass; no extra syscall
// is needed. Values are in KiB in the status file; this function converts to bytes.
//
// VmHWM (high-water mark) is the peak RSS since the process started. Since Chromium
// subprocesses are spawned fresh per session, VmHWM is effectively the session peak.
func processMemoryStats(pid int, procRoot string) (rss, peakRSS int64, _ error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, 0, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, 0, fmt.Errorf("unexpected VmRSS line format: %q", line)
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parsing VmRSS value %q: %w", fields[1], err)
			}
			rss = kb * 1024
		case strings.HasPrefix(line, "VmHWM:"):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, 0, fmt.Errorf("unexpected VmHWM line format: %q", line)
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parsing VmHWM value %q: %w", fields[1], err)
			}
			peakRSS = kb * 1024
		}
	}

	if rss == 0 && peakRSS == 0 {
		return 0, 0, fmt.Errorf("VmRSS/VmHWM not found in /proc/%d/status", pid)
	}

	return rss, peakRSS, nil
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
