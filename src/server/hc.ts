import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { config } from "./config.ts";
import { must, run } from "./sh.ts";

const hcConfigHome = join(config.dataDir, "hc-config");
mkdirSync(join(hcConfigHome, "herdr-core"), { recursive: true });
writeFileSync(join(hcConfigHome, "herdr-core", "config.toml"), "notes = false\n");

export const hcEnv = { HC_SESSION: config.hcSession, XDG_CONFIG_HOME: hcConfigHome };

async function hc(args: string[], opts: { input?: string; timeoutMs?: number } = {}) {
  const sep = args.indexOf("--");
  const withJson = sep < 0 ? [...args, "--json"] : [...args.slice(0, sep), "--json", ...args.slice(sep)];
  const out = await must(["hc", ...withJson], { env: hcEnv, ...opts });
  return out.trim() ? JSON.parse(out) : {};
}

export type HcTerminal = {
  address: string;
  name: string | null;
  status?: string;
  labels?: Record<string, string>;
  exit_code?: number | null;
  finished_ms?: number | null;
};

export async function ensureServer() {
  await run(["hc", "server", "start"], { env: hcEnv, timeoutMs: 20_000 });
}

export async function addMachine(name: string, via: string) {
  await run(["hc", "machine", "rm", name], { env: hcEnv });
  return hc(["machine", "add", "--via", via, "--name", name, "--temp", "--install"], { timeoutMs: 120_000 });
}

export async function removeMachine(name: string) {
  await run(["hc", "machine", "rm", name], { env: hcEnv });
}

export async function runTerminal(opts: {
  machine: string;
  name: string;
  cwd: string;
  argv: string[];
  labels?: Record<string, string>;
  keep?: boolean;
}): Promise<HcTerminal> {
  const args = ["run", "--bg", "--no-parent", "--outlive-parent", "--on", opts.machine, "--name", opts.name, "--cwd", opts.cwd];
  for (const [k, v] of Object.entries(opts.labels ?? {})) args.push("--label", `${k}=${v}`);
  if (opts.keep) args.push("--keep");
  args.push("--", ...opts.argv);
  return hc(args, { timeoutMs: 120_000 });
}

export async function getTerminal(address: string): Promise<HcTerminal | null> {
  try {
    return await hc(["get", address]);
  } catch {
    return null;
  }
}

export async function readTerminal(address: string, lines = 400): Promise<string> {
  const r = await run(["hc", "read", address, "--lines", String(lines)], { env: hcEnv, timeoutMs: 20_000 });
  return r.out;
}

export async function send(address: string, text: string) {
  return hc(["send", address, text], { timeoutMs: 30_000 });
}

export async function interrupt(address: string) {
  return hc(["interrupt", address], { timeoutMs: 30_000 });
}

export async function kill(address: string) {
  await run(["hc", "kill", address], { env: hcEnv, timeoutMs: 30_000 });
}

export async function exec(machine: string, argv: string[], opts: { input?: string; timeoutSec?: number } = {}) {
  const args = ["exec", "--on", machine, "--timeout", String(opts.timeoutSec ?? 120)];
  if (opts.input !== undefined) args.push("-i");
  return run(["hc", ...args, "--", ...argv], { env: hcEnv, input: opts.input, timeoutMs: ((opts.timeoutSec ?? 120) + 10) * 1000 });
}

export type HcEvent = { kind?: string; terminal?: HcTerminal; address?: string; status?: string; [k: string]: unknown };

export function followEvents(onEvent: (e: HcEvent) => void): () => void {
  let stopped = false;
  let proc: ReturnType<typeof Bun.spawn> | undefined;
  const loop = async () => {
    while (!stopped) {
      proc = Bun.spawn(["hc", "events", "--json"], { env: { ...process.env, ...hcEnv }, stdout: "pipe", stderr: "ignore" });
      const reader = (proc.stdout as ReadableStream<Uint8Array>).getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl: number;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          try {
            onEvent(JSON.parse(line));
          } catch {}
        }
      }
      if (!stopped) await Bun.sleep(2000);
    }
  };
  loop();
  return () => {
    stopped = true;
    proc?.kill();
  };
}
