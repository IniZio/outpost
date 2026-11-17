// Package api is Outpost's HTTP API: JSON over net/http, the timeline as server-sent events, and
// the agent's screen as text. Handlers decode, authenticate, check access and call lifecycle. The
// wire types are api/client's, so the client and the server share one definition.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/lifecycle"
	"github.com/IniZio/outpost/internal/schedule"
)

// Machines lists and revokes machines. cmd/outpost's adapter also kicks a revoked machine's link.
type Machines interface {
	Machines(ctx context.Context) ([]lifecycle.MachineState, error)
	DeleteMachine(ctx context.Context, id domain.MachineID) (bool, error)
}

// Pairing stores pairing attempts: ports.Enrollments under api's name.
type Pairing interface {
	Begin(ctx context.Context, e domain.Enrollment) error
	Confirm(ctx context.Context, code domain.UserCode, owner domain.Login, now time.Time) (domain.Enrollment, error)
	Collect(ctx context.Context, device [32]byte, now time.Time) (domain.Enrollment, bool, error)
}

// Repos reads and writes repo rows.
type Repos interface {
	Repos(ctx context.Context) ([]domain.Repo, error)
	Repo(ctx context.Context, name domain.RepoName) (domain.Repo, error)
	SetRepo(ctx context.Context, r domain.Repo) error
}

// Secrets stores a person's secrets. Values go in and never come out.
type Secrets interface {
	SetSecret(ctx context.Context, owner domain.Login, name, value string) error
}

// Config wires a Server.
type Config struct {
	Tasks    *lifecycle.Runner
	Auth     *auth.Service
	Machines Machines
	Pairing  Pairing
	Repos    Repos
	// Secrets is nil until the secrets store is wired, and PUT /api/secrets answers 501.
	Secrets Secrets
	// Schedules is nil when this server runs no schedules, and their routes answer 501.
	Schedules *schedule.Service
	// Setups is nil when this server builds no setups, and their routes answer 501.
	Setups *lifecycle.Setups
	Clock  domain.Clock
	// Web is the root of the web files: watch.html, watch.js and watch.css, and optionally app/
	// with the built UI. Nil serves the embedded files.
	Web fs.FS
	// PublicURL is where people reach the server, for sign-in links.
	PublicURL string
	Log       *slog.Logger
}

// Server serves the API.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	screens *screenCache
}

// New builds the routes.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), screens: newScreenCache(cfg.Clock)}
	s.routes()
	return s
}

// ServeHTTP serves the API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/status", s.open(s.status))
	m.HandleFunc("POST /api/sessions", s.open(s.signIn))
	m.HandleFunc("GET /login", s.open(s.redeem))
	m.HandleFunc("POST /api/sessions/web-link", s.act(s.webLink))
	m.HandleFunc("DELETE /api/sessions/current", s.watch(s.signOut))
	m.HandleFunc("GET /api/me", s.watch(s.me))
	s.taskRoutes()
	s.machineRoutes()
	m.HandleFunc("GET /api/repos", s.watch(s.repos))
	m.HandleFunc("PUT /api/repos/{owner}/{name}", s.act(s.setRepo))
	m.HandleFunc("PUT /api/secrets/{name}", s.act(s.setSecret))
	s.scheduleRoutes()
	s.setupRoutes()
	s.webRoutes()
}

type handler func(w http.ResponseWriter, r *http.Request, p auth.Principal) error

// act serves h for an API token. watch serves it for any session. open needs none.
func (s *Server) act(h handler) http.HandlerFunc { return s.authed(h, true) }

func (s *Server) watch(h handler) http.HandlerFunc { return s.authed(h, false) }

func (s *Server) authed(h handler, needAct bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.cfg.Auth.Authenticate(r)
		if err == nil && needAct && !p.Act {
			err = errWatchOnly
		}
		if err == nil && needAct && p.Cookie && !sameOrigin(r) {
			err = errCrossOrigin
		}
		if err == nil {
			err = h(w, r, p)
		}
		if err != nil {
			s.fail(w, r, err)
		}
	}
}

func (s *Server) open(h func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	}
}

// Errors of the API's own.
var (
	errWatchOnly   = errors.New("this session can only watch; use an API token")
	errCrossOrigin = errors.New("a browser may act only from this server's own pages")
	errNotFound    = errors.New("not found")
	errBadRequest  = errors.New("bad request")
	errNoSecrets   = errors.New("secrets are not stored on this server yet")
)

type problem struct {
	err    error
	status int
	code   string
}

// problems maps every error a handler can meet to a status and a stable code. Anything else is a
// 500 whose cause goes to the log only.
var problems = []problem{
	{auth.ErrNoSession, http.StatusUnauthorized, "no_session"},
	{auth.ErrNotAllowed, http.StatusForbidden, "not_allowed"},
	{errWatchOnly, http.StatusForbidden, "watch_only"},
	{errCrossOrigin, http.StatusForbidden, "cross_origin"},
	{domain.ErrForbidden, http.StatusForbidden, "forbidden"},
	{lifecycle.ErrNotFound, http.StatusNotFound, "not_found"},
	{schedule.ErrNotFound, http.StatusNotFound, "not_found"},
	{errNotFound, http.StatusNotFound, "not_found"},
	{domain.ErrUnknownPairing, http.StatusNotFound, "not_found"},
	{domain.ErrUnknownMachine, http.StatusNotFound, "not_found"},
	{errBadRequest, http.StatusBadRequest, "bad_request"},
	{domain.ErrInvalid, http.StatusUnprocessableEntity, "invalid"},
	{domain.ErrBadPairing, http.StatusUnprocessableEntity, "invalid"},
	{domain.ErrExecNotTool, http.StatusUnprocessableEntity, "invalid"},
	{domain.ErrNotReady, http.StatusConflict, "not_ready"},
	{lifecycle.ErrNotRunning, http.StatusConflict, "not_running"},
	{domain.ErrClosing, http.StatusConflict, "closing"},
	{domain.ErrEnrollUsed, http.StatusConflict, "used"},
	{domain.ErrReleased, http.StatusGone, "released"},
	{domain.ErrEnrollExpired, http.StatusGone, "expired"},
	{domain.ErrTooManyPairings, http.StatusTooManyRequests, "rate_limited"},
	{errNoSecrets, http.StatusNotImplemented, "not_built"},
	{errNoSchedules, http.StatusNotImplemented, "not_built"},
	{errNoSetups, http.StatusNotImplemented, "not_built"},
	{lifecycle.ErrNoSetup, http.StatusNotFound, "not_found"},
	{lifecycle.ErrBuilding, http.StatusConflict, "building"},
	{lifecycle.ErrNoBuilder, http.StatusConflict, "no_machine"},
	{lifecycle.ErrNotLeading, http.StatusServiceUnavailable, "not_leading"},
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	for _, p := range problems {
		if errors.Is(err, p.err) {
			if p.status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "2")
			}
			reply(w, p.status, client.Error{Code: p.code, Message: err.Error()})
			return
		}
	}
	if r.Context().Err() != nil {
		return
	}
	id := strconv.FormatInt(s.cfg.Clock.Now().UnixNano(), 36)
	s.cfg.Log.ErrorContext(r.Context(), "request failed", "id", id, "method", r.Method, "path", r.URL.Path, "err", err)
	reply(w, http.StatusInternalServerError, client.Error{Code: "internal", Message: "something went wrong; see the server log for " + id})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body of at most 1 MiB with no unknown fields.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", errBadRequest, err)
	}
	return nil
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) error {
	reply(w, http.StatusOK, client.Status{Leading: s.cfg.Tasks.Leading()})
	return nil
}

// sameOrigin reports whether a browser request came from a page of this server. Browsers send
// Origin on every write, so a missing or foreign one is refused: a cookie alone must not act.
func sameOrigin(r *http.Request) bool {
	o, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && o.Host != "" && o.Host == r.Host
}
