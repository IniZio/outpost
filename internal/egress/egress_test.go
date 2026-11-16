package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
	"github.com/IniZio/outpost/internal/secrets"
)

const (
	claudePlaceholder = "outpost-placeholder-claude-oauth-token"
	proxyToken        = "proxy-token-0123456789"
)

type tasks map[domain.TaskID]domain.Task

func (m tasks) Load(_ context.Context, id domain.TaskID) (domain.Task, error) {
	t, ok := m[id]
	if !ok {
		return domain.Task{}, ports.ErrNotFound
	}
	return t, nil
}

type world struct {
	e     *Egress
	vault *secrets.Vault
	tasks tasks
}

func newWorld(t *testing.T) *world {
	t.Helper()
	k, err := secrets.ParseKey(secrets.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.NewVault(k, portstest.NewSecretRows(), domain.NewManualClock(time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = vault.Put(ctx, "ana", "github", "gho_real")
	_ = vault.Put(ctx, "ana", "claude", "sk-ant-oat01-real")
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &world{vault: vault, tasks: tasks{
		1: {ID: 1, Owner: "ana", BoxGen: 2, Spec: domain.Spec{Repo: "acme/app", Also: []domain.RepoName{"acme/lib"}}, Life: domain.Starting{}},
	}}
	w.e, err = New(Config{
		Key: k, Vault: vault, Tasks: w.tasks, ProxyURL: "http://ob-proxy:8080", CAFile: ca, ProxyToken: proxyToken,
		Rules: []Rule{GitHub(), Bearer("api.anthropic.com", claudePlaceholder, "claude")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func target(task domain.TaskID, gen domain.BoxGen) ports.Target {
	return ports.Target{Task: task, Run: 1, Gen: gen}
}

// login reads the proxy login back out of a box's env, as the box's tools would send it.
func login(t *testing.T, c ports.Creds) GrantRequest {
	t.Helper()
	u, err := url.Parse(c.Env["HTTPS_PROXY"])
	if err != nil || u.User == nil {
		t.Fatalf("HTTPS_PROXY %q", c.Env["HTTPS_PROXY"])
	}
	pass, _ := u.User.Password()
	return GrantRequest{User: u.User.Username(), Password: pass}
}

func TestCredentialsHoldOnlyPlaceholdersAndTheLogin(t *testing.T) {
	w := newWorld(t)
	c, err := w.e.Credentials(context.Background(), target(1, 2), "ana", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := json.Marshal(c.Env); bytes.Contains(env, []byte("gho_real")) || bytes.Contains(env, []byte("sk-ant")) {
		t.Fatal("the box env holds a real secret")
	}
	if c.Env["GH_TOKEN"] != GitHubPlaceholder || c.Env["GITHUB_TOKEN"] != GitHubPlaceholder || string(c.CA) != "PEM" {
		t.Fatalf("placeholders and CA: %v %q", c.Env, c.CA)
	}
	if c.Env["NODE_EXTRA_CA_CERTS"] != CAPath || c.Env["NO_PROXY"] == "" || c.Env["https_proxy"] != c.Env["HTTPS_PROXY"] {
		t.Fatalf("proxy env: %v", c.Env)
	}
	if q := login(t, c); q.User != "t1g2" || !strings.HasPrefix(c.Env["HTTP_PROXY"], "http://t1g2:") {
		t.Fatalf("the login names the task and box generation: %+v", q)
	}
}

func TestCredentialsNeedEveryRuleSecretAndTheCA(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.e.Credentials(ctx, target(1, 2), "bo", nil); !errors.Is(err, ports.ErrNotFound) || !strings.Contains(err.Error(), "github") {
		t.Fatalf("an owner without the github secret cannot start: %v", err)
	}
	w.e.cfg.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := w.e.Credentials(ctx, target(1, 2), "ana", nil); err == nil {
		t.Fatal("no CA, no credentials")
	}
}

func TestAGrantCarriesTheReposAndTheOwnersSecrets(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	c, _ := w.e.Credentials(ctx, target(1, 2), "ana", nil)
	g, err := w.e.Grant(ctx, login(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if g.Task != 1 || g.Owner != "ana" || !slices.Equal(g.Repos, []domain.RepoName{"acme/app", "acme/lib"}) || len(g.Rules) != 2 {
		t.Fatalf("grant %+v", g)
	}
	gh, claude := g.Rules[0], g.Rules[1]
	if !gh.GitHub || gh.Value != "gho_real" || gh.Placeholder != GitHubPlaceholder || !slices.Contains(gh.Hosts, "api.github.com") {
		t.Fatalf("github rule %+v", gh)
	}
	if claude.GitHub || claude.Value != "sk-ant-oat01-real" || claude.Header != "Authorization" || claude.Placeholder != claudePlaceholder {
		t.Fatalf("claude rule %+v", claude)
	}
}

func TestAGrantNeedsTheCurrentLiveBoxGeneration(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	login1 := func(gen domain.BoxGen) GrantRequest {
		c, _ := w.e.Credentials(ctx, target(1, gen), "ana", nil)
		return login(t, c)
	}
	if _, err := w.e.Grant(ctx, login1(1)); !errors.Is(err, ErrGone) {
		t.Fatalf("an older box generation has no grant: %v", err)
	}
	tk := w.tasks[1]
	for _, life := range []domain.Life{domain.Queued{}, domain.Ended{ReleasedAt: time.Unix(1, 0)}} {
		tk.Life = life
		w.tasks[1] = tk
		if _, err := w.e.Grant(ctx, login1(2)); !errors.Is(err, ErrGone) {
			t.Fatalf("%T: %v", life, err)
		}
	}
	tk.Life = domain.Ended{BoxKept: true}
	w.tasks[1] = tk
	if _, err := w.e.Grant(ctx, login1(2)); err != nil {
		t.Fatalf("an ended task that keeps its box still has its grant: %v", err)
	}
	if _, err := w.e.Grant(ctx, GrantRequest{User: "t9g1", Password: w.e.sign.password(Login{Task: 9, Gen: 1})}); !errors.Is(err, ErrGone) {
		t.Fatalf("an unknown task has no grant: %v", err)
	}
}

func TestABadLoginHasNoGrant(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	c, _ := w.e.Credentials(ctx, target(1, 2), "ana", nil)
	good := login(t, c)
	for _, q := range []GrantRequest{
		{User: good.User, Password: good.Password + "x"},
		{User: "t1g3", Password: good.Password},
		{User: "t01g2", Password: good.Password},
		{User: "t1g2x", Password: good.Password},
		{User: "x", Password: good.Password},
		{User: "t0g1", Password: w.e.sign.password(Login{Task: 0, Gen: 1})},
	} {
		if _, err := w.e.Grant(ctx, q); !errors.Is(err, ErrBadLogin) {
			t.Errorf("%q: %v", q.User, err)
		}
	}
}

func TestParseUserRoundTrips(t *testing.T) {
	l := Login{Task: 42, Gen: 7}
	if got, err := ParseUser(l.User()); err != nil || got != l || l.User() != "t42g7" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	w := newWorld(t)
	good := w.e.cfg
	for name, mut := range map[string]func(*Config){
		"https proxy":  func(c *Config) { c.ProxyURL = "https://p:8080" },
		"no port":      func(c *Config) { c.ProxyURL = "http://p" },
		"user in url":  func(c *Config) { c.ProxyURL = "http://u:p@p:8080" },
		"short token":  func(c *Config) { c.ProxyToken = "short" },
		"no vault":     func(c *Config) { c.Vault = nil },
		"no ca file":   func(c *Config) { c.CAFile = "" },
		"bad url text": func(c *Config) { c.ProxyURL = "::" },
	} {
		c := good
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}

func post(t *testing.T, h http.Handler, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/internal/egress/grant", bytes.NewReader(b))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestTheGrantHandler(t *testing.T) {
	w := newWorld(t)
	h := w.e.GrantHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c, _ := w.e.Credentials(context.Background(), target(1, 2), "ana", nil)
	good := login(t, c)
	rec := post(t, h, proxyToken, good)
	var g Grant
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &g) != nil || g.Task != 1 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for name, want := range map[string]struct {
		bearer string
		body   any
		code   int
	}{
		"no bearer":  {"", good, http.StatusUnauthorized},
		"bad bearer": {proxyToken + "x", good, http.StatusUnauthorized},
		"bad body":   {proxyToken, "nope", http.StatusBadRequest},
		"bad login":  {proxyToken, GrantRequest{User: good.User, Password: "x"}, http.StatusForbidden},
		"gone box":   {proxyToken, GrantRequest{User: "t9g1", Password: w.e.sign.password(Login{Task: 9, Gen: 1})}, http.StatusGone},
	} {
		if rec := post(t, h, want.bearer, want.body); rec.Code != want.code || strings.Contains(rec.Body.String(), "gho_real") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	w.e.cfg.Tasks = failing{}
	if rec := post(t, h, proxyToken, good); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Errorf("a store failure is a bare 500: %d %s", rec.Code, rec.Body)
	}
}

type failing struct{}

func (failing) Load(context.Context, domain.TaskID) (domain.Task, error) {
	return domain.Task{}, errors.New("db down")
}

// The proxy addon (images/proxy/grants.py) reads these keys; renaming one breaks it silently.
func TestTheGrantWireShapeIsWhatTheProxyReads(t *testing.T) {
	w := newWorld(t)
	c, _ := w.e.Credentials(context.Background(), target(1, 2), "ana", nil)
	rec := post(t, w.e.GrantHandler(slog.New(slog.NewTextHandler(io.Discard, nil))), proxyToken, map[string]string{
		"user": login(t, c).User, "password": login(t, c).Password,
	})
	var g struct {
		Task  int      `json:"task"`
		Repos []string `json:"repos"`
		Rules []map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil || g.Task != 1 || len(g.Repos) != 2 || len(g.Rules) != 2 {
		t.Fatalf("%v %s", err, rec.Body)
	}
	for _, k := range []string{"hosts", "header", "placeholder", "value", "github"} {
		if _, ok := g.Rules[0][k]; !ok {
			t.Errorf("a rule has no %q", k)
		}
	}
}

func TestBoxesCarryTheOwnersGitIdentity(t *testing.T) {
	w := newWorld(t)
	calls := 0
	w.e.cfg.Profile = func(_ context.Context, token string) (Profile, error) {
		calls++
		if token != "gho_real" {
			t.Fatalf("token %q", token)
		}
		return Profile{Login: "ana", ID: 42, Name: "Ana Lee"}, nil
	}
	ctx := context.Background()
	c, err := w.e.Credentials(ctx, target(1, 2), "ana", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "42+ana@users.noreply.github.com"
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		if c.Env[k] != want {
			t.Fatalf("%s = %q", k, c.Env[k])
		}
	}
	if c.Env["GIT_AUTHOR_NAME"] != "Ana Lee" || c.Env["GIT_COMMITTER_NAME"] != "Ana Lee" {
		t.Fatalf("names: %v", c.Env)
	}
	if _, err := w.e.Credentials(ctx, target(1, 3), "ana", nil); err != nil || calls != 1 {
		t.Fatalf("one GitHub call per owner: %d %v", calls, err)
	}
}

func TestGitIdentityFallsBackToTheLogin(t *testing.T) {
	w := newWorld(t)
	w.e.cfg.Profile = func(context.Context, string) (Profile, error) { return Profile{Login: "ana", ID: 42}, nil }
	c, err := w.e.Credentials(context.Background(), target(1, 2), "ana", nil)
	if err != nil || c.Env["GIT_AUTHOR_NAME"] != "ana" {
		t.Fatalf("no display name falls back to the login: %v %v", c.Env, err)
	}
}
