GO_VERSION := 1.26.11
TYPOS_VERSION := 1.51.1

export GOTOOLCHAIN := local
TOOL := go tool -modfile=tools/go.mod
TYPOS ?= typos

PG_IMAGE := postgres:17

.PHONY: help setup lint audit test test-pg ci build hc-golden it it-sweep tools-image

help:
	@echo "targets: setup lint audit test test-pg ci build hc-golden it tools-image"

setup:
	@go version | grep -q 'go$(GO_VERSION) ' || { echo "setup: need Go $(GO_VERSION) (mise install)"; exit 1; }
	@typos --version 2>/dev/null | grep -qx 'typos-cli $(TYPOS_VERSION)' || cargo install --locked typos-cli@$(TYPOS_VERSION)
	@go mod download && go mod download -modfile=tools/go.mod
	@git config core.hooksPath .githooks
	@echo "setup: Go $(GO_VERSION), pinned tools and the pre-push hook present"

# Lint also reads the files behind build tags, so the opt-in tests cannot rot unseen.
lint:
	$(TOOL) golangci-lint fmt --diff ./...
	$(TOOL) golangci-lint run --build-tags docker,hcgolden ./...
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
		go test -race -cover -count=1 ./internal/store/... ./cmd/...

# hc-golden records the real hc's answers into internal/hc/testdata. Run it after an hc upgrade.
hc-golden:
	go test -tags hcgolden -run TestRecordTranscripts -count=1 ./internal/hc/

it:
	@set -e; id=$$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n'); echo "make it: run $$id"; \
	trap '$(MAKE) --no-print-directory it-sweep ID='"$$id" EXIT; \
	OUTPOST_IT_ID=$$id go test -tags docker -count=1 -timeout 15m -run Docker -v ./internal/boxchain/

it-sweep:
	@test -n "$(ID)"
	@docker ps -aq --filter label=dev.outpost.box.it$(ID).task | xargs -r docker rm -f >/dev/null
	@docker volume ls -q --filter label=dev.outpost.box.it$(ID).task | xargs -r docker volume rm -f >/dev/null
	@docker image ls -q --filter label=dev.outpost.box.it$(ID).task | xargs -r docker image rm -f >/dev/null
	@docker image ls -q --filter label=dev.outpost.it=$(ID) | xargs -r docker image rm -f >/dev/null
	@docker network rm obit-$(ID) >/dev/null 2>&1 || true

TOOLS_IMAGE ?= ob-tools:dev
tools-image:
	mkdir -p bin/tools-image
	cp images/tools/* bin/tools-image/
	cp -L "$$(command -v hc)" bin/tools-image/hc
	docker build -t $(TOOLS_IMAGE) bin/tools-image

ci: lint audit test build

build:
	go build -o bin/ ./cmd/...
