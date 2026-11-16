import { db, now } from "./db.ts";

type Listener = (msg: { type: string; taskId?: number; [k: string]: unknown }) => void;
const listeners = new Set<Listener>();

export function subscribe(l: Listener) {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function publish(msg: Parameters<Listener>[0]) {
  for (const l of listeners) {
    try {
      l(msg);
    } catch {}
  }
}

export function taskEvent(taskId: number, kind: string, text: string) {
  db.query("INSERT INTO task_events (task_id, at, kind, text) VALUES (?, ?, ?, ?)").run(taskId, now(), kind, text);
  publish({ type: "task-event", taskId, kind, text, at: now() });
}
