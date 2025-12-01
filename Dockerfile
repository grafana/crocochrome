FROM --platform=$BUILDPLATFORM ghcr.io/grafana/grafana-build-tools:1.23.1@sha256:66fb15e42d9b1f3ba68b28d8117c3dd6f598d5975ca8d3a01238054930356df9 AS buildtools
WORKDIR /crocochrome

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Build with CGO_ENABLED=0 as grafana-build-tools is debian-based.
RUN --mount=type=cache,target=/root/.cache/go-build \
  --mount=type=cache,target=/root/go/pkg \
  CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /usr/local/bin/crocochrome ./cmd

FROM ghcr.io/grafana/chromium-swiftshader-alpine:142.0.7444.59-r0-3.22.2@sha256:4bfff84902c23158c54dbcf94ec8267624ee30700e8c642cba9b7ebbdc756785

RUN <<EOF
  adduser --home / --uid 6666 --shell /bin/nologin --disabled-password k6
  apk --no-cache add --repository community tini nsjail
EOF

# The crocochrome binary has extra capabilities, so we make sure only the k6 user (and not nobody) can run it.
COPY --from=buildtools --chown=k6:k6 --chmod=0500 /usr/local/bin/crocochrome /usr/local/bin/crocochrome

USER k6

ENTRYPOINT [ "tini", "--", "/usr/local/bin/crocochrome" ]
