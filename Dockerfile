FROM --platform=$BUILDPLATFORM ghcr.io/grafana/grafana-build-tools:v0.23.1@sha256:780624baada530c2e80c6d7afb3c15c4790e24f7bb90fe17eec81aabaecbbc77 as buildtools
WORKDIR /crocochrome

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Build with CGO_ENABLED=0 as grafana-build-tools is debian-based.
RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/go/pkg \
  CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /usr/local/bin/crocochrome ./cmd

FROM alpine:3.20.3@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d as setcapper

RUN apk --no-cache add libcap

COPY --from=buildtools /usr/local/bin/crocochrome /usr/local/bin/crocochrome

# The following capabilities are used by sm-k6-runner to sandbox the k6 binary. More details about what each cap is used
# for can be found in /sandbox/sandbox.go.
# WARNING: The container MUST be also granted all of the following capabilities too, or the CRI will refuse to start it.
RUN setcap cap_setuid,cap_setgid,cap_kill,cap_chown,cap_dac_override,cap_fowner+ep /usr/local/bin/crocochrome

# WARNING: Do NOT upgrade alpine, as this release is the last one containing a working chromium.
# 3.20.0 onwards do not support listening on addresses other than localhost, which is required for crocochrome to work.
# https://issues.chromium.org/issues/327558594
FROM alpine:3.20.3@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d

RUN adduser --home / --uid 6666 --shell /bin/nologin --disabled-password k6

# Renovate updates the pinned packages below.
# The --repository arg is required for renovate to know which alpine repo it should look for updates in.
# To keep the renovate regex simple, only keep one package installation per line.
RUN apk --no-cache add --repository community tini=0.19.0-r3 && \
  apk --no-cache add --repository community chromium-swiftshader=130.0.6723.91-r0

# As we rely on file capabilities, we cannot set `allowPrivilegeEscalation: false` in k8s. As a workaround, and to lower
# potential attack surface, we get rid of any file that has the setuid bit set, such as
# /usr/lib/chromium/chrome-sandbox.
RUN find / -type f -perm -4000 -delete

# The crocochrome binary has extra capabilities, so we make sure only the k6 user (and not nobody) can run it.
COPY --from=setcapper --chown=k6:k6 --chmod=0500 /usr/local/bin/crocochrome /usr/local/bin/crocochrome

USER k6

ENTRYPOINT [ "tini", "--", "/usr/local/bin/crocochrome" ]
