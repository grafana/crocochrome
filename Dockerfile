FROM --platform=$BUILDPLATFORM ghcr.io/grafana/grafana-build-tools:v1.43.1@sha256:884b1156cb42d8fc8609f1e0547107c081b3567c41e0a29f53bcee3da2713ed2 AS buildtools
WORKDIR /crocochrome

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Build with CGO_ENABLED=0 as grafana-build-tools is debian-based.
RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/go/pkg \
  make GOOS=$TARGETOS GOARCH=$TARGETARCH DISTDIR=/usr/local/bin LOCAL=true build

# For setting caps, use the same image than the final layer is using to avoid pulling two distinct ones.
FROM ghcr.io/grafana/chromium-swiftshader-alpine:149.0.7827.53-r0-3.23.4@sha256:23f3e68d0a482c2d25a325f0e3f3b4e4e2888c5a2812eadacc80f761b5208a70 AS setcapper

RUN apk --no-cache add libcap

COPY --from=buildtools /usr/local/bin/crocochrome /usr/local/bin/crocochrome

# The following capabilities are used by sm-k6-runner to sandbox the k6 binary. More details about what each cap is used
# for can be found in /sandbox/sandbox.go.
# WARNING: The container MUST be also granted all of the following capabilities too, or the CRI will refuse to start it.
RUN setcap cap_setuid,cap_setgid,cap_kill,cap_chown,cap_dac_override,cap_fowner+ep /usr/local/bin/crocochrome

FROM ghcr.io/grafana/chromium-swiftshader-alpine:149.0.7827.53-r0-3.23.4@sha256:23f3e68d0a482c2d25a325f0e3f3b4e4e2888c5a2812eadacc80f761b5208a70

RUN adduser --home / --uid 6666 --shell /bin/nologin --disabled-password k6

RUN apk --no-cache add --repository community tini

# As we rely on file capabilities, we cannot set `allowPrivilegeEscalation: false` in k8s. As a workaround, and to lower
# potential attack surface, we get rid of any file that has the setuid bit set, such as
# /usr/lib/chromium/chrome-sandbox.
RUN find / -type f -perm -4000 -delete

# The crocochrome binary has extra capabilities, so we make sure only the k6 user (and not nobody) can run it.
COPY --from=setcapper --chown=k6:k6 --chmod=0500 /usr/local/bin/crocochrome /usr/local/bin/crocochrome

USER k6

ENTRYPOINT [ "tini", "--", "/usr/local/bin/crocochrome" ]
