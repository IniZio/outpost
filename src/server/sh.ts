export type RunResult = { code: number; out: string; err: string };

export async function run(
  argv: string[],
  opts: { input?: string; env?: Record<string, string>; timeoutMs?: number; cwd?: string } = {},
): Promise<RunResult> {
  const proc = Bun.spawn(argv, {
    stdin: opts.input !== undefined ? new TextEncoder().encode(opts.input) : "ignore",
    stdout: "pipe",
    stderr: "pipe",
    cwd: opts.cwd,
    env: { ...process.env, ...opts.env },
  });
  let timer: Timer | undefined;
  if (opts.timeoutMs) timer = setTimeout(() => proc.kill(), opts.timeoutMs);
  const [out, err, code] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  if (timer) clearTimeout(timer);
  return { code, out, err };
}

export async function must(argv: string[], opts: Parameters<typeof run>[1] = {}): Promise<string> {
  const r = await run(argv, opts);
  if (r.code !== 0) {
    throw new Error(`${argv.slice(0, 3).join(" ")} failed (${r.code}): ${(r.err || r.out).trim().slice(-800)}`);
  }
  return r.out;
}

export function shq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}
