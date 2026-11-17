package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/schedule"
)

var errNoSchedules = errors.New("schedules are not run by this server")

func (s *Server) scheduleRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/schedules", s.watch(s.listSchedules))
	m.HandleFunc("GET /api/schedules/{name}", s.watch(s.getSchedule))
	m.HandleFunc("DELETE /api/schedules/{name}", s.act(s.deleteSchedule))
	m.HandleFunc("PUT /api/schedules/{name}", s.act(s.putSchedule))
	m.HandleFunc("POST /api/schedules/{name}/fire", s.act(s.fireSchedule))
	for verb, on := range map[string]bool{"enable": true, "disable": false} {
		m.HandleFunc("POST /api/schedules/{name}/"+verb, s.act(func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			return s.enableSchedule(w, r, p, on)
		}))
	}
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func scheduleView(e schedule.Entry) client.Schedule {
	v := client.Schedule{
		ID: int64(e.ID), Name: e.Name, Cron: e.Cron, TZ: e.TZ, Repo: string(e.Spec.Repo), Prompt: e.Spec.Prompt, Title: e.Spec.Title,
		TimeoutMin: int(e.Spec.Timeout / time.Minute), Enabled: e.Enabled,
		Next: optTime(e.Next), LastDue: optTime(e.LastDue), LastTask: int64(e.LastTask), LastSkipped: optTime(e.LastSkipped),
	}
	for _, a := range e.Spec.Also {
		v.Also = append(v.Also, string(a))
	}
	return v
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Schedules == nil {
		return errNoSchedules
	}
	es, err := s.cfg.Schedules.List(r.Context(), p.Login)
	if err != nil {
		return err //nolint:wrapcheck // schedule wraps its own
	}
	out := make([]client.Schedule, 0, len(es))
	for _, e := range es {
		out = append(out, scheduleView(e))
	}
	reply(w, http.StatusOK, out)
	return nil
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Schedules == nil {
		return errNoSchedules
	}
	e, err := s.cfg.Schedules.Get(r.Context(), p.Login, r.PathValue("name"))
	if err != nil {
		return err //nolint:wrapcheck // schedule wraps its own
	}
	reply(w, http.StatusOK, scheduleView(e))
	return nil
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Schedules == nil {
		return errNoSchedules
	}
	if err := s.cfg.Schedules.Delete(r.Context(), p.Login, r.PathValue("name")); err != nil {
		return err //nolint:wrapcheck // schedule wraps its own
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) putSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Schedules == nil {
		return errNoSchedules
	}
	var in client.Schedule
	if err := decode(w, r, &in); err != nil {
		return err
	}
	spec := domain.Spec{Title: in.Title, Repo: domain.RepoName(in.Repo), Prompt: in.Prompt, Timeout: time.Duration(in.TimeoutMin) * time.Minute}
	for _, a := range in.Also {
		spec.Also = append(spec.Also, domain.RepoName(a))
	}
	e, err := s.cfg.Schedules.Put(r.Context(), p.Login, r.PathValue("name"), schedule.Input{Cron: in.Cron, TZ: in.TZ, Spec: spec, Enabled: in.Enabled})
	if err != nil {
		return err //nolint:wrapcheck // schedule wraps its own
	}
	reply(w, http.StatusOK, scheduleView(e))
	return nil
}

func (s *Server) enableSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal, on bool) error {
	if s.cfg.Schedules == nil {
		return errNoSchedules
	}
	e, err := s.cfg.Schedules.SetEnabled(r.Context(), p.Login, r.PathValue("name"), on)
	if err != nil {
		return err //nolint:wrapcheck // schedule wraps its own
	}
	reply(w, http.StatusOK, scheduleView(e))
	return nil
}

func (s *Server) fireSchedule(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if s.cfg.Schedules == nil {
		return errNoSchedules
	}
	t, err := s.cfg.Schedules.FireNow(r.Context(), p.Login, r.PathValue("name"))
	if err != nil {
		return err //nolint:wrapcheck // schedule wraps its own
	}
	reply(w, http.StatusCreated, view(t, domain.AccessControl))
	return nil
}
