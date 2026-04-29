# Chromium Observability

crocochrome exposes two sources of observability for Chromium sessions: structured
log entries emitted at session teardown and a Prometheus counter for OOM kills.
Together they allow operators to understand per-check memory usage, attribute that
usage to specific Chromium process types, and detect kernel OOM kills.

## Enabling

The OOM kill counter (`sm_crocochrome_chromium_oom_kills_total`) is always active.
Per-process RSS collection is opt-in:

```
crocochrome -process-metrics
```

Without this flag, no new log entries are emitted and behaviour is identical to
before this feature was introduced. The Prometheus metrics are registered
unconditionally but will have zero observations when the flag is off.

---

## Log entries

Two structured log entries are emitted per session at teardown, both at `INFO`
level. Every entry carries the session context fields `sessionID`, `id`
(check ID), `tenantID`, and `regionID`, which can be used to correlate entries
across the two types and across multiple executions of the same check.

### `chromium process memory`

One entry per OS process found in the container cgroup at teardown. Covers every
process running inside the container — Chromium subprocesses as well as crocochrome
and its supervisor.

| Field | Description |
|---|---|
| `processType` | Process classification (see table below) |
| `pid` | OS process ID |
| `rss` | Current resident set size in bytes at the moment of collection |
| `peakRSS` | Peak RSS since the process started (Linux `VmHWM`), in bytes |

**Process types:**

| Type | What it is |
|---|---|
| `browser` | Main Chromium browser process |
| `renderer` | Renderer process (Blink + V8); one or more per session depending on site isolation |
| `gpu-process` | GPU compositor (SwiftShader software renderer in containers) |
| `network-service` | Network service utility |
| `utility` | Other Chromium utility process (e.g. storage service) |
| `zygote` | Chromium zygote — the process-spawning intermediary |
| `crashpad` | Crash reporting handler; ~2 MiB, noise for memory analysis |
| `crocochrome` | The crocochrome binary itself |
| `tini` | Container init process; ~0 MiB, noise for memory analysis |
| `unknown` | Process exited before cmdline could be read (expected race at teardown) |

**Important caveats on `rss` and `peakRSS`:**

- **Do not sum `rss` across entries.** Each process's `rss` includes shared library
  pages that are physically shared between processes but counted once per process in
  `VmRSS`. Summing across a session inflates the total by approximately 4–5×. Use
  `cgroupRSS` from `chromium session memory` for the accurate container total.
- **`peakRSS` is the session peak for Chromium subprocesses** because they are
  spawned fresh per session. For `crocochrome` and `tini`, which persist across
  sessions, it reflects the peak since container start.
- **`rss` is a point-in-time snapshot at teardown**, not the peak. A process that
  spiked and freed memory before teardown will show a lower `rss` but a higher
  `peakRSS`.

---

### `chromium session memory`

One entry per session. Provides the accurate container-level memory total and
process count.

| Field | Description |
|---|---|
| `cgroupRSS` | Total memory used by all processes in the container cgroup, in bytes. Kernel-maintained; deduplicates shared pages. This is the ground truth for "how much memory did this check use." |
| `processCount` | Number of processes found in the cgroup at teardown |

`cgroupRSS` is read from `memory.current` (cgroupsv2) or `memory.usage_in_bytes`
(cgroupsv1). Unlike the sum of per-process `rss` values, it counts each physical
page exactly once regardless of how many processes share it.

Note: `cgroupRSS` is a point-in-time value read at the end of the session. For
sessions that were OOM-killed, the killed processes have already freed their memory
by teardown time, so `cgroupRSS` will be lower than the peak that triggered the kill.

---

## Prometheus metrics

### `sm_crocochrome_chromium_oom_kills_total`

Counter. Incremented once per process killed by the kernel OOM killer within the
container cgroup during a session. Detected by sampling the cgroup `oom_kill`
counter before and after `cmd.Run()`.

A non-zero rate indicates the container's memory limit was exceeded and the kernel
intervened. Unlike `signal: killed` in process exit logs (which is identical for
both OOM kills and normal SIGKILL teardown), this counter distinguishes the two.

Always active; does not require `-process-metrics`.

---

## Practical queries

### Loki

**Total memory used by a specific check over time:**
```logql
{namespace="sm-k6-mq", container="crocochrome"}
  |= "chromium session memory"
  | json
  | id=`<check-id>`
  | unwrap cgroupRSS
  | __error__=""
```

**Checks exceeding a memory threshold:**
```logql
{namespace="sm-k6-mq", container="crocochrome"}
  |= "chromium session memory"
  | json
  | cgroupRSS > 1073741824
  | line_format "check={{.id}} tenant={{.tenantID}} total={{.cgroupRSS}}"
```

**Per-process-type memory breakdown for a specific check:**
```logql
{namespace="sm-k6-mq", container="crocochrome"}
  |= "chromium process memory"
  | json
  | id=`<check-id>`
  | line_format "type={{.processType}} rss={{.rss}} peak={{.peakRSS}}"
```

**GPU process peak RSS trend for a specific check:**
```logql
{namespace="sm-k6-mq", container="crocochrome"}
  |= "chromium process memory"
  | json
  | id=`<check-id>` and processType=`gpu-process`
  | unwrap peakRSS
  | __error__=""
```

### Prometheus

**OOM kill rate across the fleet:**
```promql
rate(sm_crocochrome_chromium_oom_kills_total[5m])
```

**OOM kills as a fraction of finished sessions:**
```promql
rate(sm_crocochrome_chromium_oom_kills_total[5m])
  /
rate(sm_crocochrome_chromium_executions_total{state="finished"}[5m])
```

---

## Summary of caveats

| Caveat | Detail |
|---|---|
| Don't sum per-process `rss` | Use `cgroupRSS` for the accurate container total; per-process VmRSS counts shared pages multiple times |
| `rss` is a teardown snapshot | Use `peakRSS` to understand the high-water mark; `rss` may be lower if memory was freed before teardown |
| `peakRSS` scope | For Chromium subprocesses (fresh per session) this equals the session peak. For `crocochrome` and `tini` it reflects the container lifetime peak |
| OOM-killed sessions | `cgroupRSS` at teardown shows post-kill state (killed processes have freed memory). The OOM kill counter is the right signal; `cgroupRSS` from healthy sessions shows the trend leading up to it |
| OOM kill attribution | The `oom_kill` delta is per-session but counts kills in the container cgroup — a lingering process from a prior session could in rare cases increment the counter for the current session |
