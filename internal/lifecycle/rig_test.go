package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

const (
	owner   = domain.Login("ana")
	machine = domain.MachineID("m1")
)

var t0 = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

// rig is a leading Runner over fakes. It does not call Run: tests drive the loops one pass at a time
// and wait with quiesce, so every step is deterministic and nothing sleeps.
type rig struct {
	t       *testing.T
	ctx     context.Context
	r       *Runner
	store   *portstest.Store
	boxes   *portstest.Boxes
	agent   *portstest.Agent
	secrets *portstest.Secrets
	clock   *domain.ManualClock
}

func readyAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time)
	close(ch)
	return ch
}

func newRig(t *testing.T, store *portstest.Store) *rig {
	t.Helper()
	return newRigWith(t, store, func(*Config) {})
}

func newRigWith(t *testing.T, store *portstest.Store, tweak func(*Config)) *rig {
	t.Helper()
	g := &rig{
		t: t, ctx: context.Background(), store: store, boxes: portstest.NewBoxes(),
		agent: &portstest.Agent{}, secrets: &portstest.Secrets{}, clock: domain.NewManualClock(t0),
	}
	store.AddMachine(domain.Machine{ID: machine, Owner: owner, Role: domain.RoleSandbox, Slots: 4}, true)
	cfg := Config{
		Store: store, Boxes: g.boxes, Agent: g.agent, Secrets: g.secrets, Clock: g.clock,
		Policy: domain.DefaultPolicy(), BaseImage: "base:1", After: readyAfter,
		Capacity: func() domain.Capacity { return domain.Capacity{Max: 4} },
	}
	tweak(&cfg)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.lead.Store(true)
	g.r = r
	t.Cleanup(g.close)
	return g
}

// close stops what the rig started, as a drain would, so a call a test left held or stalled ends
// before the test does.
func (g *rig) close() {
	g.r.lead.Store(false)
	g.r.stop(errDraining)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.r.quiesce(ctx); err != nil {
		g.t.Errorf("rig did not drain: %v", err)
	}
}

func (g *rig) task(id domain.TaskID) domain.Task {
	g.t.Helper()
	tk, err := g.r.Task(g.ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	return tk
}

func (g *rig) created(kind domain.Kind) domain.TaskID {
	g.t.Helper()
	spec := domain.Spec{Repo: "acme/app", Prompt: "upgrade deps"}
	var (
		tk  domain.Task
		err error
	)
	if kind == domain.KindInteractive {
		tk, err = g.r.Create(g.ctx, owner, kind, spec)
	} else {
		tk, err = g.r.Fire(g.ctx, domain.Schedule{ID: 1, Owner: owner, Kind: kind, Spec: spec, Enabled: true})
	}
	if err != nil {
		g.t.Fatal(err)
	}
	return tk.ID
}

// grant places the task on the rig's machine, as one scheduler pass would.
func (g *rig) grant(id domain.TaskID) {
	g.t.Helper()
	if _, err := g.r.step(g.ctx, id, domain.SlotGranted{Machine: machine}); err != nil {
		g.t.Fatal(err)
	}
}

// settle waits for every lane and job to finish.
func (g *rig) settle() {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	defer cancel()
	if err := g.r.quiesce(ctx); err != nil {
		g.t.Fatal(err)
	}
}

// started creates a task and takes it through its start.
func (g *rig) started(kind domain.Kind) domain.TaskID {
	g.t.Helper()
	id := g.created(kind)
	g.grant(id)
	g.settle()
	g.wantStatus(id, domain.StatusWorking)
	return id
}

func (g *rig) feed(id domain.TaskID, s domain.Signal) {
	g.t.Helper()
	if _, err := g.r.step(g.ctx, id, s); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) wantStatus(id domain.TaskID, want domain.Status) {
	g.t.Helper()
	if got := g.task(id).Status(); got != want {
		g.t.Fatalf("status %v, want %v", got, want)
	}
}

func calls(b *portstest.Boxes, method string) []portstest.Call {
	var out []portstest.Call
	for _, c := range b.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func methods(b *portstest.Boxes) []string {
	var out []string
	for _, c := range b.Calls() {
		out = append(out, c.Method)
	}
	return out
}
