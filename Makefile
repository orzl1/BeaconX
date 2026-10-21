BINARY      := bin/beaconx
MODULE      := github.com/beaconx/beaconx
VERSION     ?= dev
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
  -X $(MODULE)/internal/version.Version=$(VERSION) \
  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
  -X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: help build run test test-race cover lint fmt tidy brand docker-build up down clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/beaconx

run: ## Run locally (requires PostgreSQL; see docker compose)
	go run ./cmd/beaconx serve

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

cover: ## Run tests and print coverage
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint: ## Format check, vet and brand check
	@out=$$(gofmt -l $$(go list -f '{{.Dir}}' ./...)); \
	  if [ -n "$$out" ]; then echo "gofmt required:"; echo "$$out"; exit 1; fi
	go vet ./...
	./scripts/check-brand.sh

fmt: ## Format the Go sources
	gofmt -w $$(go list -f '{{.Dir}}' ./...)

tidy: ## Tidy go.mod / go.sum
	go mod tidy

brand: ## Check the tree for third-party project traces
	./scripts/check-brand.sh

docker-build: ## Build the container image
	docker build -t beaconx:dev .

up: ## Start the full stack with docker compose
	docker compose up -d --build

down: ## Stop the full stack
	docker compose down

clean: ## Remove build artefacts
	rm -rf bin dist coverage.out
