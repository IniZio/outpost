import { randomBytes } from "node:crypto";
import { config } from "./config.ts";
import { db, now, type User } from "./db.ts";
import * as gh from "./github.ts";

const COOKIE = "outpost_session";

export async function signIn(token: string): Promise<{ user: User; session: string }> {
  const me = await gh.whoami(token);
  if (config.allowedOrgs.length) {
    const mine = await gh.orgs(token).catch(() => []);
    if (!mine.some((o) => config.allowedOrgs.includes(o))) {
      throw new Error(`${me.login} is not in ${config.allowedOrgs.join(" or ")}`);
    }
  }
  db.query(
    "INSERT INTO users (login, name, avatar, gh_token, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(login) DO UPDATE SET name=excluded.name, avatar=excluded.avatar, gh_token=excluded.gh_token",
  ).run(me.login, me.name, me.avatar_url, token, now());
  const user = db.query<User, [string]>("SELECT * FROM users WHERE login = ?").get(me.login)!;
  const session = randomBytes(24).toString("base64url");
  db.query("INSERT INTO sessions (token, user_id, created_at) VALUES (?, ?, ?)").run(session, user.id, now());
  return { user, session };
}

export function sessionCookie(session: string) {
  return `${COOKIE}=${session}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${60 * 60 * 24 * 90}`;
}

export function clearCookie() {
  return `${COOKIE}=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0`;
}

export function userFromRequest(req: Request): User | null {
  const cookie = req.headers.get("cookie") ?? "";
  const m = cookie.match(new RegExp(`${COOKIE}=([\\w-]+)`));
  if (!m) return null;
  return db.query<User, [string]>("SELECT u.* FROM users u JOIN sessions s ON s.user_id = u.id WHERE s.token = ?").get(m[1]!) ?? null;
}

export function signOut(req: Request) {
  const m = (req.headers.get("cookie") ?? "").match(new RegExp(`${COOKIE}=([\\w-]+)`));
  if (m) db.query("DELETE FROM sessions WHERE token = ?").run(m[1]!);
}

export function isLoopback(ip: string | undefined) {
  return !!ip && (ip === "127.0.0.1" || ip === "::1" || ip === "::ffff:127.0.0.1");
}
