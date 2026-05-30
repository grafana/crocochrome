# Testing strategy

**Sources:** `internal/**/*_test.go`, `internal/testutil/`, `integration/`

## Overview

Crocochrome is tested at two levels:

1. **Unit tests** that exercise individual packages with a mock Chromium, so they
   run fast and need no real browser.
2. **Integration tests** that build the real container image and drive it with
   real `k6` browser tests via [testcontainers], validating the whole stack
   end-to-end.

[testcontainers]: https://golang.testcontainers.org/

The split exists because launching a real Chromium is slow and Linux/Docker
specific; most logic (session bookkeeping, the readiness client, metrics, proc
parsing) can be tested deterministically without it.

## Running tests

| Command | What runs |
|---------|-----------|
| `make test` | all unit tests (`go test -v ./...`) inside the buildtools container |
| `make test-short` | unit tests with `-short` (integration test self-skips) |
| `make test-integration` | `go test -tags=integration ./integration/...` on the host |

See [build-and-packaging.md](build-and-packaging.md) for why
`test-integration` is the one target that runs on the host rather than inside the
buildtools container (it needs the host Docker socket).

## Unit tests & test doubles (`internal/testutil`)

Because a real browser is unavailable in unit tests, `internal/testutil`
provides a **mock Chromium** and a process-liveness helper.

### Mock Chromium (`testutil/chromium.go`)

- `HTTPInfo(t, handler)` — starts an `httptest.Server` that serves
  `GET /json/version` with the supplied handler and returns its port. Cleaned up
  via `t.Cleanup`. This lets the [readiness client](chromium-client.md) and the
  [supervisor](supervisor.md) be pointed at a fake "Chromium".
- `ChromiumVersionHandler` — returns a realistic `/json/version` JSON body
  (including a `webSocketDebuggerUrl`), mimicking a real headless Chromium.
- `InternalServerErrorHandler` — returns `500`, used to drive the failure /
  retry-exhaustion paths.

### Heartbeat helper (`testutil/heartbeat.go`)

`Heartbeat` verifies that processes Crocochrome launches are actually started and
later killed, without racing on process state:

- `NewHeartbeat(t)` writes a small `heartbeat.sh` into `t.TempDir()`. The script
  appends the current Unix timestamp to a `canary-<pid>` file once per second.
  It does **not** run the script — the caller does (e.g. as a stand-in for
  Chromium). It takes `syscall.ForkLock` while writing the script to avoid the
  FD-leak-into-`exec` problem ([golang/go#22315]) that would cause `ETXBSY`.
- `AssertAliveDead(alive, dead)` waits ~2s and then counts canary files whose
  timestamp is fresh (process alive) vs. stale (process dead), failing the test
  if the counts don't match expectations.

[golang/go#22315]: https://github.com/golang/go/issues/22315

### What unit tests cover

Each package carries its own `_test.go` (e.g. `internal/http/http_test.go`,
tests in `internal/crocochrome/`). Notably, the `proc.go`/`cgroup.go` logic is
testable off real hardware because `Options.ProcFSRoot` and
`CgroupMemoryEventsPath` can point at temp-directory fixtures.

## Integration tests (`integration/`)

Built behind the `//go:build integration` tag so they only run via
`make test-integration`.

### `buildcontainer_test.go`

`buildImage(repoRoot, name)` shells out to `docker build` directly (with a random
tag when none is given, enabling parallel runs). It exists because testcontainers
cannot build images that use BuildKit features such as `COPY --chown` and
multiarch — which the [Dockerfile](build-and-packaging.md) relies on.

### `integration_test.go` — `TestIntegration`

The flow:

1. Builds the crocochrome image with `buildImage`.
2. Creates an attachable Docker network.
3. Starts the crocochrome container, mounting a writable volume at
   `/chromium-tmp` (required since the rootfs is read-only) and exposing `8080`.
4. For each k6 image (`grafana/k6:1.7.1` and `grafana/k6:2.0.0`):
   - creates a session via the API,
   - copies a [browser test script](#the-k6-script) into the k6 container,
   - runs `k6 run` with `K6_BROWSER_WS_URL` pointed at
     `ws://<croco>:8080/proxy/<sessionID>`,
   - asserts the run exits `0` and its output contains no `error`,
   - deletes the session on cleanup.
5. `version metric is sane` — scrapes `/metrics` and checks `sm_crocochrome_info`
   has a well-formed `version`/`commit`/`timestamp` and value `1`.
6. `Handles SIGTERM gracefully` (**must be last** in the suite) — creates a
   session, sends the container SIGTERM, and confirms it exits cleanly (code `0`
   within a 5s stop timeout) after the session is deleted, validating the
   [graceful-shutdown ordering](entrypoint-and-lifecycle.md#graceful-shutdown).

> The **single-session constraint** is honored explicitly in the test: sessions
> are created and deleted sequentially, never in parallel (see the comment
> "Crocochrome can only run one session at a time").

### The k6 script

An inline k6 browser script (`scriptk6io`) navigates to
`https://test.k6.io/my_messages.php`, logs in, and checks the resulting heading —
i.e. it exercises a real CDP-driven browser session proxied through Crocochrome.

## When to update

- A new test helper is added to `internal/testutil`, or the mock Chromium
  response shape changes → update the test-doubles section.
- The integration suite gains/loses a sub-test, or the k6 image versions change
  → update the `TestIntegration` flow list.
- The constraint that the SIGTERM test must run last is removed (the TODO in the
  code mentions splitting it out) → update that note.
- How tests are run (Makefile targets, build tags) changes → update the
  "Running tests" table.
