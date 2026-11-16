GO_VERSION := 1.26.11
TYPOS_VERSION := 1.51.1

export GOTOOLCHAIN := local
TOOL := go tool -modfile=tools/go.mod
TYPOS ?= typos

PG_IMAGE := postgres:17

.PHONY: help setup lint audit test test-pg ci build

help:
	@echo "targets: setup lint audit test test-pg ci build"

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

# test-pg runs the tests against a throwaway Postgres on a free port. Plain `make test` skips them
# unless OUTPOST_TEST_DATABASE_URL is set, as CI does with a service container.
test-pg:
	@set -e; \
	c=$$(docker run -d -e POSTGRES_PASSWORD=pw -p 127.0.0.1::5432 $(PG_IMAGE)); \
	trap 'docker stop $$c >/dev/null; docker container rm $$c >/dev/null' EXIT; \
	port=$$(docker port $$c 5432/tcp | head -n1 | sed 's/.*://'); \
	until docker exec $$c pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; do sleep 0.5; done; \
	OUTPOST_TEST_DATABASE_URL=postgres://postgres:pw@127.0.0.1:$$port/postgres \
		go test -race -cover -count=1 ./internal/store/...

ci: lint audit test build

build:
	go build -o bin/ ./cmd/...
