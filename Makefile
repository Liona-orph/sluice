# Sluice.
#
# Everything here runs offline. `make ci` is what CI runs, so a green ci target
# locally means a green pull request.

SHELL           := /bin/bash
GO              ?= go
BINARY          := sluice
PKG             := github.com/Liona-orph/sluice
# Pinned. A linter that changes under you turns an unrelated pull request red and
# teaches everyone to ignore the linter.
GOLANGCI_VERSION := v1.62.2
GOLANGCI        := $(CURDIR)/bin/golangci-lint

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE ?= $(shell date -u +%Y-%m-%d)
LDFLAGS   := -s -w \
             -X main.version=$(VERSION) \
             -X main.commit=$(COMMIT) \
             -X main.buildDate=$(BUILDDATE)

DOCKER_IMAGE ?= sluice
DOCKER_TAG   ?= $(VERSION)

.DEFAULT_GOAL := help

## help: list the targets
help:
	@echo "Sluice $(VERSION)"
	@echo
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | awk -F': ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## build: compile the binary with version information
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/sluice

## test: run the test suite
test:
	$(GO) test ./... -count=1

## test-race: run the test suite under the race detector
test-race:
	CGO_ENABLED=1 $(GO) test ./... -race -count=1

## lint: run golangci-lint, installing the pinned version if needed
lint: $(GOLANGCI)
	$(GOLANGCI) run ./...

$(GOLANGCI):
	@mkdir -p $(dir $(GOLANGCI))
	GOBIN="$(CURDIR)/bin" $(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_VERSION)

## test-tooling: prove linting cannot reuse a stale global binary
test-tooling:
	./scripts/test_makefile_tooling.sh

## fmt: format every Go file, and fail if anything changed
fmt:
	@gofmt -w .
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi

## vet: run go vet
vet:
	$(GO) vet ./...

## cover: produce coverage.out and coverage.html, and print the total
cover:
	$(GO) test ./... -count=1 -coverprofile=coverage.out -covermode=atomic
	$(GO) tool cover -html=coverage.out -o coverage.html
	@$(GO) tool cover -func=coverage.out | tail -1

## bench: run the benchmarks quoted in the README
bench:
	$(GO) test ./... -run '^$$' -bench . -benchmem -benchtime 300x

## fuzz: fuzz the redactor for 60s, which is where a silent leak would live
fuzz:
	$(GO) test ./internal/redact -run '^$$' -fuzz FuzzRedact -fuzztime 60s

## run: start the gateway with the built-in offline defaults
run:
	$(GO) run ./cmd/sluice serve

## docker: build the distroless image
docker:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILDDATE=$(BUILDDATE) \
	  -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

## ci: everything CI runs, in the order it runs it
ci: test-tooling fmt vet lint test-race build
	@echo "green"

## clean: remove build and coverage artefacts
clean:
	rm -f $(BINARY) coverage.out coverage.html

.PHONY: help build test test-race lint test-tooling fmt vet cover bench fuzz run docker ci clean
