import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

type Me = { user: { login: string; name: string | null; avatar: string | null; profile_repo: string | null; mac_target: string | null; mac_pubkey: string | null } | null; setup: Setup };
type Setup = {
  docker: boolean; kvm: boolean; images: { box: boolean; desk: boolean }; agentKey: boolean; profile: string | null; maxRunning: number;
  imageBuild: { state: "idle" | "building" | "failed" | "done"; step: string; error: string };
};
type Task = {
  id: number; kind: "task" | "setup"; title: string; repo: string; also: string[]; prompt: string; status: string; agent_status: string | null;
  phase: string | null; summary: string | null; error: string | null; created_at: number; started_at: number | null; finished_at: number | null;
  released_at: number | null; workflow: string | null; image: string | null;
};
type TaskEvent = { at: number; kind: string; text: string };
type RepoInfo = { full_name: string; ready_image: string | null; ready_at: number | null; notes: string | null };
type RepoOption = { full_name: string; description: string | null; ready: boolean };
type Workflow = {
  id: number; name: string; cron: string; timezone: string; repos: string[]; also: string[]; prompt: string; timeout_min: number;
  enabled: boolean; next_due: number | null; runs: { repo: string; due_at: number; task_id: number | null; status: string | null }[];
};

async function api<T>(path: string, init: RequestInit & { body?: BodyInit | null } = {}): Promise<T> {
  const res = await fetch(path, { ...init, headers: { "content-type": "application/json", ...(init.headers ?? {}) } });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data as { error?: string }).error ?? `${res.status}`);
  return data as T;
}
const post = <T,>(path: string, body?: object) => api<T>(path, { method: "POST", body: JSON.stringify(body ?? {}) });

function usePath(): [string, (p: string) => void] {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const on = () => setPath(location.pathname);
    addEventListener("popstate", on);
    return () => removeEventListener("popstate", on);
  }, []);
  const go = useCallback((p: string) => {
    history.pushState(null, "", p);
    setPath(p);
  }, []);
  return [path, go];
}

function useLive(onMsg: (m: { type: string; taskId?: number }) => void) {
  const ref = useRef(onMsg);
  ref.current = onMsg;
  useEffect(() => {
    let ws: WebSocket | null = null;
    let dead = false;
    const connect = () => {
      ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/events`);
      ws.onmessage = (e) => ref.current(JSON.parse(e.data));
      ws.onclose = () => !dead && setTimeout(connect, 1500);
    };
    connect();
    return () => {
      dead = true;
      ws?.close();
    };
  }, []);
}

const STATUS_LABEL: Record<string, string> = {
  queued: "Queued", starting: "Starting", working: "Working", waiting: "Your turn", done: "Done", failed: "Failed", stopped: "Stopped",
};

function ago(ms: number | null) {
  if (!ms) return "";
  const s = Math.round((Date.now() - ms) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return new Date(ms).toLocaleDateString();
}
const clock = (ms: number) => new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });

let toastTimer: ReturnType<typeof setTimeout> | undefined;
function toast(msg: string) {
  let el = document.querySelector(".toast") as HTMLDivElement | null;
  if (!el) {
    el = document.createElement("div");
    el.className = "toast";
    document.body.append(el);
  }
  el.textContent = msg;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el?.remove(), 3500);
}

function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [path, go] = usePath();
  const [tasks, setTasks] = useState<Task[]>([]);
  const refreshMe = useCallback(() => api<Me>("/api/me").then(setMe), []);
  const refreshTasks = useCallback(() => api<{ tasks: Task[] }>("/api/tasks").then((r) => setTasks(r.tasks)).catch(() => {}), []);
  useEffect(() => void refreshMe(), [refreshMe]);
  useEffect(() => {
    if (me?.user) void refreshTasks();
  }, [me?.user, refreshTasks]);

  if (!me) return null;
  if (!me.user) return <SignIn onDone={refreshMe} />;
  return <Shell me={me} path={path} go={go} tasks={tasks} refreshTasks={refreshTasks} refreshMe={refreshMe} />;
}

function Shell(props: { me: Me; path: string; go: (p: string) => void; tasks: Task[]; refreshTasks: () => void; refreshMe: () => void }) {
  const { me, path, go, tasks, refreshTasks, refreshMe } = props;
  const [tick, setTick] = useState(0);
  useLive((m) => {
    if (m.type === "task" || m.type === "task-event") refreshTasks();
    if (m.type === "setup") refreshMe();
    setTick((t) => t + 1);
  });
  const taskId = path.match(/^\/t\/(\d+)/)?.[1];
  const link = (to: string) => (e: React.MouseEvent) => {
    e.preventDefault();
    go(to);
  };
  const active = tasks.filter((t) => ["queued", "starting", "working", "waiting"].includes(t.status));
  const recent = tasks.filter((t) => !active.includes(t)).slice(0, 40);
  return (
    <div className="app">
      <aside className="side">
        <div className="brand"><div className="logo">▲</div>Outpost</div>
        <nav className="nav">
          <a href="/" className={path === "/" ? "on" : ""} onClick={link("/")}>＋ New task</a>
          <a href="/workflows" className={path === "/workflows" ? "on" : ""} onClick={link("/workflows")}>⟳ Schedules</a>
          <a href="/settings" className={path === "/settings" ? "on" : ""} onClick={link("/settings")}>⚙ Settings</a>
        </nav>
        <div className="tasks">
          {active.length > 0 && <h4>Running</h4>}
          {active.map((t) => <TaskItem key={t.id} t={t} on={taskId === String(t.id)} onClick={link(`/t/${t.id}`)} />)}
          {recent.length > 0 && <h4>Recent</h4>}
          {recent.map((t) => <TaskItem key={t.id} t={t} on={taskId === String(t.id)} onClick={link(`/t/${t.id}`)} />)}
          {tasks.length === 0 && <p className="muted small" style={{ padding: "8px 10px" }}>Tasks you start show up here.</p>}
        </div>
        <div className="me">
          {me.user!.avatar && <img src={me.user!.avatar} alt="" />}
          <div className="grow">{me.user!.login}</div>
          <button className="btn sm" onClick={() => post("/api/logout").then(() => location.assign("/"))}>Sign out</button>
        </div>
      </aside>
      <main className="main">
        {taskId ? <TaskView key={taskId} id={Number(taskId)} tick={tick} go={go} />
          : path === "/workflows" ? <Workflows tick={tick} go={go} />
          : path === "/settings" ? <Settings me={me} refreshMe={refreshMe} />
          : <Home me={me} go={go} refreshMe={refreshMe} />}
      </main>
    </div>
  );
}

function TaskItem({ t, on, onClick }: { t: Task; on: boolean; onClick: (e: React.MouseEvent) => void }) {
  return (
    <a href={`/t/${t.id}`} className={`task-item ${on ? "on" : ""}`} onClick={onClick}>
      <div className="t">{t.title}</div>
      <div className="s"><span className={`dot ${t.status}`} />{STATUS_LABEL[t.status]} · {t.repo.split("/")[1]} · {ago(t.created_at)}</div>
    </a>
  );
}

function SignIn({ onDone }: { onDone: () => void }) {
  const [token, setToken] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true);
    setErr("");
    try {
      await post("/api/login", { token });
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="center">
      <div className="card signin">
        <div className="brand" style={{ padding: "0 0 10px" }}><div className="logo">▲</div>Outpost</div>
        <h2 style={{ margin: "4px 0 6px" }}>Sign in with your GitHub identity</h2>
        <p className="muted">Agents work as you: they clone, push and open PRs with the same access your <code>gh</code> has. Run this and paste what it prints:</p>
        <div className="copy" style={{ margin: "10px 0 14px" }}><code>gh auth token</code><button className="btn sm" onClick={() => navigator.clipboard.writeText("gh auth token").then(() => toast("Copied"))}>Copy</button></div>
        <input className="input mono" placeholder="gho_…" value={token} onChange={(e) => setToken(e.target.value)} onKeyDown={(e) => e.key === "Enter" && submit()} autoFocus />
        {err && <div className="err">{err}</div>}
        <div className="row" style={{ marginTop: 12 }}><div className="grow" /><button className="btn primary" disabled={!token || busy} onClick={submit}>{busy ? "Checking…" : "Continue"}</button></div>
      </div>
    </div>
  );
}

function Checklist({ setup, refreshMe }: { setup: Setup; refreshMe: () => void }) {
  const [key, setKey] = useState("");
  const [err, setErr] = useState("");
  const saveKey = async () => {
    setErr("");
    const name = key.trim().startsWith("sk-ant-api") ? "ANTHROPIC_API_KEY" : "CLAUDE_CODE_OAUTH_TOKEN";
    try {
      await api(`/api/secrets/${name}`, { method: "PUT", body: JSON.stringify({ value: key.trim() }) });
      setKey("");
      refreshMe();
      toast("Saved. Your agents can sign in to Claude now.");
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  const imagesOk = setup.images.box && setup.images.desk;
  if (setup.docker && imagesOk && setup.agentKey) return null;
  return (
    <div className="card checklist" style={{ marginBottom: 18 }}>
      <div style={{ fontWeight: 650, marginBottom: 10 }}>Finish setting up</div>
      <div className="item">
        <div className={`check ${setup.docker ? "yes" : ""}`}>{setup.docker ? "✓" : ""}</div>
        <div className="grow"><b>Docker</b><div className="muted small">{setup.docker ? "Found. Each task gets its own computer here." : "Outpost runs each task's computer with Docker on this machine. Install Docker and let this user use it."}</div></div>
      </div>
      <div className="item">
        <div className={`check ${imagesOk ? "yes" : ""}`}>{imagesOk ? "✓" : ""}</div>
        <div className="grow"><b>Computer images</b>
          <div className="muted small">
            {imagesOk ? "Built." : setup.imageBuild.state === "building" ? <><span className="dot working" style={{ display: "inline-block", marginRight: 6 }} />{setup.imageBuild.step}… (first time only, a few minutes)</>
              : setup.imageBuild.state === "failed" ? <>The build failed. <a href="#" onClick={(e) => { e.preventDefault(); void post("/api/setup/images").then(refreshMe); }}>Try again</a><div className="err mono">{setup.imageBuild.error}</div></>
              : <a href="#" onClick={(e) => { e.preventDefault(); void post("/api/setup/images").then(refreshMe); }}>Build them now</a>}
          </div>
        </div>
      </div>
      <div className="item">
        <div className={`check ${setup.agentKey ? "yes" : ""}`}>{setup.agentKey ? "✓" : ""}</div>
        <div className="grow"><b>Claude sign-in for your agents</b>
          {setup.agentKey ? <div className="muted small">Saved.</div> : <>
            <div className="muted small" style={{ marginBottom: 8 }}>Run <span className="kbd">claude setup-token</span> on your machine and paste the token (or an Anthropic API key). It stays with your account.</div>
            <div className="row"><input className="input mono grow" type="password" placeholder="sk-ant-oat01-…" value={key} onChange={(e) => setKey(e.target.value)} onKeyDown={(e) => e.key === "Enter" && key && saveKey()} /><button className="btn primary" disabled={!key} onClick={saveKey}>Save</button></div>
            {err && <div className="err">{err}</div>}
          </>}
        </div>
      </div>
    </div>
  );
}

function RepoPicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [repos, setRepos] = useState<RepoOption[]>([]);
  const [open, setOpen] = useState(false);
  const [hi, setHi] = useState(0);
  useEffect(() => void api<{ repos: RepoOption[] }>("/api/repos").then((r) => setRepos(r.repos)).catch(() => {}), []);
  const shown = useMemo(() => repos.filter((r) => r.full_name.toLowerCase().includes(value.toLowerCase())).slice(0, 30), [repos, value]);
  return (
    <div className="repo-pick">
      <input className="input mono" placeholder="owner/repo" value={value} onFocus={() => setOpen(true)} onBlur={() => setTimeout(() => setOpen(false), 150)}
        onChange={(e) => { onChange(e.target.value); setOpen(true); setHi(0); }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") setHi((h) => Math.min(h + 1, shown.length - 1));
          else if (e.key === "ArrowUp") setHi((h) => Math.max(h - 1, 0));
          else if (e.key === "Enter" && open && shown[hi]) { e.preventDefault(); onChange(shown[hi]!.full_name); setOpen(false); }
        }} />
      {open && shown.length > 0 && (
        <div className="menu">
          {shown.map((r, i) => (
            <div key={r.full_name} className={`opt ${i === hi ? "hi" : ""}`} onMouseDown={() => { onChange(r.full_name); setOpen(false); }}>
              <span className="mono grow">{r.full_name}</span>{r.ready && <span className="badge ok">ready</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function Home({ me, go, refreshMe }: { me: Me; go: (p: string) => void; refreshMe: () => void }) {
  const [prompt, setPrompt] = useState("");
  const [repo, setRepo] = useState(() => localStorage.getItem("outpost.repo") ?? "");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const isIssue = /^(https:\/\/github\.com\/[\w.-]+\/[\w.-]+\/issues\/\d+|[\w.-]+\/[\w.-]+#\d+)$/.test(prompt.trim());
  const ready = me.setup.docker && me.setup.images.box && me.setup.images.desk && me.setup.agentKey;
  const start = async () => {
    setBusy(true);
    setErr("");
    try {
      const t = await post<Task>("/api/tasks", { repo, prompt });
      localStorage.setItem("outpost.repo", t.repo);
      go(`/t/${t.id}`);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="page">
      <h1>What should an agent work on?</h1>
      <p className="lede">Each task gets its own computer with Docker and Android emulators. Watch it work, step in any time, and get a PR back.</p>
      <Checklist setup={me.setup} refreshMe={refreshMe} />
      {me.setup.imageBuild.state === "building" && me.setup.images.box && me.setup.images.desk && (
        <div className="card small" style={{ marginBottom: 14 }}><span className="dot working" style={{ display: "inline-block", marginRight: 8 }} />Updating the computer images ({me.setup.imageBuild.step.toLowerCase()}). Tasks you start now wait in the queue until it finishes.</div>
      )}
      <div className="card composer">
        <textarea className="input" placeholder={"Describe the task, or paste an issue link like\nhttps://github.com/owner/repo/issues/42"} value={prompt} onChange={(e) => setPrompt(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && (e.metaKey || e.ctrlKey) && start()} autoFocus />
        <div className="bar">
          {isIssue ? <span className="badge ok">Issue: the agent will open a PR that closes it</span> : <RepoPicker value={repo} onChange={setRepo} />}
          <div className="grow" />
          <span className="muted small">⌘↵</span>
          <button className="btn primary" disabled={busy || !ready || !prompt.trim() || (!isIssue && !repo)} onClick={start}>{busy ? "Starting…" : "Start task"}</button>
        </div>
        {err && <div className="err">{err}</div>}
      </div>
      <div className="muted small" style={{ marginTop: 14 }}>
        Tip: open <a href="/workflows" onClick={(e) => { e.preventDefault(); go("/workflows"); }}>Schedules</a> to run a task every weekday, such as a daily dependency update.
      </div>
    </div>
  );
}

function XTerm({ taskId, which, live, started }: { taskId: number; which: "agent" | "shell"; live: boolean; started: boolean }) {
  const host = useRef<HTMLDivElement>(null);
  const [gone, setGone] = useState(false);
  useEffect(() => {
    if (!host.current || !live) return;
    setGone(false);
    const term = new Terminal({
      fontFamily: 'ui-monospace, "SF Mono", "JetBrains Mono", Menlo, monospace', fontSize: 13, cursorBlink: true, scrollback: 10000,
      theme: { background: "#262624", foreground: "#e8e8e4" }, allowProposedApi: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    fit.fit();
    const ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/term/${taskId}/${which}?cols=${term.cols}&rows=${term.rows}`);
    ws.binaryType = "arraybuffer";
    ws.onmessage = (e) => term.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data));
    ws.onclose = () => setGone(true);
    const sub = term.onData((d) => ws.readyState === 1 && ws.send(d));
    const ro = new ResizeObserver(() => {
      fit.fit();
      if (ws.readyState === 1) ws.send(`\u0000resize:${term.cols}x${term.rows}`);
    });
    ro.observe(host.current);
    term.focus();
    return () => {
      ro.disconnect();
      sub.dispose();
      ws.close();
      term.dispose();
    };
  }, [taskId, which, live]);
  if (!live) return <FinalScreen taskId={taskId} started={started} />;
  return (
    <>
      <div className="xterm-host" ref={host} />
      {gone && <div className="muted small" style={{ background: "#262624", color: "#9a9a94", padding: "4px 12px" }}>Disconnected. <a href="#" onClick={(e) => { e.preventDefault(); location.reload(); }}>Reconnect</a></div>}
    </>
  );
}

function FinalScreen({ taskId, started }: { taskId: number; started: boolean }) {
  const [text, setText] = useState<string | null | undefined>(undefined);
  const ref = useRef<HTMLPreElement>(null);
  useEffect(() => void api<{ text: string | null }>(`/api/tasks/${taskId}/screen`).then((r) => setText(r.text)).catch(() => setText(null)), [taskId]);
  useEffect(() => {
    if (ref.current) ref.current.scrollTop = ref.current.scrollHeight;
  }, [text]);
  if (text === undefined) return <div className="screen-empty" />;
  if (!started) return <div className="screen-empty">The agent never started: setting up its computer failed. The reason is on the right; Run again retries.</div>;
  if (!text) return <div className="screen-empty">The agent's screen is gone (this computer was cleaned up). The summary and timeline are on the right.</div>;
  return (
    <pre ref={ref} className="mono" style={{ flex: 1, margin: 0, overflow: "auto", background: "#262624", color: "#d8d8d4", padding: "8px 12px", whiteSpace: "pre-wrap" }}>
      {text}
      {"\n"}
      <span style={{ color: "#9a9a94" }}>— The agent stopped. Send a message below to wake its computer and continue. —</span>
    </pre>
  );
}

function TaskView({ id, tick, go }: { id: number; tick: number; go: (p: string) => void }) {
  const [data, setData] = useState<{ task: Task; events: TaskEvent[]; repo: RepoInfo } | null>(null);
  const [tab, setTab] = useState<"agent" | "shell">("agent");
  const [msg, setMsg] = useState("");
  const [files, setFiles] = useState<{ path: string; size: number }[]>([]);
  const [err, setErr] = useState("");
  const load = useCallback(() => api<{ task: Task; events: TaskEvent[]; repo: RepoInfo }>(`/api/tasks/${id}`).then(setData).catch((e) => setErr(e.message)), [id]);
  useEffect(() => void load(), [load, tick]);
  useEffect(() => {
    if (!data?.task || data.task.released_at) return;
    void api<{ files: { path: string; size: number }[] }>(`/api/tasks/${id}/artifacts`).then((r) => setFiles(r.files)).catch(() => {});
  }, [id, data?.task.status, tick]);
  if (err && !data) return <div className="page"><div className="err">{err}</div></div>;
  if (!data) return null;
  const t = data.task;
  const live = ["starting", "working", "waiting"].includes(t.status) && t.status !== "starting";
  const finished = ["done", "failed", "stopped"].includes(t.status);
  const started = data.events.some((e) => e.text === "The agent is working");
  const canMessage = !t.released_at && started;
  const act = async (path: string, ok: string) => {
    try {
      await post(`/api/tasks/${id}/${path}`);
      toast(ok);
      load();
    } catch (e) {
      toast((e as Error).message);
    }
  };
  const send = async () => {
    const text = msg.trim();
    if (!text) return;
    setMsg("");
    try {
      await post(`/api/tasks/${id}/message`, { text });
      load();
    } catch (e) {
      toast((e as Error).message);
      setMsg(text);
    }
  };
  const openShell = async () => {
    try {
      await post(`/api/tasks/${id}/shell`);
      setTab("shell");
    } catch (e) {
      toast((e as Error).message);
    }
  };
  return (
    <div className="taskview">
      <div className="taskhead">
        <span className={`dot ${t.status}`} />
        <div className="grow" style={{ minWidth: 0 }}>
          <h2 title={t.title}>{t.title}</h2>
          <div className="muted small">
            <a href={`https://github.com/${t.repo}`} target="_blank" rel="noreferrer" className="mono">{t.repo}</a>
            {t.workflow && <> · schedule <b>{t.workflow}</b></>} · {STATUS_LABEL[t.status]}{t.phase ? `: ${t.phase}` : ""}
          </div>
        </div>
        <div className="actions">
          {(t.status === "working" || t.status === "waiting") && <button className="btn sm" onClick={() => act("interrupt", "Interrupted")}>Interrupt</button>}
          {(t.status === "working" || t.status === "waiting" || t.status === "queued") && <button className="btn sm danger" onClick={() => confirm("Stop this task? Its computer sleeps; you can still read everything.") && act("stop", "Stopped")}>Stop</button>}
          {finished && <button className="btn sm" onClick={() => post<Task>(`/api/tasks/${id}/retry`).then((n) => go(`/t/${n.id}`))}>Run again</button>}
        </div>
      </div>
      <div className="taskbody">
        <div className="screen">
          <div className="tabs">
            <button className={tab === "agent" ? "on" : ""} onClick={() => setTab("agent")}>Agent</button>
            <button className={tab === "shell" ? "on" : ""} onClick={() => (tab === "shell" ? null : openShell())}>Computer shell</button>
          </div>
          {t.status === "queued" || t.status === "starting" ? (
            <div className="screen-empty"><div><div style={{ fontSize: 15, color: "#e8e8e4", marginBottom: 6 }}>{t.phase ?? (t.status === "queued" ? "Waiting in the queue" : "Starting")}…</div><div>The agent's screen appears here as soon as it starts.</div></div></div>
          ) : (
            <XTerm key={tab} taskId={id} which={tab} live={live} started={started} />
          )}
        </div>
        <aside className="inspector">
          {(t.summary || t.error) && <>
            <h5>{t.error ? "What went wrong" : t.status === "waiting" ? "Latest from the agent" : "Summary"}</h5>
            {t.error && <div className="err" style={{ marginTop: 0, marginBottom: 8 }}>{t.error}</div>}
            {t.summary && <div className="summary">{t.summary}</div>}
          </>}
          {files.length > 0 && <>
            <h5>Artifacts</h5>
            {files.map((f) => <div key={f.path} className="small"><a href={`/api/tasks/${id}/artifact?path=${encodeURIComponent(f.path)}`} target="_blank" rel="noreferrer" className="mono">{f.path}</a> <span className="muted">{Math.ceil(f.size / 1024)} KB</span></div>)}
          </>}
          <h5>Computer</h5>
          <div className="small">
            {data.repo.ready_image ? <>Starts from {t.repo}'s ready image <span className="muted">({ago(data.repo.ready_at)})</span>.</> : <>No ready image for {t.repo} yet, so tasks start fresh and install what they need.</>}
          </div>
          <div className="actions" style={{ marginTop: 8 }}>
            {!t.released_at && t.kind === "setup" && t.status === "failed" && (t.error ?? "").startsWith("Could not save") && <button className="btn sm primary" onClick={() => act("save-ready", "Saving… this takes a few minutes; watch the timeline")}>Retry saving</button>}
            {!t.released_at && t.kind === "task" && (live) && <button className="btn sm" onClick={() => confirm(`Save this task's computer as ${t.repo}'s ready image? New tasks on the repo will start from it, with everything it installed.`) && act("save-ready", "Saving… this takes a few minutes; watch the timeline")}>Save as ready image</button>}
            {!data.repo.ready_image && <button className="btn sm" onClick={() => post<Task>(`/api/repos/${t.repo}/prepare`).then((n) => go(`/t/${n.id}`))}>Prepare {t.repo.split("/")[1]}</button>}
          </div>
          <h5>Timeline</h5>
          <ul className="timeline">
            {data.events.map((e, i) => <li key={i} className={e.kind}><span className="at">{clock(e.at)}</span><span className="tx">{e.text}</span></li>)}
          </ul>
          <h5>Task</h5>
          <div className="summary small">{t.prompt}</div>
        </aside>
      </div>
      <div className="steer">
        <textarea className="input grow" placeholder={!started ? (finished ? "The agent never started, so there is nothing to follow up. Use Run again." : "You can message the agent once it starts.") : finished ? (t.released_at ? "This computer was cleaned up." : "Send a follow-up: the computer wakes and the agent picks up where it left off") : "Message the agent (Enter to send, Shift+Enter for a new line)"} value={msg} disabled={!canMessage}
          onChange={(e) => setMsg(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); void send(); } }} />
        <button className="btn primary" disabled={!msg.trim()} onClick={send}>Send</button>
      </div>
    </div>
  );
}

function cronText(cron: string) {
  const [m, h, dom, mon, dow] = cron.split(/\s+/);
  if (dom === "*" && mon === "*" && /^\d+$/.test(m ?? "") && /^\d+$/.test(h ?? "")) {
    const at = `${h!.padStart(2, "0")}:${m!.padStart(2, "0")}`;
    if (dow === "1-5") return `Weekdays at ${at}`;
    if (dow === "*") return `Daily at ${at}`;
  }
  return cron;
}

const DEPS_TEMPLATE = `Review open Dependabot PRs and security alerts on the default branch. Merge only the PRs that are clearly safe (patch/minor, green CI); never dismiss alerts.

Then audit dependencies on the default branch tip and upgrade what is vulnerable or outdated. Push a branch and open a PR with a clear summary table. If nothing needs upgrading, say so.

For any upgrade PR, prove it live beyond unit tests: bring the stack up (Docker Compose or the repo's usual way), exercise the features the upgraded packages touch, and put logs and screenshots in /run/outpost/artifacts/.

End with: Dependabot actions taken, the upgrade PR URL or "nothing upgraded", the live test results, and anything blocked.`;

function Workflows({ tick, go }: { tick: number; go: (p: string) => void }) {
  const [list, setList] = useState<Workflow[] | null>(null);
  const [edit, setEdit] = useState<Partial<Workflow> | null>(null);
  const [importing, setImporting] = useState(false);
  const load = useCallback(() => api<{ workflows: Workflow[] }>("/api/workflows").then((r) => setList(r.workflows)), []);
  useEffect(() => void load(), [load, tick]);
  const toggle = async (w: Workflow) => {
    await api(`/api/workflows/${w.id}`, { method: "PUT", body: JSON.stringify({ ...w, enabled: !w.enabled }) });
    load();
  };
  const runNow = async (w: Workflow) => {
    const r = await post<{ tasks: number[] }>(`/api/workflows/${w.id}/run`);
    if (r.tasks[0]) go(`/t/${r.tasks[0]}`);
  };
  return (
    <div className="page">
      <div className="row" style={{ marginBottom: 6 }}>
        <h1 className="grow">Schedules</h1>
        <button className="btn" onClick={() => setImporting(true)}>Import from moor</button>
        <button className="btn primary" onClick={() => setEdit({ name: "", cron: "0 9 * * 1-5", timezone: Intl.DateTimeFormat().resolvedOptions().timeZone, repos: [], also: [], prompt: "", timeout_min: 120, enabled: true })}>New schedule</button>
      </div>
      <p className="lede">A schedule starts a task on each of its repos at set times. Each run gets a fresh computer and finishes on its own.</p>
      {list && list.length === 0 && (
        <div className="card">
          <b>Start with a daily dependency update</b>
          <p className="muted small">Every weekday morning, an agent reviews Dependabot PRs, upgrades vulnerable packages, proves the upgrade live and opens a PR.</p>
          <button className="btn primary" onClick={() => setEdit({ name: "Daily dependency update", cron: "0 9 * * 1-5", timezone: Intl.DateTimeFormat().resolvedOptions().timeZone, repos: [], also: [], prompt: DEPS_TEMPLATE, timeout_min: 120, enabled: true })}>Use this template</button>
        </div>
      )}
      {list?.map((w) => (
        <div className="card wf" key={w.id}>
          <div style={{ minWidth: 0 }}>
            <div className="row"><b>{w.name}</b>{!w.enabled && <span className="badge">paused</span>}</div>
            <div className="muted small">{cronText(w.cron)} ({w.timezone}) · {w.repos.join(", ")}{w.also.length ? ` + ${w.also.length} more` : ""} · up to {w.timeout_min} min</div>
            <div className="muted small">{w.next_due ? `Next run ${new Date(w.next_due).toLocaleString()}` : "Paused"}</div>
            <div className="runs">{w.runs.slice().reverse().map((r) => <a key={`${r.repo}-${r.due_at}`} className={r.status ?? ""} title={`${r.repo} ${new Date(r.due_at).toLocaleString()}: ${r.status ?? "?"}`} href={r.task_id ? `/t/${r.task_id}` : "#"} onClick={(e) => { e.preventDefault(); if (r.task_id) go(`/t/${r.task_id}`); }} />)}</div>
          </div>
          <div className="row" style={{ alignSelf: "start" }}>
            <button className="btn sm" onClick={() => runNow(w)}>Run now</button>
            <button className="btn sm" onClick={() => setEdit(w)}>Edit</button>
            <button className={`toggle ${w.enabled ? "on" : ""}`} title={w.enabled ? "Pause" : "Resume"} onClick={() => toggle(w)} />
          </div>
        </div>
      ))}
      {edit && <WorkflowEditor w={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); load(); }} />}
      {importing && <ImportMoor onClose={() => setImporting(false)} onDone={() => { setImporting(false); load(); }} />}
    </div>
  );
}

function WorkflowEditor({ w, onClose, onSaved }: { w: Partial<Workflow>; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState({ ...w, repos: (w.repos ?? []).join("\n"), also: (w.also ?? []).join("\n") });
  const [err, setErr] = useState("");
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => setF({ ...f, [k]: e.target.value });
  const save = async () => {
    setErr("");
    const body = { ...f, repos: f.repos.split(/\s+/).filter(Boolean), also: f.also.split(/\s+/).filter(Boolean), timeout_min: Number(f.timeout_min) };
    try {
      if (w.id) await api(`/api/workflows/${w.id}`, { method: "PUT", body: JSON.stringify(body) });
      else await post("/api/workflows", body);
      onSaved();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  const remove = async () => {
    if (!w.id || !confirm(`Delete ${w.name}?`)) return;
    await api(`/api/workflows/${w.id}`, { method: "DELETE" });
    onSaved();
  };
  return (
    <div className="modal-back" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal">
        <h3>{w.id ? "Edit schedule" : "New schedule"}</h3>
        <label className="field"><span>Name</span><input className="input" value={f.name ?? ""} onChange={set("name")} /></label>
        <div className="row">
          <label className="field grow"><span>When (cron)</span><input className="input mono" value={f.cron ?? ""} onChange={set("cron")} /><span className="muted small">{cronText(f.cron ?? "")}</span></label>
          <label className="field grow"><span>Time zone</span><input className="input" value={f.timezone ?? ""} onChange={set("timezone")} /></label>
          <label className="field" style={{ width: 120 }}><span>Timeout (min)</span><input className="input" value={String(f.timeout_min ?? 120)} onChange={set("timeout_min")} /></label>
        </div>
        <div className="row">
          <label className="field grow"><span>Repos (one run each)</span><textarea className="input mono" style={{ minHeight: 60 }} value={f.repos} onChange={set("repos")} placeholder="owner/repo" /></label>
          <label className="field grow"><span>Also clone beside each</span><textarea className="input mono" style={{ minHeight: 60 }} value={f.also} onChange={set("also")} placeholder="optional" /></label>
        </div>
        <label className="field"><span>What the agent does</span><textarea className="input" style={{ minHeight: 220 }} value={f.prompt ?? ""} onChange={set("prompt")} /></label>
        {err && <div className="err">{err}</div>}
        <div className="row">
          {w.id && <button className="btn danger" onClick={remove}>Delete</button>}
          <div className="grow" />
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn primary" onClick={save}>Save</button>
        </div>
      </div>
    </div>
  );
}

function ImportMoor({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [text, setText] = useState("");
  const [err, setErr] = useState("");
  const submit = async () => {
    setErr("");
    try {
      const parsed = JSON.parse(text);
      const workflows = (parsed.workflows ?? parsed) as Workflow[];
      const r = await post<{ imported: number }>("/api/workflows/import", { workflows });
      toast(`Imported ${r.imported}, paused so they don't run twice. Resume each when moor's copy is off.`);
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <div className="modal-back" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal">
        <h3>Import schedules from moor</h3>
        <p className="muted small">Paste the JSON from moor's <code>list_workflows</code> (or <code>moor workflow ls --json</code>). Imports arrive paused, and moor-only steps (<code>.moor.yaml</code> setup, <code>.moor/artifacts</code>) are rewritten for Outpost.</p>
        <textarea className="input mono" style={{ minHeight: 240 }} value={text} onChange={(e) => setText(e.target.value)} placeholder='{"workflows": [...]}' />
        {err && <div className="err">{err}</div>}
        <div className="row" style={{ marginTop: 12 }}><div className="grow" /><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={!text} onClick={submit}>Import</button></div>
      </div>
    </div>
  );
}

function MacCard({ me, refreshMe }: { me: Me; refreshMe: () => void }) {
  const [target, setTarget] = useState(me.user?.mac_target ?? "");
  const [result, setResult] = useState<{ ok: boolean; message: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    try {
      await api("/api/mac", { method: "PUT", body: JSON.stringify({ target }) });
      refreshMe();
      toast(target ? "Saved. Add the public key on the Mac, then test." : "Removed.");
    } catch (e) {
      toast((e as Error).message);
    }
  };
  const test = async () => {
    setBusy(true);
    setResult(null);
    try {
      setResult(await post<{ ok: boolean; message: string }>("/api/mac/test"));
    } catch (e) {
      setResult({ ok: false, message: (e as Error).message });
    } finally {
      setBusy(false);
    }
  };
  const pub = me.user?.mac_pubkey;
  return (
    <div className="card">
      <b>Mac for iOS</b>
      <p className="muted small">Tasks that touch an iOS app build and run the simulator on this Mac over ssh. Outpost makes its own key for it; your personal keys never leave your machine.</p>
      <div className="row"><input className="input mono grow" placeholder="you@mac.local" value={target} onChange={(e) => setTarget(e.target.value)} /><button className="btn" onClick={save}>Save</button></div>
      {me.user?.mac_target && pub && <>
        <p className="small" style={{ marginBottom: 6 }}>On the Mac, add this line to <code>~/.ssh/authorized_keys</code>:</p>
        <div className="copy"><code>{pub}</code><button className="btn sm" onClick={() => navigator.clipboard.writeText(pub).then(() => toast("Copied"))}>Copy</button></div>
        <div className="row" style={{ marginTop: 10 }}>
          <button className="btn" disabled={busy} onClick={test}>{busy ? "Testing…" : "Test connection"}</button>
          {result && <span className={`small ${result.ok ? "" : "err"}`} style={{ marginTop: 0, color: result.ok ? "var(--ok)" : undefined }}>{result.ok ? "✓ " : ""}{result.message}</span>}
        </div>
      </>}
    </div>
  );
}

function Settings({ me, refreshMe }: { me: Me; refreshMe: () => void }) {
  const [secrets, setSecrets] = useState<{ name: string; path: string | null }[]>([]);
  const [profile, setProfile] = useState(me.user?.profile_repo ?? "");
  const [form, setForm] = useState({ name: "", value: "", path: "" });
  const [err, setErr] = useState("");
  const load = useCallback(() => api<{ secrets: { name: string; path: string | null }[] }>("/api/secrets").then((r) => setSecrets(r.secrets)), []);
  useEffect(() => void load(), [load]);
  const saveProfile = async () => {
    try {
      await api("/api/profile", { method: "PUT", body: JSON.stringify({ profile_repo: profile }) });
      toast("Saved. New tasks apply it.");
      refreshMe();
    } catch (e) {
      toast((e as Error).message);
    }
  };
  const addSecret = async () => {
    setErr("");
    try {
      await api(`/api/secrets/${encodeURIComponent(form.name)}`, { method: "PUT", body: JSON.stringify({ value: form.value, path: form.path || undefined }) });
      setForm({ name: "", value: "", path: "" });
      load();
      refreshMe();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <div className="page">
      <h1>Settings</h1>
      <p className="lede">Everything here belongs to you ({me.user?.login}). Nothing is stored in your repos.</p>
      <div className="card">
        <b>Agent profile</b>
        <p className="muted small">A GitHub repo with your agent setup: <code>AGENTS.md</code>, <code>skills/</code>, <code>claude/settings.json</code> and plugins in <code>outpost-profile.yaml</code> (a moor profile works as is). Applied once to your agent's home and refreshed when it changes.</p>
        <div className="row"><input className="input mono grow" placeholder="https://github.com/you/agent-profile" value={profile} onChange={(e) => setProfile(e.target.value)} /><button className="btn" onClick={saveProfile}>Save</button></div>
      </div>
      <MacCard me={me} refreshMe={refreshMe} />
      <div className="card">
        <b>Secrets</b>
        <p className="muted small">Environment variables for your tasks' computers (Claude's sign-in stays with the agent). Give a path to write a secret as a file instead, such as <code>~/.ssh/id_ed25519</code> for a Mac you build iOS on.</p>
        {secrets.map((s) => (
          <div className="row" key={s.name} style={{ padding: "6px 0", borderTop: "1px solid var(--line)" }}>
            <span className="mono grow">{s.name}</span>{s.path && <span className="muted small mono">{s.path}</span>}
            <button className="btn sm danger" onClick={() => api(`/api/secrets/${encodeURIComponent(s.name)}`, { method: "DELETE" }).then(() => { load(); refreshMe(); })}>Remove</button>
          </div>
        ))}
        <div className="row" style={{ marginTop: 10 }}>
          <input className="input mono" style={{ width: 200 }} placeholder="NAME" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          <input className="input mono" style={{ width: 200 }} placeholder="file path (optional)" value={form.path} onChange={(e) => setForm({ ...form, path: e.target.value })} />
        </div>
        <textarea className="input mono" style={{ minHeight: 70, marginTop: 8 }} placeholder="value" value={form.value} onChange={(e) => setForm({ ...form, value: e.target.value })} />
        {err && <div className="err">{err}</div>}
        <div className="row" style={{ marginTop: 8 }}><div className="grow" /><button className="btn" disabled={!form.name || !form.value} onClick={addSecret}>Add secret</button></div>
      </div>
      <div className="card">
        <b>This machine</b>
        <div className="small" style={{ marginTop: 6 }}>Docker {me.setup.docker ? "✓" : "✗"} · KVM for Android {me.setup.kvm ? "✓" : "✗ (Android emulators need a machine with /dev/kvm)"} · up to {me.setup.maxRunning} tasks at once</div>
      </div>
    </div>
  );
}

createRoot(document.getElementById("root")!).render(<App />);
