// Types written by hand from api/client/wire.go.

export type Task = {
  id: number;
  owner: string;
  kind: string; // interactive, scheduled or setup
  repo: string;
  also?: string[];
  title?: string;
  prompt: string;
  status: string; // queued, starting, working, waiting, done, failed or stopped
  phase?: string;
  machine?: string;
  summary?: string;
  error?: string;
  created_at: string;
  ended_at?: string;
  schedule_id?: number;
  access: string; // watch or control
};

export type Schedule = {
  id?: number;
  name: string;
  cron: string;
  tz: string;
  repo: string;
  also?: string[];
  prompt: string;
  title?: string;
  timeout_min?: number;
  enabled: boolean;
  next?: string;
  last_due?: string;
  last_task?: number;
  last_skipped?: string;
};

export type Repo = { name: string };
export type Machine = { id: string; name: string; online: boolean };
export type Me = { login: string; act: boolean };
export type NewTask = { repo: string; also?: string[]; prompt: string; title?: string; timeout_min?: number };

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const e = await res.json().catch(() => ({ error: "http", message: res.statusText }));
    throw new ApiError(res.status, e.error, e.message);
  }
  if (res.status === 204) return undefined as T;
  const type = res.headers.get("Content-Type") ?? "";
  return (type.includes("json") ? res.json() : res.text()) as Promise<T>;
}

const enc = encodeURIComponent;

export const api = {
  me: () => call<Me>("GET", "/api/me"),
  signOut: () => call<void>("DELETE", "/api/sessions/current"),
  tasks: (limit = 200) => call<Task[]>("GET", `/api/tasks?limit=${limit}`),
  task: (id: number) => call<Task>("GET", `/api/tasks/${id}`),
  screen: (id: number) => call<string>("GET", `/api/tasks/${id}/screen?lines=80`),
  createTask: (t: NewTask) => call<Task>("POST", "/api/tasks", t),
  message: (id: number, text: string) => call<Task>("POST", `/api/tasks/${id}/message`, { text }),
  stop: (id: number) => call<Task>("POST", `/api/tasks/${id}/stop`),
  schedules: () => call<Schedule[]>("GET", "/api/schedules"),
  schedule: (name: string) => call<Schedule>("GET", `/api/schedules/${enc(name)}`),
  runs: (name: string) => call<Task[]>("GET", `/api/tasks?schedule=${enc(name)}`),
  putSchedule: (s: Schedule) => call<Schedule>("PUT", `/api/schedules/${enc(s.name)}`, s),
  setEnabled: (name: string, on: boolean) =>
    call<Schedule>("POST", `/api/schedules/${enc(name)}/${on ? "enable" : "disable"}`),
  fire: (name: string) => call<Task>("POST", `/api/schedules/${enc(name)}/fire`),
  deleteSchedule: (name: string) => call<void>("DELETE", `/api/schedules/${enc(name)}`),
  repos: () => call<Repo[]>("GET", "/api/repos"),
  machines: () => call<Machine[]>("GET", "/api/machines"),
};
