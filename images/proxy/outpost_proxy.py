"""mitmproxy addon for Outpost tasks.

Each task reaches the proxy with its own credentials in HTTPS_PROXY. TLS to
GitHub is opened so the placeholder token can be swapped for the requester's
real one, under the rules in policy.py. Every other connection is a plain
tunnel. Port 8081 never opens TLS; nested Docker builds use it, since they
don't trust the proxy's CA.

Outpost writes /data/policy.json. This addon appends to /data/events.jsonl.
"""

import asyncio
import base64
import ipaddress
import json
import os
import socket
import time
import urllib.request

from mitmproxy import ctx, http, tls
from mitmproxy.proxy import server_hooks

import policy

DATA = os.environ.get("OUTPOST_PROXY_DATA", "/data")
POLICY = os.path.join(DATA, "policy.json")
EVENTS = os.path.join(DATA, "events.jsonl")
TUNNEL_ONLY_PORT = 8081
SHARED = ipaddress.ip_network("100.64.0.0/10")


class Policy:
    def __init__(self):
        self.mtime = 0.0
        self.data = {"tasks": {}, "tokens": {}, "allow_private": []}

    def get(self):
        try:
            m = os.stat(POLICY).st_mtime
            if m != self.mtime:
                with open(POLICY) as f:
                    self.data = json.load(f)
                self.mtime = m
        except (OSError, ValueError) as e:
            ctx.log.warn(f"policy: {e}")
        return self.data


def blocked_address(ip: str) -> bool:
    a = ipaddress.ip_address(ip)
    return a.is_private or a.is_loopback or a.is_link_local or a.is_multicast or a.is_reserved or a.is_unspecified or a in SHARED


class Outpost:
    def __init__(self):
        self.policy = Policy()
        self.conn_task: dict[str, str] = {}
        self.seen_hosts: set[tuple[str, str, int]] = set()
        self.repo_ids: dict[int, str | None] = {}
        self.node_cache: dict[str, dict | None] = {}

    # --- bookkeeping -------------------------------------------------------

    def event(self, task: str, kind: str, **fields):
        line = json.dumps({"at": int(time.time() * 1000), "task": int(task), "kind": kind, **fields})
        with open(EVENTS, "a") as f:
            f.write(line + "\n")

    def task_from_header(self, value: str | None):
        if not value or not value.lower().startswith("basic "):
            return None
        try:
            user, _, secret = base64.b64decode(value[6:]).decode().partition(":")
        except Exception:
            return None
        if not user.startswith("t"):
            return None
        t = self.policy.get()["tasks"].get(user[1:])
        return user[1:] if t and secret and t.get("secret") == secret else None

    def task_of(self, flow) -> str | None:
        return self.conn_task.get(flow.client_conn.id)

    def note_host(self, task: str, host: str, port: int):
        key = (task, host, port)
        if key not in self.seen_hosts:
            self.seen_hosts.add(key)
            self.event(task, "host", host=host, port=port)

    def client_disconnected(self, client):
        self.conn_task.pop(client.id, None)

    # --- who may connect where --------------------------------------------

    def http_connect(self, flow: http.HTTPFlow):
        task = self.task_from_header(flow.request.headers.get("Proxy-Authorization"))
        if not task:
            flow.response = http.Response.make(407, b"outpost: proxy credentials missing or wrong\n", {"Proxy-Authenticate": 'Basic realm="outpost"'})
            return
        self.conn_task[flow.client_conn.id] = task
        self.note_host(task, flow.request.host, flow.request.port)

    def requestheaders(self, flow: http.HTTPFlow):
        if flow.request.method == "CONNECT":
            return
        task = self.task_of(flow)
        if not task:
            # Plain-HTTP proxying carries the credentials on every request.
            task = self.task_from_header(flow.request.headers.get("Proxy-Authorization"))
            if not task:
                flow.response = http.Response.make(407, b"outpost: proxy credentials missing or wrong\n", {"Proxy-Authenticate": 'Basic realm="outpost"'})
                return
            self.note_host(task, flow.request.host, flow.request.port)
        flow.request.headers.pop("Proxy-Authorization", None)
        flow.metadata["outpost_task"] = task
        if self.is_github(flow):
            if flow.request.host == "api.github.com" and flow.request.path.split("?")[0] == "/graphql":
                return  # judged in request(), once the body is in
            self.judge_rest(flow, task)
            if not flow.response:
                flow.request.stream = True

    def tls_clienthello(self, data: tls.ClientHelloData):
        client = data.context.client
        server = data.context.server
        task = self.conn_task.get(client.id)
        target = server.address[0] if server.address else None
        sni = data.client_hello.sni
        intercept = (
            task is not None
            and client.sockname[1] != TUNNEL_ONLY_PORT
            and target in policy.GITHUB_HOSTS
            and sni == target
        )
        if not intercept:
            data.ignore_connection = True

    async def server_connect(self, data: server_hooks.ServerConnectionHookData):
        host, port = data.server.address
        allowed = {(a["host"], int(a["port"])) for a in self.policy.get().get("allow_private", [])}
        if (host, port) in allowed:
            return
        try:
            infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
        except OSError as e:
            data.server.error = f"cannot resolve {host}: {e}"
            return
        for info in infos:
            if blocked_address(str(info[4][0])):
                data.server.error = f"outpost: {host} is a private address"
                return

    # --- GitHub credentials ------------------------------------------------

    def is_github(self, flow) -> bool:
        return flow.request.host in policy.GITHUB_HOSTS and flow.request.scheme == "https"

    def task_cfg(self, task: str):
        p = self.policy.get()
        t = p["tasks"].get(task) or {}
        return t, p["tokens"].get(t.get("user", ""), "")

    def judge_rest(self, flow, task):
        t, token = self.task_cfg(task)
        repos = {r.lower() for r in t.get("repos", [])}
        path = flow.request.path.split("?")[0]
        method = flow.request.method
        if path.endswith("/info/refs") and policy.info_refs_service(flow.request.path.partition("?")[2]) == "git-receive-pack":
            method = "POST"  # judge a push's first request as the push
        allow, reason = policy.decide_rest(method, flow.request.host, path, repos, self.repo_by_id_sync(token))
        self.apply(flow, task, t, token, allow, reason)

    def repo_by_id_sync(self, token):
        def look(rid: int):
            if rid not in self.repo_ids:
                try:
                    req = urllib.request.Request(f"https://api.github.com/repositories/{rid}", headers={"Authorization": f"token {token}", "Accept": "application/vnd.github+json"})
                    with urllib.request.urlopen(req, timeout=10) as r:
                        self.repo_ids[rid] = json.load(r).get("full_name")
                except Exception:
                    self.repo_ids[rid] = None
            return self.repo_ids[rid]
        return look

    def responseheaders(self, flow: http.HTTPFlow):
        if self.is_github(flow):
            flow.response.stream = True

    async def request(self, flow: http.HTTPFlow):
        task = flow.metadata.get("outpost_task")
        if not task or flow.response or not self.is_github(flow):
            return
        if not (flow.request.host == "api.github.com" and flow.request.path.split("?")[0] == "/graphql"):
            return
        t, token = self.task_cfg(task)
        repos = {r.lower() for r in t.get("repos", [])}
        try:
            body = json.loads(flow.request.get_text(strict=False) or "null")
        except ValueError:
            body = None
        verdict, detail = policy.graphql_plan(body)
        if verdict == "read":
            return self.apply(flow, task, t, token, True, "read")
        if verdict == "deny":
            return self.apply(flow, task, t, token, False, detail)
        ids, names = detail
        for n in names:
            if n.lower() not in repos:
                return self.apply(flow, task, t, token, False, f"the mutation names {n}, which is not one of this task's repos")
        nodes = await asyncio.to_thread(self.resolve_nodes, sorted(ids), token)
        if nodes is None:
            return self.apply(flow, task, t, token, False, "could not look up the ids in the mutation")
        allow, reason = policy.judge_nodes(nodes, repos)
        self.apply(flow, task, t, token, allow, reason)

    def resolve_nodes(self, ids, token):
        todo = [i for i in ids if i not in self.node_cache]
        if todo:
            req = urllib.request.Request(
                "https://api.github.com/graphql",
                data=json.dumps({"query": policy.NODES_QUERY, "variables": {"ids": todo}}).encode(),
                headers={"Authorization": f"bearer {token}", "Content-Type": "application/json"},
            )
            try:
                with urllib.request.urlopen(req, timeout=15) as r:
                    answer = json.load(r)
            except Exception as e:
                ctx.log.warn(f"node lookup: {e}")
                return None
            got = (answer.get("data") or {}).get("nodes")
            if not isinstance(got, list) or len(got) != len(todo):
                return None
            for i, n in zip(todo, got):
                self.node_cache[i] = n
        return [self.node_cache[i] for i in ids]

    def apply(self, flow, task, t, token, allow, reason):
        enforce = t.get("mode") == "enforce"
        if not allow:
            self.event(task, "withheld" if enforce else "would_withhold", method=flow.request.method, host=flow.request.host, path=flow.request.path.split("?")[0], reason=reason)
            if enforce:
                return self.refuse(flow, reason)
        sent = flow.request.headers.get("Authorization")
        if not sent or not token:
            return  # only replace a credential the client meant to send
        scheme = sent.split(" ", 1)[0]
        if scheme.lower() == "basic":
            flow.request.headers["Authorization"] = "Basic " + base64.b64encode(f"x-access-token:{token}".encode()).decode()
        else:
            flow.request.headers["Authorization"] = f"{scheme} {token}"

    def refuse(self, flow, reason):
        msg = f"outpost credential_withheld: {reason}. Outpost only gives this task GitHub write access to its own repos."
        path = flow.request.path.split("?")[0]
        if flow.request.host == "github.com" and path.endswith("/info/refs"):
            line = f"ERR {msg}\n".encode()
            body = f"{len(line) + 4:04x}".encode() + line
            svc = policy.info_refs_service(flow.request.path.split("?", 1)[1] if "?" in flow.request.path else "") or "git-receive-pack"
            flow.response = http.Response.make(200, body, {"Content-Type": f"application/x-{svc}-advertisement", "Cache-Control": "no-cache"})
        elif flow.request.host == "github.com":
            flow.response = http.Response.make(403, (msg + "\n").encode(), {"Content-Type": "text/plain"})
        else:
            flow.response = http.Response.make(403, json.dumps({"message": msg, "documentation_url": ""}).encode(), {"Content-Type": "application/json"})


addons = [Outpost()]
