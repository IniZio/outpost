package api

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/lifecycle"
)

var errNoSetups = errors.New("setup builds are not run by this server")

func (s *Server) setupRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/repos/{owner}/{name}/setups", s.watch(s.listSetups))
	m.HandleFunc("POST /api/repos/{owner}/{name}/setups", s.act(s.putSetup))
	m.HandleFunc("POST /api/repos/{owner}/{name}/setups/{rev}/build", s.act(s.buildSetup))
}

func repoParam(r *http.Request) domain.RepoName {
	return domain.RepoName(r.PathValue("owner") + "/" + r.PathValue("name"))
}

func setupView(x lifecycle.SetupRev) client.SetupRev {
	v := client.SetupRev{
		Repo: string(x.Repo), Rev: x.Rev, Status: string(x.Status), Files: slices.Sorted(maps.Keys(x.Files)),
		Machine: string(x.Machine), Image: x.Image, Digest: x.Digest, Inner: x.Inner, Commit: x.Commit, Log: x.Log,
		CreatedAt: x.CreatedAt, CreatedBy: string(x.CreatedBy),
	}
	if !x.BuiltAt.IsZero() {
		at := x.BuiltAt
		v.BuiltAt = &at
	}
	return v
}

func (s *Server) listSetups(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	if s.cfg.Setups == nil {
		return errNoSetups
	}
	revs, err := s.cfg.Setups.List(r.Context(), repoParam(r))
	if err != nil {
		return err //nolint:wrapcheck // lifecycle wraps
	}
	out := make([]client.SetupRev, 0, len(revs))
	for _, x := range revs {
		out = append(out, setupView(x))
	}
	reply(w, http.StatusOK, out)
	return nil
}

func (s *Server) putSetup(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Setups == nil {
		return errNoSetups
	}
	var in client.SetupFiles
	if err := decode(w, r, &in); err != nil {
		return err
	}
	x, err := s.cfg.Setups.Put(r.Context(), repoParam(r), in.Files, p.Login)
	if err != nil {
		return err //nolint:wrapcheck // lifecycle wraps
	}
	reply(w, http.StatusCreated, setupView(x))
	return nil
}

func (s *Server) buildSetup(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Setups == nil {
		return errNoSetups
	}
	rev, err := strconv.Atoi(r.PathValue("rev"))
	if err != nil || rev <= 0 {
		return fmt.Errorf("%w: bad revision %q", errBadRequest, r.PathValue("rev"))
	}
	var in client.SetupBuild
	if err := decode(w, r, &in); err != nil {
		return err
	}
	x, err := s.cfg.Setups.Build(r.Context(), repoParam(r), rev, p.Login, in.Machine)
	if err != nil {
		return err //nolint:wrapcheck // lifecycle wraps
	}
	reply(w, http.StatusAccepted, setupView(x))
	return nil
}
