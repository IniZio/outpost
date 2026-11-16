package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
)

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) error {
	var in client.SignIn
	if err := decode(w, r, &in); err != nil {
		return err
	}
	tok, login, err := s.cfg.Auth.Exchange(r.Context(), in.GitHubToken)
	if err != nil {
		return err //nolint:wrapcheck // auth's errors are already worded for people
	}
	reply(w, http.StatusCreated, client.Session{Token: tok, Login: string(login)})
	return nil
}

func (s *Server) webLink(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	link, err := s.cfg.Auth.WebLink(r.Context(), p.Login)
	if err != nil {
		return err //nolint:wrapcheck // auth's errors are already worded for people
	}
	next := r.URL.Query().Get("next")
	u := strings.TrimRight(s.cfg.PublicURL, "/") + "/login?t=" + url.QueryEscape(link)
	if safeNext(next) {
		u += "&next=" + url.QueryEscape(next)
	}
	reply(w, http.StatusCreated, client.WebLink{URL: u})
	return nil
}

// redeem spends a web link, sets the watch-only cookie and sends the browser on to a local path.
func (s *Server) redeem(w http.ResponseWriter, r *http.Request) error {
	c, err := s.cfg.Auth.Redeem(r.Context(), r.URL.Query().Get("t"))
	if err != nil {
		return err //nolint:wrapcheck // auth's errors are already worded for people
	}
	http.SetCookie(w, c)
	next := r.URL.Query().Get("next")
	if !safeNext(next) {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
	return nil
}

// safeNext keeps a redirect on this server: a path, never a scheme or another host.
func safeNext(next string) bool {
	return strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.ContainsAny(next, "\\\r\n")
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	if err := s.cfg.Auth.SignOut(r); err != nil {
		return err //nolint:wrapcheck // auth wraps its own
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, p auth.Principal) error {
	reply(w, http.StatusOK, client.Me{Login: string(p.Login), Act: p.Act})
	return nil
}
