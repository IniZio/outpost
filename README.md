# Outpost

Hand coding tasks to agents. Each task gets its own computer with Docker and Android emulators. You watch the agent work live in the browser, step in at any time, and get a pull request back.

Outpost needs no config file in your repo. What a repo needs lives in Outpost.

## Quick start

On a Linux machine with Docker and the GitHub CLI signed in:

```sh
git clone <this repo> outpost && cd outpost
bun install
bun start
```

Open http://localhost:7300.

- On this machine you're signed in already, as the account your `gh` uses.
- The first start builds the two computer images in the background. That takes a few minutes, and the home page shows the progress.
- Agents use Claude. If you use Claude Code on this machine, its sign-in is reused while it stays valid. For unattended schedules, run `claude setup-token` and paste the token on the home page.

Then type a task, or paste an issue link, pick a repo and press **Start task**.

## How a task runs

Each task gets two containers that share a workspace:

| | Desk | Box |
|---|---|---|
| Runs | The agent (Claude Code), your profile, plugins and skills | Builds, tests, Docker, emulators, the app's stack |
| Size | Small and steady (3 GB cap) | Big (16 GB cap), on `/dev/kvm` when the host has it |
| On failure | Keeps running | Restarts from its image; the agent is told and carries on |

- The agent's Bash commands run in the box over ssh. Claude Code's `CLAUDE_CODE_SHELL_PREFIX` points at `box-shell`, which sends them there.
- Its file tools work on `/workspace`, which both containers mount.
- Hooks and MCP servers stay on the desk.
- The box joins the desk's network, so ports the app opens in the box are reachable from the desk.

A box running out of memory therefore never kills the agent or loses its session. In moor, both lived in the same VM, so they died together.

The agent runs in a terminal managed by herdr-core (`hc`). The web view attaches to that terminal, so what you see is the agent's real screen. You can type into it, or use the message box. **Computer shell** opens a root shell in the box.

## Ready images

A repo's first task starts from a plain Ubuntu box and installs what it needs. To skip that next time, there are two ways:

- **Prepare** a repo. An agent reads the repo, installs its toolchains, proves the build and tests work, and writes notes. Outpost then saves the box as the repo's ready image.
- **Save as ready image** from any running task.

New tasks start from the ready image as a copy-on-write Docker layer. Heavy SDKs such as Android are stored once per machine, never downloaded per task. The idea comes from [geocodio/yak](https://github.com/geocodio/yak).

## Schedules

A schedule runs a task on each of its repos at a cron time, for example a daily dependency update every weekday at 09:00. Each run gets a fresh computer, ends on its own when the agent finishes, and frees its box. The workspace and agent stay around for a day so you can read what happened or send a follow-up.

**Import from moor** takes the JSON from moor's `list_workflows`. Imported schedules arrive paused, and moor-only steps (`.moor.yaml` setup, `.moor/artifacts`) are rewritten.

## Your settings

- **Agent profile.** Name a GitHub repo that holds your agent setup: an AGENTS file, skills, Claude settings and a plugin list. A moor profile repo works as is. It is applied once to your agent's home, which persists across your tasks.
- **Secrets.** Each one is an environment variable for your tasks' boxes, or a file, such as an ssh key for the Mac you build iOS on.

## Configuration

Set these as environment variables on the server.

| Variable | Default | Meaning |
|---|---|---|
| `OUTPOST_PORT` | `7300` | HTTP port |
| `OUTPOST_DATA` | `~/.local/share/outpost` | SQLite database |
| `OUTPOST_MAX_RUNNING` | `4` | Tasks running at once; more wait in a queue |
| `OUTPOST_BOX_MEMORY` / `OUTPOST_DESK_MEMORY` | `16g` / `3g` | Container memory caps |
| `OUTPOST_MODEL` | `opus` | Claude model for agents |
| `OUTPOST_KEEP_HOURS` | `24` | How long a finished task's computer is kept |
| `OUTPOST_ALLOWED_ORGS` | none | Only members of these GitHub orgs may sign in |
| `OUTPOST_LOCAL_USER` | `1` | Sign in loopback visitors as this machine's `gh` user |

Visitors from other machines sign in by pasting a GitHub token (`gh auth token`).
