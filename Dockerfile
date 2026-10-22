FROM --platform=$BUILDPLATFORM ghcr.io/grafana/grafana-build-tools:v1.48.1@sha256:a172971a884afe099dbc15feac37e0c7f53208b772639ae327ca7abe13ffed5d AS buildtools
WORKDIR /crocochrome

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Build with CGO_ENABLED=0 as grafana-build-tools is debian-based.
RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/go/pkg \
  make GOOS=$TARGETOS GOARCH=$TARGETARCH DISTDIR=/usr/local/bin LOCAL=true build

# For setting caps, use the same image than the final layer is using to avoid pulling two distinct ones.
FROM ghcr.io/grafana/chromium-swiftshader-alpine:155.0.8059.84-r0-3.24.2@sha256:0cb6ab82a71126d01c6db2fb649484bf10c081a86cbf661f752873819f2663ef AS setcapper

RUN apk --no-cache add libcap

COPY --from=buildtools /usr/local/bin/crocochrome /usr/local/bin/crocochrome

# The following capabilities are used by sm-k6-runner to sandbox the k6 binary. More details about what each cap is used
# for can be found in /sandbox/sandbox.go.
# WARNING: The container MUST be also granted all of the following capabilities too, or the CRI will refuse to start it.
RUN setcap cap_setuid,cap_setgid,cap_kill,cap_chown,cap_dac_override,cap_fowner+ep /usr/local/bin/crocochrome

FROM ghcr.io/grafana/chromium-swiftshader-alpine:155.0.8059.84-r0-3.24.2@sha256:0cb6ab82a71126d01c6db2fb649484bf10c081a86cbf661f752873819f2663ef

RUN adduser --home / --uid 6666 --shell /bin/nologin --disabled-password k6

RUN apk --no-cache add --repository community tini

# Refresh the CA bundle in case the base image ships an outdated one (CVE-2026-111943).
RUN apk --no-cache add --upgrade ca-certificates ca-certificates-bundle

# As we rely on file capabilities, we cannot set `allowPrivilegeEscalation: false` in k8s. As a workaround, and to lower
# potential attack surface, we get rid of any file that has the setuid bit set, such as
# /usr/lib/chromium/chrome-sandbox.
RUN find / -type f -perm -4000 -delete

# The crocochrome binary has extra capabilities, so we make sure only the k6 user (and not nobody) can run it.
COPY --from=setcapper --chown=k6:k6 --chmod=0500 /usr/local/bin/crocochrome /usr/local/bin/crocochrome

USER k6

ENTRYPOINT [ "tini", "--", "/usr/local/bin/crocochrome" ]
