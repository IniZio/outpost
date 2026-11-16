// Package github is Outpost's GitHub client: who a token belongs to, and later repo read access for
// sharing checks.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/IniZio/outpost/internal/domain"
)

// ErrBadToken is a token GitHub does not accept.
var ErrBadToken = errors.New("github: token not accepted")

// Client calls the GitHub REST API. The zero value talks to api.github.com.
type Client struct {
	HTTP *http.Client
	API  string // base URL; empty is https://api.github.com
}

// Profile is who a token belongs to, as git commits name them.
type Profile struct {
	Login domain.Login
	ID    int64
	Name  string // the display name; empty when unset
}

// User returns the login a token belongs to.
func (c Client) User(ctx context.Context, token string) (domain.Login, error) {
	p, err := c.Profile(ctx, token)
	return p.Login, err
}

// Profile returns the login, numeric id and display name a token belongs to.
func (c Client) Profile(ctx context.Context, token string) (Profile, error) {
	api, hc := c.API, c.HTTP
	if api == "" {
		api = "https://api.github.com"
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/user", nil)
	if err != nil {
		return Profile{}, fmt.Errorf("github: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := hc.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("github: user: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return Profile{}, ErrBadToken
	}
	if res.StatusCode != http.StatusOK {
		return Profile{}, fmt.Errorf("github: user: %s", res.Status)
	}
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil || u.Login == "" || u.ID == 0 {
		return Profile{}, fmt.Errorf("github: user: unreadable answer: %w", errors.Join(err, errors.New("no login")))
	}
	return Profile{Login: domain.Login(u.Login), ID: u.ID, Name: u.Name}, nil
}
