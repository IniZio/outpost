import { readFileSync } from "node:fs";
import { join } from "node:path";
import { config } from "../config.ts";

const boxEntrypoint = readFileSync(join(import.meta.dir, "../../../images/box/entrypoint.sh"), "utf8");
import { must, run } from "../sh.ts";

export type Computer = {
  desk: string;
  box: string;
  volumes: { workspace: string; run: string; docker: string; home: string };
};

export type BoxSpec = {
  image: string;
  env: Record<string, string>;
  files: Record<string, string>;
  devices: string[];
};

export function names(taskId: number, userLogin: string): Computer {
  const p = `outpost-t${taskId}`;
  return {
    desk: `${p}-desk`,
    box: `${p}-box`,
    volumes: {
      workspace: `${p}-workspace`,
      run: `${p}-run`,
      docker: `${p}-docker`,
      home: `outpost-home-${userLogin.toLowerCase()}`,
    },
  };
}

function envArgs(env: Record<string, string>): string[] {
  return Object.entries(env).flatMap(([k, v]) => ["-e", `${k}=${v}`]);
}

const labels = (taskId: number, role: string) => ["--label", `outpost.task=${taskId}`, "--label", `outpost.role=${role}`];

const NETWORK = "outpost";
let networkMtu = 1500;

async function routeMtu(host: string): Promise<number | null> {
  const r = await run(["ip", "route", "get", host]);
  const dev = r.out.match(/ dev (\S+)/)?.[1];
  if (!dev) return null;
  const mtu = Number((await run(["cat", `/sys/class/net/${dev}/mtu`])).out.trim());
  return mtu > 0 ? mtu : null;
}

export async function ensureNetwork() {
  const mtus = (await Promise.all(["140.82.112.6", "1.1.1.1", "8.8.8.8"].map(routeMtu))).filter((m): m is number => !!m);
  networkMtu = Math.min(1500, ...mtus);
  const cur = await run(["docker", "network", "inspect", NETWORK, "-f", '{{index .Options "com.docker.network.driver.mtu"}}']);
  if (cur.code === 0 && Number(cur.out.trim() || 1500) === networkMtu) return networkMtu;
  if (cur.code === 0) await run(["docker", "network", "rm", NETWORK]);
  await must(["docker", "network", "create", "--opt", `com.docker.network.driver.mtu=${networkMtu}`, NETWORK]);
  return networkMtu;
}

export async function createDesk(taskId: number, c: Computer, env: Record<string, string>) {
  await run(["docker", "rm", "-f", c.desk]);
  await must([
    "docker", "run", "-d", "--name", c.desk, "--hostname", "desk",
    "--network", NETWORK,
    ...labels(taskId, "desk"),
    "--memory", config.deskMemory,
    "--restart", "unless-stopped",
    "-v", `${c.volumes.workspace}:/workspace`,
    "-v", `${c.volumes.run}:/run/outpost`,
    "-v", `${c.volumes.home}:/root`,
    ...envArgs(env),
    config.deskImage,
  ]);
  await must(["docker", "exec", c.desk, "sh", "-c",
    "mkdir -p /run/outpost/ssh /run/outpost/tmp /run/outpost/artifacts /var/lib/outpost-hc/config/herdr-core && rm -f /run/outpost/box-ready && " +
    "echo 'notes = false' > /var/lib/outpost-hc/config/herdr-core/config.toml && " +
    "[ -f /run/outpost/ssh/id ] || ssh-keygen -q -t ed25519 -N '' -f /run/outpost/ssh/id"]);
}

export async function writeRunFile(c: Computer, path: string, content: string) {
  await must(["docker", "exec", "-i", c.desk, "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", `/run/outpost/${path}`], { input: content });
}

export async function readRunFile(c: Computer, path: string): Promise<string | null> {
  const r = await run(["docker", "exec", c.desk, "cat", `/run/outpost/${path}`]);
  return r.code === 0 ? r.out : null;
}

export async function startBox(taskId: number, c: Computer, spec: BoxSpec) {
  await run(["docker", "rm", "-f", c.box]);
  await run(["docker", "exec", c.desk, "rm", "-rf", "/run/outpost/box-ready", "/run/outpost/files"]);
  for (const [path, content] of Object.entries(spec.files)) {
    await writeRunFile(c, `files${path.startsWith("/") ? path : `/root/${path}`}`, content);
  }
  await writeRunFile(c, "bin/outpost-box", boxEntrypoint);
  const deviceArgs: string[] = [];
  if (spec.devices.includes("android")) deviceArgs.push("--device", "/dev/kvm");
  await must([
    "docker", "run", "-d", "--name", c.box,
    ...labels(taskId, "box"),
    "--network", `container:${c.desk}`,
    "--privileged",
    "--memory", config.boxMemory,
    "--restart", "on-failure:5",
    "-v", `${c.volumes.workspace}:/workspace`,
    "-v", `${c.volumes.run}:/run/outpost`,
    "-v", `${c.volumes.docker}:/var/lib/docker`,
    ...deviceArgs,
    ...envArgs({ ...spec.env, OUTPOST_MTU: String(networkMtu) }),
    "--entrypoint", "/usr/bin/tini",
    spec.image,
    "--", "sh", "/run/outpost/bin/outpost-box",
  ]);
  for (let i = 0; i < 120; i++) {
    const r = await run(["docker", "exec", c.desk, "test", "-e", "/run/outpost/box-ready"]);
    if (r.code === 0) {
      const probe = await run(["docker", "exec", c.desk, "box-shell",
        "source /x/shell-snapshots/snapshot-probe 2>/dev/null || true && eval 'true' < /dev/null && pwd -P >| /run/outpost/tmp/probe-cwd"]);
      if (probe.code === 0) return;
    }
    const state = await run(["docker", "inspect", "-f", "{{.State.Status}}", c.box]);
    if (state.out.trim() === "exited") {
      const logs = await run(["docker", "logs", "--tail", "30", c.box]);
      throw new Error(`the box stopped while starting: ${(logs.err || logs.out).trim().slice(-600)}`);
    }
    await Bun.sleep(500);
  }
  throw new Error("the box did not answer over ssh within a minute");
}

export async function markBoxDown(c: Computer) {
  await run(["docker", "exec", c.desk, "rm", "-f", "/run/outpost/box-ready"]);
}

export async function stopBox(c: Computer) {
  await run(["docker", "rm", "-f", c.box]);
  await run(["docker", "exec", c.desk, "rm", "-f", "/run/outpost/box-ready"]);
}

const SEED = "/opt/outpost/seed";

export async function commitBox(c: Computer, image: string) {
  await must(["docker", "exec", c.box, "sh", "-c",
    `rm -rf ${SEED} && mkdir -p ${SEED} && ` +
    `if docker info >/dev/null 2>&1; then imgs=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -v '<none>'); ` +
    `if [ -n "$imgs" ]; then docker save $imgs -o ${SEED}/images.tar; fi; fi && ` +
    `cp -a /workspace ${SEED}/workspace`], { timeoutMs: 30 * 60_000 });
  const changes = ["--change", 'ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/outpost-box"]'];
  try {
    const committed = await run(["docker", "commit", ...changes, c.box, image], { timeoutMs: 30 * 60_000 });
    if (committed.code !== 0) {
      const flat = await run(["sh", "-c",
        `docker export ${c.box} | docker import ${[...changes, "--change", "WORKDIR /workspace", "--change", "ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"].map((a) => `'${a}'`).join(" ")} - ${image}`],
        { timeoutMs: 60 * 60_000 });
      if (flat.code !== 0) throw new Error(`docker commit failed (${committed.err.trim().slice(-200)}), and so did export/import: ${flat.err.trim().slice(-300)}`);
    }
  } finally {
    await run(["docker", "exec", c.box, "rm", "-rf", SEED]);
  }
}

export async function seedWorkspace(c: Computer, image: string): Promise<boolean> {
  const has = await run(["docker", "run", "--rm", "--entrypoint", "test", image, "-d", `${SEED}/workspace`]);
  if (has.code !== 0) return false;
  await must(["docker", "run", "--rm", "--entrypoint", "sh", "-v", `${c.volumes.workspace}:/workspace`, image, "-c",
    `cp -a ${SEED}/workspace/. /workspace/`], { timeoutMs: 20 * 60_000 });
  return true;
}

export async function loadSeedImages(c: Computer): Promise<boolean> {
  const r = await run(["docker", "exec", c.box, "sh", "-c",
    `[ -f ${SEED}/images.tar ] || exit 3; for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done; docker load -q -i ${SEED}/images.tar`], { timeoutMs: 20 * 60_000 });
  if (r.code === 3) return false;
  if (r.code !== 0) throw new Error(`could not load the ready image's Docker images: ${(r.err || r.out).trim().slice(-300)}`);
  return true;
}

export async function boxState(c: Computer): Promise<{ running: boolean; oomKilled: boolean; restarts: number } | null> {
  const r = await run(["docker", "inspect", "-f", "{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}}", c.box]);
  if (r.code !== 0) return null;
  const [running, oom, restarts] = r.out.trim().split(" ");
  return { running: running === "true", oomKilled: oom === "true", restarts: Number(restarts) };
}

export async function release(c: Computer) {
  await run(["docker", "rm", "-f", c.box, c.desk]);
  await run(["docker", "volume", "rm", c.volumes.workspace, c.volumes.run, c.volumes.docker]);
}

export function deskVia(c: Computer) {
  return `docker exec -i -e XDG_STATE_HOME=/var/lib/outpost-hc -e XDG_CONFIG_HOME=/var/lib/outpost-hc/config ${c.desk} sh -c`;
}

export function followDockerEvents(onEvent: (e: { task: number; role: string; action: string }) => void): () => void {
  let stopped = false;
  let proc: ReturnType<typeof Bun.spawn> | undefined;
  (async () => {
    while (!stopped) {
      proc = Bun.spawn(["docker", "events", "--format", "{{json .}}", "--filter", "type=container", "--filter", "label=outpost.task"], { stdout: "pipe", stderr: "ignore" });
      const reader = (proc.stdout as ReadableStream<Uint8Array>).getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl: number;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl);
          buf = buf.slice(nl + 1);
          try {
            const e = JSON.parse(line);
            const a = e.Actor?.Attributes ?? {};
            onEvent({ task: Number(a["outpost.task"]), role: a["outpost.role"], action: e.Action ?? e.status });
          } catch {}
        }
      }
      if (!stopped) await Bun.sleep(2000);
    }
  })();
  return () => {
    stopped = true;
    proc?.kill();
  };
}

export async function preflight(): Promise<{ docker: boolean; kvm: boolean; images: { box: boolean; desk: boolean } }> {
  const docker = (await run(["docker", "info", "--format", "{{.ServerVersion}}"])).code === 0;
  const kvm = (await run(["test", "-e", "/dev/kvm"])).code === 0;
  const has = async (img: string) => (await run(["docker", "image", "inspect", img])).code === 0;
  return { docker, kvm, images: { box: docker && (await has(config.boxImage)), desk: docker && (await has(config.deskImage)) } };
}
