// Templates only fill the prompt and the schedule. Nothing else in the UI knows what a dependency
// is. For the prototype they live here; later the server serves them.

import { type When, nowWhen, whenOf } from "./when";

export type Template = { title: string; prompt: string; when: When; timeout_min: number };

const deps = `Check this repo's dependencies and upgrade what is safe.

1. Find outdated packages and known vulnerabilities with the repo's own tools.
2. Upgrade every version that fixes a known vulnerability. Upgrade patch and minor versions released at least 3 days ago. Do not upgrade major versions: list each one under "Needs you" with the reason.
3. Put all upgrades in one pull request on a branch named outpost/deps-<date>. If your earlier pull request is still open, update it instead. Open it as soon as the commits are pushed, so CI runs while you test.
4. Prove it. Build and run the tests. Start the app and try only the parts the upgrades touch. Say which parts you skipped and why.
5. Wait for CI in the background and fix small failures on the same branch. If the proof fails, make the pull request a draft.

Never merge a pull request. Never dismiss a security alert. Leave other bots' pull requests alone.`;

const vulns = `Report this repo's known vulnerabilities. Change nothing.

1. Run the repo's own audit tools and read the repository's security alerts.
2. List each finding: package, version, severity, and the fixed version if there is one.
3. Say which findings reach code this repo runs, and which only sit in dev tools.

Do not edit files, open pull requests or dismiss alerts. Start your summary with one sentence: how many findings, and the worst severity.`;

const ci = `The latest CI run on this repo's default branch, or on its newest open pull request, is failing. Find out why and fix it.

1. Read the failing job's log. Reproduce the failure locally if you can.
2. Make the smallest change that fixes it, on a new branch named outpost/fix-ci-<date>.
3. Open a pull request, and say what failed and why the change fixes it.

Never merge a pull request. If the failure is not in this repo's code, say so and stop.`;

export const templates: Template[] = [
  { title: "Dependency check", prompt: deps, when: whenOf("0 8 * * 1,3,5"), timeout_min: 120 },
  { title: "Vulnerability report", prompt: vulns, when: whenOf("0 9 * * *"), timeout_min: 60 },
  { title: "Fix failing CI", prompt: ci, when: nowWhen, timeout_min: 60 },
];
