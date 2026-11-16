import { randomBytes } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, openSync, readFileSync, readSync, closeSync, renameSync, statSync, writeFileSync, truncateSync } from "node:fs";
import { join } from "node:path";
import { config } from "./config.ts";
import { db, now, type Task } from "./db.ts";
import { taskEvent } from "./bus.ts";
import { run } from "./sh.ts";

// The GitHub credential proxy (images/proxy). It runs as its own container so
// tasks keep their network while the server restarts.

export const PROXY = "outpost-proxy";
const dir = join(config.dataDir, "proxy");
const policyFile = join(dir, "policy.json");
const eventsFile = join(dir, "events.jsonl");
const offsetFile = join(dir, "events.offset");
const caFile = join(dir, "ca", "mitmproxy-ca-cert.pem");

export const PLACEHOLDER_TOKEN = "outpost-proxy";
export const CA_PATH = "/usr/local/share/ca-certificates/outpost-proxy.crt";
const NO_PROXY = "localhost,127.0.0.1,0.0.0.0,::1,10.0.2.2,.local,.internal";

export const newSecret = () => randomBytes(18).toString("base64url");

export function writePolicy() {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const rows = db.query<{ id: number; repo: string; also: string; proxy_mode: string; proxy_secret: string; login: string; gh_token: string }, []>(
    `SELECT t.id, t.repo, t.also, t.proxy_mode, t.proxy_secret, u.login, u.gh_token FROM tasks t JOIN users u ON u.id = t.user_id
     WHERE t.proxy_secret IS NOT NULL AND t.released_at IS NULL`,
  ).all();
  type TaskPolicy = { secret: string; user: string; repos: string[]; mode: string };
  const tasks: Record<string, TaskPolicy> = {};
  const tokens: Record<string, string> = {};
  for (const r of rows) {
    tasks[String(r.id)] = { secret: r.proxy_secret, user: r.login, repos: [r.repo, ...(JSON.parse(r.also) as string[])], mode: r.proxy_mode };
    tokens[r.login] = r.gh_token;
  }
  const body = JSON.stringify({ tasks, tokens, allow_private: [] });
  const tmp = `${policyFile}.tmp`;
  writeFileSync(tmp, body, { mode: 0o600 });
  renameSync(tmp, policyFile);
}

async function imageId(ref: string) {
  const r = await run(["docker", "image", "inspect", "-f", "{{.Id}}", ref]);
  return r.code === 0 ? r.out.trim() : null;
}

export async function ensureProxy() {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  chmodSync(dir, 0o700);
  writePolicy();
  const want = await imageId(config.proxyImage);
  if (!want) throw new Error(`the proxy image ${config.proxyImage} is missing`);
  const cur = await run(["docker", "inspect", "-f", '{{.Image}} {{.State.Running}} {{index .Config.Labels "outpost.data"}}', PROXY]);
  const [img, running, data] = cur.out.trim().split(" ");
  if (cur.code === 0 && img === want && running === "true" && data === dir) return;
  await run(["docker", "rm", "-f", PROXY]);
  const uid = process.getuid?.() ?? 0;
  const gid = process.getgid?.() ?? 0;
  const r = await run([
    "docker", "run", "-d", "--name", PROXY, "--hostname", PROXY,
    "--network", "outpost", "--restart", "unless-stopped",
    "--user", `${uid}:${gid}`, "--label", "outpost.role=proxy", "--label", `outpost.data=${dir}`,
    "-v", `${dir}:/data`, config.proxyImage,
  ]);
  if (r.code !== 0) throw new Error(`could not start the proxy: ${r.err.trim().slice(-300)}`);
  for (let i = 0; i < 40 && !existsSync(caFile); i++) await Bun.sleep(250);
  if (!existsSync(caFile)) throw new Error("the proxy did not write its CA");
}

export const caPem = () => readFileSync(caFile, "utf8");

export function proxyEnv(task: Pick<Task, "id" | "proxy_secret">, port = 8080) {
  const url = `http://t${task.id}:${task.proxy_secret}@${PROXY}:${port}`;
  return {
    GH_TOKEN: PLACEHOLDER_TOKEN,
    HTTPS_PROXY: url, HTTP_PROXY: url, https_proxy: url, http_proxy: url,
    NO_PROXY, no_proxy: NO_PROXY,
    YARN_HTTPS_PROXY: url, YARN_HTTP_PROXY: url,
    NODE_EXTRA_CA_CERTS: CA_PATH,
    REQUESTS_CA_BUNDLE: "/etc/ssl/certs/ca-certificates.crt",
  };
}

type ProxyEvent = { task: number; kind: string; host?: string; port?: number; method?: string; path?: string; reason?: string; at: number };

function isProxyEvent(v: unknown): v is ProxyEvent {
  const o = v as Partial<ProxyEvent> | null;
  return !!o && typeof o.task === "number" && typeof o.kind === "string";
}

function onEvent(e: ProxyEvent) {
  if (!db.query("SELECT 1 FROM tasks WHERE id = ?").get(e.task)) return;
  if (e.kind === "host") {
    db.query("INSERT OR IGNORE INTO task_hosts (task_id, host, port, first_at) VALUES (?, ?, ?, ?)").run(e.task, e.host ?? "", e.port ?? 0, e.at ?? now());
  } else if (e.kind === "withheld") {
    taskEvent(e.task, "warning", `GitHub credential withheld: ${e.method} ${e.host}${e.path} (${e.reason})`);
  } else if (e.kind === "would_withhold") {
    taskEvent(e.task, "audit", `GitHub credential would be withheld: ${e.method} ${e.host}${e.path} (${e.reason})`);
  }
}

export function followEvents() {
  let offset = Number(existsSync(offsetFile) ? readFileSync(offsetFile, "utf8") : 0) || 0;
  let carry = "";
  setInterval(() => {
    if (!existsSync(eventsFile)) return;
    const size = statSync(eventsFile).size;
    if (size < offset) offset = 0;
    if (size === offset) {
      if (size > 8 * 1024 * 1024) {
        truncateSync(eventsFile, 0);
        offset = 0;
        writeFileSync(offsetFile, "0");
      }
      return;
    }
    const fd = openSync(eventsFile, "r");
    const buf = Buffer.alloc(size - offset);
    readSync(fd, buf, 0, buf.length, offset);
    closeSync(fd);
    offset = size;
    const lines = (carry + buf.toString("utf8")).split("\n");
    carry = lines.pop() ?? "";
    for (const line of lines) {
      try {
        const e: unknown = JSON.parse(line);
        if (isProxyEvent(e)) onEvent(e);
      } catch {}
    }
    writeFileSync(offsetFile, String(offset - Buffer.byteLength(carry)));
  }, 2000);
}
