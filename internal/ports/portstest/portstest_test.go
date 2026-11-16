package portstest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

var t0 = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

func queued(t *testing.T, s *portstest.Store, owner domain.Login) (domain.Task, ports.Commit) {
	t.Helper()
	id, err := s.NewTaskID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res, err := domain.New(id, owner, domain.KindInteractive, domain.Spec{Repo: "acme/app"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return res.Task, ports.Commit{Task: res.Task.Record(), Entries: res.Timeline}
}

func TestStoreCommitsOnlyOnTheVersionItWasGiven(t *testing.T) {
	ctx := context.Background()
	s := portstest.NewStore()
	task, c := queued(t, s, "ana")
	if _, err := s.Commit(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, c); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("inserting twice: %v", err)
	}
	res, err := domain.Apply(task, domain.SlotGranted{Machine: "m1"}, t0, domain.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	next := ports.Commit{Prev: 9, Task: res.Task.Record()}
	if _, err := s.Commit(ctx, next); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("a stale version: %v", err)
	}
	next.Prev = 1
	if _, err := s.Commit(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, task.ID)
	if err != nil || got.Version != 2 || got.Status() != domain.StatusStarting {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	if _, err := s.Load(ctx, 99); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := s.Commit(ctx, ports.Commit{Prev: 2, Task: res.Task.Record()}); err == nil {
		t.Fatal("a record that does not follow its predecessor was stored")
	}
}

func TestStoreForcedFaults(t *testing.T) {
	ctx := context.Background()
	s := portstest.NewStore()
	_, c := queued(t, s, "ana")
	boom := errors.New("boom")
	s.ConflictNext(1)
	s.FailCommits(1, boom)
	if _, err := s.Commit(ctx, c); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.Commit(ctx, c); !errors.Is(err, boom) {
		t.Fatalf("second: %v", err)
	}
	c.Outbox = []ports.OutboxRow{{Kind: ports.RowStopBox}}
	ids, err := s.Commit(ctx, c)
	if err != nil || len(ids) != 1 {
		t.Fatalf("third: %v %v", ids, err)
	}
	s.FailDones(1, boom)
	if err := s.Done(ctx, ids[0]); !errors.Is(err, boom) {
		t.Fatalf("done: %v", err)
	}
	if rows, _ := s.Pending(ctx); len(rows) != 1 {
		t.Fatalf("pending %v", rows)
	}
	if err := s.Done(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.Pending(ctx); len(rows) != 0 {
		t.Fatalf("pending %v", rows)
	}
}

func filled(t *testing.T) *portstest.Store {
	t.Helper()
	s := portstest.NewStore()
	for _, who := range []domain.Login{"ana", "bo", "ana"} {
		_, c := queued(t, s, who)
		if _, err := s.Commit(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestStoreQueries(t *testing.T) {
	ctx := context.Background()
	s := filled(t)
	mine, _ := s.Tasks(ctx, ports.TaskQuery{Owner: "ana"})
	if len(mine) != 2 || mine[0].ID != 3 {
		t.Fatalf("newest first: %+v", mine)
	}
	page, _ := s.Tasks(ctx, ports.TaskQuery{Before: 3, Limit: 1})
	if len(page) != 1 || page[0].ID != 2 {
		t.Fatalf("page %+v", page)
	}
	if live, _ := s.Live(ctx); len(live) != 3 {
		t.Fatalf("live %d", len(live))
	}
	if tl, _ := s.Timeline(ctx, 1, 0); len(tl) != 1 {
		t.Fatalf("timeline %v", tl)
	}
	if tl, _ := s.Timeline(ctx, 1, 1); len(tl) != 0 {
		t.Fatalf("timeline after the end %v", tl)
	}
}

func TestStoreCrashKeepsWhatWasCommitted(t *testing.T) {
	ctx := context.Background()
	s := filled(t)
	after := s.Crash()
	_, c := queued(t, after, "cy")
	if _, err := after.Commit(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Tasks(ctx, ports.TaskQuery{}); len(got) != 3 {
		t.Fatalf("the crashed store changed its source: %d tasks", len(got))
	}
	if got, _ := after.Tasks(ctx, ports.TaskQuery{}); len(got) != 4 || got[0].ID != 4 {
		t.Fatalf("the restarted store lost state or reused an id: %+v", got)
	}
}

func TestStoreWakeIndexAndLiveEndedWakes(t *testing.T) {
	ctx := context.Background()
	s := portstest.NewStore()
	if _, ok, _ := s.NextWake(ctx); ok {
		t.Fatal("an empty store has a wake")
	}
	task, c := queued(t, s, "ana")
	ended := task
	ended.Version, ended.AgentRun, ended.BoxGen, ended.NextAsk = 1, 1, 1, 3
	ended.Life = domain.Ended{Outcome: domain.OutcomeDone, At: t0, ReleaseAt: t0.Add(time.Hour), Ensure: 3}
	c.Task = ended.Record()
	if _, err := s.Commit(ctx, c); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.Live(ctx); len(live) != 1 {
		t.Fatal("an ended task still waiting for a wake's answer must be live")
	}
	next, ok, _ := s.NextWake(ctx)
	if !ok || !next.Equal(t0.Add(time.Hour)) {
		t.Fatalf("next wake %v %v", next, ok)
	}
	if due, _ := s.Due(ctx, t0); len(due) != 0 {
		t.Fatalf("due early: %v", due)
	}
	if due, _ := s.Due(ctx, t0.Add(time.Hour)); len(due) != 1 {
		t.Fatalf("due: %v", due)
	}
}

func TestStoreAppliesATemplateRowAndMachines(t *testing.T) {
	ctx := context.Background()
	s := portstest.NewStore()
	_, c := queued(t, s, "ana")
	c.Template = &ports.TemplateRow{Repo: "acme/app", Template: ports.Template{Handle: "ready-1", Machine: "m2"}, At: t0}
	if _, err := s.Commit(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Repo(ctx, "acme/app")
	if r.ReadyImage != "ready-1" || r.ReadyOn != "m2" || !r.ReadyAt.Equal(t0) {
		t.Fatalf("repo %+v", r)
	}
	if r, _ := s.Repo(ctx, "acme/none"); r.Name != "acme/none" || r.ReadyImage != "" {
		t.Fatalf("unknown repo %+v", r)
	}
	s.SetRepo(domain.Repo{Name: "acme/none", Notes: "n"})
	s.AddMachine(domain.Machine{ID: "m1"}, false)
	s.SetOnline("m1", true)
	if ms, _ := s.Machines(ctx); len(ms) != 1 || !ms[0].Online {
		t.Fatalf("machines %+v", ms)
	}
}

func TestStoreLeaseIsHeldByOneAtATime(t *testing.T) {
	s := portstest.NewStore()
	release, err := s.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := s.Lease(ctx)
		got <- err
	}()
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("a second lease: %v", err)
	}
	release()
	release()
	if release2, err := s.Lease(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		release2()
	}
}

func target(task domain.TaskID, run domain.AgentRun, gen domain.BoxGen) ports.Target {
	return ports.Target{Task: task, Machine: "m1", Run: run, Gen: gen}
}

func TestBoxesConvergeAndTheStateAroundIt(t *testing.T) {
	ctx := context.Background()
	b := portstest.NewBoxes()
	var phases []string
	plan := ports.Plan{Target: target(1, 1, 1), Repos: []domain.RepoName{"acme/app"}}
	got, err := b.Converge(ctx, plan, func(phase, _ string) { phases = append(phases, phase) })
	if err != nil || !got.Revived || len(phases) != 2 {
		t.Fatalf("converge %+v %v %v", got, err, phases)
	}
	if got, _ := b.Converge(ctx, plan, nil); got.Revived {
		t.Fatal("a retried converge under a live terminal must not be a revival")
	}
	if st, _ := b.Inspect(ctx, plan.Target); !st.Running {
		t.Fatal("converged box is not running")
	}
	if st, _ := b.Inspect(ctx, target(1, 1, 2)); st.Running {
		t.Fatal("another generation reads as running")
	}
	_ = b.Kill(ctx, plan.Target)
	if got, _ := b.Converge(ctx, plan, nil); !got.Revived {
		t.Fatal("converge after a kill must revive")
	}
}

func TestBoxesStopInspectAndTheRest(t *testing.T) {
	ctx := context.Background()
	b := portstest.NewBoxes()
	plan := ports.Plan{Target: target(1, 1, 1), Repos: []domain.RepoName{"acme/app"}}
	_, _ = b.Converge(ctx, plan, nil)
	_ = b.Stop(ctx, plan.Target)
	if st, _ := b.Inspect(ctx, plan.Target); st.Running {
		t.Fatal("stopped box is running")
	}
	b.SetInspect(1, ports.BoxState{Running: true, OOMKilled: true})
	if st, _ := b.Inspect(ctx, plan.Target); !st.OOMKilled {
		t.Fatal("override ignored")
	}
	_ = b.Release(ctx, plan.Target)
	b.SetObservation(1, domain.AgentObservation{Present: true})
	if o, _ := b.Observe(ctx, plan.Target); !o.Present {
		t.Fatal("observation ignored")
	}
	if tpl, _ := b.SaveTemplate(ctx, plan.Target); tpl.Handle != "ready-1" || tpl.Machine != "m1" {
		t.Fatalf("template %+v", tpl)
	}
	_ = b.Type(ctx, plan.Target, "hi")
	_ = b.Interrupt(ctx, plan.Target)
	if n := len(b.Calls()); n != 9 {
		t.Fatalf("%d calls recorded", n)
	}
}

func TestBoxesFilesAndFailures(t *testing.T) {
	ctx := context.Background()
	b := portstest.NewBoxes()
	tg := target(1, 1, 1)
	if ok, _ := b.Exists(ctx, tg, "/x"); ok {
		t.Fatal("file before touch")
	}
	if _, err := b.ReadFile(ctx, tg, "/x"); err == nil {
		t.Fatal("read of a missing file")
	}
	b.Touch(1, "/x")
	if data, err := b.ReadFile(ctx, tg, "/x"); err != nil || string(data) != "/x" {
		t.Fatalf("read %q %v", data, err)
	}
	_ = b.Remove(ctx, tg, "/x")
	if ok, _ := b.Exists(ctx, tg, "/x"); ok {
		t.Fatal("file after remove")
	}
	boom := errors.New("boom")
	b.Fail("Exists", boom)
	if _, err := b.Exists(ctx, tg, "/x"); !errors.Is(err, boom) {
		t.Fatalf("exists: %v", err)
	}
	if _, err := b.ReadFile(ctx, tg, "/x"); !errors.Is(err, boom) {
		t.Fatalf("read: %v", err)
	}
}

func TestBoxesGateAndStall(t *testing.T) {
	b := portstest.NewBoxes()
	plan := ports.Plan{Target: target(1, 1, 1), Repos: []domain.RepoName{"acme/app"}}
	b.Hold(1)
	done := make(chan error, 1)
	go func() { _, err := b.Converge(context.Background(), plan, nil); done <- err }()
	if c := <-b.Entered(); c.Method != "Converge" {
		t.Fatalf("entered %+v", c)
	}
	boom := errors.New("boom")
	b.Let(1, boom)
	if err := <-done; !errors.Is(err, boom) {
		t.Fatalf("held converge: %v", err)
	}
	b.Let(1, nil)

	ctx, cancel := context.WithCancel(context.Background())
	b.Hold(1)
	go func() { _, err := b.Converge(ctx, plan, nil); done <- err }()
	<-b.Entered()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled converge: %v", err)
	}

	b.Stall("Kill")
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { done <- b.Kill(ctx2, plan.Target) }()
	if c := <-b.Entered(); c.Method != "Kill" {
		t.Fatalf("entered %+v", c)
	}
	cancel2()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stalled kill: %v", err)
	}
}

func TestBoxesSignalWaitsForAWatchAndStopsAfterIt(t *testing.T) {
	b := portstest.NewBoxes()
	got := make(chan domain.TaskID, 2)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		_ = b.Watch(ctx, "m1", func(task domain.TaskID, _ domain.Signal) { got <- task })
		close(stopped)
	}()
	b.Signal("m1", 7, domain.AgentExited{})
	if task := <-got; task != 7 {
		t.Fatalf("task %d", task)
	}
	cancel()
	<-stopped
	b.Signal("m1", 8, domain.AgentExited{})
	select {
	case task := <-got:
		t.Fatalf("a stopped watch still got task %d", task)
	default:
	}
}

func TestAgentAndSecrets(t *testing.T) {
	ctx := context.Background()
	a := &portstest.Agent{}
	f := ports.LaunchFor{Target: target(1, 2, 1), Owner: "ana", Prompt: "go"}
	l, err := a.Launch(ctx, f)
	if err != nil || l.Argv[len(l.Argv)-1] != "go" || l.Env["AGENT_KEY"] == "" || len(l.Home) != 1 {
		t.Fatalf("launch %+v %v", l, err)
	}
	f.Resume = true
	if l, _ := a.Launch(ctx, f); l.Argv[len(l.Argv)-1] != "--resume" {
		t.Fatalf("resume argv %v", l.Argv)
	}
	boom := errors.New("boom")
	a.FailLaunch(boom)
	if _, err := a.Launch(ctx, f); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	a.SetSummary("done")
	a.FailSummary(boom)
	if s, err := a.LastMessage(ctx, nil, f.Target); s != "done" || !errors.Is(err, boom) {
		t.Fatalf("%q %v", s, err)
	}
	s := &portstest.Secrets{}
	if p, err := s.Placeholders(ctx, "ana", nil); err != nil || p["GH_TOKEN"] == "" {
		t.Fatalf("%v %v", p, err)
	}
	s.Fail(boom)
	if _, err := s.Placeholders(ctx, "ana", nil); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
