package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/api"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/lifecycle"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

// rig is the API over a leading Runner and the in-memory fakes, served by httptest.
type rig struct {
	t       *testing.T
	srv     *httptest.Server
	auth    *auth.Service
	store   *portstest.Store
	boxes   *portstest.Boxes
	pairing *pairing
	repos   *repos
	secrets *secrets
	owner   *client.Client
}

type option func(*api.Config)

func newRig(t *testing.T, opts ...option) *rig {
	t.Helper()
	g := &rig{t: t, store: portstest.NewStore(), boxes: portstest.NewBoxes(), pairing: newPairing(), repos: &repos{}, secrets: &secrets{}}
	g.store.AddMachine(domain.Machine{ID: "m1", Name: "box1", Owner: "ana", Role: domain.RoleSandbox, Slots: 2}, true)
	runner := newRunner(t, g.store, g.boxes)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = runner.Run(ctx) }()
	<-runner.Started()
	t.Cleanup(func() { cancel(); <-done })
	g.auth = auth.New(auth.Config{
		Sessions: portstest.NewSessions(), Clock: domain.SystemClock{}, Allowed: []domain.Login{"ana", "bob"},
		GitHubUser: func(_ context.Context, tok string) (domain.Login, error) { return domain.Login(tok), nil },
	})
	cfg := api.Config{
		Tasks: runner, Auth: g.auth, Machines: machines{g.store}, Pairing: g.pairing, Repos: g.repos,
		Secrets: g.secrets, Clock: domain.SystemClock{}, PublicURL: "http://outpost.test",
	}
	for _, o := range opts {
		o(&cfg)
	}
	g.srv = httptest.NewServer(api.New(cfg))
	t.Cleanup(g.srv.Close)
	g.owner = g.as("ana")
	return g
}

// newRunner is a Runner over the fakes. It leads only once Run holds the lease.
func newRunner(t *testing.T, store *portstest.Store, boxes *portstest.Boxes) *lifecycle.Runner {
	t.Helper()
	agent := &portstest.Agent{}
	agent.SetSummary("all done")
	r, err := lifecycle.New(lifecycle.Config{
		Store: store, Boxes: boxes, Agent: agent, Secrets: &portstest.Secrets{},
		Clock: domain.SystemClock{}, Policy: domain.DefaultPolicy(), BaseImage: "base:1", PollEvery: 20 * time.Millisecond,
		Capacity: func() domain.Capacity { return domain.Capacity{Max: 2} },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// as is a client with an API token for login.
func (g *rig) as(login domain.Login) *client.Client {
	g.t.Helper()
	tok, err := g.auth.Mint(context.Background(), login, "test")
	if err != nil {
		g.t.Fatal(err)
	}
	c := client.New(g.srv.URL, tok)
	c.Retry = 10 * time.Millisecond
	return c
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// code is the API error code of err, or "" when it is not one.
func code(err error) string {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// eventually polls until ok or the test's deadline.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second) //nolint:forbidigo // a test's wall-clock deadline
	for !ok() {
		if time.Now().After(deadline) { //nolint:forbidigo // a test's wall-clock deadline
			t.Fatalf("never: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type machines struct{ s *portstest.Store }

func (m machines) Machines(ctx context.Context) ([]lifecycle.MachineState, error) {
	return m.s.Machines(ctx) //nolint:wrapcheck // a fake
}

func (m machines) DeleteMachine(_ context.Context, id domain.MachineID) (bool, error) {
	m.s.SetOnline(id, false)
	return true, nil
}

// pairing runs the domain's enrollment transitions over a map, as the store does under its lock.
type pairing struct {
	mu   sync.Mutex
	rows map[domain.UserCode]domain.Enrollment
	n    int
}

func newPairing() *pairing { return &pairing{rows: map[domain.UserCode]domain.Enrollment{}} }

func (p *pairing) Begin(_ context.Context, e domain.Enrollment) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows[e.User] = e
	return nil
}

func (p *pairing) Confirm(_ context.Context, code domain.UserCode, owner domain.Login, now time.Time) (domain.Enrollment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.rows[code]
	if !ok {
		return e, domain.ErrUnknownPairing
	}
	e, err := e.Confirm(owner, now)
	if err != nil {
		return e, err //nolint:wrapcheck // a fake
	}
	p.n++
	e = e.Enrolled(domain.MachineID("pm" + string(rune('0'+p.n))))
	p.rows[code] = e
	return e, nil
}

func (p *pairing) Collect(_ context.Context, device [32]byte, now time.Time) (domain.Enrollment, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.rows {
		if e.Device == device {
			next, ok, err := e.Collect(now)
			p.rows[k] = next
			return next, ok, err //nolint:wrapcheck // a fake
		}
	}
	return domain.Enrollment{}, false, domain.ErrUnknownPairing
}

type repos struct {
	mu   sync.Mutex
	rows []domain.Repo
}

func (r *repos) Repos(context.Context) ([]domain.Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Repo(nil), r.rows...), nil
}

func (r *repos) Repo(_ context.Context, name domain.RepoName) (domain.Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.Name == name {
			return x, nil
		}
	}
	return domain.Repo{Name: name}, nil
}

func (r *repos) SetRepo(_ context.Context, x domain.Repo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.rows {
		if r.rows[i].Name == x.Name {
			r.rows[i] = x
			return nil
		}
	}
	r.rows = append(r.rows, x)
	return nil
}

type secrets struct {
	mu  sync.Mutex
	got map[string]string
}

func (s *secrets) SetSecret(_ context.Context, owner domain.Login, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.got == nil {
		s.got = map[string]string{}
	}
	s.got[string(owner)+"/"+name] = value
	return nil
}

var (
	_ http.Handler   = (*api.Server)(nil)
	_ ports.Sessions = (*portstest.Sessions)(nil)
)
