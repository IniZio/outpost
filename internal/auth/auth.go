// Package auth turns tokens into logins. An API token, sent as a bearer, acts for its login. The
// CLI gets one by trading a GitHub token, or an admin mints one. A one-time web link sets a cookie
// that only watches, so the cookie never authorises a change and needs no CSRF guard.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// CookieName is the web session's cookie.
const CookieName = "outpost_session"

// Errors the API maps to 401 and 403.
var (
	ErrNoSession  = errors.New("auth: sign in first")
	ErrNotAllowed = errors.New("auth: this GitHub login may not use this server")
)

// Principal is who made a request. Act is false for a watch-only web session. Cookie is true when
// the browser's cookie, not a bearer token, proved it, which is when the API checks the origin.
type Principal struct {
	Login  domain.Login
	Act    bool
	Cookie bool
}

// Config wires a Service. Zero TTLs take the defaults.
type Config struct {
	Sessions ports.Sessions
	Clock    domain.Clock
	// GitHubUser returns the login a GitHub token belongs to.
	GitHubUser func(ctx context.Context, token string) (domain.Login, error)
	// Allowed are the logins that may trade a GitHub token. Empty allows nobody.
	Allowed []domain.Login
	// Secure marks the cookie Secure, for a server behind https.
	Secure bool
	// WebControl lets a web session act, not only watch. It is for a local trial of the web UI and
	// off by default.
	WebControl bool
	TokenTTL   time.Duration
	LinkTTL    time.Duration
	WebTTL     time.Duration
	Rand       io.Reader
}

// Service mints and checks tokens.
type Service struct{ cfg Config }

// New fills cfg's defaults.
func New(cfg Config) *Service {
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = 90 * 24 * time.Hour
	}
	if cfg.LinkTTL == 0 {
		cfg.LinkTTL = 2 * time.Minute
	}
	if cfg.WebTTL == 0 {
		cfg.WebTTL = 12 * time.Hour
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Service{cfg: cfg}
}

var prefixes = map[ports.SessionKind]string{ports.SessionAPI: "otp_", ports.SessionLink: "otl_", ports.SessionWeb: "otw_"}

// Mint stores a new API token for login and returns it. Only its holder ever sees it again.
func (s *Service) Mint(ctx context.Context, login domain.Login, name string) (string, error) {
	return s.put(ctx, login, ports.SessionAPI, name, s.cfg.TokenTTL)
}

// Exchange trades a GitHub token for an API token, for an allowed login.
func (s *Service) Exchange(ctx context.Context, githubToken string) (string, domain.Login, error) {
	login, err := s.cfg.GitHubUser(ctx, githubToken)
	if err != nil {
		return "", "", fmt.Errorf("auth: check the GitHub token: %w", err)
	}
	if !slices.Contains(s.cfg.Allowed, login) {
		return "", login, ErrNotAllowed
	}
	tok, err := s.Mint(ctx, login, "github")
	return tok, login, err
}

// WebLink returns a one-time code that Redeem turns into a web cookie, which only watches unless
// Config.WebControl is set.
func (s *Service) WebLink(ctx context.Context, login domain.Login) (string, error) {
	return s.put(ctx, login, ports.SessionLink, "web-link", s.cfg.LinkTTL)
}

// Redeem spends a web link and returns the cookie of a new web session.
func (s *Service) Redeem(ctx context.Context, link string) (*http.Cookie, error) {
	x, err := s.cfg.Sessions.Spend(ctx, hash(link), s.cfg.Clock.Now())
	if errors.Is(err, ports.ErrNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("auth: spend link: %w", err)
	}
	tok, err := s.put(ctx, x.Login, ports.SessionWeb, "web", s.cfg.WebTTL)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name: CookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.cfg.Secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.cfg.WebTTL / time.Second),
	}, nil
}

// Authenticate reads the bearer token, or else the web cookie.
func (s *Service) Authenticate(r *http.Request) (Principal, error) {
	raw, ok := bearer(r)
	if !ok {
		c, err := r.Cookie(CookieName)
		if err != nil {
			return Principal{}, ErrNoSession
		}
		raw = c.Value
	}
	x, err := s.lookup(r.Context(), raw)
	if err != nil {
		return Principal{}, err
	}
	if !ok && x.Kind != ports.SessionWeb || ok && x.Kind != ports.SessionAPI {
		return Principal{}, ErrNoSession
	}
	act := x.Kind == ports.SessionAPI || x.Kind == ports.SessionWeb && s.cfg.WebControl
	return Principal{Login: x.Login, Act: act, Cookie: !ok}, nil
}

// SignOut deletes the session the request carries.
func (s *Service) SignOut(r *http.Request) error {
	raw, ok := bearer(r)
	if !ok {
		c, err := r.Cookie(CookieName)
		if err != nil {
			return nil
		}
		raw = c.Value
	}
	if err := s.cfg.Sessions.DeleteSession(r.Context(), hash(raw)); err != nil {
		return fmt.Errorf("auth: sign out: %w", err)
	}
	return nil
}

func (s *Service) lookup(ctx context.Context, raw string) (ports.Session, error) {
	x, err := s.cfg.Sessions.Session(ctx, hash(raw), s.cfg.Clock.Now())
	if errors.Is(err, ports.ErrNotFound) || err == nil && !strings.HasPrefix(raw, prefixes[x.Kind]) {
		return ports.Session{}, ErrNoSession
	}
	if err != nil {
		return ports.Session{}, fmt.Errorf("auth: read session: %w", err)
	}
	return x, nil
}

func (s *Service) put(ctx context.Context, login domain.Login, kind ports.SessionKind, name string, ttl time.Duration) (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(s.cfg.Rand, b[:]); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	tok := prefixes[kind] + base64.RawURLEncoding.EncodeToString(b[:])
	x := ports.Session{Hash: hash(tok), Login: login, Kind: kind, Name: name, Expires: s.cfg.Clock.Now().Add(ttl)}
	if err := s.cfg.Sessions.PutSession(ctx, x); err != nil {
		return "", fmt.Errorf("auth: store session: %w", err)
	}
	return tok, nil
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	return tok, ok && tok != ""
}

func hash(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}
