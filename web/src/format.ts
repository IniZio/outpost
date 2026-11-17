import type { Schedule, Task } from "./api";

// The six words of the design. A missed slot reads Skipped.
export type Look = { word: string; dot: "" | "run" | "you" | "bad" | "off" };

export function look(status: string): Look {
  switch (status) {
    case "queued":
    case "starting":
      return { word: "Starting", dot: "run" };
    case "working":
      return { word: "Working", dot: "run" };
    case "waiting":
      return { word: "Waiting for you", dot: "you" };
    case "done":
      return { word: "Ended", dot: "" };
    case "failed":
      return { word: "Failed", dot: "bad" };
    case "stopped":
      return { word: "Stopped", dot: "" };
    default:
      return { word: status, dot: "" };
  }
}

export const ended = (t: Task) => t.status === "done" || t.status === "failed" || t.status === "stopped";

export const firstLine = (s: string | undefined) =>
  (s ?? "")
    .split("\n")
    .map((l) => l.replace(/^[#>*\s-]+/, "").trim())
    .find((l) => l !== "") ?? "";

export const taskTitle = (t: Task) => t.title || firstLine(t.prompt).slice(0, 80) || `Task ${t.id}`;

// headline is what a row shows for a run: the first line of the summary, else the status.
export function headline(t: Task): string {
  const first = firstLine(t.summary);
  if (t.status === "failed") return first || `Failed: ${firstLine(t.error) || "no reason given"}`;
  if (first) return first;
  switch (t.status) {
    case "waiting":
      return "Waiting for you";
    case "stopped":
      return "Stopped";
    case "done":
      return "Ended without a summary";
    default:
      return t.phase || look(t.status).word;
  }
}

const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "";
  const s = Math.round((new Date(iso).getTime() - now) / 1000);
  const units: [Intl.RelativeTimeFormatUnit, number][] = [["day", 86400], ["hour", 3600], ["minute", 60]];
  for (const [u, n] of units) if (Math.abs(s) >= n) return rtf.format(Math.round(s / n), u);
  return "just now";
}

const dayTime = new Intl.DateTimeFormat("en", { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hour12: false });
export const stamp = (iso: string | undefined) => (iso ? dayTime.format(new Date(iso)) : "");
const shortStamp = new Intl.DateTimeFormat("en", { weekday: "short", hour: "2-digit", minute: "2-digit", hour12: false });

export function scheduleWhen(s: Schedule): string {
  if (!s.enabled) return "paused";
  return s.next ? `next ${shortStamp.format(new Date(s.next))}` : "";
}

export function duration(from: string, to: string | undefined, now = Date.now()): string {
  const s = Math.max(0, Math.round(((to ? new Date(to).getTime() : now) - new Date(from).getTime()) / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.round(s / 60);
  return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, "0")} min`;
}

export const prLinks = (text: string | undefined): { url: string; label: string }[] => {
  const seen = new Set<string>();
  const out: { url: string; label: string }[] = [];
  for (const m of (text ?? "").matchAll(/https:\/\/github\.com\/([\w.-]+\/[\w.-]+)\/pull\/(\d+)/g)) {
    if (seen.has(m[0])) continue;
    seen.add(m[0]);
    out.push({ url: m[0], label: `${m[1]}#${m[2]}` });
  }
  return out;
};
