# Chromium Observability

crocochrome exposes three sources of observability for Chromium sessions: structured
log entries emitted at session teardown, a Prometheus counter for OOM kills, and a
Prometheus histogram for collection overhead. Together they allow operators to
understand per-check memory usage, attribute that usage to specific Chromium process
types, and detect kernel OOM kills.

## Enabling

The OOM kill counter (`sm_crocochrome_chromium_oom_kills_total`) is always active.
The log-based collection is split across two independent flags:

```
crocochrome -process-metrics          # per-process RSS, peakRSS, and session summary
crocochrome -cdp-metrics              # per-renderer JS heap and DOM metrics via CDP
crocochrome -process-metrics -cdp-metrics  # both
```

**`-process-metrics`** enables per-process RSS collection via `cgroup.procs` and
`/proc/<pid>/status`. This is fast (file reads only, no network calls) and adds
negligible overhead to `DELETE /sessions`. Recommended to enable fleet-wide.

**`-cdp-metrics`** enables CDP `Performance.getMetrics` collection via `/json/list`.
This opens a WebSocket to each renderer process and adds up to 300ms to the
`DELETE /sessions` response path. Enable deliberately after evaluating the latency
impact for your deployment.

Without either flag, behaviour is identical to before this feature was introduced.
The Prometheus metrics are registered unconditionally but will have zero observations
when their respective flags are off.

---

## Log entries

Three structured log entries are emitted per session at teardown, all at `INFO`
level. Every entry carries the session context fields `sessionID`, `id`
(check ID), `tenantID`, and `regionID`, which can be used to correlate entries
across the three types and across multiple executions of the same check.

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
| `zygote` | Chromium zygote — the process-spawning intermediary; not a user-visible component |
| `crashpad` | Crash reporting handler; ~2 MiB, noise for memory analysis |
| `crocochrome` | The crocochrome binary itself |
| `tini` | Container init process; ~0 MiB, noise for memory analysis |
| `unknown` | Process exited before cmdline could be read (expected race at teardown) |

**Important caveats on `rss` and `peakRSS`:**

- **Do not sum `rss` across entries.** Each process's `rss` includes shared library
  pages (glibc, libchromium, etc.) that are physically shared between processes but
  counted once per process in `VmRSS`. Summing across a session inflates the total
  by approximately 4–5×. Use `cgroupRSS` from `chromium session memory` for the
  accurate container total.
- **`peakRSS` is the session peak for Chromium subprocesses** because they are
  spawned fresh per session. For `crocochrome` and `tini`, which persist across
  sessions, it reflects the peak since container start.
- **`rss` is a point-in-time snapshot at teardown**, not the peak. A process that
  spiked and freed memory before teardown will show a lower `rss` but a higher
  `peakRSS`.

---

### `chromium session memory`

One entry per session. Provides the accurate container-level memory total and
summary counts.

| Field | Description |
|---|---|
| `cgroupRSS` | Total memory used by all processes in the container cgroup, in bytes. Kernel-maintained; deduplicates shared pages. This is the ground truth for "how much memory did this check use." |
| `processCount` | Number of processes found in the cgroup at teardown |

`cgroupRSS` is read from `memory.current` (cgroupsv2) or `memory.usage_in_bytes`
(cgroupsv1). Unlike the sum of per-process `rss` values, it counts each physical
page exactly once regardless of how many processes share it.

This entry is gated on `-process-metrics`.

---

### `chromium renderer summary`

One entry per session, emitted when `-cdp-metrics` is enabled. `rendererCount`
is a CDP concept, so it lives on its own line independent of `-process-metrics`
rather than on `chromium session memory`.

| Field | Description |
|---|---|
| `rendererCount` | Number of CDP page targets from which renderer metrics were collected |

---

### `chromium renderer metrics`

One entry per renderer page target, collected via the Chrome DevTools Protocol
(CDP `Performance.getMetrics`). These are **renderer-internal V8 and Blink counters**,
not OS-level memory numbers.

| Field | Description |
|---|---|
| `targetURL` | URL of the page rendered by this renderer process |
| `JSHeapUsedSize` | Bytes of live JS objects in V8's heap |
| `JSHeapTotalSize` | Total V8 heap capacity committed (used + free) |
| `Nodes` | Live DOM node count |
| `Documents` | Live Document object count |
| `Frames` | In-process frame count |
| `JSEventListeners` | Registered JS event listener count |
| `LayoutCount` | Cumulative forced layout operations since renderer start |
| `RecalcStyleCount` | Cumulative style recalculations |
| `ScriptDuration` | Cumulative JS execution time in seconds |
| `TaskDuration` | Cumulative main-thread task time in seconds |

**What these tell you:** `JSHeapUsedSize` and `Nodes` are the most operationally
useful. A high `JSHeapUsedSize` indicates JS memory pressure in the renderer. A
high `Nodes` count or one that grows across successive sessions for the same check
indicates a DOM leak in the page or check script.

**What these do not tell you:** These metrics are renderer-internal and do not cover
OS-level memory usage, GPU memory, or the browser or network service processes.
They complement `chromium process memory` rather than replacing it.

**Simple checks** (no page navigation, idle tabs) will show `JSHeapUsedSize ≈ 0`
and low `Nodes`. Useful signal only appears for checks that actually run JavaScript
on real pages.

---

## Prometheus metrics

### `sm_crocochrome_chromium_oom_kills_total`

Counter. Incremented once per process killed by the kernel OOM killer within the
container cgroup during a session. Detected by sampling the cgroup `oom_kill`
counter before and after `cmd.Run()`.

A non-zero rate indicates the container's memory limit was exceeded and the kernel
intervened. Unlike `signal: killed` in process exit logs (which is identical for
both OOM kills and normal SIGKILL teardown), this counter distinguishes the two.

Always active; does not require `-cdp-metrics`.

### `sm_crocochrome_cdp_collection_duration_seconds`

Histogram. Records the wall-clock time spent on the CDP collection window only —
the `/json/list` enumeration plus the per-renderer `Performance.getMetrics`
round-trips — when `-cdp-metrics` is enabled. It does **not** include the process
RSS walk. Zero observations when the flag is off.

CDP metrics are collected while Chromium is still alive, before the session
context is cancelled (SIGKILL). In normal operation the distribution therefore
sits well below the 300ms collection ceiling — typically under ~50ms, scaling
with the number of renderer targets. Observations near 300ms indicate a renderer
that stopped responding and hit the collection timeout, not the common case.

This overhead sits inside the `DELETE /sessions` response path — see
[Deployment note in the PR](https://github.com/grafana/crocochrome/pull/438) for
the full critical chain.

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

**Renderer JS heap pressure (JS-heavy checks):**
```logql
{namespace="sm-k6-mq", container="crocochrome"}
  |= "chromium renderer metrics"
  | json
  | JSHeapUsedSize > 524288000
  | line_format "check={{.id}} url={{.targetURL}} heap={{.JSHeapUsedSize}} nodes={{.Nodes}}"
```

**GPU process peak RSS trend for a specific check (e.g. investigating GPU pressure):**
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

**OOM kills as a fraction of finished sessions (should be near zero):**
```promql
rate(sm_crocochrome_chromium_oom_kills_total[5m])
  /
rate(sm_crocochrome_chromium_executions_total{state="finished"}[5m])
```

**p99 CDP collection overhead:**
```promql
histogram_quantile(0.99,
  sum(rate(sm_crocochrome_cdp_collection_duration_seconds_bucket[30m]))
  by (le, cluster)
)
```

---

## Summary of caveats

| Caveat | Detail |
|---|---|
| Don't sum per-process `rss` | Use `cgroupRSS` for the accurate container total; per-process VmRSS counts shared pages multiple times |
| `rss` is a teardown snapshot | Use `peakRSS` to understand the high-water mark; `rss` may be lower if memory was freed before teardown |
| `peakRSS` is per-process-lifetime | For Chromium subprocesses (fresh per session) this equals the session peak. For `crocochrome` and `tini` it reflects the container lifetime peak |
| Renderer metrics require active checks | `JSHeapUsedSize` and `Nodes` are only meaningful for checks that navigate real pages with JavaScript |
| CDP collection adds latency to `DELETE` | The 300ms collection timeout sits inside the `DELETE /sessions` HTTP response path; see `sm_crocochrome_cdp_collection_duration_seconds` to observe this overhead |
| OOM kill attribution | The `oom_kill` delta is per-session but counts kills in the container cgroup — a lingering process from a prior session could in rare cases increment the counter for the current session |
