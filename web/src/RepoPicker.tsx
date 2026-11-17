import { useState } from "react";
import { api } from "./api";
import { usePoll } from "./hooks";

// RepoPicker takes one or more repos. The first is the one the agent works in; the rest are
// cloned beside it. Names the server knows are offered, and any owner/name can be typed.
export function RepoPicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const [text, setText] = useState("");
  const known = usePoll(api.repos, 60000, [], false).data ?? [];
  const add = (raw: string) => {
    const name = raw.trim().replace(/^https:\/\/github\.com\//, "").replace(/\.git$/, "");
    if (/^[\w.-]+\/[\w.-]+$/.test(name) && !value.includes(name)) onChange([...value, name]);
    setText("");
  };
  return (
    <span className="repos">
      {value.map((r) => (
        <span key={r} className="chip mono">
          {r}
          <button className="x" aria-label={`Remove ${r}`} onClick={() => onChange(value.filter((v) => v !== r))}>
            ×
          </button>
        </span>
      ))}
      <input
        className="inp mono"
        list="known-repos"
        placeholder={value.length ? "add repo" : "owner/repo"}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          if (known.some((k) => k.name === e.target.value)) add(e.target.value);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === ",") {
            e.preventDefault();
            add(text);
          }
        }}
        onBlur={() => text && add(text)}
      />
      <datalist id="known-repos">
        {known.map((k) => <option key={k.name} value={k.name} />)}
      </datalist>
    </span>
  );
}
