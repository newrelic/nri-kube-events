# Copyright 2019 New Relic Corporation. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
NATIVEOS	 := $(shell go version | awk -F '[ /]' '{print $$4}')
NATIVEARCH	 := $(shell go version | awk -F '[ /]' '{print $$5}')
TOOLS_DIR    := ./bin/dev-tools
INTEGRATION  = nri-kube-events
GOFLAGS       = -mod=readonly
DOCKER_IMAGE_NAME ?= newrelic/nri-kube-events
BUILD_TARGET ?= bin/$(INTEGRATION)

# GOOS and GOARCH will likely come from env
GOOS ?=
GOARCH ?=
CGO_ENABLED ?= 0

BUILD_DATE := $(shell date)
TAG ?= dev
COMMIT ?= $(shell git rev-parse HEAD || echo "unknown")

LDFLAGS ?= -ldflags="-X 'main.integrationVersion=$(TAG)' -X 'main.gitCommit=$(COMMIT)' -X 'main.buildDate=$(BUILD_DATE)' "


ifneq ($(strip $(GOOS)), )
BUILD_TARGET := $(BUILD_TARGET)-$(GOOS)
endif

ifneq ($(strip $(GOARCH)), )
BUILD_TARGET := $(BUILD_TARGET)-$(GOARCH)
endif

all: build

build: clean test compile

clean:
	@echo "=== $(INTEGRATION) === [ clean ]: Removing binaries and coverage file..."
	@rm -rfv bin

fmt:
	@echo "=== $(INTEGRATION) === [ fmt ]: Running Gofmt...."
	@go fmt ./...

compile:
	@echo "=== $(INTEGRATION) === [ compile ]: Building $(INTEGRATION)..."
	CGO_ENABLED=$(CGO_ENABLED) go build $(LDFLAGS) -o $(BUILD_TARGET) ./cmd/nri-kube-events

compile-multiarch:
	$(MAKE) compile GOOS=linux GOARCH=amd64
	$(MAKE) compile GOOS=linux GOARCH=arm
	$(MAKE) compile GOOS=linux GOARCH=arm64

test: test-unit
test-unit:
	@echo "=== $(INTEGRATION) === [ test ]: Running unit tests..."
	@go test -v -race ./...

test-integration:
	@echo "=== $(INTEGRATION) === [ test ]: Running integration tests..."
	@go test -v -tags integration  ./test/integration

docker:
	$(MAKE) compile GOOS=linux GOARCH=amd64
	DOCKER_BUILDKIT=1 docker build . -t $(DOCKER_IMAGE_NAME)

docker-multiarch: compile-multiarch
	@docker buildx build . -t $(DOCKER_IMAGE_NAME)

buildThirdPartyNotice:
	@go list -m -json all | go-licence-detector -rules ./assets/licence/rules.json  -noticeTemplate ./assets/licence/THIRD_PARTY_NOTICES.md.tmpl -noticeOut THIRD_PARTY_NOTICES.md -includeIndirect -overrides ./assets/licence/overrides

.PHONY: all build clean fmt compile compile-multiarch test test-unit docker docker-multiarch buildThirdPartyNotice
