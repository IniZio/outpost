import { config } from "./config.ts";
import { db, type Repo, type Task, type TaskStatus, type User, type Workflow } from "./db.ts";
import * as auth from "./auth.ts";
import * as gh from "./github.ts";
import * as hc from "./hc.ts";
import * as dk from "./compute/docker.ts";
import * as tasks from "./tasks.ts";
import * as wf from "./workflows.ts";
import { run } from "./sh.ts";
import { ensureImages, imageBuild } from "./images.ts";
import { hostClaudeToken } from "./local.ts";

type Ctx = { req: Request; url: URL; user: User; params: string[] };
type Handler = (ctx: Ctx) => Promise<Response | object> | Response | object;

const routes: { method: string; pattern: RegExp; handler: Handler }[] = [];
const route = (method: string, path: string, handler: Handler) => {
  routes.push({ method, pattern: new RegExp(`^${path.replace(/:[a-z]+/g, "([^/]+)")}$`), handler });
};

export class HttpError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

const json = async <T>(req: Request): Promise<T> => {
  try {
    return (await req.json()) as T;
  } catch {
    throw new HttpError(400, "expected a JSON body");
  }
};

function ownTask(user: User, id: string): Task {
  const t = tasks.getTask(Number(id));
  if (!t || t.user_id !== user.id) throw new HttpError(404, "no such task");
  return t;
}

function taskView(t: Task) {
  const wfName = t.workflow_id ? db.query<{ name: string }, [number]>("SELECT name FROM workflows WHERE id = ?").get(t.workflow_id)?.name : null;
  return { ...t, also: JSON.parse(t.also), workflow: wfName ?? null };
}

route("GET", "/api/tasks", ({ user, url }) => {
  const limit = Number(url.searchParams.get("limit") ?? 100);
  const rows = db.query<Task, [number, number]>("SELECT * FROM tasks WHERE user_id = ? ORDER BY id DESC LIMIT ?").all(user.id, limit);
  return { tasks: rows.map(taskView) };
});

route("POST", "/api/tasks", async ({ req, user }) => {
  const body = await json<{ repo?: string; prompt?: string; also?: string[]; issue?: string }>(req);
  let repo = body.repo?.trim();
  let prompt = body.prompt?.trim() ?? "";
  let title: string | undefined;
  const ref = gh.parseIssueRef(body.issue ?? prompt);
  if (ref) {
    const issue = await gh.issue(user.gh_token, ref.repo, ref.number);
    repo = ref.repo;
    title = `#${issue.number} ${issue.title}`;
    prompt = `Resolve GitHub issue ${ref.repo}#${issue.number} and open a pull request that closes it.\n\nTitle: ${issue.title}\nURL: ${issue.html_url}\n\n${issue.body ?? ""}`;
  }
  if (!repo || !/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new HttpError(400, "pick a repo (owner/name)");
  if (!prompt) throw new HttpError(400, "say what the agent should do");
  await gh.repo(user.gh_token, repo).catch(() => {
    throw new HttpError(400, `${repo} isn't reachable with your GitHub identity`);
  });
  const t = tasks.createTask({ user, repo, prompt, title, also: body.also });
  return taskView(t);
});

route("GET", "/api/tasks/:id", ({ user, params }) => {
  const t = ownTask(user, params[0]!);
  const events = db.query("SELECT at, kind, text FROM task_events WHERE task_id = ? ORDER BY id").all(t.id);
  return { task: taskView(t), events, repo: tasks.getRepo(t.repo) };
});

route("POST", "/api/tasks/:id/message", async ({ req, user, params }) => {
  const t = ownTask(user, params[0]!);
  const { text } = await json<{ text: string }>(req);
  if (!text?.trim()) throw new HttpError(400, "empty message");
  await tasks.sendMessage(t.id, text.trim());
  return { ok: true };
});

route("POST", "/api/tasks/:id/stop", async ({ user, params }) => {
  await tasks.stop(ownTask(user, params[0]!).id);
  return { ok: true };
});

route("POST", "/api/tasks/:id/interrupt", async ({ user, params }) => {
  await hc.interrupt(tasks.agentAddress(ownTask(user, params[0]!).id));
  return { ok: true };
});

route("POST", "/api/tasks/:id/resume", async ({ user, params }) => {
  await tasks.resume(ownTask(user, params[0]!).id);
  return { ok: true };
});

route("POST", "/api/tasks/:id/save-ready", async ({ user, params }) => {
  const t = ownTask(user, params[0]!);
  const st = await dk.boxState(dk.names(t.id, user.login));
  if (!st?.running) throw new HttpError(409, "the box is asleep; wake it (send a message) before saving it");
  void tasks.saveReadyAndSettle(t.id);
  return { ok: true };
});

route("POST", "/api/tasks/:id/retry", ({ user, params }) => {
  const t = ownTask(user, params[0]!);
  const n = tasks.createTask({ user, repo: t.repo, prompt: t.prompt, title: t.title, also: JSON.parse(t.also), kind: t.kind, timeoutMin: t.timeout_min ?? undefined });
  return taskView(n);
});

route("POST", "/api/tasks/:id/shell", async ({ user, params }) => {
  const t = ownTask(user, params[0]!);
  const address = `task-${t.id}/shell`;
  const existing = await hc.getTerminal(address);
  if (!existing || existing.finished_ms) {
    if (!(await dk.boxState(dk.names(t.id, user.login)))?.running) await tasks.resume(t.id);
    await hc.runTerminal({
      machine: `task-${t.id}`, name: "shell", cwd: "/workspace",
      argv: ["ssh", "-q", "-t", "-p", "2222", "-i", "/run/outpost/ssh/id", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
        "root@127.0.0.1", `cd /workspace/${t.repo.split("/")[1]} && exec bash -l`],
    });
  }
  return { address };
});

route("GET", "/api/tasks/:id/screen", async ({ user, params }) => {
  const t = ownTask(user, params[0]!);
  if (t.released_at) return { text: null };
  return { text: await hc.readTerminal(tasks.agentAddress(t.id), 3000).catch(() => null) };
});

route("GET", "/api/tasks/:id/artifacts", async ({ user, params }) => {
  const t = ownTask(user, params[0]!);
  const c = dk.names(t.id, user.login);
  const r = await run(["docker", "exec", c.desk, "sh", "-c", "cd /run/outpost/artifacts 2>/dev/null && find . -type f -printf '%P\\t%s\\t%T@\\n' | head -200"]);
  const files = r.out.split("\n").filter(Boolean).map((l) => {
    const [path, size, mtime] = l.split("\t");
    return { path, size: Number(size), mtime: Math.round(Number(mtime) * 1000) };
  });
  return { files };
});

route("GET", "/api/tasks/:id/artifact", async ({ user, params, url }) => {
  const t = ownTask(user, params[0]!);
  const path = url.searchParams.get("path") ?? "";
  if (!path || path.includes("..")) throw new HttpError(400, "bad path");
  const c = dk.names(t.id, user.login);
  const p = Bun.spawn(["docker", "exec", c.desk, "cat", `/run/outpost/artifacts/${path}`], { stdout: "pipe" });
  const type = Bun.file(path).type || "application/octet-stream";
  return new Response(p.stdout as ReadableStream, { headers: { "content-type": type.startsWith("text/") ? `${type}; charset=utf-8` : type } });
});

route("GET", "/api/repos", async ({ user }) => {
  const recent = await gh.recentRepos(user.gh_token).catch(() => []);
  const known = db.query<Repo, []>("SELECT * FROM repos").all();
  const byName = new Map(known.map((r) => [r.full_name, r]));
  const used = db.query<{ repo: string; n: number }, [number]>("SELECT repo, COUNT(*) AS n FROM tasks WHERE user_id = ? GROUP BY repo ORDER BY MAX(id) DESC").all(user.id);
  const names = [...new Set([...used.map((u) => u.repo), ...recent.map((r) => r.full_name)])];
  return {
    repos: names.map((n) => {
      const r = byName.get(n);
      const g = recent.find((x) => x.full_name === n);
      return { full_name: n, description: g?.description ?? null, private: g?.private ?? null, ready: !!r?.ready_image, ready_at: r?.ready_at ?? null };
    }),
  };
});

route("GET", "/api/repos/:owner/:name", ({ params }) => {
  const r = tasks.getRepo(`${params[0]}/${params[1]}`);
  return { ...r, devices: JSON.parse(r.devices) };
});

route("POST", "/api/repos/:owner/:name/prepare", ({ user, params }) => {
  const repo = `${params[0]}/${params[1]}`;
  const t = tasks.createTask({ user, repo, kind: "setup", prompt: `Prepare a computer for ${repo}`, title: `Prepare ${repo}`, timeoutMin: 90 });
  return taskView(t);
});

route("GET", "/api/workflows", ({ user }) => ({
  workflows: wf.listWorkflows(user.id).map((w) => ({ ...w, repos: JSON.parse(w.repos), also: JSON.parse(w.also), enabled: !!w.enabled, next_due: w.enabled ? wf.nextDue(w) : null, runs: wf.runsOf(w.id, 5) })),
}));

type WorkflowBody = { name: string; cron: string; timezone?: string; repos: string[]; also?: string[]; prompt: string; timeout_min?: number; enabled?: boolean };

route("POST", "/api/workflows", async ({ req, user }) => {
  const b = await json<WorkflowBody>(req);
  return { id: wf.saveWorkflow(user, { ...b, timezone: b.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone }) };
});

route("PUT", "/api/workflows/:id", async ({ req, user, params }) => {
  const b = await json<WorkflowBody>(req);
  return { id: wf.saveWorkflow(user, { ...b, id: Number(params[0]), timezone: b.timezone ?? "UTC" }) };
});

route("DELETE", "/api/workflows/:id", ({ user, params }) => {
  wf.deleteWorkflow(user, Number(params[0]));
  return { ok: true };
});

route("POST", "/api/workflows/:id/run", ({ user, params }) => {
  const w = db.query<Workflow, [number, number]>("SELECT * FROM workflows WHERE id = ? AND user_id = ?").get(Number(params[0]), user.id);
  if (!w) throw new HttpError(404, "no such workflow");
  return { tasks: wf.fire(w) };
});

route("POST", "/api/workflows/import", async ({ req, user }) => {
  const b = await json<{ workflows: (WorkflowBody & { timezone: string; timeout_minutes?: number })[]; adapt?: boolean }>(req);
  const ids = b.workflows.map((w) =>
    wf.saveWorkflow(user, {
      ...w,
      timeout_min: w.timeout_min ?? w.timeout_minutes,
      prompt: b.adapt === false ? w.prompt : adaptMoorPrompt(w.prompt),
      enabled: false,
    }),
  );
  return { imported: ids.length, ids };
});

export function adaptMoorPrompt(prompt: string) {
  return prompt
    .replace(/\n\*\*Moor setup[^\n]*\n/g, "\n")
    .replaceAll(".moor/artifacts/", "/run/outpost/artifacts/")
    .replace(/Do not commit anything under `\.moor\/` into (the|a) PR branch\.\s*/g, "")
    .replace(/\s*\(do not commit `\.moor\/`\)/g, "")
    .replace(/Clear leftover Tasks\/`waiting_on` before idling so harvest sees a true idle\./g, "")
    .trim();
}

route("GET", "/api/secrets", ({ user }) => ({
  secrets: db.query<{ name: string; path: string | null }, [number]>("SELECT name, path FROM secrets WHERE user_id = ? ORDER BY name").all(user.id),
}));

route("PUT", "/api/secrets/:name", async ({ req, user, params }) => {
  const name = decodeURIComponent(params[0]!);
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) throw new HttpError(400, "secret names look like ENV_NAMES");
  const { value, path } = await json<{ value: string; path?: string }>(req);
  if (!value) throw new HttpError(400, "empty value");
  db.query("INSERT INTO secrets (user_id, name, value, path) VALUES (?, ?, ?, ?) ON CONFLICT(user_id, name) DO UPDATE SET value=excluded.value, path=excluded.path").run(
    user.id, name, value.endsWith("\n") || !path ? value : `${value}\n`, path?.trim() || null,
  );
  return { ok: true };
});

route("DELETE", "/api/secrets/:name", ({ user, params }) => {
  db.query("DELETE FROM secrets WHERE user_id = ? AND name = ?").run(user.id, decodeURIComponent(params[0]!));
  return { ok: true };
});

const MAC_KEY = "OUTPOST_MAC_KEY";

route("PUT", "/api/mac", async ({ req, user }) => {
  const { target } = await json<{ target: string | null }>(req);
  const v = target?.trim() || null;
  if (v && !/^[\w.-]+@[\w.:-]+$/.test(v)) throw new HttpError(400, "use user@host, for example me@mac.local");
  if (!v) {
    db.query("UPDATE users SET mac_target = NULL WHERE id = ?").run(user.id);
    return { ok: true };
  }
  let pub = user.mac_pubkey;
  if (!pub) {
    const dir = await run(["mktemp", "-d"]);
    const keyPath = `${dir.out.trim()}/key`;
    const r = await run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", `outpost-${user.login}`, "-f", keyPath]);
    if (r.code !== 0) throw new HttpError(500, `could not make a key: ${r.err}`);
    const priv = await Bun.file(keyPath).text();
    pub = (await Bun.file(`${keyPath}.pub`).text()).trim();
    await run(["rm", "-rf", dir.out.trim()]);
    db.query("INSERT INTO secrets (user_id, name, value, path) VALUES (?, ?, ?, ?) ON CONFLICT(user_id, name) DO UPDATE SET value=excluded.value, path=excluded.path")
      .run(user.id, MAC_KEY, priv, "/root/.ssh/outpost_mac");
  }
  db.query("UPDATE users SET mac_target = ?, mac_pubkey = ? WHERE id = ?").run(v, pub, user.id);
  return { ok: true, pubkey: pub };
});

route("POST", "/api/mac/test", async ({ user }) => {
  if (!user.mac_target) throw new HttpError(400, "save a Mac first");
  const key = db.query<{ value: string }, [number, string]>("SELECT value FROM secrets WHERE user_id = ? AND name = ?").get(user.id, MAC_KEY);
  if (!key) throw new HttpError(400, "no key yet; save the Mac again");
  const r = await run(["docker", "run", "--rm", "-i", "--network", "outpost", "--entrypoint", "bash", config.boxImage, "-c",
    `install -m 600 /dev/stdin /tmp/k && ssh -i /tmp/k -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 ${user.mac_target} 'sw_vers -productVersion; xcodebuild -version 2>/dev/null | head -1; xcrun simctl list devices available 2>/dev/null | grep -c iPhone'`],
    { input: key.value, timeoutMs: 40_000 });
  if (r.code !== 0) {
    const msg = (r.err || r.out).trim().split("\n").slice(-2).join(" ");
    return { ok: false, message: /Permission denied/.test(msg) ? "The Mac refused the key. Add the public key below to ~/.ssh/authorized_keys on the Mac." : msg };
  }
  const [macos, xcode, sims] = r.out.trim().split("\n");
  return { ok: true, message: `macOS ${macos}${xcode ? `, ${xcode}` : ", no Xcode found"}${sims ? `, ${sims} iPhone simulators` : ""}` };
});

route("PUT", "/api/profile", async ({ req, user }) => {
  const { profile_repo } = await json<{ profile_repo: string | null }>(req);
  const v = profile_repo?.trim() || null;
  if (v && !/^https:\/\/github\.com\/[\w.-]+\/[\w.-]+(#\S+)?$/.test(v)) throw new HttpError(400, "use https://github.com/OWNER/NAME, optionally #ref");
  db.query("UPDATE users SET profile_repo = ? WHERE id = ?").run(v, user.id);
  return { ok: true };
});

export async function setupState(user: User | null) {
  const pre = await dk.preflight();
  const hasAgentKey = user
    ? !!db.query("SELECT 1 FROM secrets WHERE user_id = ? AND name IN ('CLAUDE_CODE_OAUTH_TOKEN','ANTHROPIC_API_KEY')").get(user.id)
    : false;
  const hostKey = user && !hasAgentKey ? await hostClaudeToken(user.login) : null;
  return { ...pre, imageBuild, agentKey: hasAgentKey || !!hostKey, agentKeyFromHost: !!hostKey, profile: user?.profile_repo ?? null, maxRunning: config.maxRunning };
}

route("GET", "/api/setup", ({ user }) => setupState(user));

route("POST", "/api/setup/images", () => {
  void ensureImages();
  return { ok: true };
});

export async function handleApi(req: Request, url: URL, user: User): Promise<Response> {
  for (const r of routes) {
    if (r.method !== req.method) continue;
    const m = url.pathname.match(r.pattern);
    if (!m) continue;
    try {
      const out = await r.handler({ req, url, user, params: m.slice(1) });
      return out instanceof Response ? out : Response.json(out);
    } catch (e) {
      const status = e instanceof HttpError ? e.status : 500;
      const message = e instanceof Error ? e.message : String(e);
      if (status === 500) console.error(e);
      return Response.json({ error: message }, { status });
    }
  }
  return Response.json({ error: "not found" }, { status: 404 });
}

export type { TaskStatus };
