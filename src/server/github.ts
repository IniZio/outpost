import { run } from "./sh.ts";

const API = "https://api.github.com";

async function gh<T>(token: string, path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(`${API}${path}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: "application/vnd.github+json",
      "User-Agent": "outpost",
      ...(init.headers ?? {}),
    },
  });
  if (!res.ok) throw new Error(`GitHub ${path}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json() as Promise<T>;
}

export type GhUser = { login: string; name: string | null; avatar_url: string };

export function whoami(token: string) {
  return gh<GhUser>(token, "/user");
}

export async function orgs(token: string): Promise<string[]> {
  const list = await gh<{ login: string }[]>(token, "/user/orgs?per_page=100");
  return list.map((o) => o.login);
}

export type GhRepo = { full_name: string; description: string | null; pushed_at: string; private: boolean; default_branch: string };

export function recentRepos(token: string) {
  return gh<GhRepo[]>(token, "/user/repos?sort=pushed&per_page=50&affiliation=owner,collaborator,organization_member");
}

export function repo(token: string, fullName: string) {
  return gh<GhRepo>(token, `/repos/${fullName}`);
}

export type GhIssue = { number: number; title: string; body: string | null; html_url: string; pull_request?: unknown };

export function issue(token: string, fullName: string, n: number) {
  return gh<GhIssue>(token, `/repos/${fullName}/issues/${n}`);
}

export async function localGhToken(): Promise<string | null> {
  const r = await run(["gh", "auth", "token"]).catch(() => null);
  const t = r?.code === 0 ? r.out.trim() : "";
  return t || null;
}

export function parseIssueRef(text: string): { repo: string; number: number } | null {
  const m = text.trim().match(/^(?:https:\/\/github\.com\/)?([\w.-]+\/[\w.-]+)(?:\/issues\/|#)(\d+)$/);
  return m ? { repo: m[1]!, number: Number(m[2]) } : null;
}
