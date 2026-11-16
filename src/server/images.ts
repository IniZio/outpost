import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { config } from "./config.ts";
import { run } from "./sh.ts";
import { publish } from "./bus.ts";

const imagesDir = join(import.meta.dir, "../../images");

export const imageBuild = { state: "idle" as "idle" | "building" | "failed" | "done", step: "", error: "", startedAt: 0 };

function sourceHash(dir: string) {
  const h = createHash("sha256");
  for (const f of readdirSync(join(imagesDir, dir)).sort()) h.update(f).update(readFileSync(join(imagesDir, dir, f)));
  return h.digest("hex").slice(0, 16);
}

async function builtHash(image: string): Promise<string | null> {
  const r = await run(["docker", "image", "inspect", "-f", '{{index .Config.Labels "outpost.hash"}}', image]);
  return r.code === 0 ? r.out.trim() : null;
}

export async function ensureImages() {
  if (imageBuild.state === "building") return;
  const todo = [
    { image: config.boxImage, dir: "box", label: "the box (task computer)" },
    { image: config.deskImage, dir: "desk", label: "the desk (agent)" },
    { image: config.proxyImage, dir: "proxy", label: "the GitHub credential proxy" },
  ];
  const stale = [];
  for (const t of todo) {
    const want = sourceHash(t.dir);
    if ((await builtHash(t.image)) !== want) stale.push({ ...t, hash: want });
  }
  if (!stale.length) {
    imageBuild.state = "done";
    return;
  }
  Object.assign(imageBuild, { state: "building", error: "", startedAt: Date.now() });
  for (const t of stale) {
    imageBuild.step = `Building ${t.label}`;
    publish({ type: "setup" });
    const r = await run(["docker", "build", "--network", "host", "--label", `outpost.hash=${t.hash}`, "-t", t.image, join(imagesDir, t.dir)], { timeoutMs: 30 * 60_000 });
    if (r.code !== 0) {
      Object.assign(imageBuild, { state: "failed", error: (r.err || r.out).trim().split("\n").slice(-8).join("\n") });
      publish({ type: "setup" });
      return;
    }
  }
  Object.assign(imageBuild, { state: "done", step: "" });
  publish({ type: "setup" });
  onImagesReady?.();
}

let onImagesReady: (() => void) | undefined;
export const whenImagesReady = (f: () => void) => (onImagesReady = f);

export const imagesReady = () => imageBuild.state === "done";
