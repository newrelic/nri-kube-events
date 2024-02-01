# Copyright 2019 New Relic Corporation. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
INTEGRATION  = nri-kube-events
DOCKER_IMAGE_NAME ?= newrelic/nri-kube-events
BIN_DIR = ./bin
BUILD_TARGET ?= $(BIN_DIR)/$(INTEGRATION)
TEST_COVERAGE_DIR := $(BIN_DIR)/test-coverage

DATE := $(shell date)
TAG ?= dev
COMMIT ?= $(shell git rev-parse HEAD || echo "unknown")

LDFLAGS ?= -ldflags="-X 'main.integrationVersion=$(TAG)' -X 'main.gitCommit=$(COMMIT)' -X 'main.buildDate=$(DATE)' "

all: build

build: clean test compile

clean:
	@echo "=== $(INTEGRATION) === [ clean ]: Removing binaries and coverage file..."
	@rm -rfv $(BIN_DIR)

fmt:
	@echo "=== $(INTEGRATION) === [ fmt ]: Running Gofmt...."
	@go fmt ./...

compile:
	@echo "=== $(INTEGRATION) === [ compile ]: Building $(INTEGRATION)..."
	@go build $(LDFLAGS) -o $(BUILD_TARGET) ./cmd/nri-kube-events

test: test-unit
test-unit:
	@echo "=== $(INTEGRATION) === [ test ]: Running unit tests..."
	@mkdir -p $(TEST_COVERAGE_DIR)
	@go test ./... -v -count=1 -coverprofile=$(TEST_COVERAGE_DIR)/coverage.out -covermode=count

test-integration:
	@echo "=== $(INTEGRATION) === [ test ]: Running integration tests..."
	@go test -v -tags integration  ./test/integration

docker:
	@docker buildx build --build-arg "TAG=$(TAG)" --build-arg "DATE=$(DATE)" --build-arg "COMMIT=$(COMMIT)" --load . -t "$(DOCKER_IMAGE_NAME)"

docker-multiarch:
	@docker buildx build --build-arg "TAG=$(TAG)" --build-arg "DATE=$(DATE)" --build-arg "COMMIT=$(COMMIT)" --platform linux/amd64,linux/arm64,linux/arm . -t "$(DOCKER_IMAGE_NAME)"

buildThirdPartyNotice:
	@go list -m -json all | go-licence-detector -rules ./assets/licence/rules.json  -noticeTemplate ./assets/licence/THIRD_PARTY_NOTICES.md.tmpl -noticeOut THIRD_PARTY_NOTICES.md -includeIndirect -overrides ./assets/licence/overrides

# rt-update-changelog runs the release-toolkit run.sh script by piping it into bash to update the CHANGELOG.md.
# It also passes down to the script all the flags added to the make target. To check all the accepted flags,
# see: https://github.com/newrelic/release-toolkit/blob/main/contrib/ohi-release-notes/run.sh
#  e.g. `make rt-update-changelog -- -v`
rt-update-changelog:
	curl "https://raw.githubusercontent.com/newrelic/release-toolkit/v1/contrib/ohi-release-notes/run.sh" | bash -s -- $(filter-out $@,$(MAKECMDGOALS))

.PHONY: all build clean fmt compile test test-unit docker docker-multiarch buildThirdPartyNotice rt-update-changelog
