# DEVELOPMENT.md

This file provides guidance to developers when working with code in this
repository.

## Commands

Build, lint, and test run inside the `grafana-build-tools` Docker image via
Make:

```bash
make build              # Build binary to dist/crocochrome
make test               # Run all tests (via Docker)
make lint               # Run golangci-lint (via Docker)
make build-container    # Build the Docker image
make test-integration   # Run integration tests
make clean              # Remove intermediate build artifacts
make distclean          # Remove all build artifacts (git clean -Xf)
make help               # List available targets
```

If you need to inspect the commands used by the make targets, you can use `make
-n <target>`. Prefer the make targets over running the commands directly as
they are the stable interfaces to these processes (and in most cases, they are
shorter, too).

By default `test`, and `lint` run inside the `grafana-build-tools` container.
Set `LOCAL=true` to run them natively instead (this is set automatically when
`CI=true`):

```bash
make LOCAL=true test
make LOCAL=true lint
```

Be warned that `LOCAL=true` might cause build issues because mismatches between
the local Go version and the one shipped with `grafana-build-tools`. Unless you
have a good reason for using the local tools, prefer the versions in the
container image. The overhead of running things in a container is about 100-200
ms per execution.

Integration tests require Docker and use testcontainers.

## Architecture

Crocochrome is a **Chromium supervisor**: an HTTP API that launches and manages
Chromium browser processes on demand, one session at a time. It is used as a
backend for Grafana Synthetic Monitoring browser checks.

### Key constraints

- **One concurrent session only** — creating a new session kills any existing
  one.
- Runs Chromium with `--no-sandbox` (user namespaces not universally available
  in containers; see `doc/chromium-sandbox.md`).
- Requires Linux capabilities (`cap_setuid`, `cap_setgid`, `cap_kill`,
  `cap_chown`, `cap_dac_override`, `cap_fowner`) set on the binary via `setcap`
  in the Dockerfile. See `doc/capabilities.md`.
- Designed for read-only root filesystems — Chromium writes to `/chromium-tmp`
  only.

### Package layout

- **`cmd/crocochrome/main.go`** — wires together HTTP server, supervisor,
  metrics, and graceful shutdown (2-minute grace period for active sessions).
- **`internal/crocochrome/`** — `Supervisor` type: session lifecycle
  management, Chromium process launch, metrics tracking.
- **`internal/http/`** — HTTP handlers. Routes: `GET/POST /sessions`, `DELETE
  /sessions/{id}`, `/proxy/{id}` (WebSocket proxy to Chromium's remote
  debugging port).
- **`internal/chromium/`** — thin client that polls Chromium's `/json/version`
  endpoint with retry logic to detect readiness.
- **`internal/metrics/`** — Prometheus metric definitions (`sm_crocochrome_*`
  namespace).
- **`internal/testutil/`** — mock Chromium server and heartbeat helpers used in
  unit tests.
- **`integration/`** — integration tests using testcontainers; build and run
  the actual container image.

### Session lifecycle

1. Client `POST /sessions` with `CheckInfo` JSON (check metadata).
2. Supervisor kills any existing session, then spawns Chromium as a subprocess
   with a dedicated temp dir and an OS-assigned debug port.
3. `internal/chromium` client polls until Chromium is ready, then returns
   `SessionInfo` (includes debug URL) to the caller.
4. Caller proxies CDP traffic through `/proxy/{id}`.
5. Session ends via `DELETE /sessions/{id}` or when the supervisor context is
   cancelled.

### Container

The final image is based on `ghcr.io/grafana/chromium-swiftshader-alpine`
(provides Chromium + SwiftShader software renderer). The build uses a
multi-stage Dockerfile: compile with `grafana-build-tools`, then copy binary
and set capabilities in the final image. Runs as user `k6` (UID 6666) under
`tini` as PID 1.

### No `.dockerignore`

The CI pipeline explicitly checks that `.dockerignore` does **not** exist — the
Docker build embeds version info from the git working tree and requires a clean
state.
