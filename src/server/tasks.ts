import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { config } from "./config.ts";
import { db, now, type Repo, type Task, type TaskStatus, type User } from "./db.ts";
import { publish, taskEvent } from "./bus.ts";
import * as hc from "./hc.ts";
import * as dk from "./compute/docker.ts";
import { run, shq } from "./sh.ts";
import { hostClaudeToken } from "./local.ts";
import { imageBuild } from "./images.ts";
import * as proxy from "./proxy.ts";

const AGENT = "agent";
export const machineOf = (id: number) => `task-${id}`;
export const agentAddress = (id: number) => `${machineOf(id)}/${AGENT}`;
const repoDir = (fullName: string) => `/workspace/${fullName.split("/")[1]}`;
const sessionIds = new Map<number, string>();
const withKvm = (devices: string[]) => (existsSync("/dev/kvm") && !devices.includes("android") ? [...devices, "android"] : devices);

export function getTask(id: number) {
  return db.query<Task, [number]>("SELECT * FROM tasks WHERE id = ?").get(id);
}

function userOf(task: Task) {
  return db.query<User, [number]>("SELECT * FROM users WHERE id = ?").get(task.user_id)!;
}

export function getRepo(fullName: string): Repo {
  db.query("INSERT OR IGNORE INTO repos (full_name) VALUES (?)").run(fullName);
  return db.query<Repo, [string]>("SELECT * FROM repos WHERE full_name = ?").get(fullName)!;
}

function update(id: number, fields: Partial<Task>) {
  const keys = Object.keys(fields);
  if (!keys.length) return;
  db.query(`UPDATE tasks SET ${keys.map((k) => `${k} = ?`).join(", ")} WHERE id = ?`).run(
    ...keys.map((k) => (fields as Record<string, string | number | null>)[k] ?? null),
    id,
  );
  publish({ type: "task", taskId: id });
}

function setStatus(id: number, status: TaskStatus, extra: Partial<Task> = {}) {
  update(id, { status, ...extra });
}

export function createTask(input: {
  user: User;
  repo: string;
  prompt: string;
  title?: string;
  also?: string[];
  kind?: "task" | "setup";
  workflowId?: number;
  timeoutMin?: number;
}): Task {
  const title = input.title ?? firstLine(input.prompt);
  const r = db
    .query(
      "INSERT INTO tasks (user_id, kind, title, repo, also, prompt, workflow_id, timeout_min, status, created_at, proxy_mode, proxy_secret) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?) RETURNING id",
    )
    .get(
      input.user.id,
      input.kind ?? "task",
      title,
      input.repo,
      JSON.stringify(input.also ?? []),
      input.prompt,
      input.workflowId ?? null,
      input.timeoutMin ?? null,
      now(),
      config.ghProxy === "off" ? null : config.ghProxy,
      config.ghProxy === "off" ? null : proxy.newSecret(),
    ) as { id: number };
  taskEvent(r.id, "queued", input.kind === "setup" ? `Preparing ${input.repo}` : "Queued");
  queueMicrotask(schedule);
  return getTask(r.id)!;
}

function firstLine(s: string) {
  const line = s.split("\n").map((l) => l.trim()).find((l) => l && !l.startsWith("/")) ?? s.trim();
  return line.length > 90 ? `${line.slice(0, 87)}…` : line;
}

const starting = new Set<number>();

export function schedule() {
  if (imageBuild.state === "building") return;
  const busy = db.query<{ n: number }, []>("SELECT COUNT(*) AS n FROM tasks WHERE status IN ('starting', 'working')").get()!.n;
  let free = config.maxRunning - busy;
  if (free <= 0) return;
  const queued = db.query<Task, []>("SELECT * FROM tasks WHERE status = 'queued' ORDER BY id").all();
  for (const t of queued) {
    if (free-- <= 0) break;
    if (starting.has(t.id)) continue;
    starting.add(t.id);
    start(t).finally(() => starting.delete(t.id));
  }
}

function secretsOf(user: User) {
  const rows = db.query<{ name: string; value: string; path: string | null }, [number]>(
    "SELECT name, value, path FROM secrets WHERE user_id = ?",
  ).all(user.id);
  const env: Record<string, string> = {};
  const files: Record<string, string> = {};
  for (const r of rows) {
    if (r.path) files[r.path] = r.value;
    else env[r.name] = r.value;
  }
  return { env, files };
}

const AGENT_ENV = ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"];

function boxSpecFor(task: Task, user: User): dk.BoxSpec {
  const { env, files } = secretsOf(user);
  const boxEnv = {
    GH_TOKEN: user.gh_token,
    OUTPOST_TASK: String(task.id),
    OUTPOST_REPO: task.repo,
    ...Object.fromEntries(Object.entries(env).filter(([k]) => !AGENT_ENV.includes(k))),
  };
  if (task.proxy_secret) {
    Object.assign(boxEnv, proxy.proxyEnv(task));
    files[proxy.CA_PATH] = proxy.caPem();
  }
  const devices = withKvm(JSON.parse(getRepo(task.repo).devices));
  if (user.mac_target && files["/root/.ssh/outpost_mac"]) {
    const [macUser, macHost] = user.mac_target.split("@");
    files["/root/.ssh/config"] = `Host mac\n  HostName ${macHost}\n  User ${macUser}\n  IdentityFile ~/.ssh/outpost_mac\n  StrictHostKeyChecking accept-new\n  ServerAliveInterval 30\n`;
    devices.push("ios");
  }
  return { image: task.image ?? config.boxImage, env: boxEnv, files, devices };
}

const managingBox = new Set<number>();

async function withBox<T>(id: number, f: () => Promise<T>): Promise<T> {
  managingBox.add(id);
  try {
    return await f();
  } finally {
    setTimeout(() => managingBox.delete(id), 3000);
  }
}

async function start(task: Task) {
  const user = userOf(task);
  const repo = getRepo(task.repo);
  const c = dk.names(task.id, user.login);
  const { env } = secretsOf(user);
  const agentEnv: Record<string, string> = { GH_TOKEN: user.gh_token, OUTPOST_TASK: String(task.id), OUTPOST_REPO: task.repo };
  if (task.proxy_secret) Object.assign(agentEnv, proxy.proxyEnv(task));
  for (const [k, v] of Object.entries(env)) if (AGENT_ENV.includes(k)) agentEnv[k] = v;
  if (!AGENT_ENV.some((k) => agentEnv[k])) {
    const host = await hostClaudeToken(user.login);
    if (host) {
      agentEnv.CLAUDE_CODE_OAUTH_TOKEN = host.token;
      taskEvent(task.id, "phase", `Using this machine's Claude sign-in (valid until ${new Date(host.expiresAt).toLocaleTimeString()})`);
    }
  }
  const image = task.kind === "setup" ? config.boxImage : (repo.ready_image ?? config.boxImage);
  try {
    setStatus(task.id, "starting", { started_at: now(), image, phase: "Preparing the computer" });
    taskEvent(task.id, "phase", repo.ready_image && task.kind !== "setup" ? `Starting from ${task.repo}'s ready image` : "Starting a fresh computer");
    if (task.proxy_secret) {
      await proxy.ensureProxy();
      taskEvent(task.id, "phase", task.proxy_mode === "enforce"
        ? "GitHub writes are limited to this task's repos"
        : "GitHub writes outside this task's repos are logged (audit mode)");
    }
    await dk.createDesk(task.id, c, agentEnv, task.proxy_secret ? proxy.caPem() : null);

    if (image !== config.boxImage) {
      update(task.id, { phase: "Copying the ready workspace" });
      if (await dk.seedWorkspace(c, image)) taskEvent(task.id, "phase", "Workspace and dependencies restored from the ready image");
    }
    update(task.id, { phase: "Starting the box" });
    const spec = boxSpecFor({ ...task, image }, user);
    await withBox(task.id, () => dk.startBox(task.id, c, spec));
    if (image !== config.boxImage) {
      update(task.id, { phase: "Loading the ready Docker images" });
      if (await dk.loadSeedImages(c)) taskEvent(task.id, "phase", "Docker images restored from the ready image");
    }

    update(task.id, { phase: `Cloning ${task.repo}` });
    for (const name of [task.repo, ...JSON.parse(task.also) as string[]]) {
      const dir = repoDir(name);
      const d = shq(dir);
      const partial = shq(dir + ".partial");
      const r = await Bun.spawn(["docker", "exec", c.box, "bash", "-lc",
        `if [ -d ${d}/.git ]; then ` +
          `cd ${d} && git remote set-head origin -a >/dev/null && b=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's#^origin/##') && ` +
          `git fetch -q origin && git checkout -q -f -B "$b" "origin/$b" && git clean -fdq; ` +
        `else rm -rf ${partial} && gh repo clone ${shq(name)} ${partial} -- --filter=blob:none && mv ${partial} ${d}; fi`], { stdout: "pipe", stderr: "pipe" });
      if ((await r.exited) !== 0) throw new Error(`could not clone ${name}: ${(await new Response(r.stderr).text()).trim().slice(-400)}`);
    }
    taskEvent(task.id, "phase", "Repo checked out at the latest default branch");

    update(task.id, { phase: "Connecting the agent" });
    await hc.addMachine(machineOf(task.id), dk.deskVia(c));

    if (user.profile_repo) {
      update(task.id, { phase: "Applying your agent profile" });
      const r = await hc.exec(machineOf(task.id), ["outpost-profile", user.profile_repo], { timeoutSec: 600 });
      taskEvent(task.id, r.code === 0 ? "phase" : "warning", r.code === 0 ? "Agent profile applied" : `Agent profile failed: ${(r.err || r.out).trim().slice(-300)}`);
    }
    const sessionId = randomUUID();
    sessionIds.set(task.id, sessionId);
    await hc.exec(machineOf(task.id), ["outpost-seed-claude", repoDir(task.repo)]);

    update(task.id, { phase: "Starting the agent" });
    const prompt = buildPrompt(task, repo, spec.devices);
    await dk.writeRunFile(c, "prompt.md", prompt);
    await dk.writeRunFile(c, "session-id", sessionId);
    await hc.runTerminal({
      machine: machineOf(task.id),
      name: AGENT,
      cwd: repoDir(task.repo),
      labels: { "outpost.task": String(task.id) },
      argv: ["claude", "--dangerously-skip-permissions", "--session-id", sessionId, "--model", config.defaultModel, prompt],
    });
    setStatus(task.id, "working", { phase: null });
    taskEvent(task.id, "phase", "The agent is working");
    armTimeout(task.id);
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    taskEvent(task.id, "error", msg);
    setStatus(task.id, "failed", { error: msg, phase: null, finished_at: now(), released_at: now() });
    await dk.release(c).catch(() => {});
    await hc.removeMachine(machineOf(task.id));
    await withBox(task.id, () => dk.stopBox(c)).catch(() => {});
    schedule();
  }
}

function buildPrompt(task: Task, repo: Repo, devices: string[]) {
  const also: string[] = JSON.parse(task.also);
  const lines = [
    `You are working on ${task.repo}, checked out at ${repoDir(task.repo)}.`,
    also.length ? `These repos are cloned beside it, each pushable: ${also.map((a) => `${a} at ${repoDir(a)}`).join(", ")}.` : "",
    "Your shell commands run on this task's own Linux computer (Ubuntu, you are root): install whatever you need with apt or mise.",
    "Docker and Docker Compose work there." + (devices.includes("android") ? " /dev/kvm is available, so an Android emulator can run." : ""),
    devices.includes("ios")
      ? `For iOS, a Mac with Xcode is reachable as \`ssh mac\` (other tasks may share it). Work in ~/outpost/task-${task.id} there (rsync the repo over, excluding node_modules, Pods and build output), and give this task its own simulator with \`xcrun simctl clone\` of an existing device instead of creating one from scratch; delete your clone when done.`
      : "",
    "GitHub access uses the requester's identity through `gh` and git; push branches and open PRs with it.",
    task.proxy_secret
      ? "HTTP(S) goes through Outpost's proxy (HTTPS_PROXY); keep it set. GitHub writes work only on this task's repos. If GitHub answers `credential_withheld`, report it; don't work around it."
      : "",
    "Put screenshots, logs and other proof you want the requester to see in /run/outpost/artifacts/.",
    repo.notes ? `\nNotes from preparing this repo's computer:\n${repo.notes.trim()}\n` : "",
  ];
  if (task.kind === "setup") {
    lines.push(
      "",
      `Your job: prepare this computer so future tasks on ${task.repo} start ready to work.`,
      "1. Read the README, AGENTS.md or CLAUDE.md, CI workflows, Dockerfiles and compose files to learn the toolchains.",
      "2. Install the toolchains and system packages (prefer mise for language runtimes; apt for system packages), and fetch dependencies so caches are warm.",
      "3. Prove it: build, run the unit tests, and if there is a compose stack bring it up once and check it answers, then bring it down.",
      "4. Write /run/outpost/notes.md: what you installed, how to build, test and start the stack, and any test users or gotchas. Keep it short and factual.",
      "Do not commit, push or open PRs, and do not edit tracked files.",
      "Nobody is watching this run. When everything above is done, run `touch /run/outpost/done`, then reply with a one-paragraph summary.",
    );
  } else {
    lines.push(
      "",
      task.workflow_id
        ? "Nobody is watching this run and nobody will answer questions: decide sensibly and note your assumptions. If a person closed a PR or issue you opened, leave it closed: they decided. When the whole task is complete, run `touch /run/outpost/done`, then end with a short summary: what you did, links to PRs, and anything left for a human."
        : "When you finish, end with a short summary: what you did, links to PRs, and anything left for a human.",
      "",
      "Task:",
      splitCommand(task.prompt).rest,
    );
  }
  const command = task.kind === "setup" ? "" : splitCommand(task.prompt).command;
  return (command ? `${command}\n\n` : "") + lines.filter((l) => l !== "").join("\n");
}

export function splitCommand(prompt: string): { command: string; rest: string } {
  const m = prompt.match(/^\s*(\/[\w:-]+)[ \t]*(?:\n|$)([\s\S]*)$/);
  return m ? { command: m[1]!, rest: m[2]!.trim() } : { command: "", rest: prompt };
}

const timers = new Map<number, Timer>();

function armTimeout(id: number) {
  const t = getTask(id);
  if (!t?.timeout_min) return;
  const left = t.started_at! + t.timeout_min * 60_000 - now();
  clearTimeout(timers.get(id));
  timers.set(id, setTimeout(() => timeOut(id), Math.max(left, 0)));
}

async function timeOut(id: number) {
  const t = getTask(id);
  if (!t || !["working", "waiting"].includes(t.status)) return;
  taskEvent(id, "error", `Timed out after ${t.timeout_min} minutes`);
  await hc.interrupt(agentAddress(id)).catch(() => {});
  await finish(id, "failed", `Timed out after ${t.timeout_min} minutes`);
}

export async function lastAssistantText(id: number): Promise<string | null> {
  const t = getTask(id);
  if (!t) return null;
  const session = sessionIds.get(id) ?? (await dk.readRunFile(dk.names(id, userOf(t).login), "session-id"))?.trim();
  if (!session) return null;
  const r = await hc.exec(machineOf(id), ["sh", "-c",
    `f=$(ls -t /root/.claude/projects/*/${session}.jsonl 2>/dev/null | head -1); [ -n "$f" ] && jq -rs '[.[] | select(.type=="assistant") | .message.content[]? | select(.type=="text") | .text] | last // empty' "$f"`]).catch(() => null);
  const text = r?.code === 0 ? r.out.trim() : "";
  return text || null;
}

async function settledSummary(id: number): Promise<string | null> {
  let summary: string | null = null;
  for (let i = 0; i < 4 && !summary; i++) {
    if (i) await Bun.sleep(2000);
    summary = await lastAssistantText(id);
  }
  return summary;
}

async function finish(id: number, status: "done" | "failed" | "stopped", error?: string) {
  const t = getTask(id);
  if (!t || ["done", "failed", "stopped"].includes(t.status)) return;
  clearTimeout(timers.get(id));
  clearTimeout(nudges.get(id)?.timer);
  nudges.delete(id);
  const summary = await settledSummary(id);
  const c = dk.names(id, userOf(t).login);
  if (t.kind === "setup" && status === "done") {
    const saveError = await saveReady(id).then(() => null, (e: Error) => e.message);
    if (saveError) {
      const msg = `Could not save the ready image: ${saveError.slice(0, 600)}`;
      setStatus(id, "failed", { summary: summary ?? t.summary, error: msg, finished_at: now(), agent_status: null, phase: null });
      taskEvent(id, "error", `${msg} The box is kept running so you can retry saving.`);
      schedule();
      return;
    }
  }
  setStatus(id, status, { summary: summary ?? t.summary, error: error ?? null, finished_at: now(), agent_status: null, phase: null });
  taskEvent(id, status, status === "done" ? "Finished" : status === "stopped" ? "Stopped" : (error ?? "Failed"));
  await withBox(id, () => dk.stopBox(c)).catch(() => {});
  schedule();
}

export async function saveReadyAndSettle(id: number) {
  try {
    await saveReady(id);
    const t = getTask(id);
    if (t?.kind === "setup" && t.status === "failed") {
      setStatus(id, "done", { error: null });
      await withBox(id, () => dk.stopBox(dk.names(id, userOf(t).login))).catch(() => {});
    }
  } catch (e) {
    taskEvent(id, "error", `Could not save the ready image: ${e instanceof Error ? e.message.slice(0, 600) : e}`);
  }
}

export async function saveReady(id: number) {
  const t = getTask(id)!;
  const c = dk.names(id, userOf(t).login);
  const notes = await dk.readRunFile(c, "notes.md");
  const tag = `outpost/ready-${t.repo.replace("/", "--").toLowerCase()}:${new Date().toISOString().slice(0, 16).replace(/[-:T]/g, "")}`;
  taskEvent(id, "phase", "Saving this computer as the repo's ready image");
  const previous = getRepo(t.repo).ready_image;
  await dk.commitBox(c, tag);
  db.query("UPDATE repos SET ready_image = ?, ready_at = ?, notes = COALESCE(?, notes), setup_task_id = ? WHERE full_name = ?").run(
    tag, now(), notes, id, t.repo,
  );
  taskEvent(id, "ready", `Saved ${tag}: new tasks on ${t.repo} start from it`);
  if (previous && previous !== tag) void run(["docker", "rmi", previous]);
  publish({ type: "repo", repo: t.repo });
  return tag;
}

export function onAgentStatus(id: number, status: string, msg: string | null = null) {
  const t = getTask(id);
  if (!t || !["working", "waiting"].includes(t.status)) return;
  if (status === "working") {
    const n = nudges.get(id);
    if (n) {
      clearTimeout(n.timer);
      n.workingSince ??= Date.now();
    }
  }
  if (status === "working" && msg !== t.phase) update(id, { phase: msg });
  if (t.agent_status === status && !(t.status === "working" && status !== "working")) return;
  update(id, { agent_status: status, ...(status === "working" ? {} : { phase: null }) });
  if (status === "working" && t.status !== "working") {
    setStatus(id, "working");
  } else if ((status === "idle" || status === "done" || status === "error") && t.status === "working" && t.agent_status === "working") {
    if (t.workflow_id || t.kind === "setup") {
      void unattendedTurnEnded(id);
    } else {
      setStatus(id, "waiting");
      void settledSummary(id).then((s) => s && update(id, { summary: s }));
      taskEvent(id, "waiting", "The agent is waiting for you");
    }
  } else if (status === "blocked") {
    taskEvent(id, "waiting", "The agent is asking a question");
    setStatus(id, "waiting");
  }
}

const nudges = new Map<number, { count: number; timer?: Timer; workingSince?: number }>();
const NUDGE_AFTER_MS = [3 * 60_000, 10 * 60_000, 10 * 60_000];
const REAL_WORK_MS = 2 * 60_000;
const GIVE_UP_AFTER_MS = 30 * 60_000;
const NUDGE =
  "[outpost] You stopped without marking this run as done. Nobody is watching this run. If the whole task is complete, run `touch /run/outpost/done` and then write your final summary. If you are waiting for something, keep going.";

async function doneMarked(id: number) {
  const t = getTask(id);
  return !!t && (await dk.readRunFile(dk.names(id, userOf(t).login), "done")) !== null;
}

async function unattendedTurnEnded(id: number) {
  if (await doneMarked(id)) return finish(id, "done");
  const n = nudges.get(id) ?? { count: 0 };
  clearTimeout(n.timer);
  nudges.set(id, n);
  if (n.workingSince && Date.now() - n.workingSince >= REAL_WORK_MS) n.count = 0;
  n.workingSince = undefined;
  if (n.count >= NUDGE_AFTER_MS.length) {
    n.timer = setTimeout(async () => {
      const cur = getTask(id);
      if (!cur || cur.status !== "working" || cur.agent_status === "working") return;
      if (await doneMarked(id)) return finish(id, "done");
      taskEvent(id, "warning", "The agent stayed idle without marking the run done, so it was finished as is.");
      await finish(id, "done");
    }, GIVE_UP_AFTER_MS);
    return;
  }
  n.timer = setTimeout(async () => {
    const cur = getTask(id);
    if (!cur || cur.status !== "working" || cur.agent_status === "working") return;
    if (await doneMarked(id)) return finish(id, "done");
    n.count++;
    taskEvent(id, "phase", "The agent paused without marking the run done; nudged it");
    await hc.send(agentAddress(id), NUDGE).catch(() => {});
  }, NUDGE_AFTER_MS[n.count]);
}

export function onAgentExit(id: number, code: number | null) {
  const t = getTask(id);
  if (!t || ["done", "failed", "stopped"].includes(t.status)) return;
  void finish(id, code === 0 ? "done" : "failed", code === 0 ? undefined : `The agent exited with code ${code}`);
}

export async function sendMessage(id: number, text: string) {
  const t = getTask(id);
  if (!t) throw new Error("no such task");
  await run(["docker", "exec", dk.names(id, userOf(t).login).desk, "rm", "-f", "/run/outpost/done"]);
  if (["done", "failed", "stopped"].includes(t.status)) await resume(id);
  else if (!(await dk.boxState(dk.names(id, userOf(t).login)))?.running) {
    taskEvent(id, "phase", "Waking the box");
    await withBox(id, () => dk.startBox(id, dk.names(id, userOf(t).login), boxSpecFor(t, userOf(t))));
  }
  await hc.send(agentAddress(id), text);
  taskEvent(id, "message", text);
  setStatus(id, "working");
}

export async function resume(id: number) {
  const t = getTask(id)!;
  const user = userOf(t);
  const c = dk.names(id, user.login);
  if (t.released_at) throw new Error("this task's computer was already cleaned up; start a new task");
  if (t.status === "queued" || t.status === "starting") throw new Error("this task's computer isn't ready yet; try again once it has started");
  taskEvent(id, "phase", "Waking the box");
  await withBox(id, () => dk.startBox(id, c, boxSpecFor(t, user)));
  const term = await hc.getTerminal(agentAddress(id));
  if (!term || term.finished_ms) {
    const session = sessionIds.get(id) ?? (await dk.readRunFile(c, "session-id"))?.trim();
    await hc.runTerminal({
      machine: machineOf(id), name: AGENT, cwd: repoDir(t.repo), labels: { "outpost.task": String(id) },
      argv: ["claude", "--dangerously-skip-permissions", ...(session ? ["--resume", session] : []), "--model", config.defaultModel],
    });
  }
  setStatus(id, "waiting", { finished_at: null, error: null });
}

export async function stop(id: number) {
  const t = getTask(id);
  if (!t || ["done", "failed", "stopped"].includes(t.status)) return;
  if (t.status === "queued") {
    setStatus(id, "stopped", { finished_at: now(), released_at: now() });
    taskEvent(id, "stopped", "Stopped before it started");
    return;
  }
  await hc.kill(agentAddress(id)).catch(() => {});
  await finish(id, "stopped");
}

const boxBackNote = (why: string) =>
  `[outpost] Your box (the computer your commands run on) ${why} and was replaced with a fresh one. The workspace is intact, but processes you started there (servers, emulators, compose stacks) are gone. Restart what you need and carry on.`;

export async function onBoxEvent(id: number, action: string) {
  if (managingBox.has(id)) return;
  const t = getTask(id);
  if (!t || !["working", "waiting"].includes(t.status)) return;
  const user = userOf(t);
  const c = dk.names(id, user.login);
  if (action === "oom" && (await dk.boxState(c))?.running) {
    taskEvent(id, "warning", "A process in the box ran out of memory and was killed. The box is still up.");
    await hc.send(agentAddress(id), "[outpost] The kernel killed a process in your box because memory ran out (the box limit is reached). The box is still up, but what that process was doing is gone. Check what died, restart it, and keep memory under the box limit.").catch(() => {});
    return;
  }
  if (action === "oom" || action === "die") {
    const st = await dk.boxState(c);
    const why = st?.oomKilled || action === "oom" ? "ran out of memory" : "stopped unexpectedly";
    await dk.markBoxDown(c);
    taskEvent(id, "warning", `The box ${why}. The agent and workspace are safe; bringing the box back.`);
    await Bun.sleep(4000);
    if (managingBox.has(id) || (await dk.boxState(c))?.running) return;
    try {
      await withBox(id, () => dk.startBox(id, c, boxSpecFor(t, user)));
      taskEvent(id, "phase", "The box is back");
      await hc.send(agentAddress(id), boxBackNote(why)).catch(() => {});
    } catch (e) {
      taskEvent(id, "error", `Could not bring the box back: ${e instanceof Error ? e.message : e}`);
    }
  } else if (action === "start" && t.started_at && now() - t.started_at > 30_000) {
    taskEvent(id, "phase", "The box is back");
    await hc.send(agentAddress(id), boxBackNote("restarted")).catch(() => {});
  }
}

export async function sleepIdle() {
  const cutoff = now() - config.idleSleepMinutes * 60_000;
  const waiting = db.query<Task & { last: number }, [number]>(
    "SELECT t.*, (SELECT MAX(at) FROM task_events e WHERE e.task_id = t.id) AS last FROM tasks t WHERE t.status = 'waiting' AND last < ?",
  ).all(cutoff);
  for (const t of waiting) {
    const c = dk.names(t.id, userOf(t).login);
    if (!(await dk.boxState(c))?.running) continue;
    await withBox(t.id, () => dk.stopBox(c)).catch(() => {});
    taskEvent(t.id, "phase", `The box went to sleep after ${config.idleSleepMinutes} idle minutes. Send a message to wake it.`);
  }
}

export async function gc() {
  const cutoff = now() - config.keepFinishedHours * 3600_000;
  const old = db.query<Task, [number]>(
    "SELECT * FROM tasks WHERE status IN ('done','failed','stopped') AND released_at IS NULL AND finished_at < ?",
  ).all(cutoff);
  for (const t of old) {
    await dk.release(dk.names(t.id, userOf(t).login)).catch(() => {});
    await hc.removeMachine(machineOf(t.id));
    update(t.id, { released_at: now() });
  }
}

export async function recover() {
  for (const t of db.query<Task, []>("SELECT * FROM tasks WHERE status IN ('starting','working','waiting')").all()) {
    if (t.status === "starting") {
      await dk.release(dk.names(t.id, userOf(t).login)).catch(() => {});
      await hc.removeMachine(machineOf(t.id));
      setStatus(t.id, "queued", { phase: null });
      taskEvent(t.id, "phase", "Outpost restarted while this computer was starting; starting over");
      continue;
    }
    const term = await hc.getTerminal(agentAddress(t.id));
    if (!term || term.finished_ms) await finish(t.id, term?.exit_code === 0 ? "done" : "failed", term ? undefined : "The agent was lost while outpost was down");
    else {
      armTimeout(t.id);
      if ((t.workflow_id || t.kind === "setup") && t.agent_status !== "working") void unattendedTurnEnded(t.id);
    }
  }
  schedule();
}
