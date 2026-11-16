"""Run with: docker run --rm --entrypoint python3 <proxy image> test_grants.py"""

import base64

from grants import Grants, github_token, intercepted_hosts, parse_basic, rule_for, swap

GRANT = {
    "task": 7, "owner": "ana", "repos": ["acme/app"],
    "rules": [
        {"hosts": ["github.com", "api.github.com"], "header": "Authorization", "placeholder": "outpost-proxy", "value": "gho_x", "github": True},
        {"hosts": ["api.anthropic.com"], "header": "Authorization", "placeholder": "PH", "value": "sk-real", "github": False},
    ],
}


def basic(s):
    return "Basic " + base64.b64encode(s.encode()).decode()


assert parse_basic(basic("t7g1:mac")) == ("t7g1", "mac")
assert parse_basic(basic("t7g1:a:b")) == ("t7g1", "a:b")
for bad in [None, "", "Bearer x", basic("nocolon"), basic(":mac"), basic("t7g1:"), "Basic !!!"]:
    assert parse_basic(bad) is None, bad

assert rule_for(GRANT, "api.anthropic.com")["value"] == "sk-real"
assert rule_for(GRANT, "example.com") is None
assert rule_for(None, "github.com") is None
assert github_token(GRANT) == "gho_x"
assert github_token({"rules": []}) == ""
assert intercepted_hosts(GRANT) == {"github.com", "api.github.com", "api.anthropic.com"}

h = {"Authorization": "Bearer PH"}
assert swap(h, GRANT["rules"][1]) and h["Authorization"] == "Bearer sk-real"
h = {"Authorization": "Bearer something-else"}
assert not swap(h, GRANT["rules"][1]) and h["Authorization"] == "Bearer something-else"
h = {}
assert not swap(h, GRANT["rules"][1]) and h == {}

now = [100.0]
calls = []


def fetch(user, password):
    calls.append(user)
    if user == "down":
        raise OSError("server down")
    return GRANT if password == "ok" else None


g = Grants(fetch, clock=lambda: now[0])
assert g.get("t7g1", "ok") is GRANT and g.get("t7g1", "ok") is GRANT and calls == ["t7g1"], "a grant is cached"
now[0] += 31
assert g.get("t7g1", "ok") is GRANT and calls == ["t7g1", "t7g1"], "and fetched again after the TTL"
assert g.get("t7g1", "bad") is None and g.get("t7g1", "bad") is None and calls.count("t7g1") == 3, "a refusal is cached briefly"
now[0] += 6
assert g.get("t7g1", "bad") is None and calls.count("t7g1") == 4, "and asked again soon"
assert g.peek("t7g1", "ok") is GRANT and g.peek("t9g1", "ok") is None
assert g.get("down", "x") is None, "a server that cannot answer gives no grant to a new login"
g.cache[("down", "y")] = (0, GRANT)
assert g.get("down", "y") is GRANT, "but keeps an expired grant it had"

print("grant tests passed")
