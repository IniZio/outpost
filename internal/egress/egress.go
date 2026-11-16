package egress

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/secrets"
)

// Rule is one credential host rule. A request to one of Hosts whose Header carries Placeholder
// leaves the proxy with the owner's secret Secret in its place. GitHub rules also get the
// proxy's GitHub write policy: writes only to the task's repos. Env names the box variables that
// hold the placeholder; an agent's own token comes from its Launch instead.
type Rule struct {
	Hosts       []string
	Header      string
	Placeholder string
	Secret      secrets.Name
	Env         []string
	GitHub      bool
}

// GitHubPlaceholder is what gh and git in a box send as their token.
const GitHubPlaceholder = "outpost-proxy"

// GitHub is the rule for the owner's gh token, as the prototype uses it.
func GitHub() Rule {
	return Rule{
		Hosts: []string{"github.com", "api.github.com", "uploads.github.com"}, Header: "Authorization",
		Placeholder: GitHubPlaceholder, Secret: "github", Env: []string{"GH_TOKEN", "GITHUB_TOKEN"}, GitHub: true,
	}
}

// Bearer is the rule for an API that takes the secret as a bearer token, such as Claude Code's
// OAuth token on api.anthropic.com.
func Bearer(host, placeholder string, secret secrets.Name) Rule {
	return Rule{Hosts: []string{host}, Header: "Authorization", Placeholder: placeholder, Secret: secret}
}

// Paths inside every box.
const (
	CAPath     = "/usr/local/share/ca-certificates/outpost-proxy.crt" // box-trust installs it here
	bundlePath = "/etc/ssl/certs/ca-certificates.crt"
	noProxy    = "localhost,127.0.0.1,0.0.0.0,::1,10.0.2.2,.local,.internal"
)

// Profile is who a GitHub token belongs to.
type Profile struct {
	Login domain.Login
	ID    int64
	Name  string // the display name; may be empty
}

// Tasks loads a task. ports.Store does.
type Tasks interface {
	Load(ctx context.Context, id domain.TaskID) (domain.Task, error)
}

// Vault reads an owner's secret. *secrets.Vault does.
type Vault interface {
	Get(ctx context.Context, owner domain.Login, name secrets.Name) (string, error)
}

// Config wires the egress side of the server.
type Config struct {
	Key        secrets.Key
	Vault      Vault
	Tasks      Tasks
	ProxyURL   string // what boxes dial, such as http://ob-proxy:8080
	CAFile     string // the proxy's public CA, which the proxy writes on its first start
	ProxyToken string // the bearer the proxy presents to the grant endpoint
	Rules      []Rule
	// Profile reads who a GitHub token belongs to. When set, every box gets the owner's git
	// identity in its env. Egress asks once per owner and remembers the answer.
	Profile func(ctx context.Context, token string) (Profile, error)
}

// Egress implements ports.Secrets and answers grants.
type Egress struct {
	cfg   Config
	sign  signer
	proxy *url.URL
	mu    sync.Mutex
	ident map[domain.Login]map[string]string
}

var _ ports.Secrets = (*Egress)(nil)

// New checks cfg.
func New(cfg Config) (*Egress, error) {
	u, err := url.Parse(cfg.ProxyURL)
	switch {
	case err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" || u.User != nil:
		return nil, fmt.Errorf("egress: proxy URL %q must be http://host:port", cfg.ProxyURL)
	case cfg.Vault == nil || cfg.Tasks == nil || cfg.CAFile == "":
		return nil, errors.New("egress: Vault, Tasks and CAFile are required")
	case len(cfg.ProxyToken) < 16:
		return nil, errors.New("egress: the proxy token needs at least 16 characters")
	}
	return &Egress{cfg: cfg, sign: newSigner(cfg.Key), proxy: u, ident: map[domain.Login]map[string]string{}}, nil
}

// Credentials is the box's env for target t: the proxy login of t's box generation in every proxy
// variable, the CA paths, and each rule's placeholder. It fails when the owner lacks a rule's
// secret, so a start fails with a message saying which, before any box work.
func (e *Egress) Credentials(ctx context.Context, t ports.Target, owner domain.Login, _ []domain.RepoName) (ports.Creds, error) {
	for _, r := range e.cfg.Rules {
		if _, err := e.cfg.Vault.Get(ctx, owner, r.Secret); err != nil {
			return ports.Creds{}, fmt.Errorf("egress: secret %s of %s: %w", r.Secret, owner, err)
		}
	}
	ca, err := os.ReadFile(e.cfg.CAFile)
	if err != nil {
		return ports.Creds{}, fmt.Errorf("egress: the proxy CA: %w", err)
	}
	env := e.env(Login{Task: t.Task, Gen: t.Gen})
	id, err := e.identity(ctx, owner)
	if err != nil {
		return ports.Creds{}, err
	}
	maps.Copy(env, id)
	return ports.Creds{Env: env, CA: ca}, nil
}

// identity is the git author and committer env of owner: their display name (or login) and their
// noreply address. The first start of an owner asks GitHub; later ones reuse the answer.
func (e *Egress) identity(ctx context.Context, owner domain.Login) (map[string]string, error) {
	if e.cfg.Profile == nil {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.ident[owner]; ok {
		return m, nil
	}
	token, err := e.cfg.Vault.Get(ctx, owner, GitHub().Secret)
	if err != nil {
		return nil, fmt.Errorf("egress: github token of %s: %w", owner, err)
	}
	p, err := e.cfg.Profile(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("egress: git identity of %s: %w", owner, err)
	}
	name := p.Name
	if name == "" {
		name = string(p.Login)
	}
	email := fmt.Sprintf("%d+%s@users.noreply.github.com", p.ID, p.Login)
	m := map[string]string{
		"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email,
	}
	e.ident[owner] = m
	return m, nil
}

func (e *Egress) env(l Login) map[string]string {
	u := *e.proxy
	u.User = url.UserPassword(l.User(), e.sign.password(l))
	proxy := u.String()
	env := map[string]string{
		"NO_PROXY": noProxy, "no_proxy": noProxy,
		"NODE_EXTRA_CA_CERTS": CAPath, "REQUESTS_CA_BUNDLE": bundlePath,
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "YARN_HTTPS_PROXY", "YARN_HTTP_PROXY"} {
		env[k] = proxy
	}
	for _, r := range e.cfg.Rules {
		for _, k := range r.Env {
			env[k] = r.Placeholder
		}
	}
	return env
}
