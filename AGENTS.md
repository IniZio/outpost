# Working on Outpost (Go)

Outpost runs coding agents on computers you can watch. This branch is the Go rewrite. The Bun prototype on `main` keeps serving until this one passes a real run.

## Run and check

```sh
mise install        # Go 1.26.11, from mise.toml
make setup          # pinned tools and the pre-push hook
make ci             # lint, audit, test, build
```

- `make lint` checks formatting (gofumpt, goimports), runs golangci-lint, and runs typos.
- `make audit` runs govulncheck and fails on an untidy `go.mod`.
- `make proxy-image` builds the credential proxy in `images/proxy` (mitmproxy and an addon). Its Python tests run during the build.
- `make test` runs `go test -race -cover`. The Postgres tests skip unless `OUTPOST_TEST_DATABASE_URL` is set. `make test-pg` starts a throwaway `postgres:17` container on a free port and runs them, and CI does the same with a service container. Each test makes its own database.
- Lint tools are pinned in `tools/go.mod` and run with `go tool -modfile=tools/go.mod`.
- `GOTOOLCHAIN=local` is set by `mise.toml` and the Makefile, so the pinned Go is the one that runs.

## Layers

| Package | Job | May import (inside Outpost) |
| --- | --- | --- |
| `cmd/outpost` | The server: `serve`, `migrate`, `images` | everything server-side |
| `cmd/outpost-agent` | The machine agent: `pair`, `run` | `domain`, `link`, `machine`, `ops`, `sandbox/*` |
| `cmd/outpost-cli` | The laptop client | `api/client` |
| `internal/domain` | Types, the task state machine, the `Clock` seam. No I/O. | standard library only |
| `internal/ports` | The interfaces: Boxes (a task's box and the agent terminal in it), Agent, Secrets, Store, Enrollments, and Sandbox (the box provider) | `domain` |
| `internal/ports/portstest` | In-memory fakes of the ports, for tests of the core and of adapters | `domain`, `ports` |
| `internal/lifecycle` | Runs a task from start to clean-up | `domain`, `ports` |
| `internal/link` | The machine link: transport, handshake, relink. Knows no operation. | `domain` |
| `internal/ops` | The catalogue of operations that cross the link: names, kinds and typed requests and replies. No behaviour. | `domain`, `link` |
| `internal/machine` | The agent's fixed operations, over the link: box jobs, converge steps, the watch feed | `domain`, `ports`, `link`, `ops`, `hc` |
| `internal/hc` | herdr-core's JSON-RPC client, and `hctest`, a fake core | standard library only |
| `internal/remote` | `ports.Boxes` over the link, on the server | `domain`, `ports`, `link`, `ops` |
| `internal/boxchain` | Tests only: the box adapters joined end to end, and the Docker integration test | the box packages |
| `internal/toolmachine` | Machine tools for tasks on a tool machine such as a Mac | `domain`, `ports`, `link`, `ops` |
| `internal/store/*` | Stores. They implement `link.KeyStore` and `link.Presence` structurally, because those interfaces name only domain types. | `domain`, `ports` |
| `internal/sandbox/*`, `agent/*`, `secrets`, `auth`, `github`, `schedule` | Adapters | `domain`, `ports` |
| `internal/egress` | Proxy policy and box egress | `domain`, `ports`, `secrets` |
| `internal/api` | HTTP API, `/mcp`, websockets, web UI | `domain`, `lifecycle`, `schedule`, `auth`, `toolmachine` |

depguard enforces this table. Other rules:

- Read the time only through `domain.Clock`. `time.Now` fails lint everywhere else.
- Only adapters (`sandbox`, `agent`, `machine`) run commands with `os/exec`.
- Adapters never import each other. A new provider or agent is a new package and one line in `cmd`.
- Functions stay under cognitive complexity 10, 60 lines and 6 arguments. Errors are checked and wrapped.

## Conventions

- Commit subjects start with Add, Fix, Remove, Update, Refactor or Test, are 50 characters or fewer, and have no body.
- Keep host names, IP addresses, client repo names, people's details and links to private docs out of committed files.
- Keep custom code small. Prefer stock tools: Docker, hc, gh and Claude Code.
