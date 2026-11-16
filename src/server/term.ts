import type { ServerWebSocket } from "bun";
import { hcEnv } from "./hc.ts";

export type TermData = { kind: "term"; address: string; observe: boolean; cols: number; rows: number; proc?: ReturnType<typeof Bun.spawn> };

export function openTerm(ws: ServerWebSocket<TermData>) {
  const d = ws.data;
  const args = ["hc", "attach", d.address, "--history", "2000"];
  if (d.observe) args.push("--observe");
  d.proc = Bun.spawn(args, {
    env: { ...process.env, ...hcEnv, TERM: "xterm-256color", COLORTERM: "truecolor" },
    terminal: {
      cols: d.cols,
      rows: d.rows,
      data(_t, bytes) {
        ws.sendBinary(bytes);
      },
    },
  });
  d.proc.exited.then(() => {
    try {
      ws.close(1000, "detached");
    } catch {}
  });
}

export function termMessage(ws: ServerWebSocket<TermData>, msg: string | Buffer) {
  const t = ws.data.proc?.terminal;
  if (!t) return;
  if (typeof msg === "string" && msg.startsWith("\u0000resize:")) {
    const [cols, rows] = msg.slice(8).split("x").map(Number);
    if (cols && rows) t.resize(cols, rows);
    return;
  }
  if (ws.data.observe) return;
  t.write(typeof msg === "string" ? msg : new Uint8Array(msg));
}

export function closeTerm(ws: ServerWebSocket<TermData>) {
  const p = ws.data.proc;
  if (!p) return;
  p.terminal?.write("\x1c");
  setTimeout(() => {
    try {
      p.kill();
      p.terminal?.close();
    } catch {}
  }, 300);
}
