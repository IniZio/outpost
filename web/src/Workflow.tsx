import { useEffect, useState } from "react";
import { Link, useNavigate, useOutletContext, useParams } from "react-router-dom";
import { ApiError, type Schedule, api } from "./api";
import { Composer, type Draft } from "./Composer";
import type { Ctx } from "./Layout";
import { headline, look, stamp } from "./format";
import { isSignedOut, usePoll } from "./hooks";
import { cronOf, describe, whenOf, zone } from "./when";

const draftOf = (s: Schedule): Draft => ({ prompt: s.prompt, repos: [s.repo, ...(s.also ?? [])], when: whenOf(s.cron) });

export function Workflow() {
  const { name = "" } = useParams();
  const { act } = useOutletContext<Ctx>();
  const nav = useNavigate();
  const sched = usePoll(() => api.schedule(name), 10000, [name]);
  const runs = usePoll(() => api.runs(name), 5000, [name]);
  const [draft, setDraft] = useState<Draft>();
  const [title, setTitle] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const s = sched.data;

  useEffect(() => {
    if (s && !draft) {
      setDraft(draftOf(s));
      setTitle(s.title || s.name);
    }
  }, [s, draft]);

  if (sched.error) {
    if (isSignedOut(sched.error)) return null;
    const gone = sched.error instanceof ApiError && sched.error.status === 404;
    return <p className="mut">{gone ? "This workflow does not exist." : sched.error.message} <Link to="/">Home</Link></p>;
  }
  if (!s || !draft) return null;

  const dirty = JSON.stringify(draft) !== JSON.stringify(draftOf(s)) || title !== (s.title || s.name);
  const guard = async (f: () => Promise<void>) => {
    setBusy(true);
    setErr("");
    try {
      await f();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const save = () =>
    guard(async () => {
      const [repo, ...also] = draft.repos;
      const saved = await api.putSchedule({
        name: s.name, cron: cronOf(draft.when) || s.cron, tz: draft.when.mode === "custom" || draft.when.mode === "now" ? s.tz : zone,
        repo, also: also.length ? also : undefined, prompt: draft.prompt, title: title ?? s.title,
        timeout_min: s.timeout_min, enabled: s.enabled,
      });
      setDraft(draftOf(saved));
      setTitle(saved.title || saved.name);
      sched.reload();
    });
  const toggle = () => guard(async () => { await api.setEnabled(s.name, !s.enabled); sched.reload(); });
  const runNow = () => guard(async () => { const t = await api.fire(s.name); nav(`/tasks/${t.id}`); });
  const remove = () =>
    guard(async () => {
      if (!confirm(`Delete "${s.title || s.name}"? Its earlier runs stay.`)) return;
      await api.deleteSchedule(s.name);
      nav("/");
    });

  const list = runs.data ?? [];
  return (
    <>
      <div className="row head">
        <input className="h1" aria-label="Title" value={title ?? ""} onChange={(e) => setTitle(e.target.value)} disabled={!act} />
        <button className="btn" disabled={!act || busy} onClick={toggle}>{s.enabled ? "Pause" : "Resume"}</button>
        <button className="btn" disabled={!act || busy} onClick={runNow}>Run now</button>
      </div>
      <Composer draft={draft} onChange={setDraft} label="Save" lit={act && dirty} busy={busy} onSubmit={save} rows={10} />
      {err && <div className="block bad-block">{err}</div>}
      <h2>Runs{s.enabled && s.next ? ` · next ${stamp(s.next)}` : s.enabled ? "" : " · paused"} <span className="mut">· {describe(draft.when)}</span></h2>
      <div className="card">
        {list.length === 0 && !s.last_skipped && <div className="li mut">No runs yet.{s.next ? ` Next: ${stamp(s.next)}.` : ""}</div>}
        {list.map((t) => (
          <Link key={t.id} className="li" to={`/tasks/${t.id}`}>
            <span className={`dot ${look(t.status).dot}`} />
            <span className="grow">{headline(t)}</span>
            <span className="when">{stamp(t.created_at)}</span>
          </Link>
        ))}
        {s.last_skipped && (
          <span className="li mut">
            <span className="dot off" />
            <span className="grow">Skipped: the server was down at the time.</span>
            <span className="when">{stamp(s.last_skipped)}</span>
          </span>
        )}
      </div>
      <p className="small"><button className="link bad" disabled={!act || busy} onClick={remove}>Delete this workflow</button></p>
    </>
  );
}
