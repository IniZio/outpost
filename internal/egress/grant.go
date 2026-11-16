package egress

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// GrantRequest is the proxy login a box presented, which the proxy forwards as is.
type GrantRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// Grant is what the proxy may do for one box generation. Values are real secrets: the grant
// crosses only the server-to-proxy hop.
type Grant struct {
	Task  domain.TaskID     `json:"task"`
	Owner domain.Login      `json:"owner"`
	Repos []domain.RepoName `json:"repos"`
	Rules []GrantRule       `json:"rules"`
}

// GrantRule is a Rule with the owner's secret in it.
type GrantRule struct {
	Hosts       []string `json:"hosts"`
	Header      string   `json:"header"`
	Placeholder string   `json:"placeholder"`
	Value       string   `json:"value"`
	GitHub      bool     `json:"github"`
}

// ErrGone is a login whose box generation is not the task's current one, or whose task released
// its box. The proxy answers the box 407.
var ErrGone = errors.New("egress: that box generation has no grant")

// Grant checks a login and returns its grant.
func (e *Egress) Grant(ctx context.Context, q GrantRequest) (Grant, error) {
	l, err := e.sign.check(q.User, q.Password)
	if err != nil {
		return Grant{}, err
	}
	t, err := e.cfg.Tasks.Load(ctx, l.Task)
	if errors.Is(err, ports.ErrNotFound) {
		return Grant{}, ErrGone
	}
	if err != nil {
		return Grant{}, fmt.Errorf("egress: task %d: %w", l.Task, err)
	}
	if w := t.BoxWant(); t.BoxGen != l.Gen || w == domain.BoxNone || w == domain.BoxReleased {
		return Grant{}, ErrGone
	}
	g := Grant{Task: t.ID, Owner: t.Owner, Repos: append([]domain.RepoName{t.Spec.Repo}, t.Spec.Also...)}
	for _, r := range e.cfg.Rules {
		v, err := e.cfg.Vault.Get(ctx, t.Owner, r.Secret)
		if err != nil {
			return Grant{}, fmt.Errorf("egress: secret %s of %s: %w", r.Secret, t.Owner, err)
		}
		g.Rules = append(g.Rules, GrantRule{Hosts: r.Hosts, Header: r.Header, Placeholder: r.Placeholder, Value: v, GitHub: r.GitHub})
	}
	return g, nil
}

// GrantPath is where the API mounts GrantHandler. It sits outside user auth: the proxy's bearer
// is its auth.
const GrantPath = "POST /internal/egress/grant"

// GrantHandler serves Grant to the proxy. A bad bearer is 401, a bad login 403, a gone box
// generation 410.
func (e *Egress) GrantHandler(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !e.bearerOK(r.Header.Get("Authorization")) {
			http.Error(w, "bad proxy token", http.StatusUnauthorized)
			return
		}
		var q GrantRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&q); err != nil {
			http.Error(w, "bad grant request", http.StatusBadRequest)
			return
		}
		g, err := e.Grant(r.Context(), q)
		if err != nil {
			code := grantStatus(err)
			if code == http.StatusInternalServerError {
				log.ErrorContext(r.Context(), "egress grant", "user", q.User, "err", err)
			}
			http.Error(w, http.StatusText(code), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g)
	})
}

func (e *Egress) bearerOK(h string) bool {
	tok, ok := strings.CutPrefix(h, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(tok), []byte(e.cfg.ProxyToken)) == 1
}

func grantStatus(err error) int {
	switch {
	case errors.Is(err, ErrBadLogin):
		return http.StatusForbidden
	case errors.Is(err, ErrGone):
		return http.StatusGone
	}
	return http.StatusInternalServerError
}
