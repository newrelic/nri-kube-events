# Copyright 2019 New Relic Corporation. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
NATIVEOS	 := $(shell go version | awk -F '[ /]' '{print $$4}')
NATIVEARCH	 := $(shell go version | awk -F '[ /]' '{print $$5}')
TOOLS_DIR    := ./bin/dev-tools
INTEGRATION  = nr-kube-events
GOLANGCILINT_VERSION = 1.17.1
IMAGE_NAME = newrelic/nr-kube-events
BUILD_TARGET ?= bin/$(INTEGRATION)

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
	@${TOOLS_DIR}/golangci-lint run

compile:
	@echo "=== $(INTEGRATION) === [ compile ]: Building $(INTEGRATION)..."
	@go build -o $(BUILD_TARGET) ./cmd/nr-kube-events

test:
	@echo "=== $(INTEGRATION) === [ test ]: Running unit tests..."
	@go test -race ./...

docker-test:
	@docker build . --target base-env -t $(IMAGE_NAME)_test
	@echo "=== $(INTEGRATION) === [ docker-test ]: Running unit tests in Docker..."
	@docker run -t $(IMAGE_NAME)_test make test

docker-lint:
	@docker build . --target base-env -t $(IMAGE_NAME)_lint
	@echo "=== $(INTEGRATION) === [ docker-lint ]: Validating source code running golangci-lint in Docker..."
	@docker run -t $(IMAGE_NAME)_lint make lint

docker-build:
	@echo "=== $(INTEGRATION) === [ docker-build ]: Building final Docker image..."
	@docker build . --target final -t $(IMAGE_NAME)

docker-lint/dockerfile:
	@echo "=== $(INTEGRATION) === [ docker-lint ]: Linting Docker image..."
	@docker run --rm -i hadolint/hadolint < Dockerfile

.PHONY: all build clean fmt lint compile test docker-build docker-test docker-lint docker-lint/dockerfile
