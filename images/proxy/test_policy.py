"""Run with: docker run --rm --entrypoint python3 <proxy image> test_policy.py"""

from policy import decide_rest, graphql_plan, judge_nodes

R = {"acme/app", "acme/lib"}


def ok(v, why=""):
    assert v[0] is True, (v, why)


def no(v, why=""):
    assert v[0] is False, (v, why)


# git
ok(decide_rest("GET", "github.com", "/other/x.git/info/refs", R))
ok(decide_rest("POST", "github.com", "/other/x.git/git-upload-pack", R))
no(decide_rest("POST", "github.com", "/other/x.git/git-receive-pack", R))
ok(decide_rest("POST", "github.com", "/Acme/App.git/git-receive-pack", R))
ok(decide_rest("POST", "github.com", "/acme/app/git-receive-pack", R))
no(decide_rest("POST", "github.com", "/acme/app%2F..%2Fother/git-receive-pack", R))
no(decide_rest("POST", "github.com", "/other/x.git/info/lfs/objects/batch", R))
ok(decide_rest("POST", "github.com", "/acme/app.git/info/lfs/objects/batch", R))

# REST
ok(decide_rest("GET", "api.github.com", "/repos/other/x/contents/a", R))
ok(decide_rest("GET", "api.github.com", "/user", R))
ok(decide_rest("POST", "api.github.com", "/repos/acme/app/pulls", R))
ok(decide_rest("PATCH", "api.github.com", "/repos/acme/app/dependabot/alerts/3", R))
ok(decide_rest("PUT", "api.github.com", "/repos/acme/app/pulls/4/merge", R))
no(decide_rest("POST", "api.github.com", "/repos/other/x/pulls", R))
no(decide_rest("DELETE", "api.github.com", "/repos/acme/app", R))
no(decide_rest("PATCH", "api.github.com", "/repos/acme/app", R))
no(decide_rest("PUT", "api.github.com", "/repos/acme/app/collaborators/eve", R))
no(decide_rest("POST", "api.github.com", "/repos/acme/app/keys", R))
no(decide_rest("PUT", "api.github.com", "/repos/acme/app/actions/secrets/X", R))
no(decide_rest("PUT", "api.github.com", "/repos/acme/app/branches/main/protection", R))
no(decide_rest("POST", "api.github.com", "/user/repos", R))
no(decide_rest("POST", "api.github.com", "/gists", R))
no(decide_rest("POST", "api.github.com", "/repos/acme/app%2f/pulls", R))
no(decide_rest("POST", "api.github.com", "/repos/acme/app%2F..%2F..%2Frepos%2Fother%2Fx/pulls", R))
ok(decide_rest("DELETE", "api.github.com", "/repos/acme/app/git/refs/heads%2Ffeature", R))
no(decide_rest("DELETE", "api.github.com", "/repos/%61cme/app/../../repos/other/x", R))
ok(decide_rest("POST", "api.github.com", "/repositories/7/issues", R, lambda i: "acme/lib"))
no(decide_rest("POST", "api.github.com", "/repositories/7/issues", R, lambda i: "other/x"))
no(decide_rest("POST", "api.github.com", "/repositories/7/issues", R, lambda i: None))
ok(decide_rest("POST", "uploads.github.com", "/repos/acme/app/releases/1/assets", R))
no(decide_rest("POST", "uploads.github.com", "/user-attachments/assets", R))

# GraphQL
assert graphql_plan({"query": "query { viewer { login } }"})[0] == "read"
assert graphql_plan({"query": "{ viewer { login } }"})[0] == "read"
assert graphql_plan({"query": "subscription { x }"})[0] == "deny"
assert graphql_plan([{"query": "{ a }"}])[0] == "deny"
assert graphql_plan({"query": "mutation { deleteRepository(input:{repositoryId:\"R1\"}) { clientMutationId } }"})[0] == "deny"
assert graphql_plan({"query": "mutation { createRepository(input:{name:\"x\", visibility:PUBLIC}) { clientMutationId } }"})[0] == "deny"
assert graphql_plan({"query": "mutation { reopenPullRequest(input:{pullRequestId:\"P\"}) { clientMutationId } }"})[0] == "deny"
assert graphql_plan({"query": "mutation($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { url } } }", "variables": {"input": {"title": "x"}}})[0] == "deny"
assert graphql_plan({"query": "mutation { addComment: deleteRepository(input:{repositoryId:\"R1\"}) { clientMutationId } }"})[0] == "deny"
v, (ids, names) = graphql_plan({
    "query": "mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id url } } }",
    "variables": {"input": {"repositoryId": "R_1", "headRepositoryId": "R_2", "baseRefName": "main", "headRefName": "x", "title": "t"}},
    "operationName": "PullRequestCreate",
})
assert v == "check" and ids == {"R_1", "R_2"} and not names, ids
v, (ids, _) = graphql_plan({"query": "mutation { addLabelsToLabelable(input:{labelableId:\"PR_1\", labelIds:[\"L_1\",\"L_2\"]}) { clientMutationId } }"})
assert ids == {"PR_1", "L_1", "L_2"}, ids
assert graphql_plan({"query": "mutation A { addComment(input:{subjectId:\"x\", body:\"b\"}) { clientMutationId } } mutation B { addComment(input:{subjectId:\"y\", body:\"b\"}) { clientMutationId } }"})[0] == "deny"

assert graphql_plan({"query": "mutation($x: ID = \"PR_other\") { closePullRequest(input:{pullRequestId:$x, clientMutationId:\"R_task\"}) { clientMutationId } }"})[0] == "deny"
assert graphql_plan({"query": "mutation($x: ID!) { closePullRequest(input:{pullRequestId:$x}) { clientMutationId } }"})[0] == "deny"
assert graphql_plan({"query": "mutation { closePullRequest(input:{clientMutationId:\"R_task\"}) { clientMutationId } }"})[0] == "deny"
assert graphql_plan({"query": "mutation { mergePullRequest(input:{labelIds:[\"L\"], clientMutationId:\"x\"}) { clientMutationId } }"})[0] == "deny"
v, (ids, _) = graphql_plan({"query": "mutation($x: ID!) { closePullRequest(input:{pullRequestId:$x, clientMutationId:\"R_task\"}) { clientMutationId } }", "variables": {"x": "PR_9"}})
assert v == "check" and ids == {"PR_9"}, ids
v, (ids, _) = graphql_plan({"query": "mutation { addPullRequestReviewComment(input:{pullRequestId:\"PR_1\", inReplyTo:\"C_other\", body:\"b\"}) { clientMutationId } }"})
assert ids == {"PR_1", "C_other"}, ids
no(decide_rest("PATCH", "api.github.com", "/repos/acme/app/", R))
no(decide_rest("PUT", "api.github.com", "/repos/acme/app/Collaborators/eve", R))
no(decide_rest("POST", "api.github.com", "/repos/acme/app/hooks/", R))

ok(judge_nodes([{"__typename": "Repository", "nameWithOwner": "Acme/App"}], R))
ok(judge_nodes([{"__typename": "PullRequest", "repository": {"nameWithOwner": "acme/app"}}, {"__typename": "User"}], R))
no(judge_nodes([{"__typename": "PullRequest", "repository": {"nameWithOwner": "other/x"}}], R))
no(judge_nodes([{"__typename": "User"}], R))
no(judge_nodes([None], R))
no(judge_nodes([{"__typename": "Organization"}], R))

print("policy tests passed")
