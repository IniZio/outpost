package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// SetupRev is domain.SetupRev under lifecycle's name, for the API.
type SetupRev = domain.SetupRev

var (
	// ErrNoSetup is an unknown setup revision.
	ErrNoSetup = ports.ErrNoSetup
	// ErrBuilding is a build asked of a revision that is building already.
	ErrBuilding = ports.ErrBuilding
	// ErrNoBuilder is a build with no machine of the person's linked to run it.
	ErrNoBuilder = errors.New("lifecycle: none of your machines is linked to build on")
)

// SetupConfig wires Setups. Profile is the agent profile commands baked into every build's home.
type SetupConfig struct {
	Store   ports.Store
	Setups  ports.Setups
	Builds  ports.Builds
	Clock   domain.Clock
	Base    string
	Profile [][]string
	Budget  time.Duration // one build's limit; zero takes 90 minutes
	Log     *slog.Logger
}

// Setups stores repos' setup revisions and runs their builds on machines. A build runs in the
// background under the context Run was given; a build marked building when the server starts is
// asked again, which joins the machine's build if it still runs.
type Setups struct {
	cfg     SetupConfig
	mu      sync.Mutex
	base    context.Context
	wg      sync.WaitGroup
	resumed chan struct{} // closed once Run has resumed the builds left building
}

// NewSetups returns the service. Builds wait for Run.
func NewSetups(cfg SetupConfig) *Setups {
	if cfg.Budget == 0 {
		cfg.Budget = 90 * time.Minute
	}
	if cfg.Clock == nil {
		cfg.Clock = domain.SystemClock{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Setups{cfg: cfg, base: context.Background(), resumed: make(chan struct{})}
}

// Run resumes the builds left building and serves new ones until ctx ends, then waits for the
// builds in flight to stop.
func (s *Setups) Run(ctx context.Context) error {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	revs, err := s.cfg.Setups.Building(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: setups to resume: %w", err)
	}
	for _, r := range revs {
		if _, err := s.start(ctx, r.Repo, r.Rev, r.Machine, true); err != nil {
			s.cfg.Log.Warn("setup build not resumed", "repo", r.Repo, "rev", r.Rev, "err", err)
		}
	}
	close(s.resumed)
	<-ctx.Done()
	s.wg.Wait()
	return nil
}

// Put stores files as the repo's next revision.
func (s *Setups) Put(ctx context.Context, repo domain.RepoName, files map[string]string, by domain.Login) (SetupRev, error) {
	if err := domain.ValidSetupFiles(files); err != nil {
		return SetupRev{}, fmt.Errorf("%w: %w", domain.ErrInvalid, err)
	}
	r, err := s.cfg.Setups.PutSetup(ctx, domain.SetupRev{Repo: repo, Files: files, CreatedAt: s.cfg.Clock.Now(), CreatedBy: by})
	if err != nil {
		return SetupRev{}, fmt.Errorf("lifecycle: put setup: %w", err)
	}
	return r, nil
}

// List lists the repo's revisions, newest first.
func (s *Setups) List(ctx context.Context, repo domain.RepoName) ([]SetupRev, error) {
	revs, err := s.cfg.Setups.Setups(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: setups: %w", err)
	}
	return revs, nil
}

// Build starts a build of the revision on one of by's linked sandbox machines: the one named
// machine (its name or id), or the first. It returns once the build is marked building. It waits
// for Run to resume the builds left building, so it never marks one that Run would resume too.
func (s *Setups) Build(ctx context.Context, repo domain.RepoName, rev int, by domain.Login, machine string) (SetupRev, error) {
	select {
	case <-s.resumed:
	case <-ctx.Done():
		return SetupRev{}, fmt.Errorf("lifecycle: waiting for builds to resume: %w", ctx.Err())
	}
	m, err := s.pick(ctx, by, machine)
	if err != nil {
		return SetupRev{}, err
	}
	return s.start(ctx, repo, rev, m, false)
}

func (s *Setups) pick(ctx context.Context, by domain.Login, want string) (domain.MachineID, error) {
	ms, err := s.cfg.Store.Machines(ctx)
	if err != nil {
		return "", fmt.Errorf("lifecycle: machines: %w", err)
	}
	for _, m := range ms {
		named := want == "" || want == m.Machine.Name || want == string(m.Machine.ID)
		if named && m.Online && m.Machine.Role == domain.RoleSandbox && m.Machine.Owner == by {
			return m.Machine.ID, nil
		}
	}
	return "", ErrNoBuilder
}

func (s *Setups) start(ctx context.Context, repo domain.RepoName, rev int, m domain.MachineID, resume bool) (SetupRev, error) {
	r, err := s.cfg.Setups.StartBuild(ctx, repo, rev, m, resume)
	if err != nil {
		return SetupRev{}, fmt.Errorf("lifecycle: start build: %w", err)
	}
	cur, err := s.cfg.Store.Repo(ctx, repo)
	if err != nil {
		return SetupRev{}, fmt.Errorf("lifecycle: repo %s: %w", repo, err)
	}
	spec := ports.SetupSpec{Repo: repo, Rev: rev, Files: r.Files, Base: s.cfg.Base, Profile: s.cfg.Profile}
	if b := cur.Setup; b.Rev != 0 && b.Rev != rev && b.Machine == m {
		spec.Keep = []int{b.Rev}
	}
	s.mu.Lock()
	base := s.base
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.run(base, r, spec)
	}()
	return r, nil
}

// run builds and records the end. A build cut by the server's own shutdown records nothing, so
// the next start resumes it.
func (s *Setups) run(base context.Context, r SetupRev, spec ports.SetupSpec) {
	ctx, cancel := context.WithTimeout(base, s.cfg.Budget)
	defer cancel()
	log := s.cfg.Log.With("repo", r.Repo, "rev", r.Rev, "machine", r.Machine)
	log.Info("setup build started")
	got, err := s.cfg.Builds.BuildSetup(ctx, r.Machine, spec, s.cfg.Budget, func(phase, note string) {
		log.Debug("setup build", "phase", phase, "note", note)
	})
	if base.Err() != nil {
		return
	}
	end := domain.SetupRev{Repo: r.Repo, Rev: r.Rev, Machine: r.Machine, BuiltAt: s.cfg.Clock.Now()}
	if err != nil {
		end.Status, end.Log = domain.SetupFailed, err.Error()
		log.Warn("setup build failed", "err", err)
	} else {
		end.Status, end.Image, end.Digest, end.Inner, end.Commit, end.Log = domain.SetupReady, got.Image, got.Digest, got.Inner, got.Commit, got.Log
		log.Info("setup build ready", "image", got.Image, "digest", got.Digest)
	}
	if err := s.cfg.Setups.EndBuild(context.WithoutCancel(ctx), end); err != nil {
		log.Error("setup build not recorded", "err", err)
	}
}
