# How Outpost is put together

## Parts

| Part | Where | Job |
|---|---|---|
| Server | `src/server` | HTTP API, web UI, scheduler and task lifecycle, in one Bun process with a SQLite file |
| hc core | session `outpost` on the server's host | Owns every agent terminal. Each desk joins it as a temporary machine through `docker exec`. |
| Desk | `images/desk` | The agent's container: Claude Code, the user's home volume, the task's network namespace |
| Box | `images/box` | The task's computer: Ubuntu, Docker inside, `/dev/kvm`, repo toolchains |

## Why the agent lives outside the box

In moor, the agent ran inside the Box. When a build or emulator pushed the Box out of memory, the agent died with it and the session was lost.

Here the agent runs on the desk and reaches the box only through tools, the same split Rivet uses:

- **Bash.** Claude Code passes every command line to `CLAUDE_CODE_SHELL_PREFIX`. The desk's `box-shell` spots Bash tool lines by their shell snapshot and `pwd -P >|` tail. It drops the desk's snapshot and runs the rest in the box over ssh on loopback. Hook and MCP server lines run on the desk. So do lines that name the desk's Claude plugin folder, such as a skill's `!` command, because plugins live only on the desk.
- **Files.** Both containers mount the task's workspace volume at `/workspace`. Claude Code's file tools work on the desk and see the same files.
- **Temp files.** `TMPDIR` is `/run/outpost/tmp` in both containers, on a shared volume. That is how the cwd file of each Bash call comes back.
- **Network.** The box joins the desk's network namespace, so the desk owns the network. Ports the app opens in the box are reachable from the desk, and a box restart doesn't take the desk's network down.

When the box dies, the server gets a Docker event and removes `/run/outpost/box-ready`, so Bash calls fail fast with a clear message. It then starts a new box from the same image and sends the agent a note.

## Ready images

A Prepare task runs an agent in a fresh box that installs the repo's toolchains and writes notes. When the agent finishes, the server runs `docker commit` on the box and saves the result as `outpost/ready-<owner>--<name>:<time>`.

Later tasks on that repo start from the image, and the notes are added to their prompt. Workspace, Docker data and `/run/outpost` are volumes, so the commit leaves them out.

## Status

hc reports the agent's state (`working`, `idle`, `done`, `blocked`, `error`) from Claude Code's own status records. A turn ends when the state leaves `working` after having been `working`. Claude reports `idle` once at start, before its first turn, and that report is ignored.

- A scheduled or Prepare task ends at its first turn end.
- An interactive task waits for its requester.
- A task left waiting puts its box to sleep after 30 minutes. A message wakes the box.

## Host notes

On hosts that route GitHub through Tailscale, the route MTU is 1280. Containers on Docker's default 1500 MTU then hang in TLS handshakes. The server creates the `outpost` network with the smallest MTU on the host's routes and passes it to the box's inner dockerd. Image builds use the host network.
