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
| GitHub access | The user's own `gh` token, from sign-in | Passed as `GH_TOKEN` to the desk and the box. `gh auth setup-git` runs in both. Commits use the user's GitHub noreply address. |
| Mac for iOS | `users.mac_target` plus a per-user key (secret `OUTPOST_MAC_KEY`) | Writes `Host mac` into the box's ssh config. The user adds the public key to the Mac once; Settings → Mac has a Test button. |
| Devices | `repos.devices`, plus automatic KVM detection | `android` is added when `/dev/kvm` exists. `ios` is added when a Mac is set up. |
| Resource limits | Environment variables `OUTPOST_BOX_MEMORY` (16g) and `OUTPOST_DESK_MEMORY` (3g); `OUTPOST_MAX_RUNNING` | No CPU or disk limits. |
| Schedules | `workflows` table (Schedules page, or imported from moor) | Fields: name, cron, time zone, repos, also, prompt, `timeout_min`, enabled. A schedule makes one task per repo and run. |
| Network | The `outpost` Docker network, with the host's route MTU | **No egress rules at all.** See the gaps below. |

## Known gaps

Most important first:

1. **No egress control, and no credential scoping.** The box can reach the whole internet. It holds the user's full `gh` token. So an agent can read or push to any repo the user can, not only the task's repos. moor had a proxy that gave out credentials only for the listed repos. It refused the rest with `credential_withheld`. Outpost has nothing like that yet. Options:
   - A per-task egress proxy with allow and deny lists, such as Squid or mitmproxy in the desk's network namespace.
   - A short-lived, fine-grained token per task.
   - Both.
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
