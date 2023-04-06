# Copyright 2019 New Relic Corporation. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
INTEGRATION  = nri-kube-events
DOCKER_IMAGE_NAME ?= newrelic/nri-kube-events
BUILD_TARGET ?= bin/$(INTEGRATION)

DATE := $(shell date)
TAG ?= dev
COMMIT ?= $(shell git rev-parse HEAD || echo "unknown")

LDFLAGS ?= -ldflags="-X 'main.integrationVersion=$(TAG)' -X 'main.gitCommit=$(COMMIT)' -X 'main.buildDate=$(DATE)' "

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
	@go build $(LDFLAGS) -o $(BUILD_TARGET) ./cmd/nri-kube-events

test: test-unit
test-unit:
	@echo "=== $(INTEGRATION) === [ test ]: Running unit tests..."
	@go test -v -race ./...

test-integration:
	@echo "=== $(INTEGRATION) === [ test ]: Running integration tests..."
	@go test -v -tags integration  ./test/integration

docker:
	@docker buildx build --build-arg "TAG=$(TAG)" --build-arg "DATE=$(DATE)" --build-arg "COMMIT=$(COMMIT)" --load . -t "$(DOCKER_IMAGE_NAME)"

docker-multiarch:
	@docker buildx build --build-arg "TAG=$(TAG)" --build-arg "DATE=$(DATE)" --build-arg "COMMIT=$(COMMIT)" --platform linux/amd64,linux/arm64,linux/arm . -t "$(DOCKER_IMAGE_NAME)"

buildThirdPartyNotice:
	@go list -m -json all | go-licence-detector -rules ./assets/licence/rules.json  -noticeTemplate ./assets/licence/THIRD_PARTY_NOTICES.md.tmpl -noticeOut THIRD_PARTY_NOTICES.md -includeIndirect -overrides ./assets/licence/overrides

.PHONY: all build clean fmt compile test test-unit docker docker-multiarch buildThirdPartyNotice
