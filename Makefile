ROOTDIR        := $(abspath $(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
DISTDIR        := $(ROOTDIR)/dist
GO_MODULE_NAME := $(shell go list -m)
GH_REPO_NAME   := $(GO_MODULE_NAME:github.com/%=%)

export GOOS
export GOARCH

.DEFAULT_GOAL := all

S := @
V :=

LOCAL ?= false

ifeq ($(strip $(CI)),true)
LOCAL := true
S     :=
endif

ifneq ($(strip $(S)),)
.SILENT:
endif

docker ?= docker
buildtools_image ?= ghcr.io/grafana/grafana-build-tools:v1.36.0
image ?= test.local/crocochrome

ifeq ($(strip $(LOCAL)),true)
buildtools :=
else
GOCACHEDIR             := $(shell go env GOCACHE)
GOMODCACHEDIR          := $(shell go env GOMODCACHE)
GOLANGCI_LINT_CACHEDIR := $(shell golangci-lint cache status 2> /dev/null | grep ^Dir: | cut -d' ' -f2)
LOCAL_UID              := $(shell id -u)
LOCAL_GID              := $(shell id -g)

# This might fail if golangci-lint is not available, which is the case if we
# are running in CI and we are being told we aren't by passing CI=false or if
# golangci-lint is not available locally.
ifeq ($(strip $(GOLANGCI_LINT_CACHEDIR)),)
GOLANGCI_LINT_CACHEDIR := $(HOME)/.cache/golangci-lint
endif

# Passing GOCACHE and GOMODCACHE in order to leverage the existing caches
# (build, modules, test). The use case here for running in a container is not
# about isolation but reproducibility. Since the toolchain will write to those
# directories, we also have to pass the user's identity (UID, GID, passwd). In
# order to make messages useful, we also pass the same directory $(ROOTDIR) and
# mount it under the same path.
#
# --net=host and mounting docker.sock are required to run integration tests, which use testcontainers.
buildtools = $(docker) run --rm -i \
			--user $(LOCAL_UID):$(LOCAL_GID) \
			--net=host \
			--volume /var/run/docker.sock:/var/run/docker.sock \
			--volume /etc/passwd:/etc/passwd:ro \
			--volume '$(ROOTDIR):$(ROOTDIR):ro' \
			--volume '$(DISTDIR):$(DISTDIR):rw' \
			--volume '$(GOCACHEDIR):$(GOCACHEDIR):rw' \
			--volume '$(GOMODCACHEDIR):$(GOMODCACHEDIR):rw' \
			--volume '$(GOLANGCI_LINT_CACHEDIR):$(GOLANGCI_LINT_CACHEDIR):rw' \
			--env 'GOCACHE=$(GOCACHEDIR)' \
			--env 'GOMODCACHE=$(GOMODCACHEDIR)' \
			--workdir '$(ROOTDIR)' \
			$(buildtools_image)
endif

.PHONY: all
all:

##@ Building

.PHONY: build
all: build
build: ## Build everything.
	$(V) $(buildtools) env CGO_ENABLED=0 go build -o '$(DISTDIR)/crocochrome' ./cmd/crocochrome/

# Provide an easy way to enter the same environment for debugging purposes.
#
# This is not externally documented because it's not part of the common follow,
# but command line completion will usually pick it up.
.PHONY: shell
shell:
	$(buildtools) /bin/bash

.PHONY: build-container
build-container: ## Build docker container image.
	$(V) $(docker) build -t $(image) .

##@ Testing

.PHONY: test
test: ## Test everything.
	$(V) $(buildtools) go test -v ./...

.PHONY: test-short
test-short: ## Run short tests only.
	$(V) $(buildtools) go test -v -short ./...

.PHONY: test-integration
test-integration: ## Run integration tests.
# This is not wrapped with buildtools, pending revision.
#
# When we set things up to run as the user, the docker socket is not readable
# from the container because it's not accessible by world. Normally it's set up
# so that it belongs to a specific group, typically `docker`. We would have to
# assume that that's the case everywhere.
	$(V) go test -v -tags=integration ./integration/...

##@ Linting

.PHONY: lint
lint: ## Run all code checks.
ifeq ($(strip $(GITHUB_ACTIONS)),true)
# In GitHub action runners these directories do not exist. Trying to create
# them from within the container doesn't work since the home directory (as
# specified by /etc/passwd) does not exist.
	$(S) mkdir -p '$(GOLANGCI_LINT_CACHEDIR)' '$(GOCACHEDIR)' '$(GOMODCACHEDIR)'
endif
	$(V) $(buildtools) golangci-lint run ./...

# This is here so that .github/workflows/push-pr.yaml is able to extract the
# golangci-lint version and download the same thing from the internet. Think
# about that for a second.
.PHONY: lint-version
lint-version:
# If we *assume* that the golangci-lint-v2 program in the image is available,
# then the correct way to call this is `golangci-lint-v2 version --json` as the
# previous `golangci-lint version --format short` was removed in v2. `sh -c` is
# there so that both the program and the pipe run inside the container.
	$(V) $(buildtools) sh -c 'golangci-lint version --json | jq -r .version'

##@ Helpers

.PHONY: help
help: ## Display this help.
	$(S) awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: clean
clean: ## Clean up intermediate build artifacts.
	$(S) echo "Cleaning intermediate build artifacts..."
	$(V) rm -rf node_modules
	$(V) rm -rf public/build
	$(V) git clean -dxf '$(DISTDIR)'

.PHONY: distclean
distclean: clean ## Clean up all build artifacts.
	$(S) echo "Cleaning all build artifacts..."
	$(V) git clean -Xf
