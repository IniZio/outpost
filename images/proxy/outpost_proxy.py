"""mitmproxy addon for Outpost boxes.

Each box reaches the proxy with its own login in HTTPS_PROXY. The proxy trades
the login for a grant from the Outpost server (grants.py). TLS is opened only
to the grant's credential hosts, so a placeholder can be swapped for the
owner's real secret: GitHub under the rules in policy.py, other hosts by the
generic header swap. Every other connection is a plain tunnel. Port 8081
never opens TLS; nested Docker builds use it, since they don't trust the
proxy's CA.

Events go to stdout as JSON lines.
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

import grants
import policy

GRANT_URL = os.environ["OUTPOST_GRANT_URL"]
PROXY_TOKEN = os.environ["OUTPOST_PROXY_TOKEN"]
TUNNEL_ONLY_PORT = 8081
SHARED = ipaddress.ip_network("100.64.0.0/10")


def blocked_address(ip: str) -> bool:
    a = ipaddress.ip_address(ip)
    return a.is_private or a.is_loopback or a.is_link_local or a.is_multicast or a.is_reserved or a.is_unspecified or a in SHARED


def refuse_login(flow):
    flow.response = http.Response.make(407, b"outpost: proxy credentials missing, wrong or expired\n", {"Proxy-Authenticate": 'Basic realm="outpost"'})


class Outpost:
    def __init__(self):
        self.grants = grants.Grants(lambda user, password: grants.fetch(GRANT_URL, PROXY_TOKEN, user, password))
        self.conn_login: dict[str, tuple[str, str]] = {}
        self.seen_hosts: set[tuple[int, str, int]] = set()
        self.repo_ids: dict[int, str | None] = {}
        self.node_cache: dict[str, dict | None] = {}

    # --- bookkeeping -------------------------------------------------------

    def event(self, grant, kind: str, **fields):
        print(json.dumps({"at": int(time.time() * 1000), "task": grant["task"], "kind": kind, **fields}), flush=True)

    async def grant_from_header(self, value: str | None):
        login = grants.parse_basic(value)
        if not login:
            return None, None
        grant = await asyncio.to_thread(self.grants.get, *login)
        return (login, grant) if grant else (None, None)

    async def grant_of(self, flow):
        login = self.conn_login.get(flow.client_conn.id)
        if not login:
            return None
        return await asyncio.to_thread(self.grants.get, *login)

    def note_host(self, grant, host: str, port: int):
        key = (grant["task"], host, port)
        if key not in self.seen_hosts:
            self.seen_hosts.add(key)
            self.event(grant, "host", host=host, port=port)

    def client_disconnected(self, client):
        self.conn_login.pop(client.id, None)

    # --- who may connect where --------------------------------------------

    async def http_connect(self, flow: http.HTTPFlow):
        login, grant = await self.grant_from_header(flow.request.headers.get("Proxy-Authorization"))
        if not grant:
            return refuse_login(flow)
        self.conn_login[flow.client_conn.id] = login
        self.note_host(grant, flow.request.host, flow.request.port)

    async def requestheaders(self, flow: http.HTTPFlow):
        if flow.request.method == "CONNECT":
            return
        grant = await self.grant_of(flow)
        if not grant:
            # Plain-HTTP proxying carries the login on every request.
            _, grant = await self.grant_from_header(flow.request.headers.get("Proxy-Authorization"))
            if not grant:
                return refuse_login(flow)
            self.note_host(grant, flow.request.host, flow.request.port)
        flow.request.headers.pop("Proxy-Authorization", None)
        flow.metadata["outpost_grant"] = grant
        if flow.request.scheme != "https":
            return
        if self.is_github(flow):
            if flow.request.host == "api.github.com" and flow.request.path.split("?")[0] == "/graphql":
                return  # judged in request(), once the body is in
            self.judge_rest(flow, grant)
        else:
            rule = grants.rule_for(grant, flow.request.host)
            if rule:
                grants.swap(flow.request.headers, rule)
        if not flow.response:
            flow.request.stream = True

    def tls_clienthello(self, data: tls.ClientHelloData):
        client = data.context.client
        server = data.context.server
        login = self.conn_login.get(client.id)
        grant = self.grants.peek(*login) if login else None
        target = server.address[0] if server.address else None
        intercept = (
            grant is not None
            and client.sockname[1] != TUNNEL_ONLY_PORT
            and target in grants.intercepted_hosts(grant)
            and data.client_hello.sni == target
        )
        if not intercept:
            data.ignore_connection = True

    async def server_connect(self, data: server_hooks.ServerConnectionHookData):
        host, port = data.server.address
        try:
            infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
        except OSError as e:
            data.server.error = f"cannot resolve {host}: {e}"
            return
        for info in infos:
            if blocked_address(str(info[4][0])):
                data.server.error = f"outpost: {host} is a private address"
                return

    def responseheaders(self, flow: http.HTTPFlow):
        flow.response.stream = True

    # --- GitHub credentials ------------------------------------------------

    def is_github(self, flow) -> bool:
        return flow.request.host in policy.GITHUB_HOSTS and flow.request.scheme == "https"

    def judge_rest(self, flow, grant):
        token = grants.github_token(grant)
        repos = {r.lower() for r in grant.get("repos", [])}
        path = flow.request.path.split("?")[0]
        method = flow.request.method
        if path.endswith("/info/refs") and policy.info_refs_service(flow.request.path.partition("?")[2]) == "git-receive-pack":
            method = "POST"  # judge a push's first request as the push
        allow, reason = policy.decide_rest(method, flow.request.host, path, repos, self.repo_by_id_sync(token))
        self.apply(flow, grant, token, allow, reason)

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

    async def request(self, flow: http.HTTPFlow):
        grant = flow.metadata.get("outpost_grant")
        if not grant or flow.response or not self.is_github(flow):
            return
        if not (flow.request.host == "api.github.com" and flow.request.path.split("?")[0] == "/graphql"):
            return
        token = grants.github_token(grant)
        repos = {r.lower() for r in grant.get("repos", [])}
        try:
            body = json.loads(flow.request.get_text(strict=False) or "null")
        except ValueError:
            body = None
        verdict, detail = policy.graphql_plan(body)
        if verdict == "read":
            return self.apply(flow, grant, token, True, "read")
        if verdict == "deny":
            return self.apply(flow, grant, token, False, detail)
        ids, names = detail
        for n in names:
            if n.lower() not in repos:
                return self.apply(flow, grant, token, False, f"the mutation names {n}, which is not one of this task's repos")
        nodes = await asyncio.to_thread(self.resolve_nodes, sorted(ids), token)
        if nodes is None:
            return self.apply(flow, grant, token, False, "could not look up the ids in the mutation")
        allow, reason = policy.judge_nodes(nodes, repos)
        self.apply(flow, grant, token, allow, reason)

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

    def apply(self, flow, grant, token, allow, reason):
        if not allow:
            self.event(grant, "withheld", method=flow.request.method, host=flow.request.host, path=flow.request.path.split("?")[0], reason=reason)
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
