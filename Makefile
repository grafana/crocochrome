docker ?= docker
buildtools_image ?= ghcr.io/grafana/grafana-build-tools:1.23.1

# --net=host and mounting docker.sock are required to run integration tests, which use testcontainers.
buildtools = $(docker) run --rm -i \
			-v $(shell realpath .):/src:ro \
			--net=host \
			-v /var/run/docker.sock:/var/run/docker.sock \
			-w /src \
			$(buildtools_image)

.PHONY: build
build:
	CGO_ENABLED=0 go build -v -o build/sm-k6-archiver ./cmd

image ?= test.local/sm-k6-archiver
.PHONY: build-container
build-container:
	$(docker) build -t $(image) .

.PHONY: test
test:
	$(buildtools) go test -v ./...

.PHONY: lint
lint:
	$(buildtools) golangci-lint run ./...

.PHONY: lint-version
lint-version:
	@$(buildtools) golangci-lint version --format short

.PHONY: drone
drone: .drone.yml

# Bunch of variables required by the generate drone script.
ROOTDIR       := $(abspath $(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
GO_MODULE_NAME := $(shell go list -m)
GH_REPO_NAME := $(GO_MODULE_NAME:github.com/%=%)
DRONE_SOURCE_FILES := $(call rwildcard, $(ROOTDIR)/scripts/configs/drone/*.jsonnet) $(call rwildcard, $(ROOTDIR)/scripts/configs/drone/*.libsonnet)
DRONE_SERVER ?= https://drone.grafana.net
export DRONE_SERVER

.drone.yml: $(DRONE_SOURCE_FILES)
	$(S) echo 'Regenerating $@...'
ifneq ($(origin DRONE_TOKEN),environment)
ifeq ($(origin DRONE_TOKEN),undefined)
	$(S) echo 'E: DRONE_TOKEN should set in the environment. Stop.'
else
	$(S) echo 'E: DRONE_TOKEN should *NOT* be set in a makefile, set it in the environment. Stop.'
endif
	$(S) false
endif
	$(V) ./scripts/generate-drone-yaml "$(buildtools_image)" "$(GH_REPO_NAME)" "$(ROOTDIR)/.drone.yml" "$(ROOTDIR)/scripts/configs/drone/main.jsonnet"
