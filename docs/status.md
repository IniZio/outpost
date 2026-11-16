# Status

What Outpost does today, how each kind of configuration is declared, what is missing, and what is still undecided. Update this page whenever one of those changes.

## What works

These have been checked on a real host against real client repos:

- **Tasks and steering.** A task starts from a description or an issue link. The web view shows the agent's live terminal. You can type into it, send messages and open a shell on the box. The timeline, artifacts and summary sit beside it.
- **Agent outside the box.** If the box is killed, the agent survives. Outpost starts a new box and tells the agent.
- **Ready images.** A Prepare task makes a repo's ready image. A task that starts from one reaches a working agent in about 30 seconds.
- **Schedules.** Cron schedules (croner, with a time zone) work with Run now. Unattended runs finish when the agent marks them done.
- **Mobile.** Android emulators run on KVM inside the box. iOS runs on a Mac reached over ssh.
- **Multi-repo tasks.** Extra repos (`also`) are cloned next to the main repo, each on its default branch.
- **Profiles.** A personal agent profile repo supplies AGENTS.md, skills, settings and plugins.
- **First real run.** A daily dependency run went end to end. It covered the Dependabot review, the audit, the PR, CI, and live checks on Docker, Android and iOS.

## How things are declared today

Nothing is declared in the client repo. That was deliberate. moor's `.moor.yaml` was the main complaint about moor.

| Concern | Where it lives | Notes |
|---|---|---|
| Toolchains and repo setup | The repo's **ready image** plus **notes**, both made by a Prepare task | The Prepare agent installs whatever the repo needs in a fresh box. Outpost saves the box with `docker commit` as `outpost/ready-<owner>--<name>:<time>`, and stores the agent's notes in the `repos` table. Later tasks start from the image, and the notes go into their prompt. |
| Docker images a repo uses | Inside the ready image (`/opt/outpost/seed/images.tar`) | The box's Docker data is a volume, so the images are saved with `docker save` and loaded again on start. |
| Workspace state such as `node_modules`, `.venv` and `.env` | Inside the ready image (`/opt/outpost/seed/workspace`) | Copied into the task's workspace volume, then `git fetch`, then `checkout -f -B <default>`, then `clean -fd`. |
| The agent's own setup (AGENTS.md, skills, plugins, model) | The user's **profile repo** (Settings → Profile), applied by `images/desk/outpost-profile` | Reads `outpost-profile.yaml` or `moor-profile.yaml` (`claude.model`, `claude.env`, `claude.marketplaces`, `claude.plugins`), plus `AGENTS.md`, `skills/`, `claude/settings.json` and `setup.sh`. It is applied once per profile commit, per user home volume. |
| Secrets | `secrets` table, per user (Settings → Secrets) | A secret without a path becomes an env var in the box. A secret with a path becomes a file. Claude credentials go to the desk only. |
| GitHub access | The user's `gh` token, held by the proxy | Tasks get a placeholder `GH_TOKEN` and their own proxy login. The proxy swaps in the real token. It allows reads on any repo and writes only on the task's repos. The rules are in the proxy image's policy.py. `OUTPOST_GH_PROXY` is `enforce` (default), `audit` or `off`. Audit keeps the real token in the task and logs what enforce would refuse. Each task keeps the mode it was created with. Commits use the user's GitHub noreply address. |
| Mac for iOS | `users.mac_target` plus a per-user key (secret `OUTPOST_MAC_KEY`) | Writes `Host mac` into the box's ssh config. The user adds the public key to the Mac once; Settings → Mac has a Test button. |
| Devices | `repos.devices`, plus automatic KVM detection | `android` is added when `/dev/kvm` exists. `ios` is added when a Mac is set up. |
| Resource limits | Environment variables `OUTPOST_BOX_MEMORY` (16g) and `OUTPOST_DESK_MEMORY` (3g); `OUTPOST_MAX_RUNNING` | No CPU or disk limits. |
| Schedules | `workflows` table (Schedules page, or imported from moor) | Fields: name, cron, time zone, repos, also, prompt, `timeout_min`, enabled. A schedule makes one task per repo and run. |
| Network | The `outpost` Docker network, with the host's route MTU | Proxy-aware tools go through the proxy. It refuses private addresses. It logs each task's hosts in `task_hosts`. Nothing else is blocked, and tools that ignore `HTTPS_PROXY` go direct. See the gaps below. |

## Known gaps

Most important first:

1. **Egress is open, and reads aren't scoped.** The token no longer enters the task (see GitHub access), and writes are limited to the task's repos. Still open:
   - The agent can read any repo the user can, and the internet is open, so it could copy private code out.
   - The next step is to put the desk on an `--internal` network, so the proxy is the only way out. Then switch to an allow list built from `task_hosts`.
   - Before that, three things must learn to use the proxy. Java and Gradle need `JAVA_TOOL_OPTIONS`. Nested Docker builds need proxy settings in the Docker client config, on port 8081, which doesn't open TLS. Ssh to the Mac needs a `ProxyCommand` through CONNECT.
   - Ready images made before the proxy hold the real token in a committed `/etc/profile.d/00-outpost.sh` layer. New ones don't. Re-prepare or flatten the old ones.
   - Short-lived GitHub App tokens per task would also narrow reads. That needs an App installed on the org.
2. **Setup can't be reproduced.** A ready image is an opaque snapshot. If it is lost, or the base image changes, the only fix is to run Prepare again. Then you hope the agent does the same thing. The notes describe the setup but can't be replayed. One idea is to have Prepare also write a replayable setup script. It would live in Outpost, not in the repo, and Outpost could rebuild the image from it.
3. **Ready images go stale.** Nothing refreshes them, or warns when the base box image or the repo's toolchain files change. Today, buildx was patched into every ready image by hand after it was added to the base image.
4. **No CPU or disk limits, and no disk housekeeping.** Ready images are 6 to 38 GB each. Finished tasks keep their containers and volumes for `OUTPOST_KEEP_HOURS` (24).
5. **Helper processes outlive restarts.** The `hc events` and `docker events` followers survive `systemctl restart` (`KillMode=process`) until their next write.
6. **One host only.** Every task runs on the Docker host where the server runs. The agreed direction is an orchestrator on the team cluster, with personal machines attached as compute. See the open questions.
7. **The Mac is shared.** Tasks share one Mac account and only isolate themselves by directory and simulator clone.

## Open questions

- **Name.** "Outpost" is a working name. Decide before open-sourcing.
- **Hosting.** The orchestrator goes on the team cluster, with personal machines (Docker plus KVM hosts) attached as compute. Still to decide:
  - How a machine attaches. Options are remote Docker over ssh or tailscale, or a small agent on the machine.
  - How hc reaches desks on remote machines.
  - Whether cluster-native pods (privileged with `hostUsers: false`) are a second compute provider for repos without mobile.
- **Egress and credentials.** Proxy, per-task tokens, or both (gap 1).
- **Replayable setup** (gap 2), and when to refresh ready images automatically (gap 3).
- **Where per-repo knowledge lives.** Today it is the notes in the database. Should it also be editable in the UI, or exportable?

## Next steps

1. Watch the first unattended week of the daily dependency schedules and fix what breaks.
2. Egress proxy and per-task credential scoping.
3. Attach a second machine for remote compute. This is the first step toward the cluster orchestrator.
4. Show a stale badge on old ready images, with a one-click re-prepare.
