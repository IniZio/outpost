import { Cron } from "croner";
import { db, now, type User, type Workflow } from "./db.ts";
import { createTask } from "./tasks.ts";
import { publish } from "./bus.ts";

const jobs = new Map<number, Cron>();

export function listWorkflows(userId: number) {
  return db.query<Workflow, [number]>("SELECT * FROM workflows WHERE user_id = ? ORDER BY name").all(userId);
}

export function nextDue(w: Workflow): number | null {
  try {
    return new Cron(w.cron, { timezone: w.timezone, paused: true }).nextRun()?.getTime() ?? null;
  } catch {
    return null;
  }
}

export function validateCron(cron: string, timezone: string) {
  new Cron(cron, { timezone, paused: true });
}

export function saveWorkflow(user: User, w: {
  id?: number; name: string; cron: string; timezone: string; repos: string[]; also?: string[];
  prompt: string; timeout_min?: number; enabled?: boolean;
}) {
  validateCron(w.cron, w.timezone);
  if (!w.repos.length) throw new Error("a workflow needs at least one repo");
  const args = [w.name, w.cron, w.timezone, JSON.stringify(w.repos), JSON.stringify(w.also ?? []), w.prompt, w.timeout_min ?? 120, w.enabled === false ? 0 : 1] as const;
  let id = w.id;
  if (id) {
    db.query("UPDATE workflows SET name=?, cron=?, timezone=?, repos=?, also=?, prompt=?, timeout_min=?, enabled=? WHERE id=? AND user_id=?").run(...args, id, user.id);
  } else {
    id = (db.query("INSERT INTO workflows (name, cron, timezone, repos, also, prompt, timeout_min, enabled, user_id, created_at) VALUES (?,?,?,?,?,?,?,?,?,?) RETURNING id").get(...args, user.id, now()) as { id: number }).id;
  }
  arm(db.query<Workflow, [number]>("SELECT * FROM workflows WHERE id = ?").get(id)!);
  publish({ type: "workflow" });
  return id;
}

export function deleteWorkflow(user: User, id: number) {
  jobs.get(id)?.stop();
  jobs.delete(id);
  db.query("DELETE FROM workflows WHERE id = ? AND user_id = ?").run(id, user.id);
}

export function fire(w: Workflow, dueAt = now()) {
  const user = db.query<User, [number]>("SELECT * FROM users WHERE id = ?").get(w.user_id);
  if (!user) return [];
  const ids: number[] = [];
  for (const repo of JSON.parse(w.repos) as string[]) {
    const claimed = db.query("INSERT OR IGNORE INTO workflow_runs (workflow_id, repo, due_at) VALUES (?, ?, ?) RETURNING id").get(w.id, repo, dueAt) as { id: number } | null;
    if (!claimed) continue;
    const task = createTask({
      user, repo, also: JSON.parse(w.also), prompt: w.prompt, title: `${w.name}`,
      workflowId: w.id, timeoutMin: w.timeout_min,
    });
    db.query("UPDATE workflow_runs SET task_id = ? WHERE id = ?").run(task.id, claimed.id);
    ids.push(task.id);
  }
  return ids;
}

function arm(w: Workflow) {
  jobs.get(w.id)?.stop();
  jobs.delete(w.id);
  if (!w.enabled) return;
  jobs.set(w.id, new Cron(w.cron, { timezone: w.timezone, protect: true }, (self: Cron) => {
    const fresh = db.query<Workflow, [number]>("SELECT * FROM workflows WHERE id = ?").get(w.id);
    if (fresh?.enabled) fire(fresh, Math.floor((self.currentRun()?.getTime() ?? now()) / 60_000) * 60_000);
  }));
}

export function startScheduler() {
  for (const w of db.query<Workflow, []>("SELECT * FROM workflows").all()) arm(w);
}

export function runsOf(workflowId: number, limit = 20) {
  return db.query(
    "SELECT r.repo, r.due_at, t.id AS task_id, t.status, t.title FROM workflow_runs r LEFT JOIN tasks t ON t.id = r.task_id WHERE r.workflow_id = ? ORDER BY r.due_at DESC LIMIT ?",
  ).all(workflowId, limit);
}
