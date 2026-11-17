GO_VERSION := 1.26.11
TYPOS_VERSION := 1.51.1

export GOTOOLCHAIN := local
TOOL := go tool -modfile=tools/go.mod
TYPOS ?= typos

PG_IMAGE := postgres:17

.PHONY: help web setup lint audit chart test test-pg ci build hc-golden it it-sweep tools-image box-image proxy-image

help:
	@echo "targets: web setup lint audit chart test test-pg ci build hc-golden it tools-image box-image proxy-image"

# web builds the single-page UI into internal/api/web/app, which `go build` embeds. Without it the
# server serves the plain watch page. It needs the pinned Node (mise install).
web:
	cd web && npm ci && npm run build

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
	c=$$(docker run -d --label dev.outpost.it=$$id -e POSTGRES_PASSWORD=pw -p 127.0.0.1::5432 $(PG_IMAGE)); \
	port=$$(docker port $$c 5432/tcp | head -n1 | sed 's/.*://'); \
	timeout 60 sh -c "until docker exec $$c pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; do sleep 0.5; done"; \
	OUTPOST_IT_ID=$$id OUTPOST_TEST_DATABASE_URL=postgres://postgres:pw@127.0.0.1:$$port/postgres \
		go test -tags docker -count=1 -timeout 15m -run Docker -v ./internal/boxchain/ ./cmd/outpost/

it-sweep:
	@test -n "$(ID)"
	@for l in dev.outpost.box.it$(ID) dev.outpost.box.it$(ID)e2e; do \
		docker ps -aq --filter label=$$l.task | xargs -r docker rm -f >/dev/null; \
		docker volume ls -q --filter label=$$l.task | xargs -r docker volume rm -f >/dev/null; \
		docker image ls -q --filter label=$$l.task | xargs -r docker image rm -f >/dev/null; \
	done
	@docker ps -aq --filter label=dev.outpost.it=$(ID) | xargs -r docker rm -f -v >/dev/null
	@docker image ls -q --filter label=dev.outpost.it=$(ID) | xargs -r docker image rm -f >/dev/null
	@docker network rm obit-$(ID) obit-$(ID)-e2e >/dev/null 2>&1 || true

TOOLS_IMAGE ?= ob-tools:dev
tools-image:
	docker build -t $(TOOLS_IMAGE) images/tools

BOX_IMAGE ?= ob-box:dev
box-image:
	docker build -t $(BOX_IMAGE) images/box

# proxy-image builds the credential proxy. Its Python tests run as a build step.
PROXY_IMAGE ?= ob-proxy:dev
proxy-image:
	docker build -q -t $(PROXY_IMAGE) images/proxy

CHART := deploy/helm/outpost
CHART_VALUES := -f deploy/helm/test-values.yaml
CHART_KEYS := host baseImage secrets.databaseUrl secrets.secretKey secrets.proxyToken proxy.caPem

chart:
	@command -v helm >/dev/null || { echo "chart: helm is not installed (mise install)"; exit 1; }
	helm lint --strict $(CHART) $(CHART_VALUES)
	helm lint --strict $(CHART) $(CHART_VALUES) -f deploy/helm/full-values.yaml
	helm template outpost $(CHART) $(CHART_VALUES) | grep -q 'maxUnavailable: 0'
	helm template outpost $(CHART) $(CHART_VALUES) | grep -q 'args: \[migrate\]'
	! helm template outpost $(CHART) $(CHART_VALUES) | grep -q secretName
	helm template outpost $(CHART) $(CHART_VALUES) -f deploy/helm/full-values.yaml | grep -q 'proxy-buffering'
	helm template outpost $(CHART) $(CHART_VALUES) --set secrets.existingSecret=mine | grep -q 'name: mine'
	@for k in $(CHART_KEYS); do \
		out=$$(helm template outpost $(CHART) $(CHART_VALUES) --set $$k= 2>&1) && { echo "chart: empty $$k rendered"; exit 1; }; \
		echo "$$out" | grep -q "$$k is required" || { echo "chart: empty $$k does not name the key"; exit 1; }; \
	done

ci: lint audit test chart build

build:
	go build -o bin/ ./cmd/...
