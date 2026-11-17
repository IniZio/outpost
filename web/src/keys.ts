import { firstLine } from "./format";

const slug = (s: string) =>
  s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40);

// workflowKey makes the key the server needs from a title and a repo, such as
// dependency-check-shop, and adds -2, -3 and so on when the key is taken.
export function workflowKey(title: string, repo: string, taken: Set<string>): string {
  const base = `${slug(firstLine(title)) || "workflow"}-${slug(repo.split("/")[1] ?? repo)}`;
  let key = base;
  for (let n = 2; taken.has(key); n++) key = `${base}-${n}`;
  return key;
}
