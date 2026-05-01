package crocochrome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupProcsPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		eventsPath string
		want       string
	}{
		{
			eventsPath: "/sys/fs/cgroup/kubepods/pod123/memory.events",
			want:       "/sys/fs/cgroup/kubepods/pod123/cgroup.procs",
		},
		{
			eventsPath: "/sys/fs/cgroup/memory/kubepods/pod123/memory.oom_control",
			want:       "/sys/fs/cgroup/memory/kubepods/pod123/cgroup.procs",
		},
	}

	for _, tc := range cases {
		got := cgroupProcsPath(tc.eventsPath)
		if got != tc.want {
			t.Errorf("cgroupProcsPath(%q) = %q, want %q", tc.eventsPath, got, tc.want)
		}
	}
}

func TestCgroupMemoryCurrentPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		eventsPath string
		want       string
	}{
		{
			eventsPath: "/sys/fs/cgroup/kubepods/pod123/memory.events",
			want:       "/sys/fs/cgroup/kubepods/pod123/memory.current",
		},
		{
			eventsPath: "/sys/fs/cgroup/memory/kubepods/pod123/memory.oom_control",
			want:       "/sys/fs/cgroup/memory/kubepods/pod123/memory.usage_in_bytes",
		},
	}

	for _, tc := range cases {
		got := cgroupMemoryCurrentPath(tc.eventsPath)
		if got != tc.want {
			t.Errorf("cgroupMemoryCurrentPath(%q) = %q, want %q", tc.eventsPath, got, tc.want)
		}
	}
}

func TestChromiumProcessType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cmdline string // null-separated args
		want    string
	}{
		// Null-byte separated: standard exec'd processes (browser, crashpad).
		{
			name:    "chromium browser process (no --type flag, null-separated)",
			cmdline: "/usr/lib/chromium/chromium\x00--no-sandbox\x00--headless\x00",
			want:    "browser",
		},
		{
			name:    "tini supervisor (bare name)",
			cmdline: "tini\x00--\x00/usr/local/bin/crocochrome\x00-cdp-metrics\x00",
			want:    "tini",
		},
		{
			name:    "tini supervisor (full path — must not misclassify as crocochrome)",
			cmdline: "/sbin/tini\x00--\x00/usr/local/bin/crocochrome\x00-cdp-metrics\x00",
			want:    "tini",
		},
		{
			name:    "crocochrome process",
			cmdline: "/usr/local/bin/crocochrome\x00-cdp-metrics\x00",
			want:    "crocochrome",
		},
		{
			name:    "chrome crashpad handler",
			cmdline: "/usr/lib/chromium/chrome_crashpad_handler\x00--monitor-self\x00--database=/tmp/crashreports\x00",
			want:    "crashpad",
		},
		{
			name:    "renderer process (null-separated)",
			cmdline: "/usr/bin/chromium\x00--type=renderer\x00--no-sandbox\x00",
			want:    "renderer",
		},
		{
			name:    "GPU process (null-separated)",
			cmdline: "/usr/bin/chromium\x00--type=gpu-process\x00",
			want:    "gpu-process",
		},
		{
			name:    "network service utility (null-separated)",
			cmdline: "/usr/bin/chromium\x00--type=utility\x00--utility-sub-type=network.mojom.NetworkService\x00",
			want:    "network-service",
		},
		{
			name:    "generic utility (null-separated)",
			cmdline: "/usr/bin/chromium\x00--type=utility\x00--utility-sub-type=audio.mojom.AudioService\x00",
			want:    "utility",
		},
		// Space-separated: Zygote-forked processes (renderer, GPU, utility, zygote).
		// Chromium's SetProcessTitleFromCommandLine() rewrites argv as a single
		// space-separated string, removing null bytes.
		{
			name:    "renderer process (space-separated, Zygote-forked)",
			cmdline: "/usr/lib/chromium/chromium --type=renderer --no-sandbox --headless --crashpad-handler-pid=100",
			want:    "renderer",
		},
		{
			name:    "GPU process (space-separated, Zygote-forked)",
			cmdline: "/usr/lib/chromium/chromium --type=gpu-process --no-sandbox --use-angle=swiftshader-webgl",
			want:    "gpu-process",
		},
		{
			name:    "zygote process (space-separated)",
			cmdline: "/usr/lib/chromium/chromium --type=zygote --no-zygote-sandbox --no-sandbox",
			want:    "zygote",
		},
		{
			name:    "network service utility (space-separated, Zygote-forked)",
			cmdline: "/usr/lib/chromium/chromium --type=utility --utility-sub-type=network.mojom.NetworkService --no-sandbox",
			want:    "network-service",
		},
		{
			name:    "storage service utility (space-separated, Zygote-forked)",
			cmdline: "/usr/lib/chromium/chromium --type=utility --utility-sub-type=storage.mojom.StorageService --no-sandbox",
			want:    "utility",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			procRoot := t.TempDir()
			pid := 1234
			pidDir := filepath.Join(procRoot, fmt.Sprintf("%d", pid))
			if err := os.MkdirAll(pidDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte(tc.cmdline), 0644); err != nil {
				t.Fatal(err)
			}

			got := chromiumProcessType(pid, procRoot)
			if got != tc.want {
				t.Errorf("chromiumProcessType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChromiumProcessType_missingCmdline(t *testing.T) {
	t.Parallel()

	procRoot := t.TempDir()
	got := chromiumProcessType(9999, procRoot)
	if got != "unknown" {
		t.Errorf("chromiumProcessType() with missing cmdline = %q, want %q", got, "unknown")
	}
}

func TestProcessMemoryStats(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		status      string
		wantRSS     int64
		wantPeakRSS int64
		wantErr     bool
	}{
		{
			name: "parses VmRSS and VmHWM correctly",
			status: `Name:	chromium
VmPeak:	 500000 kB
VmHWM:	 200000 kB
VmSize:	 400000 kB
VmRSS:	 153600 kB
VmData:	 200000 kB
`,
			wantRSS:     153600 * 1024,
			wantPeakRSS: 200000 * 1024,
		},
		{
			name: "peak equals current when process hasn't freed memory",
			status: `Name:	chromium
VmHWM:	 153600 kB
VmRSS:	 153600 kB
`,
			wantRSS:     153600 * 1024,
			wantPeakRSS: 153600 * 1024,
		},
		{
			name:    "returns error when both VmRSS and VmHWM absent",
			status:  "Name:\tchromium\nVmSize:\t100 kB\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			procRoot := t.TempDir()
			pid := 42
			pidDir := filepath.Join(procRoot, fmt.Sprintf("%d", pid))
			if err := os.MkdirAll(pidDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pidDir, "status"), []byte(tc.status), 0644); err != nil {
				t.Fatal(err)
			}

			gotRSS, gotPeak, err := processMemoryStats(pid, procRoot)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotRSS != tc.wantRSS {
				t.Errorf("rss = %d, want %d", gotRSS, tc.wantRSS)
			}
			if gotPeak != tc.wantPeakRSS {
				t.Errorf("peakRSS = %d, want %d", gotPeak, tc.wantPeakRSS)
			}
		})
	}
}

func TestCollectProcessMetrics(t *testing.T) {
	t.Parallel()

	cgroupDir := t.TempDir()
	procRoot := t.TempDir()

	// Write cgroup files (cgroupsv2 layout).
	eventsPath := filepath.Join(cgroupDir, "memory.events")
	if err := os.WriteFile(eventsPath, []byte("oom_kill 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("100\n200\n300\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "memory.current"), []byte("524288000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Write fake /proc entries.
	writeProc := func(pid int, cmdline, vmRSSLine, vmHWMLine string) {
		t.Helper()
		dir := filepath.Join(procRoot, fmt.Sprintf("%d", pid))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0644); err != nil {
			t.Fatal(err)
		}
		status := fmt.Sprintf("Name:\tchromium\n%s\n%s\n", vmHWMLine, vmRSSLine)
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeProc(100, "/usr/bin/chromium\x00--no-sandbox\x00", "VmRSS:\t 150000 kB", "VmHWM:\t 180000 kB")       // browser
	writeProc(200, "/usr/bin/chromium\x00--type=renderer\x00", "VmRSS:\t 300000 kB", "VmHWM:\t 400000 kB")    // renderer
	writeProc(300, "/usr/bin/chromium\x00--type=gpu-process\x00", "VmRSS:\t 200000 kB", "VmHWM:\t 220000 kB") // GPU

	processes, cgroupRSS, err := collectProcessMetrics(eventsPath, procRoot)
	if err != nil {
		t.Fatalf("collectProcessMetrics() error: %v", err)
	}

	if cgroupRSS != 524288000 {
		t.Errorf("cgroupRSS = %d, want 524288000", cgroupRSS)
	}

	if len(processes) != 3 {
		t.Fatalf("got %d processes, want 3", len(processes))
	}

	byPID := make(map[int]processInfo)
	for _, p := range processes {
		byPID[p.PID] = p
	}

	cases := []struct {
		pid         int
		wantType    string
		wantRSS     int64
		wantPeakRSS int64
	}{
		{100, "browser", 150000 * 1024, 180000 * 1024},
		{200, "renderer", 300000 * 1024, 400000 * 1024},
		{300, "gpu-process", 200000 * 1024, 220000 * 1024},
	}

	for _, tc := range cases {
		p, ok := byPID[tc.pid]
		if !ok {
			t.Errorf("PID %d not found in results", tc.pid)
			continue
		}
		if p.Type != tc.wantType {
			t.Errorf("PID %d: type = %q, want %q", tc.pid, p.Type, tc.wantType)
		}
		if p.RSS != tc.wantRSS {
			t.Errorf("PID %d: RSS = %d, want %d", tc.pid, p.RSS, tc.wantRSS)
		}
		if p.PeakRSS != tc.wantPeakRSS {
			t.Errorf("PID %d: PeakRSS = %d, want %d", tc.pid, p.PeakRSS, tc.wantPeakRSS)
		}
	}
}

func TestCollectProcessMetrics_skipsExitedProcesses(t *testing.T) {
	t.Parallel()

	cgroupDir := t.TempDir()
	procRoot := t.TempDir()

	eventsPath := filepath.Join(cgroupDir, "memory.events")
	if err := os.WriteFile(eventsPath, []byte("oom_kill 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// cgroup.procs lists PID 999 which has no /proc entry (already exited).
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("999\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "memory.current"), []byte("1048576\n"), 0644); err != nil {
		t.Fatal(err)
	}

	processes, cgroupRSS, err := collectProcessMetrics(eventsPath, procRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(processes) != 0 {
		t.Errorf("expected 0 processes (all exited), got %d", len(processes))
	}
	if cgroupRSS != 1048576 {
		t.Errorf("cgroupRSS = %d, want 1048576", cgroupRSS)
	}
}

func TestCollectProcessMetrics_cgroupsv1Layout(t *testing.T) {
	t.Parallel()

	cgroupDir := t.TempDir()
	procRoot := t.TempDir()

	// cgroupsv1 layout: memory.oom_control instead of memory.events.
	eventsPath := filepath.Join(cgroupDir, "memory.oom_control")
	if err := os.WriteFile(eventsPath, []byte("oom_kill 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("101\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// cgroupsv1 uses memory.usage_in_bytes.
	if err := os.WriteFile(filepath.Join(cgroupDir, "memory.usage_in_bytes"), []byte("262144000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pidDir := filepath.Join(procRoot, "101")
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("/usr/bin/chromium\x00"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "status"), []byte("VmHWM:\t 100000 kB\nVmRSS:\t 100000 kB\n"), 0644); err != nil {
		t.Fatal(err)
	}

	processes, cgroupRSS, err := collectProcessMetrics(eventsPath, procRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cgroupRSS != 262144000 {
		t.Errorf("cgroupRSS = %d, want 262144000", cgroupRSS)
	}
	if len(processes) != 1 || processes[0].Type != "browser" {
		t.Errorf("unexpected processes: %+v", processes)
	}
}
