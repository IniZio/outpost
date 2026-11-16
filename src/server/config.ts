import { homedir } from "node:os";
import { join } from "node:path";
import { mkdirSync } from "node:fs";

const env = process.env;

export const config = {
  port: Number(env.OUTPOST_PORT ?? 7300),
  host: env.OUTPOST_HOST ?? "0.0.0.0",
  dataDir: env.OUTPOST_DATA ?? join(homedir(), ".local/share/outpost"),
  hcSession: env.OUTPOST_HC_SESSION ?? "outpost",
  boxImage: env.OUTPOST_BOX_IMAGE ?? "outpost/box:dev",
  deskImage: env.OUTPOST_DESK_IMAGE ?? "outpost/desk:dev",
  proxyImage: env.OUTPOST_PROXY_IMAGE ?? "outpost/proxy:dev",
  ghProxy: (["off", "audit", "enforce"].includes(env.OUTPOST_GH_PROXY ?? "") ? env.OUTPOST_GH_PROXY : "enforce") as "off" | "audit" | "enforce",
  boxMemory: env.OUTPOST_BOX_MEMORY ?? "16g",
  deskMemory: env.OUTPOST_DESK_MEMORY ?? "3g",
  maxRunning: Number(env.OUTPOST_MAX_RUNNING ?? 4),
  keepFinishedHours: Number(env.OUTPOST_KEEP_HOURS ?? 24),
  idleSleepMinutes: Number(env.OUTPOST_IDLE_SLEEP_MIN ?? 30),
  localUser: env.OUTPOST_LOCAL_USER !== "0",
  publicUrl: env.OUTPOST_PUBLIC_URL ?? "",
  allowedOrgs: (env.OUTPOST_ALLOWED_ORGS ?? "").split(",").map((s) => s.trim()).filter(Boolean),
  defaultModel: env.OUTPOST_MODEL ?? "opus",
};

mkdirSync(config.dataDir, { recursive: true });
