package api

import (
	"fmt"
	"net/http"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
)

func repoView(r domain.Repo) client.Repo {
	v := client.Repo{
		Name: string(r.Name), ReadyImage: r.ReadyImage, ReadyOn: string(r.ReadyOn), Notes: r.Notes,
		SetupRev: r.Setup.Rev, SetupImage: r.Setup.Image, SetupOn: string(r.Setup.Machine),
	}
	if !r.ReadyAt.IsZero() {
		at := r.ReadyAt
		v.ReadyAt = &at
	}
	return v
}

func (s *Server) repos(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	rs, err := s.cfg.Repos.Repos(r.Context())
	if err != nil {
		return fmt.Errorf("api: repos: %w", err)
	}
	out := make([]client.Repo, 0, len(rs))
	for _, x := range rs {
		out = append(out, repoView(x))
	}
	reply(w, http.StatusOK, out)
	return nil
}

// setRepo sets a repo's ready image, the machine that holds it, and its notes. Setting an image
// registers a template made elsewhere, such as one prepared on the prototype.
func (s *Server) setRepo(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var in client.Repo
	if err := decode(w, r, &in); err != nil {
		return err
	}
	name := domain.RepoName(r.PathValue("owner") + "/" + r.PathValue("name"))
	if in.ReadyImage != "" && in.ReadyOn == "" {
		return fmt.Errorf("%w: a ready image needs the machine that holds it", errBadRequest)
	}
	repo, err := s.cfg.Repos.Repo(r.Context(), name)
	if err != nil {
		return fmt.Errorf("api: repo: %w", err)
	}
	repo.Name, repo.ReadyImage, repo.ReadyOn, repo.Notes = name, in.ReadyImage, domain.MachineID(in.ReadyOn), in.Notes
	if in.ReadyImage != "" && repo.ReadyAt.IsZero() {
		repo.ReadyAt = s.cfg.Clock.Now()
	}
	if err := s.cfg.Repos.SetRepo(r.Context(), repo); err != nil {
		return fmt.Errorf("api: set repo: %w", err)
	}
	reply(w, http.StatusOK, repoView(repo))
	return nil
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Secrets == nil {
		return errNoSecrets
	}
	var in client.Text
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Text == "" {
		return fmt.Errorf("%w: the secret is empty", errBadRequest)
	}
	if err := s.cfg.Secrets.SetSecret(r.Context(), p.Login, r.PathValue("name"), in.Text); err != nil {
		return fmt.Errorf("api: set secret: %w", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
