FROM --platform=$BUILDPLATFORM ghcr.io/grafana/grafana-build-tools:v1.48.0@sha256:f29499edc64eaab08a8bc724f43b3eb15717182a45f12e229d427039ad74d334 AS buildtools
WORKDIR /crocochrome

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Build with CGO_ENABLED=0 as grafana-build-tools is debian-based.
RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/go/pkg \
  make GOOS=$TARGETOS GOARCH=$TARGETARCH DISTDIR=/usr/local/bin LOCAL=true build

# For setting caps, use the same image than the final layer is using to avoid pulling two distinct ones.
FROM ghcr.io/grafana/chromium-swiftshader-alpine:152.0.7977.82-r1-3.24.2@sha256:90f7fbf603a7482689db1d5d996aef895dbb6ddf125df0f204cfb1b3852eb91a AS setcapper

RUN apk --no-cache add libcap

COPY --from=buildtools /usr/local/bin/crocochrome /usr/local/bin/crocochrome

# The following capabilities are used by crocochrome to run chromium as a different user, and to clean up after it. More
# details about what each cap is used for can be found in doc/capabilities.md.
# WARNING: The container MUST be also granted all of the following capabilities too, or the CRI will refuse to start it.
RUN setcap cap_setuid,cap_setgid,cap_kill,cap_chown,cap_dac_override,cap_fowner+ep /usr/local/bin/crocochrome

# Steps shared by the hardened and the unprivileged images.
FROM ghcr.io/grafana/chromium-swiftshader-alpine:152.0.7977.82-r1-3.24.2@sha256:90f7fbf603a7482689db1d5d996aef895dbb6ddf125df0f204cfb1b3852eb91a AS base

RUN adduser --home / --uid 6666 --shell /bin/nologin --disabled-password k6

RUN apk --no-cache add --repository community tini

# As the hardened image relies on file capabilities, we cannot set `allowPrivilegeEscalation: false` in k8s. As a
# workaround, and to lower potential attack surface, we get rid of any file that has the setuid bit set, such as
# /usr/lib/chromium/chrome-sandbox. The unprivileged image does not need this, but keeps it as defense in depth.
RUN find / -type f -perm -4000 -delete

# Unprivileged image, built with `--target unprivileged`. The crocochrome binary has no file capabilities, and runs
# chromium as its own user. See doc/capabilities.md.
FROM base AS unprivileged

# The binary has no extra capabilities, so it can be run by any user. This allows running the image with an arbitrary
# UID.
COPY --from=buildtools --chmod=0555 /usr/local/bin/crocochrome /usr/local/bin/crocochrome

USER k6

ENTRYPOINT [ "tini", "--", "/usr/local/bin/crocochrome", "-user-group=0" ]

# Hardened image. This MUST remain the last stage, so it is the default target.
FROM base AS hardened

# The crocochrome binary has extra capabilities, so we make sure only the k6 user (and not nobody) can run it.
COPY --from=setcapper --chown=k6:k6 --chmod=0500 /usr/local/bin/crocochrome /usr/local/bin/crocochrome

USER k6

ENTRYPOINT [ "tini", "--", "/usr/local/bin/crocochrome" ]
