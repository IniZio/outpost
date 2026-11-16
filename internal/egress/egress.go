package egress

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"

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
}

// Egress implements ports.Secrets and answers grants.
type Egress struct {
	cfg   Config
	sign  signer
	proxy *url.URL
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
	return &Egress{cfg: cfg, sign: newSigner(cfg.Key), proxy: u}, nil
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
	return ports.Creds{Env: e.env(Login{Task: t.Task, Gen: t.Gen}), CA: ca}, nil
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
