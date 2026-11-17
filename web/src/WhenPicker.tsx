import { useState } from "react";
import { type When, cronOf, describe, presets, valid, zone } from "./when";

export function WhenPicker({ value, onChange }: { value: When; onChange: (w: When) => void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button className="chip" onClick={() => setOpen(!open)} aria-expanded={open}>
        {describe(value)} {open ? "▴" : "▾"}
      </button>
      {open && (
        <div className="pop small when-pop">
          <div className="row">
            {presets.map((p) => (
              <button key={p.mode} className={`chip${value.mode === p.mode ? " on" : ""}`} onClick={() => onChange({ ...value, mode: p.mode, cron: p.mode === "custom" ? cronOf(value) : value.cron })}>
                {p.label}
              </button>
            ))}
            {value.mode !== "now" && value.mode !== "custom" && (
              <>
                <span>at</span>
                <input className="inp mono" type="time" value={value.time} onChange={(e) => onChange({ ...value, time: e.target.value })} />
              </>
            )}
            {value.mode === "custom" && (
              <input className="inp mono" placeholder="m h dom mon dow" value={value.cron} onChange={(e) => onChange({ ...value, cron: e.target.value })} />
            )}
          </div>
          <div className="mut when-next">
            {value.mode === "now" ? "Runs once, right away." : valid(value) ? `Rule ${cronOf(value)}, in ${zone}.` : "A rule has five fields."}
          </div>
        </div>
      )}
    </>
  );
}
