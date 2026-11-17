import { useState } from "react";
import { Link, useLocation, useNavigate, useOutletContext } from "react-router-dom";
import { type Schedule, type Task, api } from "./api";
import { Composer, type Draft } from "./Composer";
import type { Ctx } from "./Layout";
import { ago, headline, look, scheduleWhen, taskTitle } from "./format";
import { usePoll } from "./hooks";
import { workflowKey } from "./keys";
import { templates } from "./templates";
import { cronOf, nowWhen, zone } from "./when";

const empty: Draft = { prompt: "", repos: [], when: nowWhen };

type Row = { key: string; to: string; dot: string; title: string; repo: string; line: string; when: string; at: number };

function rows(schedules: Schedule[], tasks: Task[]): Row[] {
  const last = new Map<number, Task>();
  for (const t of tasks) if (t.schedule_id && !last.has(t.schedule_id)) last.set(t.schedule_id, t);
  const out: Row[] = schedules.map((s) => {
    const t = s.id ? last.get(s.id) : undefined;
    return {
      key: `w-${s.name}`,
      to: `/workflows/${encodeURIComponent(s.name)}`,
      dot: !s.enabled ? "off" : t ? look(t.status).dot : "",
      title: s.title || s.name,
      repo: s.repo,
      line: t ? headline(t) : "No runs yet",
      when: scheduleWhen(s),
      at: t ? Date.parse(t.created_at) : Date.parse(s.next ?? "") || 0,
    };
  });
  for (const t of tasks) {
    if (t.kind !== "interactive") continue;
    out.push({
      key: `t-${t.id}`,
      to: `/tasks/${t.id}`,
      dot: look(t.status).dot,
      title: taskTitle(t),
      repo: t.repo,
      line: headline(t),
      when: `once · ${ago(t.created_at)}`,
      at: Date.parse(t.created_at),
    });
  }
  return out.sort((a, b) => b.at - a.at);
}

export function Home() {
  const { act } = useOutletContext<Ctx>();
  const nav = useNavigate();
  const seed = useLocation().state as Draft | null;
  const [draft, setDraft] = useState<Draft>(seed ?? empty);
  const [title, setTitle] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [all, setAll] = useState(false);
  const schedules = usePoll(api.schedules, 10000, []);
  const tasks = usePoll(() => api.tasks(), 10000, []);
  const machines = usePoll(api.machines, 15000, []);

  const pick = (i: number) => {
    const t = templates[i];
    setTitle(t.title);
    setDraft({ prompt: t.prompt, repos: draft.repos, when: t.when });
  };
  const submit = async () => {
    setBusy(true);
    setErr("");
    try {
      const [repo, ...also] = draft.repos;
      const name = title || draft.prompt;
      if (draft.when.mode === "now") {
        const t = await api.createTask({ repo, also: also.length ? also : undefined, prompt: draft.prompt, title: title || undefined });
        nav(`/tasks/${t.id}`);
        return;
      }
      const key = workflowKey(name, repo, new Set((schedules.data ?? []).map((s) => s.name)));
      const s = await api.putSchedule({
        name: key, cron: cronOf(draft.when), tz: zone, repo, also: also.length ? also : undefined,
        prompt: draft.prompt, title: title || name.split("\n")[0].slice(0, 60), enabled: true,
      });
      nav(`/workflows/${encodeURIComponent(s.name)}`);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const list = rows(schedules.data ?? [], tasks.data ?? []);
  const recent = Date.now() - 14 * 86400_000;
  const shown = all ? list : list.filter((r) => r.at >= recent || !r.to.startsWith("/tasks/"));
  const noMachine = machines.data !== undefined && !machines.data.some((m) => m.online);

  return (
    <>
      <Composer
        draft={draft}
        onChange={(d) => {
          if (d.prompt !== draft.prompt && title) setTitle("");
          setDraft(d);
        }}
        label={draft.when.mode === "now" ? "Run" : "Schedule"}
        lit={act && !noMachine}
        busy={busy}
        onSubmit={submit}
        rows={5}
      />
      {err && <div className="block bad-block">{err}</div>}
      {noMachine && (
        <div className="block">
          <b>Before this can run</b>
          <div className="mt">A machine to run on. Run this on a Linux machine with Docker. This line goes away when it connects.</div>
          <input className="inp mono full" readOnly value={`outpost-agent pair --server ${location.origin}`} onFocus={(e) => e.target.select()} />
        </div>
      )}
      <div className="row small mt">
        <span className="mut">Start from</span>
        {templates.map((t, i) => (
          <button key={t.title} className={`chip${title === t.title ? " on" : ""}`} onClick={() => pick(i)}>{t.title}</button>
        ))}
      </div>
      <div className="gap" />
      {schedules.data && tasks.data && list.length === 0 && <p className="mut">Nothing yet. Describe a job above and press Run.</p>}
      {shown.length > 0 && (
        <div className="card">
          {shown.map((r) => (
            <Link key={r.key} className="li" to={r.to}>
              <span className={`dot ${r.dot}`} />
              <span className="grow">
                <b>{r.title}</b> <span className="mut mono">{r.repo}</span>
                <br />
                <span className="mut">{r.line}</span>
              </span>
              <span className="when">{r.when}</span>
            </Link>
          ))}
        </div>
      )}
      {!all && shown.length < list.length && (
        <p className="small"><button className="link" onClick={() => setAll(true)}>Older</button></p>
      )}
    </>
  );
}
