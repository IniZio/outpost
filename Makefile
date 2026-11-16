GO_VERSION := 1.26.8
TYPOS_VERSION := 1.51.1

export GOTOOLCHAIN := local
TOOL := go tool -modfile=tools/go.mod
TYPOS ?= typos

.PHONY: help setup lint audit test ci build

help:
	@echo "targets: setup lint audit test ci build"

setup:
	@go version | grep -q 'go$(GO_VERSION) ' || { echo "setup: need Go $(GO_VERSION) (mise install)"; exit 1; }
	@typos --version 2>/dev/null | grep -qx 'typos-cli $(TYPOS_VERSION)' || cargo install --locked typos-cli@$(TYPOS_VERSION)
	@go mod download && go mod download -modfile=tools/go.mod
	@git config core.hooksPath .githooks
	@echo "setup: Go $(GO_VERSION), pinned tools and the pre-push hook present"

lint:
	$(TOOL) golangci-lint fmt --diff ./...
	$(TOOL) golangci-lint run ./...
	$(TYPOS)

audit:
	$(TOOL) govulncheck ./...
	go mod tidy -diff
	cd tools && go mod tidy -diff

test:
	go test -race -cover ./...

ci: lint audit test build

build:
	go build -o bin/ ./cmd/...
