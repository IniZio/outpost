import { expect, test } from "bun:test";

process.env.OUTPOST_DATA = `${import.meta.dir}/../../.data/test-${process.pid}`;
const { adaptMoorPrompt } = await import("./api.ts");

const moorPrompt = `/plan-mode

You are root inside the Box: install toolchains.

**Moor setup (do first if missing/broken):** make this repo workable as a Moor Box. Add a root \`.moor.yaml\`. Do not commit anything under \`.moor/\` into the PR branch.

Check out the default branch.
2. **Backend:** exercise those features live. Attach logs/screenshots under \`.moor/artifacts/\` (do not commit \`.moor/\`).
End idle with a summary. Clear leftover Tasks/\`waiting_on\` before idling so harvest sees a true idle.`;

test("moor-only steps are dropped and artifact paths rewritten", () => {
  const out = adaptMoorPrompt(moorPrompt);
  expect(out).not.toContain(".moor.yaml");
  expect(out).not.toContain(".moor/");
  expect(out).not.toContain("harvest");
  expect(out).toContain("/run/outpost/artifacts/");
  expect(out).toContain("/plan-mode");
  expect(out).toContain("Check out the default branch.");
});

test("a leading slash command is kept first", async () => {
  const { splitCommand } = await import("./tasks.ts");
  expect(splitCommand("/plan-mode\n\nDo the thing")).toEqual({ command: "/plan-mode", rest: "Do the thing" });
  expect(splitCommand("Do /not split")).toEqual({ command: "", rest: "Do /not split" });
});
