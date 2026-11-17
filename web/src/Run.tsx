import { useState } from "react";
import { Link, useNavigate, useOutletContext, useParams } from "react-router-dom";
import { ApiError, api } from "./api";
import { Markdown } from "./Markdown";
import type { Ctx } from "./Layout";
import { duration, ended, look, prLinks, stamp, taskTitle } from "./format";
import { isSignedOut, usePoll } from "./hooks";
import { nowWhen } from "./when";

export function Run() {
  const id = Number(useParams().id);
  const { act } = useOutletContext<Ctx>();
  const nav = useNavigate();
  const task = usePoll(() => api.task(id), 2500, [id]);
  const t = task.data;
  const working = t?.status === "working" || t?.status === "starting";
  const screen = usePoll(() => api.screen(id), 1500, [id], !!t && !ended(t) && t.status !== "queued");
  const sched = usePoll(api.schedules, 60000, [id], !!t?.schedule_id);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  if (task.error) {
    if (isSignedOut(task.error)) return null;
    const gone = task.error instanceof ApiError && task.error.status === 404;
    return <p className="mut">{gone ? "This run does not exist." : task.error.message} <Link to="/">Home</Link></p>;
  }
  if (!t) return null;

  const l = look(t.status);
  const over = ended(t);
  const owner = sched.data?.find((s) => s.id === t.schedule_id);
  const guard = async (f: () => Promise<unknown>) => {
    setBusy(true);
    setErr("");
    try {
      await f();
      task.reload();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const send = () => guard(async () => { await api.message(t.id, text); setText(""); });
  const again = () => nav("/", { state: { prompt: t.prompt, repos: [t.repo, ...(t.also ?? [])], when: nowWhen } });
  const prs = prLinks(t.summary);
  const note = t.summary || (t.error ? `Failed: ${t.error}` : "");
  const term = screen.data !== undefined && <pre className="term">{screen.data.trimEnd() || "(the terminal is empty)"}</pre>;

  return (
    <>
      <div className="row head">
        <h1 className="grow">{owner ? <Link to={`/workflows/${encodeURIComponent(owner.name)}`}>{owner.title || owner.name}</Link> : taskTitle(t)}</h1>
        {!over && <button className="btn" disabled={!act || busy} onClick={() => guard(() => api.stop(t.id))}>Stop</button>}
        {over && t.kind === "interactive" && <button className="btn" onClick={again}>Run again</button>}
      </div>
      <p className="mut status">
        <span className={`dot ${l.dot}`} /> {l.word} · <span className="mono">{t.repo}</span> · {stamp(t.created_at)} · {duration(t.created_at, t.ended_at)}
        {t.phase && !over && <> · {t.phase}</>}
      </p>
      {prs.length > 0 && (
        <div className="row mb">
          {prs.map((p) => <a key={p.url} className="chip" href={p.url} target="_blank" rel="noreferrer noopener">{p.label} ↗</a>)}
        </div>
      )}
      {working && term}
      {!working && note && <div className="card pad"><Markdown text={note} /></div>}
      {!working && !note && !over && <p className="mut">{t.phase || "The agent has not said anything yet."}</p>}
      {err && <div className="block bad-block">{err}</div>}
      {!over || t.kind === "interactive" || t.schedule_id ? (
        <div className="card row compose">
          <input
            className="grow msg"
            placeholder={over ? "Tell the agent to continue" : "Tell the agent"}
            value={text}
            disabled={!act}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && text.trim() && !busy && send()}
          />
          <button className="btn pri" disabled={!act || busy || !text.trim()} onClick={send}>Send</button>
        </div>
      ) : null}
      {!working && screen.data !== undefined && (
        <details className="fold"><summary>Terminal</summary>{term}</details>
      )}
    </>
  );
}
