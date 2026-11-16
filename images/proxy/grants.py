"""Grants from the Outpost server, and the generic header swap.

A box's proxy login is "t<task>g<gen>:<mac>". The proxy trades it for a grant
at OUTPOST_GRANT_URL, which names the task's repos and, for each credential
host rule, the placeholder to look for and the owner's secret to put in its
place. Grants are cached for TTL seconds, so a released box loses access
within that time.
"""

import base64
import json
import threading
import time
import urllib.error
import urllib.request

TTL = 30.0
REFUSED_TTL = 5.0


def parse_basic(value):
    """Returns (user, password) from a Basic Proxy-Authorization value, or None."""
    if not value or not value.lower().startswith("basic "):
        return None
    try:
        user, sep, password = base64.b64decode(value[6:], validate=True).decode().partition(":")
    except Exception:
        return None
    if not sep or not user or not password:
        return None
    return user, password


def rule_for(grant, host):
    for r in (grant or {}).get("rules", []):
        if host in r.get("hosts", []):
            return r
    return None


def github_token(grant):
    for r in (grant or {}).get("rules", []):
        if r.get("github"):
            return r.get("value", "")
    return ""


def intercepted_hosts(grant):
    return {h for r in (grant or {}).get("rules", []) for h in r.get("hosts", [])}


def swap(headers, rule):
    """Replaces the rule's placeholder in its header. Only a credential the client meant to send
    is replaced, so a request without the placeholder leaves the proxy as it came."""
    name = rule.get("header", "Authorization")
    sent = headers.get(name)
    placeholder = rule.get("placeholder", "")
    if not sent or not placeholder or placeholder not in sent:
        return False
    headers[name] = sent.replace(placeholder, rule.get("value", ""))
    return True


def fetch(url, token, user, password):
    """Asks the server for a grant. Returns the grant, or None when the login has none."""
    body = json.dumps({"user": user, "password": password}).encode()
    req = urllib.request.Request(url, data=body, method="POST", headers={
        "Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        if e.code in (403, 404, 410):
            return None
        raise


class Grants:
    """A cache of grants by login. fetch(user, password) returns a grant or None, and raises when
    the server cannot answer; a failed fetch keeps serving a grant it already had."""

    def __init__(self, fetch, clock=time.monotonic):
        self.fetch = fetch
        self.clock = clock
        self.lock = threading.Lock()
        self.cache = {}

    def get(self, user, password):
        key = (user, password)
        now = self.clock()
        with self.lock:
            hit = self.cache.get(key)
        if hit and hit[0] > now:
            return hit[1]
        try:
            grant = self.fetch(user, password)
        except Exception:
            return hit[1] if hit else None
        with self.lock:
            self.cache[key] = (now + (TTL if grant else REFUSED_TTL), grant)
        return grant

    def peek(self, user, password):
        with self.lock:
            hit = self.cache.get((user, password))
        return hit[1] if hit else None
