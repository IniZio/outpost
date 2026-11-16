#!/usr/bin/env bun
import index from "../../web/index.html";
import { config } from "./config.ts";
import * as auth from "./auth.ts";
import { handleApi, setupState } from "./api.ts";
import { subscribe } from "./bus.ts";
import * as hc from "./hc.ts";
import * as dk from "./compute/docker.ts";
import * as tasks from "./tasks.ts";
import * as gh from "./github.ts";
import { startScheduler } from "./workflows.ts";
import { closeTerm, openTerm, termMessage, type TermData } from "./term.ts";
import { db, type User } from "./db.ts";
import { ensureImages, whenImagesReady } from "./images.ts";
import { detectLocalLogin } from "./local.ts";
import * as proxy from "./proxy.ts";

type EventsData = { kind: "events"; user: User; unsub?: () => void };
type WsData = TermData | EventsData;

await hc.ensureServer();

const terminals = new Map<string, { task: number; name: string }>();
const taskOfAddress = (address: string) => {
  const known = terminals.get(address);
  if (known) return known;
  const m = address.match(/^task-(\d+)\//);
  return m ? { task: Number(m[1]), name: "" } : null;
};

function parseStatus(v: unknown): { state: string; msg: string | null } | null {
  if (!v || typeof v !== "object") return null;
  const o = v as { state?: unknown; msg?: unknown };
  return typeof o.state === "string" ? { state: o.state, msg: typeof o.msg === "string" ? o.msg : null } : null;
}

hc.followEvents((e) => {
  const p = (e as { params?: { type: string; terminal: string; data: Record<string, unknown> } }).params;
  if (!p) {
    const snap = e as { terminals?: { address: string; name: string | null; status: unknown }[] };
    for (const t of snap.terminals ?? []) {
      const m = t.address.match(/^task-(\d+)\//);
      if (!m || !t.name) continue;
      terminals.set(t.address, { task: Number(m[1]), name: t.name });
      const st = parseStatus(t.status);
      if (t.name === "agent" && st) tasks.onAgentStatus(Number(m[1]), st.state, st.msg);
    }
    return;
  }
  if (p.type === "created") {
    const m = p.terminal.match(/^task-(\d+)\//);
    if (m) terminals.set(p.terminal, { task: Number(m[1]), name: String(p.data.name ?? "") });
    return;
  }
  const owner = taskOfAddress(p.terminal);
  if (!owner || owner.name !== "agent") return;
  if (p.type === "status") {
    const st = parseStatus(p.data.status);
    if (st) tasks.onAgentStatus(owner.task, st.state, st.msg);
  }
  if (p.type === "finished") tasks.onAgentExit(owner.task, (p.data.exit_code as number | null) ?? null);
});

dk.followDockerEvents((e) => {
  if (e.role === "box" && ["die", "oom", "start"].includes(e.action)) void tasks.onBoxEvent(e.task, e.action);
});

whenImagesReady(() => tasks.schedule());
void ensureImages();
console.log(`task network MTU ${await dk.ensureNetwork()}`);
await detectLocalLogin();
await tasks.recover();
proxy.writePolicy();
proxy.followEvents();
startScheduler();
setInterval(() => void tasks.gc(), 15 * 60_000);
setInterval(() => {
  tasks.schedule();
  void tasks.sleepIdle();
  proxy.writePolicy();
}, 30_000);

async function localUser(): Promise<User | null> {
  if (!config.localUser) return null;
  const token = await gh.localGhToken();
  if (!token) return null;
  const existing = db.query<User, [string]>("SELECT * FROM users WHERE gh_token = ?").get(token);
  if (existing) return existing;
  return (await auth.signIn(token)).user;
}

const server = Bun.serve<WsData, never>({
  port: config.port,
  hostname: config.host,
  idleTimeout: 120,
  routes: {
    "/": index,
    "/t/*": index,
    "/workflows": index,
    "/settings": index,
    "/welcome": index,
  },
  async fetch(req, srv) {
    const url = new URL(req.url);
    let user = auth.userFromRequest(req);
    const headers = new Headers();
    if (!user && auth.isLoopback(srv.requestIP(req)?.address)) {
      user = await localUser();
      if (user) {
        const { session } = await auth.signIn(user.gh_token);
        headers.append("set-cookie", auth.sessionCookie(session));
      }
    }

    if (url.pathname === "/api/login" && req.method === "POST") {
      try {
        const { token } = (await req.json()) as { token: string };
        const { user: u, session } = await auth.signIn(token.trim());
        return Response.json({ login: u.login }, { headers: { "set-cookie": auth.sessionCookie(session) } });
      } catch (e) {
        return Response.json({ error: e instanceof Error ? e.message : String(e) }, { status: 401 });
      }
    }
    if (url.pathname === "/api/logout" && req.method === "POST") {
      auth.signOut(req);
      return Response.json({ ok: true }, { headers: { "set-cookie": auth.clearCookie() } });
    }
    if (url.pathname === "/api/me") {
      const setup = await setupState(user);
      const body = { user: user ? { login: user.login, name: user.name, avatar: user.avatar, profile_repo: user.profile_repo, mac_target: user.mac_target, mac_pubkey: user.mac_pubkey } : null, setup };
      return Response.json(body, { headers });
    }
    if (!url.pathname.startsWith("/api/")) return new Response("not found", { status: 404 });
    if (!user) return Response.json({ error: "sign in first" }, { status: 401 });

    const origin = req.headers.get("origin");
    if (req.headers.get("upgrade") && origin && new URL(origin).host !== url.host) {
      return new Response("cross-origin websocket refused", { status: 403 });
    }
    if (url.pathname === "/api/events") {
      if (srv.upgrade(req, { data: { kind: "events", user } })) return undefined as unknown as Response;
      return new Response("expected a websocket", { status: 400 });
    }
    const term = url.pathname.match(/^\/api\/term\/(\d+)\/(agent|shell)$/);
    if (term) {
      const t = tasks.getTask(Number(term[1]));
      if (!t || t.user_id !== user.id) return new Response("no such task", { status: 404 });
      const data: TermData = {
        kind: "term",
        address: `task-${t.id}/${term[2]}`,
        observe: url.searchParams.get("observe") === "1",
        cols: Number(url.searchParams.get("cols") ?? 120),
        rows: Number(url.searchParams.get("rows") ?? 40),
      };
      if (srv.upgrade(req, { data })) return undefined as unknown as Response;
      return new Response("expected a websocket", { status: 400 });
    }
    const res = await handleApi(req, url, user);
    headers.forEach((v, k) => res.headers.append(k, v));
    return res;
  },
  websocket: {
    open(ws) {
      if (ws.data.kind === "term") return openTerm(ws as never);
      const d = ws.data;
      d.unsub = subscribe((msg) => {
        if (msg.taskId) {
          const t = tasks.getTask(msg.taskId);
          if (!t || t.user_id !== d.user.id) return;
        }
        ws.send(JSON.stringify(msg));
      });
    },
    message(ws, msg) {
      if (ws.data.kind === "term") termMessage(ws as never, msg);
    },
    close(ws) {
      if (ws.data.kind === "term") closeTerm(ws as never);
      else ws.data.unsub?.();
    },
  },
  development: process.env.NODE_ENV !== "production" ? { hmr: false, console: false } : false,
});

const shown = config.host === "0.0.0.0" ? "localhost" : config.host;
console.log(`outpost is up: http://${shown}:${server.port}`);
