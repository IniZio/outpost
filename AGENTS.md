# Working on Outpost

Outpost runs coding agents on computers you can watch. Each task gets a desk container, where the agent runs, and a separate box container, where its shell commands run. Read `docs/architecture.md` for how the parts fit, and `docs/status.md` for what works, how things are configured today, the known gaps and the open questions.

## Run and check

```sh
bun install
OUTPOST_DATA=$PWD/.data bun src/server/main.ts   # http://localhost:7300
bun run typecheck
bun test
```

- `bun run images` builds the box and desk images. The server also rebuilds them on start when their sources change.
- A deployed instance usually runs as the systemd user unit in `deploy/outpost.service`, with its environment in `~/.config/outpost/env`. Restart it with `systemctl --user restart outpost`. Restarting is safe while tasks run: agents live in hc, and the server re-attaches on start.
- Settings are environment variables, listed in `src/server/config.ts`.

## Conventions

- Commit subjects start with Add, Fix, Remove, Update, Refactor or Test, are 50 characters or fewer, and have no body. A house-rules hook enforces this.
- Keep host names, IP addresses, client repo names and people's details out of committed files. They go in `CLAUDE.local.md`, which is gitignored.
- Keep custom code small. Prefer stock tools: Docker, hc, gh and Claude Code.
- Think of first-time users. Defaults should work out of the box, and every setup step belongs in the UI checklist rather than in a doc.
- Before you change task lifecycle code (`src/server/tasks.ts`), read the Status section of `docs/architecture.md`. Agent status reports have quirks that are easy to break.

## Testing a change for real

Unit tests cover little. To check a change, start a task in the web UI or with `POST /api/tasks` (`{repo, prompt, also?}`, sent with an `Origin: http://localhost:7300` header). Then:

- watch `GET /api/tasks/:id/screen` and the `task_events` table;
- open the box with `docker exec outpost-t<ID>-box …`.

A read-only rehearsal prompt that tells the agent not to push is the safe way to exercise a real repo.
