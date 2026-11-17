import { RepoPicker } from "./RepoPicker";
import { WhenPicker } from "./WhenPicker";
import { type When, valid } from "./when";

export type Draft = { prompt: string; repos: string[]; when: When };

// Composer is the one box: what the agent does, where, and when. Home and the Workflow page both
// use it.
export function Composer(props: {
  draft: Draft;
  onChange: (d: Draft) => void;
  label: string;
  lit: boolean; // the button is live
  busy: boolean;
  onSubmit: () => void;
  rows?: number;
}) {
  const d = props.draft;
  const ready = d.prompt.trim() !== "" && d.repos.length > 0 && valid(d.when);
  const on = ready && props.lit && !props.busy;
  return (
    <div className="card">
      <textarea
        className="prompt"
        rows={props.rows ?? 4}
        placeholder="What should the agent do?"
        value={d.prompt}
        onChange={(e) => props.onChange({ ...d, prompt: e.target.value })}
      />
      <div className="bar row">
        <RepoPicker value={d.repos} onChange={(repos) => props.onChange({ ...d, repos })} />
        <WhenPicker value={d.when} onChange={(when) => props.onChange({ ...d, when })} />
        <span className="grow" />
        <button className="btn pri" disabled={!on} onClick={props.onSubmit}>
          {props.label}
        </button>
      </div>
    </div>
  );
}
