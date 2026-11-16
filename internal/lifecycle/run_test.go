package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// afterCtl stands in for time.After. A wait of poll never ends, and every other wait is recorded
// and ends when the test says.
type afterCtl struct {
	poll  time.Duration
	waits chan time.Duration
	wake  chan time.Time
	never chan time.Time
}

func newAfterCtl(poll time.Duration) *afterCtl {
	return &afterCtl{poll: poll, waits: make(chan time.Duration, 16), wake: make(chan time.Time), never: make(chan time.Time)}
}

func (a *afterCtl) after(d time.Duration) <-chan time.Time {
	if d == a.poll {
		return a.never
	}
	select {
	case a.waits <- d:
	default:
	}
	return a.wake
}

// running starts Run on a rig and returns a stop that ends it and reports its result.
func running(t *testing.T, g *rig) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(g.ctx)
	done := make(chan error, 1)
	go func() { done <- g.r.Run(ctx) }()
	return func() error { cancel(); return <-done }
}

// waitFor follows the task's timeline until an entry says text.
func (g *rig) waitFor(id domain.TaskID, text string) {
	g.t.Helper()
	for row, err := range g.r.Follow(g.ctx, id, 0) {
		if err != nil {
			g.t.Fatal(err)
		}
		if row.Text == text {
			return
		}
	}
	g.t.Fatalf("the timeline ended before %q", text)
}

// Run takes the lease, drives a task from creation to a sleeping box with nothing but the
// scheduler's poke and the wake loop's one timer, and gives the lease back when it ends.
func TestRunDrivesTasksAndGivesTheLeaseBack(t *testing.T) {
	const poll = 24 * time.Hour
	store := newStore()
	ctl := newAfterCtl(poll)
	const idle = 17 * time.Minute
	g := newIdleRig(t, store, func(c *Config) {
		c.After, c.PollEvery = ctl.after, poll
		c.Policy.IdleSleep = idle
	})
	stop := running(t, g)
	<-g.r.started

	tk, err := g.r.Create(g.ctx, owner, domain.KindInteractive, domain.Spec{Repo: "acme/app", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	g.waitFor(tk.ID, "The agent is working")
	g.feed(tk.ID, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.feed(tk.ID, domain.AgentReport{Run: 1, Status: domain.HCIdle})

	for d := range ctl.waits {
		if d == idle {
			break
		}
	}
	g.clock.Advance(idle)
	go func() {
		select {
		case ctl.wake <- time.Time{}:
		case <-g.ctx.Done():
		}
	}()
	g.waitFor(tk.ID, "The box went to sleep after 17 idle minutes. Send a message to wake it.")

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Do(g.ctx, tk.ID, domain.UserStop{By: owner}); err == nil {
		t.Fatal("a stopped runner still takes commands")
	}
	release, err := store.Lease(context.Background())
	if err != nil {
		t.Fatalf("the lease was not given back: %v", err)
	}
	release()
}

// During a deploy the new process waits for the old one's lease and refuses writes until it has it.
func TestANewProcessLeadsOnlyAfterTheOldOneLetsGo(t *testing.T) {
	store := newStore()
	old := newIdleRig(t, store, func(c *Config) { c.After = newAfterCtl(0).after })
	stopOld := running(t, old)
	<-old.r.started

	next := newIdleRig(t, store, func(c *Config) { c.After = newAfterCtl(0).after })
	stopNext := running(t, next)
	if _, err := next.r.Create(next.ctx, owner, domain.KindInteractive, domain.Spec{Repo: "acme/app"}); err == nil {
		t.Fatal("a process without the lease took a write")
	}
	if err := stopOld(); err != nil {
		t.Fatal(err)
	}
	<-next.r.started
	if _, err := next.r.Create(next.ctx, owner, domain.KindInteractive, domain.Spec{Repo: "acme/app"}); err != nil {
		t.Fatal(err)
	}
	if err := stopNext(); err != nil {
		t.Fatal(err)
	}
}

// A drain ends the start in flight without an answer, so the next process's Recovered, and not a
// spurious failure, decides what happens to it.
func TestADrainAnswersNothingAndLeavesTheTaskForRecovered(t *testing.T) {
	store := newStore()
	g := newIdleRig(t, store, func(c *Config) { c.After = newAfterCtl(0).after })
	stop := running(t, g)
	<-g.r.started
	g.boxes.Hold(1)
	tk, err := g.r.Create(g.ctx, owner, domain.KindInteractive, domain.Spec{Repo: "acme/app"})
	if err != nil {
		t.Fatal(err)
	}
	g.r.Poke()
	<-g.boxes.Entered()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Load(g.ctx, tk.ID); got.Status() != domain.StatusStarting {
		t.Fatalf("status %v: a drained start must not be answered", got.Status())
	}
	if got := calls(g.boxes, "Release"); len(got) != 0 {
		t.Fatalf("releases %+v: the next process's Recovered cleans up", got)
	}
}

func TestNewChecksItsConfig(t *testing.T) {
	g := newIdleRig(t, newStore(), func(*Config) {})
	cfg := g.r.cfg
	cfg.Store = nil
	if _, err := New(cfg); err == nil {
		t.Error("a runner with no store")
	}
	cfg = g.r.cfg
	cfg.Capacity = nil
	if _, err := New(cfg); err == nil {
		t.Error("a runner with no capacity")
	}
	if b := (Budgets{Probe: time.Second}).filled(); b.Probe != time.Second || b.Start != DefaultBudgets().Start {
		t.Errorf("budgets %+v", b)
	}
}
