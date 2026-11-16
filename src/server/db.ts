import { Database } from "bun:sqlite";
import { join } from "node:path";
import { config } from "./config.ts";

export const db = new Database(join(config.dataDir, "outpost.db"), { create: true });
db.exec("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;");

db.exec(`
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  login TEXT UNIQUE NOT NULL,
  name TEXT,
  avatar TEXT,
  gh_token TEXT NOT NULL,
  profile_repo TEXT,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  value TEXT NOT NULL,
  path TEXT,
  PRIMARY KEY (user_id, name)
);
CREATE TABLE IF NOT EXISTS repos (
  full_name TEXT PRIMARY KEY,
  ready_image TEXT,
  ready_at INTEGER,
  notes TEXT,
  setup_task_id INTEGER,
  devices TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS tasks (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  kind TEXT NOT NULL DEFAULT 'task',
  title TEXT NOT NULL,
  repo TEXT NOT NULL,
  also TEXT NOT NULL DEFAULT '[]',
  prompt TEXT NOT NULL,
  workflow_id INTEGER,
  timeout_min INTEGER,
  status TEXT NOT NULL,
  agent_status TEXT,
  phase TEXT,
  image TEXT,
  summary TEXT,
  error TEXT,
  created_at INTEGER NOT NULL,
  started_at INTEGER,
  finished_at INTEGER,
  released_at INTEGER
);
CREATE TABLE IF NOT EXISTS task_events (
  id INTEGER PRIMARY KEY,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  at INTEGER NOT NULL,
  kind TEXT NOT NULL,
  text TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workflows (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  name TEXT NOT NULL,
  cron TEXT NOT NULL,
  timezone TEXT NOT NULL,
  repos TEXT NOT NULL,
  also TEXT NOT NULL DEFAULT '[]',
  prompt TEXT NOT NULL,
  timeout_min INTEGER NOT NULL DEFAULT 120,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, name)
);
CREATE TABLE IF NOT EXISTS workflow_runs (
  id INTEGER PRIMARY KEY,
  workflow_id INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  repo TEXT NOT NULL,
  due_at INTEGER NOT NULL,
  task_id INTEGER,
  UNIQUE (workflow_id, repo, due_at)
);
`);

for (const [table, col, type] of [
  ["users", "mac_target", "TEXT"],
  ["users", "mac_pubkey", "TEXT"],
  ["tasks", "proxy_mode", "TEXT"],
  ["tasks", "proxy_secret", "TEXT"],
] as const) {
  const cols = db.query<{ name: string }, []>(`PRAGMA table_info(${table})`).all().map((c) => c.name);
  if (!cols.includes(col)) db.exec(`ALTER TABLE ${table} ADD COLUMN ${col} ${type}`);
}

db.exec(`
CREATE TABLE IF NOT EXISTS task_hosts (
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  first_at INTEGER NOT NULL,
  PRIMARY KEY (task_id, host, port)
);
`);

export type User = {
  id: number;
  login: string;
  name: string | null;
  avatar: string | null;
  gh_token: string;
  profile_repo: string | null;
  mac_target: string | null;
  mac_pubkey: string | null;
};

export type Task = {
  id: number;
  user_id: number;
  kind: "task" | "setup";
  title: string;
  repo: string;
  also: string;
  prompt: string;
  workflow_id: number | null;
  timeout_min: number | null;
  status: TaskStatus;
  agent_status: string | null;
  phase: string | null;
  image: string | null;
  summary: string | null;
  error: string | null;
  created_at: number;
  started_at: number | null;
  finished_at: number | null;
  released_at: number | null;
  proxy_mode: "audit" | "enforce" | null;
  proxy_secret: string | null;
};

export type TaskStatus = "queued" | "starting" | "working" | "waiting" | "done" | "failed" | "stopped";

export type Repo = {
  full_name: string;
  ready_image: string | null;
  ready_at: number | null;
  notes: string | null;
  setup_task_id: number | null;
  devices: string;
};

export type Workflow = {
  id: number;
  user_id: number;
  name: string;
  cron: string;
  timezone: string;
  repos: string;
  also: string;
  prompt: string;
  timeout_min: number;
  enabled: number;
  created_at: number;
};

export const now = () => Date.now();
