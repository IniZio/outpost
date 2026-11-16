"""Which GitHub requests get the requester's token.

Reads are allowed on any repo. Writes are allowed only on the task's repos,
and only through the paths and GraphQL mutations listed here.
"""

import re
from urllib.parse import unquote

GITHUB_HOSTS = {"github.com", "api.github.com", "uploads.github.com"}
READ_METHODS = {"GET", "HEAD", "OPTIONS"}

GIT_PATH = re.compile(r"^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack|info/lfs/.*)$")
API_REPO_PATH = re.compile(r"^/repos/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)(/.*)?$")
API_REPO_ID_PATH = re.compile(r"^/repositories/(\d+)(/.*)?$")

# Writes under a task repo that change who can reach the repo or what runs
# with its secrets. An agent doing dependency work never needs these.
SENSITIVE_REST = re.compile(
    r"^(|/collaborators.*|/hooks.*|/keys.*|/transfer|/actions/secrets.*|/actions/variables.*|/actions/permissions.*"
    r"|/environments.*|/rulesets.*|/branches/[^/]+/protection.*|/invitations.*|/teams.*|/autolinks.*|/pages.*"
    r"|/dependabot/secrets.*|/codespaces/secrets.*|/vulnerability-alerts|/automated-security-fixes|/private-vulnerability-reporting)$"
)

ALLOWED_MUTATIONS = {
    "createPullRequest", "updatePullRequest", "closePullRequest", "mergePullRequest",
    "markPullRequestReadyForReview", "convertPullRequestToDraft",
    "enablePullRequestAutoMerge", "disablePullRequestAutoMerge", "updatePullRequestBranch",
    "addPullRequestReview", "submitPullRequestReview", "addPullRequestReviewThread",
    "addPullRequestReviewComment", "resolveReviewThread", "unresolveReviewThread",
    "requestReviews", "requestReviewsByLogin",
    "addComment", "updateIssueComment",
    "addLabelsToLabelable", "removeLabelsFromLabelable",
    "addAssigneesToAssignable", "removeAssigneesFromAssignable", "replaceActorsForAssignable",
    "createIssue", "updateIssue", "closeIssue",
}

# Node types that name a person rather than a repo, such as a reviewer or assignee.
NEUTRAL_TYPES = {"User", "Bot", "Team", "Mannequin"}


def norm(owner: str, name: str) -> str:
    if name.endswith(".git"):
        name = name[:-4]
    return f"{owner}/{name}".lower()


def decide_rest(method: str, host: str, path: str, repos: set[str], repo_by_id=None):
    """Return (allow, reason) for a non-GraphQL request. `path` has no query string.

    `repos` holds the task's repos, lower-cased. `repo_by_id(id)` returns the
    full name for a /repositories/<id> path, or None.
    """
    method = method.upper()
    path = unquote(path)
    if "/../" in path or path.endswith("/..") or "/./" in path or "//" in path or "\\" in path:
        if method in READ_METHODS:
            return True, "read"
        return False, "unusual path"

    if host == "github.com":
        m = GIT_PATH.match(path)
        if m:
            repo = norm(m.group(1), m.group(2))
            if repo in repos:
                return True, f"task repo {repo}"
            kind = m.group(3)
            if kind == "git-upload-pack" or (kind == "info/refs" and method in READ_METHODS):
                return True, "read"
            if kind.startswith("info/lfs") and method in READ_METHODS:
                return True, "read"
            return False, f"git write to {repo}, which is not one of this task's repos"
        if method in READ_METHODS:
            return True, "read"
        return False, f"{method} {path} on github.com"

    if host in ("api.github.com", "uploads.github.com"):
        if method in READ_METHODS:
            return True, "read"
        repo, rest = None, ""
        m = API_REPO_PATH.match(path)
        if m:
            repo, rest = norm(m.group(1), m.group(2)), m.group(3) or ""
        else:
            m = API_REPO_ID_PATH.match(path)
            if m and repo_by_id:
                full = repo_by_id(int(m.group(1)))
                repo, rest = (full.lower() if full else None), m.group(2) or ""
        if repo is None:
            return False, f"{method} {path} is not under a repo"
        if repo not in repos:
            return False, f"{method} on {repo}, which is not one of this task's repos"
        if SENSITIVE_REST.match(rest):
            return False, f"{method} {rest or '/'} changes the repo's access or settings"
        return True, f"task repo {repo}"

    return False, f"unknown host {host}"


def info_refs_service(query: str) -> str | None:
    m = re.search(r"(?:^|&)service=([a-z-]+)", query or "")
    return m.group(1) if m else None


ID_KEY = re.compile(r"(^id$|Id$|Ids$)")
REPO_NAME_KEY = re.compile(r"(?i)nameWithOwner$")


def graphql_plan(body: dict):
    """Classify a GraphQL request.

    Returns ("read", None), ("deny", reason) or ("check", (ids, repo_names)),
    where every id must resolve to a node in a task repo (or a person) and
    every repo name must be a task repo.
    """
    from graphql import parse, GraphQLSyntaxError
    from graphql.language import ast

    if not isinstance(body, dict) or not isinstance(body.get("query"), str):
        return "deny", "not a single GraphQL request"
    try:
        doc = parse(body["query"])
    except GraphQLSyntaxError as e:
        return "deny", f"GraphQL that doesn't parse: {e.message}"
    ops = [d for d in doc.definitions if isinstance(d, ast.OperationDefinitionNode)]
    name = body.get("operationName")
    if name:
        ops = [o for o in ops if o.name and o.name.value == name]
    if len(ops) != 1:
        return "deny", "the GraphQL request doesn't name exactly one operation"
    op = ops[0]
    kind = op.operation.value
    if kind == "query":
        return "read", None
    if kind != "mutation":
        return "deny", f"GraphQL {kind}"

    variables = body.get("variables") or {}
    if not isinstance(variables, dict):
        return "deny", "GraphQL variables aren't an object"
    ids: set[str] = set()
    names: set[str] = set()

    def take(key, value):
        if isinstance(value, str):
            if ID_KEY.search(key):
                ids.add(value)
            elif REPO_NAME_KEY.search(key):
                names.add(value)
        elif isinstance(value, list):
            for v in value:
                take(key, v)
        elif isinstance(value, dict):
            for k, v in value.items():
                take(k, v)

    def literal(key, node):
        if isinstance(node, ast.VariableNode):
            take(key, variables.get(node.name.value))
        elif isinstance(node, ast.StringValueNode):
            take(key, node.value)
        elif isinstance(node, ast.ListValueNode):
            for v in node.values:
                literal(key, v)
        elif isinstance(node, ast.ObjectValueNode):
            for f in node.fields:
                literal(f.name.value, f.value)

    for sel in op.selection_set.selections:
        if not isinstance(sel, ast.FieldNode):
            return "deny", "fragments at the top of a mutation"
        field = sel.name.value
        if field == "__typename":
            continue
        if field not in ALLOWED_MUTATIONS:
            return "deny", f"the GraphQL mutation {field} is not allowed"
        before = len(ids) + len(names)
        for arg in sel.arguments or []:
            literal(arg.name.value, arg.value)
        if len(ids) + len(names) == before:
            return "deny", f"{field} names no repo, pull request or issue"
    return "check", (ids, names)


NODES_QUERY = """query($ids: [ID!]!) { nodes(ids: $ids) { __typename
  ... on Repository { nameWithOwner }
  ... on PullRequest { repository { nameWithOwner } }
  ... on Issue { repository { nameWithOwner } }
  ... on Label { repository { nameWithOwner } }
  ... on Milestone { repository { nameWithOwner } }
  ... on IssueComment { repository { nameWithOwner } }
  ... on PullRequestReview { repository { nameWithOwner } }
  ... on PullRequestReviewComment { repository { nameWithOwner } }
  ... on PullRequestReviewThread { repository { nameWithOwner } }
  ... on Ref { repository { nameWithOwner } }
  ... on Commit { repository { nameWithOwner } }
} }"""


def judge_nodes(nodes: list, repos: set[str]):
    """Given the `nodes` answer for a mutation's ids, return (allow, reason)."""
    scoped = 0
    for n in nodes:
        if n is None:
            return False, "an id in the mutation doesn't resolve"
        t = n.get("__typename")
        if t in NEUTRAL_TYPES:
            continue
        full = n.get("nameWithOwner") or (n.get("repository") or {}).get("nameWithOwner")
        if not full:
            return False, f"a {t} id in the mutation"
        if full.lower() not in repos:
            return False, f"the mutation touches {full}, which is not one of this task's repos"
        scoped += 1
    if not scoped:
        return False, "the mutation names no repo, pull request or issue"
    return True, "task repo"
