FROM --platform=$BUILDPLATFORM ghcr.io/grafana/grafana-build-tools:v1.9.0@sha256:4c061aa2fc854b33dcc2d2707bee6d0dee5a523bbf243f85e8f08b6ea0d89beb AS buildtools
WORKDIR /crocochrome

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Build with CGO_ENABLED=0 as grafana-build-tools is debian-based.
RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/go/pkg \
  CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /usr/local/bin/crocochrome ./cmd

# For setting caps, use the same image than the final layer is using to avoid pulling two distinct ones.
FROM ghcr.io/grafana/chromium-swiftshader-alpine:138.0.7204.93-r0-3.22.0@sha256:5c5424e5544d3822427491bbf67815f80d0bf736874a7d26166331674fc2f9b5 AS setcapper

RUN apk --no-cache add libcap

COPY --from=buildtools /usr/local/bin/crocochrome /usr/local/bin/crocochrome

# The following capabilities are used by sm-k6-runner to sandbox the k6 binary. More details about what each cap is used
# for can be found in /sandbox/sandbox.go.
# WARNING: The container MUST be also granted all of the following capabilities too, or the CRI will refuse to start it.
RUN setcap cap_setuid,cap_setgid,cap_kill,cap_chown,cap_dac_override,cap_fowner+ep /usr/local/bin/crocochrome

FROM ghcr.io/grafana/chromium-swiftshader-alpine:138.0.7204.93-r0-3.22.0@sha256:5c5424e5544d3822427491bbf67815f80d0bf736874a7d26166331674fc2f9b5

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
