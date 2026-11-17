import type { ReactNode } from "react";

// A small Markdown renderer for the agent's summary. It builds React elements, never HTML, and
// links only http and https, because the agent wrote the text.

const inline = /(`[^`]+`|\*\*[^*]+\*\*|\[[^\]]+\]\(https?:\/\/[^)\s]+\)|https?:\/\/[^\s)<]+)/g;

function spans(text: string): ReactNode[] {
  return text.split(inline).map((part, i) => {
    if (part.startsWith("`") && part.endsWith("`") && part.length > 1) return <code key={i}>{part.slice(1, -1)}</code>;
    if (part.startsWith("**") && part.endsWith("**") && part.length > 4) return <b key={i}>{part.slice(2, -2)}</b>;
    const link = /^\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)$/.exec(part);
    if (link) return <a key={i} href={link[2]} target="_blank" rel="noreferrer noopener">{link[1]}</a>;
    if (/^https?:\/\//.test(part)) return <a key={i} href={part} target="_blank" rel="noreferrer noopener">{part}</a>;
    return part;
  });
}

type Block = { kind: "p" | "ul" | "ol" | "pre" | "h"; lines: string[] };

function blocks(src: string): Block[] {
  const out: Block[] = [];
  let fence: Block | null = null;
  for (const line of src.split("\n")) {
    if (line.trim().startsWith("```")) {
      if (fence) {
        out.push(fence);
        fence = null;
      } else fence = { kind: "pre", lines: [] };
      continue;
    }
    if (fence) {
      fence.lines.push(line);
      continue;
    }
    const kind = /^#{1,6}\s/.test(line) ? "h" : /^\s*[-*]\s/.test(line) ? "ul" : /^\s*\d+[.)]\s/.test(line) ? "ol" : line.trim() ? "p" : null;
    const last = out[out.length - 1];
    if (!kind) out.push({ kind: "p", lines: [] });
    else if (last && last.kind === kind && kind !== "h" && (kind !== "p" || last.lines.length > 0)) last.lines.push(line);
    else out.push({ kind, lines: [line] });
  }
  if (fence) out.push(fence);
  return out.filter((b) => b.lines.length > 0);
}

export function Markdown({ text }: { text: string }) {
  return (
    <>
      {blocks(text).map((b, i) => {
        switch (b.kind) {
          case "pre":
            return <pre key={i} className="code">{b.lines.join("\n")}</pre>;
          case "h":
            return <p key={i}><b>{spans(b.lines[0].replace(/^#+\s*/, ""))}</b></p>;
          case "ul":
            return <ul key={i}>{b.lines.map((l, j) => <li key={j}>{spans(l.replace(/^\s*[-*]\s+/, ""))}</li>)}</ul>;
          case "ol":
            return <ol key={i}>{b.lines.map((l, j) => <li key={j}>{spans(l.replace(/^\s*\d+[.)]\s+/, ""))}</li>)}</ol>;
          default:
            return <p key={i}>{b.lines.flatMap((l, j) => (j ? [<br key={`b${j}`} />, ...spans(l)] : spans(l)))}</p>;
        }
      })}
    </>
  );
}
