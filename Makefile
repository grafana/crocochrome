ROOTDIR        := $(abspath $(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
GO_MODULE_NAME := $(shell go list -m)
GH_REPO_NAME   := $(GO_MODULE_NAME:github.com/%=%)
DISTDIR        := $(ROOTDIR)/dist

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
buildtools_image ?= ghcr.io/grafana/grafana-build-tools:v1.35.1
image ?= test.local/crocochrome

ifeq ($(strip $(LOCAL)),true)
buildtools :=
else
# --net=host and mounting docker.sock are required to run integration tests, which use testcontainers.
buildtools = $(docker) run --rm -i \
			-v $(ROOTDIR):/src:ro \
			--net=host \
			-v /var/run/docker.sock:/var/run/docker.sock \
			-w /src \
			$(buildtools_image)
endif

.PHONY: all
all:

##@ Building

.PHONY: build
all: build
build: ## Build everything.
# This is intentionally not wrapped with buildtools: Creating a docker
# container for every single build is slow. Other ecosystems are OK with that,
# but the bar is much higher with Go. Some room for optimization exists, but
# that work needs to be done.
#
# The obvious downside is that `go` is whatever is installed locally, which
# might cause issues.
	CGO_ENABLED=0 go build -o $(DISTDIR)/crocochrome ./cmd/crocochrome/

.PHONY: build-container
build-container: ## Build docker container image.
	$(docker) build -t $(image) .

##@ Testing

.PHONY: test
test: ## Test everything.
	$(buildtools) go test -v ./...

.PHONY: test-integration
test-integration: ## Run integration tests.
	go test -v ./integration/...

##@ Linting

.PHONY: lint
lint: ## Run all code checks.
	$(buildtools) golangci-lint run ./...

.PHONY: lint-version
lint-version:
	@$(buildtools) golangci-lint version --format short

##@ Helpers

.PHONY: help
help: ## Display this help.
	$(S) awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: clean
clean: ## Clean up intermediate build artifacts.
	$(S) echo "Cleaning intermediate build artifacts..."
	$(V) rm -rf node_modules
	$(V) rm -rf public/build
	$(V) rm -rf "$(DISTDIR)/build"
	$(V) rm -rf "$(DISTDIR)/publish"

.PHONY: distclean
distclean: clean ## Clean up all build artifacts.
	$(S) echo "Cleaning all build artifacts..."
	$(V) git clean -Xf
