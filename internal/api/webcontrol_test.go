package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IniZio/outpost/internal/api"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

// webSession is a rig whose server lets web sessions act, and the cookie of one.
func webSession(t *testing.T, control bool) (*rig, *http.Cookie) {
	t.Helper()
	a := auth.New(auth.Config{
		Sessions: portstest.NewSessions(), Clock: domain.SystemClock{}, Allowed: []domain.Login{"ana"}, WebControl: control,
	})
	g := newRig(t, func(c *api.Config) { c.Auth = a })
	link, err := a.WebLink(context.Background(), "ana")
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := a.Redeem(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	return g, cookie
}

func write(t *testing.T, g *rig, c *http.Cookie, origin string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx(t), http.MethodPost, g.srv.URL+"/api/tasks", strings.NewReader(`{"repo":"o/r","prompt":"p"}`))
	req.AddCookie(c)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

func TestAWebSessionActsOnlyWhenAllowedAndFromItsOwnPages(t *testing.T) {
	g, c := webSession(t, true)
	own := g.srv.URL
	for origin, want := range map[string]int{own: http.StatusCreated, "": http.StatusForbidden, "http://evil.example": http.StatusForbidden, "null": http.StatusForbidden} {
		if got := write(t, g, c, origin); got != want {
			t.Errorf("origin %q: %d, want %d", origin, got, want)
		}
	}
	g, c = webSession(t, false)
	if got := write(t, g, c, g.srv.URL); got != http.StatusForbidden {
		t.Errorf("a watch-only session wrote: %d", got)
	}
}
