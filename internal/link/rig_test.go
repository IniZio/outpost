package link_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/link/linktest"
)

var clock domain.SystemClock

type (
	text struct{ S string }
	none struct{}
)

var (
	cat    = link.NewCatalogue()
	echo   = link.NewOp[text, text](cat, "t.echo", link.ServedByAgent)
	gate   = link.NewOp[text, text](cat, "t.gate", link.ServedByAgent)
	watch  = link.NewResumableOp[text, struct{ Opens int }](cat, "t.watch", link.ServedByAgent)
	status = link.NewOp[none, struct{ Machine domain.MachineID }](cat, "t.status", link.ServedByServer)
	report = link.NewResumableOp[none, none](cat, "t.report", link.ServedByServer)
)

// world is what the two ends of a rig record, so a test can see what each served.
type world struct {
	started chan string // a gate handler started
	release chan struct{}

	mu       sync.Mutex
	watchers map[string]int // adopted state per watch id: how many times it was opened
	created  int            // how many distinct watch states were created
	reports  []domain.LinkGen
}

func newWorld() *world {
	return &world{started: make(chan string, 8), release: make(chan struct{}), watchers: map[string]int{}}
}

func (w *world) agentRegistry() *link.Registry {
	r := link.NewRegistry(cat, link.ServedByAgent, 0)
	link.Handle(r, echo, func(_ context.Context, _ link.Meta, q text) (text, error) { return q, nil })
	link.Handle(r, gate, func(ctx context.Context, _ link.Meta, q text) (text, error) {
		w.started <- q.S
		select {
		case <-w.release:
			return text{"released " + q.S}, nil
		case <-ctx.Done():
			return text{}, ctx.Err()
		}
	})
	link.HandleResumable(r, watch, func(_ context.Context, _ link.Meta, q text) (struct{ Opens int }, link.Bytes, error) {
		return struct{ Opens int }{w.adopt(q.S)}, func(ctx context.Context, _ link.Stream) error { <-ctx.Done(); return nil }, nil
	})
	return r
}

// adopt is what a resumable handler must do: take the state it already holds instead of creating it.
func (w *world) adopt(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.watchers[id] == 0 {
		w.created++
	}
	w.watchers[id]++
	return w.watchers[id]
}

func (w *world) serverRegistry() *link.Registry {
	r := link.NewRegistry(cat, link.ServedByServer, 0)
	link.Handle(r, status, func(_ context.Context, m link.Meta, _ none) (struct{ Machine domain.MachineID }, error) {
		return struct{ Machine domain.MachineID }{m.Machine}, nil
	})
	link.HandleResumable(r, report, func(_ context.Context, m link.Meta, _ none) (none, link.Bytes, error) {
		w.mu.Lock()
		w.reports = append(w.reports, m.Gen)
		w.mu.Unlock()
		return none{}, func(ctx context.Context, _ link.Stream) error { <-ctx.Done(); return nil }, nil
	})
	return r
}

func (w *world) reportGens() []domain.LinkGen {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]domain.LinkGen(nil), w.reports...)
}

func newRig(t *testing.T, edit func(*linktest.Options)) (*linktest.Rig, *world) {
	t.Helper()
	w := newWorld()
	o := linktest.Options{Server: w.serverRegistry(), Agent: w.agentRegistry()}
	if edit != nil {
		edit(&o)
	}
	return linktest.New(t, o), w
}

func deadline(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestAgentAndServerCallEachOther(t *testing.T) {
	rig, _ := newRig(t, nil)
	ctx := deadline(t, 5*time.Second)
	got, err := link.Call(ctx, rig.Hub.Link(rig.Machine), echo, text{"hi"})
	if err != nil || got.S != "hi" {
		t.Fatalf("server to agent: %v %v", got, err)
	}
	st, err := link.Call(ctx, rig.Agent.Server(), status, none{})
	if err != nil || st.Machine != rig.Machine {
		t.Fatalf("agent to server, with the machine the handshake proved: %v %v", st, err)
	}
	info, ok := rig.Hub.Link(rig.Machine).Info()
	if !ok || info.Gen != 1 || info.Build != "linktest" || rig.Agent.State() != link.StateUp || !rig.Store.Online(rig.Machine) {
		t.Fatalf("link not recorded: %+v up=%v state=%v online=%v", info, ok, rig.Agent.State(), rig.Store.Online(rig.Machine))
	}
}

func TestRevokeKicksALiveLink(t *testing.T) {
	rig, _ := newRig(t, nil)
	link1 := rig.Hub.Link(rig.Machine)
	rig.Revoke(rig.Hub)
	if !linktest.Eventually(func() bool { _, ok := link1.Info(); return !ok }) {
		t.Fatal("revoking a machine must end its live link at once")
	}
	start := clock.Now()
	if _, err := link.Call(deadline(t, 5*time.Second), link1, echo, text{}); !errors.Is(err, link.ErrOffline) {
		t.Fatalf("a revoked machine is offline: %v", err)
	}
	if clock.Now().Sub(start) > time.Second {
		t.Fatal("a call for a revoked machine must not wait for its link to move")
	}
	if !linktest.Eventually(func() bool { return rig.Agent.State() == link.StateRefused }) {
		t.Fatalf("the agent must see the refusal on its redial, state %v", rig.Agent.State())
	}
	if rig.Store.Online(rig.Machine) {
		t.Fatal("the store must not call a revoked machine online")
	}
}

func TestDeadPeerIsDetectedByKeepalive(t *testing.T) {
	rig, _ := newRig(t, func(o *linktest.Options) {
		o.Timing = linktest.FastTiming()
		o.Timing.KeepAlive = 100 * time.Millisecond
	})
	link1 := rig.Hub.Link(rig.Machine)
	rig.Blackhole()
	if !linktest.Eventually(func() bool { _, ok := link1.Info(); return !ok }) {
		t.Fatal("a silent network must be noticed by keepalive and the link dropped")
	}
	if !linktest.Eventually(func() bool { return !rig.Store.Online(rig.Machine) }) {
		t.Fatal("the store must be told the machine went offline")
	}
}

func TestFiniteOpFinishesWhileTheAgentRelinksToTheNewHub(t *testing.T) {
	rig, w := newRig(t, nil)
	ctx := deadline(t, 10*time.Second)
	old := rig.Hub
	reply := make(chan text, 1)
	go func() {
		got, err := link.Call(ctx, old.Link(rig.Machine), gate, text{"job"})
		if err != nil {
			t.Error(err)
		}
		reply <- got
	}()
	<-w.started

	next, drained := rig.Deploy(old)
	rig.WaitLinked(next)
	select {
	case got := <-reply:
		t.Fatalf("the op must still be running on the old link: %v", got)
	case err := <-drained:
		t.Fatalf("the old hub must wait for the finite op, drain returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := link.Call(ctx, old.Link(rig.Machine), echo, text{}); !errors.Is(err, link.ErrDraining) {
		t.Fatalf("a draining hub takes no new ops: %v", err)
	}
	if got, err := link.Call(ctx, next.Link(rig.Machine), echo, text{"new"}); err != nil || got.S != "new" {
		t.Fatalf("new ops go to the new hub: %v %v", got, err)
	}

	close(w.release)
	if got := <-reply; got.S != "released job" {
		t.Fatalf("the finite op must finish on the link it started on: %v", got)
	}
	if err := <-drained; err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestLateOfflineFromTheOldLinkDoesNotMarkTheNewOneOffline(t *testing.T) {
	rig, _ := newRig(t, nil)
	old := rig.Hub
	next, drained := rig.Deploy(old)
	rig.WaitLinked(next)
	if err := <-drained; err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !linktest.Eventually(func() bool { _, ok := old.Link(rig.Machine).Info(); return !ok }) {
		t.Fatal("the old hub must let go of its link")
	}
	time.Sleep(100 * time.Millisecond)
	if !rig.Store.Online(rig.Machine) || rig.Store.Gen(rig.Machine) != 2 {
		t.Fatalf("the old link's offline arrived late and must change nothing: online=%v gen=%d", rig.Store.Online(rig.Machine), rig.Store.Gen(rig.Machine))
	}
}

func TestCallDuringTheDeployOverlapWaitsForTheLinkToMove(t *testing.T) {
	rig, _ := newRig(t, nil)
	old := rig.Hub
	next := rig.NextHub()
	got := make(chan error, 1)
	go func() {
		_, err := link.Call(deadline(t, 10*time.Second), next.Link(rig.Machine), echo, text{"x"})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a call for a machine whose link has not moved yet must wait, got %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	go func() { _ = old.Drain(deadline(t, 5*time.Second)) }()
	if err := <-got; err != nil {
		t.Fatalf("the call must complete once the agent relinks: %v", err)
	}
}

func TestCallOnANewServerWaitsForAMachineStillLinkedToTheOldOne(t *testing.T) {
	rig, _ := newRig(t, func(o *linktest.Options) {
		o.Timing = linktest.FastTiming()
		o.Timing.LinkWait = 500 * time.Millisecond
	})
	old := rig.Hub
	next := rig.NextHub()
	time.Sleep(600 * time.Millisecond)
	got := make(chan error, 1)
	go func() {
		_, err := link.Call(deadline(t, 10*time.Second), next.Link(rig.Machine), echo, text{"x"})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a machine the store says is linked elsewhere must be waited for, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	go func() { _ = old.Drain(deadline(t, 5*time.Second)) }()
	if err := <-got; err != nil {
		t.Fatalf("the call must complete once the agent relinks: %v", err)
	}
}

func TestCallFailsAtOnceForAMachineOfflineLongerThanTheLinkWait(t *testing.T) {
	rig, _ := newRig(t, func(o *linktest.Options) {
		o.Timing = linktest.FastTiming()
		o.Timing.LinkWait = 150 * time.Millisecond
	})
	rig.Stop()
	if !linktest.Eventually(func() bool { return !rig.Store.Online(rig.Machine) }) {
		t.Fatal("the store must learn the machine went offline")
	}
	l := rig.Hub.Link(rig.Machine)
	start := clock.Now()
	if _, err := link.Call(deadline(t, 5*time.Second), l, echo, text{}); !errors.Is(err, link.ErrOffline) {
		t.Fatalf("got %v", err)
	}
	if took := clock.Now().Sub(start); took < 50*time.Millisecond || took > 2*time.Second {
		t.Fatalf("a machine that just dropped is waited for, up to the link wait: %v", took)
	}
	time.Sleep(200 * time.Millisecond)
	start = clock.Now()
	_, err := link.Call(deadline(t, 5*time.Second), l, echo, text{})
	if !errors.Is(err, link.ErrOffline) || clock.Now().Sub(start) > 50*time.Millisecond {
		t.Fatalf("after the link wait an offline machine fails at once: %v in %v", err, clock.Now().Sub(start))
	}
}

func TestCallFailsAtOnceForAMachineThatNeverLinked(t *testing.T) {
	rig, _ := newRig(t, nil)
	start := clock.Now()
	if _, err := link.Call(deadline(t, 5*time.Second), rig.Hub.Link("nobody"), echo, text{}); !errors.Is(err, link.ErrOffline) {
		t.Fatalf("got %v", err)
	}
	if clock.Now().Sub(start) > 50*time.Millisecond {
		t.Fatal("a machine the store never saw has no link to wait for")
	}
}

func TestCallStopsWaitingWhenItsContextEnds(t *testing.T) {
	rig, _ := newRig(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := link.Call(ctx, rig.NextHub().Link(rig.Machine), echo, text{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestResumableOpsAreReopenedOnTheNewLink(t *testing.T) {
	rig, w := newRig(t, nil)
	old := rig.Hub
	ctx := deadline(t, 10*time.Second)

	var uses atomic.Int32
	held := make(chan error, 1)
	go func() {
		held <- link.Hold(ctx, old.Link(rig.Machine), watch, text{"box-1"}, func(_ context.Context, _ struct{ Opens int }, s link.Stream) {
			uses.Add(1)
			_, _ = io.Copy(io.Discard, s)
		})
	}()
	agentHold := make(chan error, 1)
	go func() {
		agentHold <- link.Hold(ctx, rig.Agent.Server(), report, none{}, func(_ context.Context, _ none, s link.Stream) {
			_, _ = io.Copy(io.Discard, s)
		})
	}()
	if !linktest.Eventually(func() bool { return uses.Load() == 1 && len(w.reportGens()) == 1 }) {
		t.Fatal("both resumable ops must open")
	}

	next, drained := rig.Deploy(old)
	rig.WaitLinked(next)
	if err := <-held; !errors.Is(err, link.ErrDraining) {
		t.Fatalf("a hold on a draining server ends, the next server holds again: %v", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("a resumable op must not hold a drain open: %v", err)
	}

	go func() {
		_ = link.Hold(ctx, next.Link(rig.Machine), watch, text{"box-1"}, func(context.Context, struct{ Opens int }, link.Stream) {})
	}()
	if !linktest.Eventually(func() bool { return w.watcherOpens("box-1") == 2 }) {
		t.Fatalf("the new server must reopen the op, got %d opens", w.watcherOpens("box-1"))
	}
	if w.createdStates() != 1 {
		t.Fatalf("the handler must adopt box-1's state on the reopen, not create it again: %d states", w.createdStates())
	}
	if !linktest.Eventually(func() bool { g := w.reportGens(); return len(g) >= 2 && g[len(g)-1] == 2 }) {
		t.Fatalf("the agent's own hold must reopen on the new hub's link: %v", w.reportGens())
	}
}

func (w *world) watcherOpens(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watchers[id]
}

func (w *world) createdStates() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.created
}

func TestKickOfAnOfflineMachineDoesNothing(t *testing.T) {
	rig, _ := newRig(t, nil)
	rig.Hub.Kick("nobody")
	if got, err := link.Call(deadline(t, 5*time.Second), rig.Hub.Link(rig.Machine), echo, text{"ok"}); err != nil || got.S != "ok" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestToolMachineGetsExecOnlyWhenItsOwnerOptedIn(t *testing.T) {
	execCat := link.NewCatalogue()
	exec := link.NewOp[none, none](execCat, "t.exec", link.ServedByAgent, link.CapExec)
	for _, c := range []struct {
		name string
		m    domain.Machine
		ok   bool
	}{
		{"sandbox machine", domain.Machine{Role: domain.RoleSandbox, Slots: 1}, false},
		{"tool machine without the opt-in", domain.Machine{Role: domain.RoleTool, Slots: 1}, false},
		{"tool machine that opted in", domain.Machine{Role: domain.RoleTool, Slots: 1, Exec: true}, true},
	} {
		agent := link.NewRegistry(execCat, link.ServedByAgent, link.CapSet(0).With(link.CapExec))
		link.Handle(agent, exec, func(context.Context, link.Meta, none) (none, error) { return none{}, nil })
		rig := linktest.New(t, linktest.Options{Server: link.NewRegistry(execCat, link.ServedByServer, 0), Agent: agent, Machine: c.m})
		_, err := link.Call(deadline(t, 5*time.Second), rig.Hub.Link(rig.Machine), exec, none{})
		if (err == nil) != c.ok || (!c.ok && !errors.Is(err, &link.Error{Code: link.CodeForbidden})) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestASecondLinkForTheSameMachineReplacesTheFirst(t *testing.T) {
	rig, _ := newRig(t, func(o *linktest.Options) {
		o.Backoff = linktest.FastBackoff()
		o.Backoff.Base, o.Backoff.Max, o.Backoff.StableAfter = 2*time.Second, 2*time.Second, time.Minute
	})
	reg := link.NewRegistry(cat, link.ServedByAgent, 0)
	link.Handle(reg, echo, func(context.Context, link.Meta, text) (text, error) { return text{"second"}, nil })
	link.Handle(reg, gate, func(context.Context, link.Meta, text) (text, error) { return text{}, nil })
	link.HandleResumable(reg, watch, func(context.Context, link.Meta, text) (struct{ Opens int }, link.Bytes, error) {
		return struct{ Opens int }{}, nil, nil
	})
	agent, err := link.NewAgent(link.AgentConfig{URL: rig.URL, ID: rig.Machine, Key: rig.Key, Reg: reg, Timing: linktest.FastTiming(), Backoff: linktest.FastBackoff()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	if !linktest.Eventually(func() bool {
		got, err := link.Call(ctx, rig.Hub.Link(rig.Machine), echo, text{})
		return err == nil && got.S == "second"
	}) {
		t.Fatal("calls must reach the machine's newest link")
	}
	if info, _ := rig.Hub.Link(rig.Machine).Info(); info.Gen < 2 {
		t.Fatalf("the second link has the next generation: %+v", info)
	}
	if !rig.Store.Online(rig.Machine) {
		t.Fatal("replacing a link must not mark the machine offline")
	}
}

func TestRelinkWaitsForTheNewHubWhenTheOldOneDrainsFirst(t *testing.T) {
	rig, _ := newRig(t, nil)
	old := rig.Hub
	drained := make(chan error, 1)
	go func() { drained <- old.Drain(deadline(t, 5*time.Second)) }()
	time.Sleep(150 * time.Millisecond)
	st, err := link.Call(deadline(t, 5*time.Second), rig.Agent.Server(), status, none{})
	if err != nil || st.Machine != rig.Machine {
		t.Fatalf("an agent op while the old link drains must still be served: %v %v", st, err)
	}
	next := rig.NextHub()
	rig.WaitLinked(next)
	if err := <-drained; err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got, err := link.Call(deadline(t, 5*time.Second), next.Link(rig.Machine), echo, text{"ok"}); err != nil || got.S != "ok" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestAFiniteOpStillRunningAtTheDrainBudgetIsCutAndNotReplayed(t *testing.T) {
	rig, w := newRig(t, nil)
	old := rig.Hub
	ctx := deadline(t, 10*time.Second)
	called := make(chan error, 1)
	go func() {
		_, err := link.Call(ctx, old.Link(rig.Machine), gate, text{"stuck"})
		called <- err
	}()
	<-w.started

	next := rig.NextHub()
	budget, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := old.Drain(budget); err == nil {
		t.Fatal("drain must say a link was cut when a finite op outlives the budget")
	}
	if err := <-called; !errors.Is(err, link.ErrLinkLost) {
		t.Fatalf("the cut op's caller learns the link was lost: %v", err)
	}
	rig.WaitLinked(next)
	select {
	case id := <-w.started:
		t.Fatalf("a cut op must never be replayed, it started again as %q", id)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAgentShutdownLetsAFiniteOpFinish(t *testing.T) {
	rig, w := newRig(t, nil)
	ctx := deadline(t, 10*time.Second)
	reply := make(chan text, 1)
	go func() {
		got, err := link.Call(ctx, rig.Hub.Link(rig.Machine), gate, text{"job"})
		if err != nil {
			t.Error(err)
		}
		reply <- got
	}()
	<-w.started
	stopped := make(chan struct{})
	go func() { rig.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("the agent must wait for a finite op, up to the drain budget")
	case <-time.After(150 * time.Millisecond):
	}
	close(w.release)
	if got := <-reply; got.S != "released job" {
		t.Fatalf("the op must finish before the agent lets go: %v", got)
	}
	<-stopped
}

func TestHoldOnAnOfflineMachineReopensAsSoonAsItLinks(t *testing.T) {
	rig, w := newRig(t, func(o *linktest.Options) {
		o.Timing = linktest.FastTiming()
		o.Timing.LinkWait = 50 * time.Millisecond
		o.Backoff = linktest.FastBackoff()
		o.Backoff.Base, o.Backoff.Max = 10*time.Second, time.Minute
	})
	rig.Stop()
	if !linktest.Eventually(func() bool { return !rig.Store.Online(rig.Machine) }) {
		t.Fatal("the store must learn the machine went offline")
	}
	time.Sleep(100 * time.Millisecond)
	l := rig.Hub.Link(rig.Machine)
	ctx := deadline(t, 10*time.Second)
	go func() {
		_ = link.Hold(ctx, l, watch, text{"box-1"}, func(_ context.Context, _ struct{ Opens int }, s link.Stream) { _, _ = io.Copy(io.Discard, s) })
	}()
	time.Sleep(100 * time.Millisecond)

	rig.Restart()
	start := clock.Now()
	if !linktest.Eventually(func() bool { return w.watcherOpens("box-1") == 1 }) || clock.Now().Sub(start) > 2*time.Second {
		t.Fatalf("a hold waiting out its backoff must reopen when the machine links, opens %d after %v", w.watcherOpens("box-1"), clock.Now().Sub(start))
	}
}
