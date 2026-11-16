"use strict";
// The read-only watch page. Every text that comes from a task (titles, timeline, screen) is set
// with textContent, never as HTML, because the agent writes it.

const main = document.getElementById("main");

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "text") e.textContent = v;
    else e.setAttribute(k, v);
  }
  for (const k of kids) e.append(k);
  return e;
}

async function get(path) {
  const r = await fetch(path, { credentials: "same-origin", headers: { Accept: "application/json" } });
  if (r.status === 401) throw new Error("signin");
  if (!r.ok) throw new Error(`${path}: ${r.status}`);
  return r;
}

function signIn() {
  main.replaceChildren(
    el("h1", { text: "Sign in to watch" }),
    el("p", { text: "Run `outpost-cli weblink` and open the link it prints. It signs this browser in to watch, read-only." }),
  );
}

const when = (s) => new Date(s).toLocaleString();

async function list() {
  const tasks = await (await get("/api/tasks?limit=100")).json();
  if (tasks.length === 0) {
    main.replaceChildren(el("p", { class: "muted", text: "No tasks yet." }));
    return;
  }
  const rows = tasks.map((t) =>
    el("tr", {},
      el("td", {}, el("a", { href: `/tasks/${t.id}`, text: `#${t.id}` })),
      el("td", { text: t.title || t.prompt.split("\n")[0].slice(0, 100) }),
      el("td", { text: t.repo }),
      el("td", {}, el("span", { class: "status", text: t.status })),
      el("td", { class: "muted", text: when(t.created_at) }),
    ));
  main.replaceChildren(el("table", {},
    el("thead", {}, el("tr", {}, ...["Task", "Title", "Repo", "Status", "Started"].map((h) => el("th", { text: h })))),
    el("tbody", {}, ...rows)));
}

async function task(id) {
  const head = el("div");
  const timeline = el("ol", { class: "timeline" });
  const screen = el("pre", { class: "screen", text: "" });
  main.replaceChildren(head, el("div", { class: "cols" }, screen, timeline));

  const refresh = async () => {
    const t = await (await get(`/api/tasks/${id}`)).json();
    const kids = [
      el("h1", { text: `#${t.id} ${t.title || t.repo}` }),
      el("p", { class: "muted" }, el("span", { class: "status", text: t.status }), ` ${t.repo} · ${t.phase || ""}`),
    ];
    if (t.summary) kids.push(el("div", { class: "summary", text: t.summary }));
    if (t.error) kids.push(el("div", { class: "summary", text: t.error }));
    head.replaceChildren(...kids);
    return t;
  };
  await refresh();
  setInterval(() => refresh().catch(() => {}), 5000);

  const events = new EventSource(`/api/tasks/${id}/events`);
  events.addEventListener("entry", (m) => {
    const e = JSON.parse(m.data);
    const stick = timeline.scrollTop + timeline.clientHeight >= timeline.scrollHeight - 4;
    timeline.append(el("li", {}, el("time", { text: new Date(e.at).toLocaleTimeString() }), e.text));
    if (stick) timeline.scrollTop = timeline.scrollHeight;
  });

  const poll = async () => {
    if (document.visibilityState === "visible") {
      try {
        const r = await fetch(`/api/tasks/${id}/screen?lines=200`, { credentials: "same-origin" });
        if (r.ok) {
          const stick = screen.scrollTop + screen.clientHeight >= screen.scrollHeight - 4;
          screen.textContent = await r.text();
          if (stick) screen.scrollTop = screen.scrollHeight;
        } else if (r.status === 409) {
          screen.textContent = "The box cannot be reached right now.";
        }
      } catch (_) { /* the next poll tries again */ }
    }
    setTimeout(poll, 1000);
  };
  poll();
}

async function start() {
  try {
    const me = await (await get("/api/me")).json();
    document.getElementById("who").textContent = me.login;
    const m = location.pathname.match(/^\/tasks\/(\d+)$/);
    await (m ? task(m[1]) : list());
  } catch (e) {
    if (e.message === "signin") signIn();
    else main.replaceChildren(el("p", { text: `Something went wrong: ${e.message}` }));
  }
}

start();
