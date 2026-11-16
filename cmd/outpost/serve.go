package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IniZio/outpost/internal/agent/claude"
	"github.com/IniZio/outpost/internal/api"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/egress"
	"github.com/IniZio/outpost/internal/github"
	"github.com/IniZio/outpost/internal/lifecycle"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/remote"
	"github.com/IniZio/outpost/internal/schedule"
	"github.com/IniZio/outpost/internal/secrets"
	"github.com/IniZio/outpost/internal/store/postgres"
)

// server is one serve process, wired. Its parts are built in order: the store, then the hub, the
// box port over it, the runner, then the HTTP routes.
type server struct {
	cfg      config
	log      *slog.Logger
	store    *postgres.Store
	hub      *link.Hub
	runner   *lifecycle.Runner
	sched    *schedule.Service
	http     *http.Server
	draining atomic.Bool
	// base is every request's parent context. The drain cancels it, so event streams end and the
	// HTTP shutdown is not held open by them.
	base       context.Context
	cancelBase context.CancelFunc
}

// openStore connects, migrates when asked, and marks every machine offline before any agent can
// link, because the flags a dead process left are stale.
func openStore(ctx context.Context, cfg config, clock domain.Clock) (*postgres.Store, error) {
	st, err := postgres.Open(ctx, cfg.DatabaseURL, postgres.WithClock(clock))
	if err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}
	if cfg.Migrate {
		if err := st.Migrate(ctx); err != nil {
			st.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return st, nil
}

func newServer(ctx context.Context, cfg config, log *slog.Logger) (*server, error) {
	if err := cfg.serveReady(); err != nil {
		return nil, err
	}
	key, err := secrets.ParseKey(cfg.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("OUTPOST_SECRET_KEY: %w", err)
	}
	clock := domain.SystemClock{}
	st, err := openStore(ctx, cfg, clock)
	if err != nil {
		return nil, err
	}
	s := &server{cfg: cfg, log: log, store: st}
	if err := s.wire(ctx, key, clock); err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}

func (s *server) wire(ctx context.Context, key secrets.Key, clock domain.Clock) error {
	if err := s.store.MarkAllOffline(ctx); err != nil {
		return err //nolint:wrapcheck // the store wraps its own
	}
	tap := &presence{Store: s.store}
	if err := s.wireLink(tap, clock); err != nil {
		return err
	}
	boxes, err := remote.New(remote.Config{Links: s.hub, Clock: clock})
	if err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	v, err := secrets.NewVault(key, s.store, clock)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	creds, grants, err := s.credentials(key, v)
	if err != nil {
		return err
	}
	maxRun := s.cfg.MaxRunning
	s.runner, err = lifecycle.New(lifecycle.Config{
		Store: s.store, Boxes: boxes, Clock: clock, Policy: domain.DefaultPolicy(), BaseImage: s.cfg.BaseImage,
		Agent: claude.New(claude.Config{
			Model: s.cfg.ClaudeModel, Bin: s.cfg.ClaudeBin, Marketplaces: s.cfg.ClaudeMarketplaces, Plugins: s.cfg.ClaudePlugins,
		}),
		Secrets:  creds,
		Capacity: func() domain.Capacity { return domain.Capacity{Max: maxRun} },
		Log:      s.log,
	})
	if err != nil {
		return fmt.Errorf("lifecycle: %w", err)
	}
	poke := s.runner.Poke
	tap.poke.Store(&poke)
	s.sched = schedule.New(schedule.Config{Store: s.store, Fire: s.runner.Fire, Clock: clock, Log: s.log})
	s.base, s.cancelBase = context.WithCancel(context.WithoutCancel(ctx))
	s.http = &http.Server{
		Handler: s.routes(v, grants, clock), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return s.base },
	}
	return nil
}

func (s *server) wireLink(tap *presence, clock domain.Clock) error {
	u, err := url.Parse(s.cfg.AgentURL)
	if err != nil {
		return fmt.Errorf("OUTPOST_AGENT_URL: %w", err)
	}
	reg := link.NewRegistry(ops.Catalogue(), link.ServedByServer, 0)
	link.HandleResumable(reg, ops.Report, func(context.Context, link.Meta, struct{}) (struct{}, link.Bytes, error) {
		return struct{}{}, nil, nil
	})
	s.hub, err = link.NewHub(link.HubConfig{
		Host: link.HostOf(u), Keys: s.store, Presence: tap, Reg: reg, Clock: clock, Log: s.log,
	})
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}
	return nil
}

// credentials is the egress over the vault when a proxy is set. With none, boxes get no
// credentials: they clone only public repos and the agent has no token.
func (s *server) credentials(key secrets.Key, v *secrets.Vault) (ports.Secrets, http.Handler, error) {
	if s.cfg.ProxyURL == "" {
		s.log.Warn("OUTPOST_PROXY_URL is not set: boxes run with no credentials")
		return noCredentials{}, nil, nil
	}
	e, err := egress.New(egress.Config{
		Key: key, Vault: v, Tasks: s.store, ProxyURL: s.cfg.ProxyURL, CAFile: s.cfg.ProxyCAFile, ProxyToken: s.cfg.ProxyToken,
		Rules: []egress.Rule{egress.GitHub(), egress.Bearer("api.anthropic.com", claude.OAuthPlaceholder, "claude")},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("egress: %w", err)
	}
	return e, e.GrantHandler(s.log), nil
}

func (s *server) routes(v *secrets.Vault, grants http.Handler, clock domain.Clock) http.Handler {
	a := auth.New(auth.Config{
		Sessions: s.store, Clock: clock, Allowed: s.cfg.Allowed,
		GitHubUser: githubUser, Secure: len(s.cfg.PublicURL) > 5 && s.cfg.PublicURL[:5] == "https",
	})
	mux := http.NewServeMux()
	mux.Handle("/", api.New(api.Config{
		Tasks: s.runner, Auth: a, Clock: clock, PublicURL: s.cfg.PublicURL, Log: s.log,
		Machines: machines{s.store, s.hub}, Pairing: pairing{s.store, s.hub}, Repos: s.store, Secrets: vault{v},
		Schedules: s.sched,
	}))
	mux.Handle("GET /machines/link", s.linkHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.ready)
	if grants != nil {
		mux.Handle(egress.GrantPath, grants)
	}
	return mux
}

// githubUser reads a GitHub token's login. A token GitHub refuses is no session, not a fault.
func githubUser(ctx context.Context, token string) (domain.Login, error) {
	l, err := github.Client{}.User(ctx, token)
	if errors.Is(err, github.ErrBadToken) {
		return "", fmt.Errorf("%w: %w", auth.ErrNoSession, err)
	}
	return l, err //nolint:wrapcheck // github wraps its own
}

// linkHandler refuses links until this process leads, so a machine never links to a standby that
// runs no feeds. The agent backs off and dials again, and reaches the leader.
func (s *server) linkHandler() http.Handler {
	hub := s.hub.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.runner.Leading() || s.draining.Load() {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "not leading", http.StatusServiceUnavailable)
			return
		}
		hub.ServeHTTP(w, r)
	})
}

// ready is up and reachable, never leading: a standby must be ready, or a rolling deploy waits for
// the old process's lease forever.
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.draining.Load() || s.store.Ping(ctx) != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serve runs until ctx ends, then drains: the runner stops leading and frees the lease, the hub
// asks every agent to relink elsewhere, and HTTP shuts down, each within the drain budget.
func (s *server) serve(ctx context.Context, ln net.Listener) error {
	defer s.store.Close()
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() { errs <- s.runner.Run(runCtx) })
	wg.Go(func() { _ = s.sched.Run(runCtx, s.runner.Started()) })
	wg.Go(func() {
		if err := s.http.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http: %w", err)
		}
	})
	var first error
	select {
	case <-ctx.Done():
	case first = <-errs:
	}
	s.log.Info("draining")
	s.draining.Store(true)
	stopRun()
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Drain)
	defer cancel()
	var dw sync.WaitGroup
	dw.Go(func() {
		// Links run on their request's context, so the base is cancelled only once they moved.
		_ = s.hub.Drain(dctx)
		s.cancelBase()
	})
	dw.Go(func() { _ = s.http.Shutdown(dctx) })
	dw.Wait()
	wg.Wait()
	return first
}
