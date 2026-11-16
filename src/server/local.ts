import { homedir } from "node:os";
import { join } from "node:path";
import { config } from "./config.ts";
import * as gh from "./github.ts";

let localLogin: string | null = null;

export async function detectLocalLogin() {
  if (!config.localUser) return;
  const token = await gh.localGhToken();
  if (token) localLogin = (await gh.whoami(token).catch(() => null))?.login ?? null;
}

export function isLocalLogin(login: string) {
  return !!localLogin && localLogin === login;
}

type ClaudeCredentials = { claudeAiOauth?: { accessToken?: string; expiresAt?: number } };

export async function hostClaudeToken(login: string): Promise<{ token: string; expiresAt: number } | null> {
  if (!isLocalLogin(login)) return null;
  const file = Bun.file(join(homedir(), ".claude/.credentials.json"));
  if (!(await file.exists())) return null;
  const creds = (await file.json().catch(() => ({}))) as ClaudeCredentials;
  const o = creds.claudeAiOauth;
  if (!o?.accessToken || !o.expiresAt || o.expiresAt < Date.now() + 15 * 60_000) return null;
  return { token: o.accessToken, expiresAt: o.expiresAt };
}
