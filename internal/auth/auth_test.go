package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

var t0 = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

func newService(clock domain.Clock) *Service {
	return New(Config{
		Sessions: portstest.NewSessions(), Clock: clock, Allowed: []domain.Login{"ana"},
		GitHubUser: func(_ context.Context, tok string) (domain.Login, error) {
			if tok == "bad" {
				return "", errors.New("refused")
			}
			return domain.Login(tok), nil
		},
	})
}

func req(bearer string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func TestAnExchangedTokenActsForItsLogin(t *testing.T) {
	clock := domain.NewManualClock(t0)
	s := newService(clock)
	ctx := context.Background()
	tok, login, err := s.Exchange(ctx, "ana")
	if err != nil || login != "ana" || !strings.HasPrefix(tok, "otp_") {
		t.Fatalf("%q %q %v", tok, login, err)
	}
	if p, err := s.Authenticate(req(tok, nil)); err != nil || p != (Principal{Login: "ana", Act: true}) {
		t.Fatalf("%+v %v", p, err)
	}
	if _, _, err := s.Exchange(ctx, "bob"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a login not on the list: %v", err)
	}
	if _, _, err := s.Exchange(ctx, "bad"); err == nil {
		t.Fatal("a token GitHub refuses")
	}
	if err := s.SignOut(req(tok, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(req(tok, nil)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a signed-out token: %v", err)
	}
}

func TestAWebLinkGivesAWatchOnlyCookieOnce(t *testing.T) {
	clock := domain.NewManualClock(t0)
	s := newService(clock)
	ctx := context.Background()
	link, err := s.WebLink(ctx, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(req(link, nil)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a link is not a bearer: %v", err)
	}
	c, err := s.Redeem(ctx, link)
	if err != nil || !c.HttpOnly || c.Name != CookieName {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := s.Redeem(ctx, link); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a link works once: %v", err)
	}
	if p, err := s.Authenticate(req("", c)); err != nil || p.Act || p.Login != "ana" {
		t.Fatalf("the cookie watches only: %+v %v", p, err)
	}
	if _, err := s.Authenticate(req(c.Value, nil)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a web session is not a bearer: %v", err)
	}
	if err := s.SignOut(req("", c)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(req("", c)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("signed out: %v", err)
	}
}

func TestTokensExpireAndJunkIsRefused(t *testing.T) {
	clock := domain.NewManualClock(t0)
	s := newService(clock)
	tok, err := s.Mint(context.Background(), "ana", "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*http.Request{req("", nil), req("otp_nope", nil), req("", &http.Cookie{Name: CookieName, Value: tok})} {
		if _, err := s.Authenticate(r); !errors.Is(err, ErrNoSession) {
			t.Fatalf("refused: %v", err)
		}
	}
	if err := s.SignOut(req("", nil)); err != nil {
		t.Fatal(err)
	}
	clock.Advance(91 * 24 * time.Hour)
	if _, err := s.Authenticate(req(tok, nil)); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an expired token: %v", err)
	}
}
