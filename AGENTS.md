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
- `make test` runs `go test -race -cover`.
- Lint tools are pinned in `tools/go.mod` and run with `go tool -modfile=tools/go.mod`.
- `GOTOOLCHAIN=local` is set by `mise.toml` and the Makefile, so the pinned Go is the one that runs.

## Layers

| Package | Job | May import (inside Outpost) |
| --- | --- | --- |
| `cmd/outpost` | The server: `serve`, `migrate`, `images` | everything server-side |
| `cmd/outpost-agent` | The machine agent: `pair`, `run` | `link`, `machine`, `sandbox/*` |
| `cmd/outpost-cli` | The laptop client | `api/client` |
| `internal/domain` | Types, the task state machine, the `Clock` seam. No I/O. | standard library only |
| `internal/ports` | The interfaces: Sandbox, Agent, Terminal, Store, Secrets, GitHub | `domain` |
| `internal/lifecycle` | Runs a task from start to clean-up | `domain`, `ports` |
| `internal/link` | The machine link | `domain` |
| `internal/machine` | The agent's fixed operations, over the link | `domain`, `ports`, `link` |
| `internal/toolmachine` | Machine tools for tasks on a tool machine such as a Mac | `domain`, `ports`, `link` |
| `internal/sandbox/*`, `agent/*`, `terminal/*`, `store/*`, `secrets`, `auth`, `github`, `schedule` | Adapters | `domain`, `ports` |
| `internal/egress` | Proxy policy and box egress | `domain`, `ports`, `secrets` |
| `internal/api` | HTTP API, `/mcp`, websockets, web UI | `domain`, `lifecycle`, `schedule`, `auth`, `toolmachine` |

depguard enforces this table. Other rules:

- Read the time only through `domain.Clock`. `time.Now` fails lint everywhere else.
- Only adapters (`sandbox`, `agent`, `terminal`, `machine`) run commands with `os/exec`.
- Adapters never import each other. A new provider or agent is a new package and one line in `cmd`.
- Functions stay under cognitive complexity 10, 60 lines and 6 arguments. Errors are checked and wrapped.

## Conventions

- Commit subjects start with Add, Fix, Remove, Update, Refactor or Test, are 50 characters or fewer, and have no body.
- Keep host names, IP addresses, client repo names, people's details and links to private docs out of committed files.
- Keep custom code small. Prefer stock tools: Docker, hc, gh and Claude Code.
