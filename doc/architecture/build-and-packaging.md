---
component: Build, container & CI
source_paths:
    - Dockerfile
    - Makefile
    - scripts/version
    - .github/workflows/
    - internal/version/version.go
last_reviewed_commit: 3eeec51064
generated: 2026-06-05
---
# Build, container & CI

**Sources:** `Dockerfile`, `Makefile`, `scripts/version`,
`.github/workflows/`, `internal/version/version.go`

## Overview

Crocochrome ships as two container images built from the same Dockerfile: a
statically-linked Go binary on top of a Chromium-bearing Alpine base. The
**hardened** image (the default) runs Chromium as a different user, using file
capabilities. The **unprivileged** image needs no capabilities and runs Chromium
as its own user. This page covers how the binary is built,
how the image is assembled (and hardened), how the Makefile orchestrates builds
and tests in a pinned toolchain container, and what CI enforces.

A recurring theme: **the build embeds version info from the git working tree**,
so anything that makes that tree look "dirty" (notably a `.dockerignore`) breaks
version reporting. CI guards against this.

## How the binary learns its version

`internal/version` reads Go's build info (`debug.ReadBuildInfo`) for the module
version, `vcs.revision` (commit), and `vcs.time` (build timestamp). These feed
the startup log line and the `sm_crocochrome_info` metric. For this to be
populated, the build must happen in a clean git checkout with full history —
hence the `.dockerignore` and `fetch-depth: 0` concerns below.

`scripts/version` produces the human-facing version string (used as the image
tag at release time).

## The Dockerfile (multi-stage)

```mermaid
flowchart LR
    A["Stage: buildtools<br/>ghcr.io/grafana/grafana-build-tools<br/>make LOCAL=true build (CGO_ENABLED=0)"]
    B["Stage: setcapper<br/>chromium-swiftshader-alpine<br/>apk add libcap; setcap on binary"]
    C["Stage: base<br/>chromium-swiftshader-alpine<br/>adduser k6; add tini;<br/>delete setuid bins"]
    D["Stage: unprivileged<br/>COPY binary 0555"]
    E["Stage: hardened (default)<br/>COPY binary 0500"]
    A -- "/usr/local/bin/crocochrome" --> B
    A -- "binary" --> D
    B -- "binary w/ file caps" --> E
    C --> D
    C --> E
```

1. **`buildtools`** — `ghcr.io/grafana/grafana-build-tools` (pinned by tag +
   digest). Compiles with `make ... LOCAL=true build` and `CGO_ENABLED=0`
   (the build image is Debian-based; a static binary keeps the final Alpine
   image happy). Honors `TARGETOS`/`TARGETARCH` for multiarch.

2. **`setcapper`** — uses the **same** `chromium-swiftshader-alpine` base as the
   final stage (so no extra image is pulled), installs `libcap`, and runs:

   ```
   setcap cap_setuid,cap_setgid,cap_kill,cap_chown,cap_dac_override,cap_fowner+ep \
       /usr/local/bin/crocochrome
   ```

   These caps let Crocochrome drop privileges, kill the Chromium tree, and
   hand over and clean up Chromium's per-session temporary directory. See
   [security.md](security.md) and [capabilities.md](../capabilities.md).

3. **`base`** — `ghcr.io/grafana/chromium-swiftshader-alpine` (Chromium +
   SwiftShader software renderer), shared by both images. It:

   - creates user `k6` (UID **6666**, no home, `/bin/nologin`, password disabled);
   - installs `tini` from the Alpine community repo;
   - **deletes every setuid binary** (`find / -type f -perm -4000 -delete`),
     notably `/usr/lib/chromium/chrome-sandbox`, to shrink attack surface
     (necessary because file caps preclude `allowPrivilegeEscalation: false`).

4. **`unprivileged`** — built with `--target unprivileged`. It:

   - copies the binary from `buildtools`, without file capabilities, owned by
     root with `--chmod=0555`, so any UID can run it;
   - runs as `USER k6` with entrypoint
     `tini -- /usr/local/bin/crocochrome -user-group=0`, so Chromium runs as the
     same user as crocochrome.

5. **`hardened`** — the last stage, and therefore the default target. It:

   - copies the binary `--chown=k6:k6 --chmod=0500` so only `k6` (not `nobody`,
     the user Chromium runs as) can read or run the capability-bearing binary;
   - runs as `USER k6` with entrypoint `tini -- /usr/local/bin/crocochrome`
     (tini as PID 1 for signal forwarding and zombie reaping).

   It must remain the last stage, so that `docker build .` keeps producing the
   hardened image.

The base image tag encodes the Chromium version (e.g.
`147.0.7727.101-r0-3.23.4`); both tag and digest are pinned and updated by
Renovate.

## The Makefile

Build, test, and lint run **inside the `grafana-build-tools` container** by
default, for a reproducible toolchain. Set `LOCAL=true` to run natively
(automatically set when `CI=true`).

| Target                | Command (inside buildtools unless LOCAL)                            | Notes                                                        |
|-----------------------|---------------------------------------------------------------------|--------------------------------------------------------------|
| `build`               | `go build -o dist/crocochrome ./cmd/crocochrome/` (`CGO_ENABLED=0`) |                                                              |
| `test`                | `go test -v ./...`                                                  |                                                              |
| `test-short`          | `go test -v -short ./...`                                           | skips integration                                            |
| `test-integration`    | `go test -v -tags=integration ./integration/...`                    | **not** wrapped in buildtools (needs the host Docker socket) |
| `lint`                | `golangci-lint run ./...`                                           |                                                              |
| `lint-version`        | extract golangci-lint version from the image                        | lets CI download a matching linter                           |
| `build-container`     | `docker build -t test.local/crocochrome .`                          | hardened image                                               |
| `build-container-unprivileged` | `docker build --target unprivileged -t test.local/crocochrome-unprivileged .` | unprivileged image                         |
| `shell`               | open a shell in buildtools                                          | debugging                                                    |
| `clean` / `distclean` | remove artifacts / `git clean -Xf`                                  |                                                              |

The buildtools wrapper mounts the repo, the Go build/module caches, the
golangci-lint cache, `/etc/passwd` (read-only), and `/var/run/docker.sock`, and
runs as the host user (`--user $(id -u):$(id -g)`, `--net=host`). The Docker
socket and host networking are what make `test-integration` (testcontainers)
work — and the reason that one target is not containerized (the socket is
usually not world-readable). See [testing.md](testing.md).

## CI workflows (`.github/workflows/`)

### `push-pr.yaml` — runs on PRs and pushes to `main`

| Job                       | What it does                                                                                                                                                  |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Build**                 | `make build`                                                                                                                                                  |
| **Test**                  | `make test` on `ubuntu-latest` and `ubuntu-24.04-arm` (matrix)                                                                                                |
| **Integration tests**     | `make test-integration` on both architectures                                                                                                                 |
| **Build container image** | `docker/build-push-action`, `push: false`, `platforms: linux/amd64,linux/arm64`, GHA cache; matrix over the `hardened` and `unprivileged` targets             |
| **Lint**                  | `make CI=false LOCAL=false S= lint` (forces the linter to run inside the container so its Go toolchain matches), preceded by the `.dockerignore` sanity check |

#### The `.dockerignore` sanity check

The lint job fails if a `.dockerignore` file exists:

```
if [[ -e ".dockerignore" ]]; then ... exit 1; fi
```

Rationale: `.dockerignore` would stop git-tracked files from being copied into
the build context, making Go see the build tree as dirty and self-report a dirty
version — which breaks the deployment process. (If one were ever truly needed,
`.gitignore` must be a superset and the check updated.)

### `publish-container.yaml` — runs on `v*` tags

Checks out with `fetch-depth: 0` (full history, required by `scripts/version`),
computes the version, sets up QEMU + Buildx (with `cache-binary: false` to avoid
[cache poisoning](https://woodruffw.github.io/zizmor/audits/#cache-poisoning)), logs into `ghcr.io`, and builds+pushes two multiarch
images with `context: .` (the full checkout, not a shallow clone): the hardened
image tagged `ghcr.io/<repo>:<version>`, and the `unprivileged` target tagged
`ghcr.io/<repo>:<version>-unprivileged`.

### Other workflows

- `publish-techdocs.yaml` — publishes this documentation (mkdocs / Backstage
  TechDocs) from `doc/`.
- `release-please.yml` — automated release PRs / notes.
- `renovate-validate.yaml`, `validate-policy-bot-config.yml` — config validation.

## When to update

- The Dockerfile stages, base images, capability set, user/UID, or hardening
  steps change → update the multi-stage section and the diagram.
- A Makefile target is added/removed or its command changes, or the buildtools
  mount/flag set changes → update the targets table and the wrapper description.
- A CI job is added/removed or its matrix/steps change → update the workflow
  tables.
- The `.dockerignore` policy changes → update that subsection (and note the CI
  job no longer named here will go stale).
- The versioning mechanism (`internal/version`, `scripts/version`) changes →
  update "How the binary learns its version".

