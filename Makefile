# Copyright 2019 New Relic Corporation. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
NATIVEOS	 := $(shell go version | awk -F '[ /]' '{print $$4}')
NATIVEARCH	 := $(shell go version | awk -F '[ /]' '{print $$5}')
TOOLS_DIR    := ./bin/dev-tools
INTEGRATION  = nri-kube-events
GOLANGCILINT_VERSION = 1.33.0
DOCKER_IMAGE_NAME ?= newrelic/nri-kube-events
BUILD_TARGET ?= bin/$(INTEGRATION)

# GOOS and GOARCH will likely come from env
GOOS ?=
GOARCH ?=
CGO_ENABLED ?= 0

ifneq ($(strip $(GOOS)), )
BUILD_TARGET := $(BUILD_TARGET)-$(GOOS)
endif

ifneq ($(strip $(GOARCH)), )
BUILD_TARGET := $(BUILD_TARGET)-$(GOARCH)
endif

all: build

build: clean lint test compile

$(TOOLS_DIR):
	@mkdir -p $@

$(TOOLS_DIR)/golangci-lint: $(TOOLS_DIR)
	@echo "=== $(INTEGRATION) === [ install-linter ]:  Downloading 'golangci-lint'"
	@wget -O - -q https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | BINDIR=$(TOOLS_DIR) sh -s v$(GOLANGCILINT_VERSION) > /dev/null 2>&1

clean:
	@echo "=== $(INTEGRATION) === [ clean ]: Removing binaries and coverage file..."
	@rm -rfv bin

fmt:
	@echo "=== $(INTEGRATION) === [ fmt ]: Running Gofmt...."
	@go fmt ./...

lint: $(TOOLS_DIR)/golangci-lint
	@echo "=== $(INTEGRATION) === [ lint ]: Validating source code running golangci-lint..."
	@${TOOLS_DIR}/golangci-lint run --verbose --timeout 2m

compile:
	@echo "=== $(INTEGRATION) === [ compile ]: Building $(INTEGRATION)..."
	CGO_ENABLED=$(CGO_ENABLED) go build -o $(BUILD_TARGET) ./cmd/nri-kube-events

compile-multiarch:
	$(MAKE) compile GOOS=linux GOARCH=amd64
	$(MAKE) compile GOOS=linux GOARCH=arm
	$(MAKE) compile GOOS=linux GOARCH=arm64

test:
	@echo "=== $(INTEGRATION) === [ test ]: Running unit tests..."
	@go test -race ./...

docker:
	$(MAKE) compile GOOS=linux GOARCH=amd64
	DOCKER_BUILDKIT=1 docker build . -t $(DOCKER_IMAGE_NAME)

docker-multiarch: compile-multiarch
	@docker buildx build . -t $(DOCKER_IMAGE_NAME)

buildThirdPartyNotice:
	@go list -m -json all | go-licence-detector -rules ./assets/licence/rules.json  -noticeTemplate ./assets/licence/THIRD_PARTY_NOTICES.md.tmpl -noticeOut THIRD_PARTY_NOTICES.md -includeIndirect -overrides ./assets/licence/overrides

.PHONY: all build clean fmt lint compile test docker-build docker-test docker-lint docker-lint/dockerfile buildThirdPartyNotice
