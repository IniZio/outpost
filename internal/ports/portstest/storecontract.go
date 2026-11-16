package portstest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// StoreRig is a store under test with the seeding the ports leave out. The fake and the Postgres
// adapter each build one, and StoreContract runs the same checks on both.
type StoreRig struct {
	Store ports.Store
	// AddMachine adds a machine row, linked or not, and returns the id the store gave it.
	AddMachine func(m domain.Machine, online bool) domain.MachineID
	// SetRepo adds or replaces a repo row.
	SetRepo func(r domain.Repo)
}

// StoreRigFor wraps the in-memory Store.
func StoreRigFor(s *Store) StoreRig {
	return StoreRig{
		Store: s,
		AddMachine: func(m domain.Machine, online bool) domain.MachineID {
			s.AddMachine(m, online)
			return m.ID
		},
		SetRepo: s.SetRepo,
	}
}

var contractT0 = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

// StoreContract checks the behaviour ports.Store promises. Every case gets a fresh rig, so a store
// that isolates each one (a schema or a database) lets them run in parallel.
func StoreContract(t *testing.T, newRig func(t *testing.T) StoreRig) {
	t.Helper()
	cases := map[string]func(*testing.T, StoreRig){
		"CAS":              contractCAS,
		"ConflictWrites":   contractConflictWritesNothing,
		"RacingWriters":    contractRacingWriters,
		"Timeline":         contractTimeline,
		"Outbox":           contractOutbox,
		"Due":              contractDue,
		"Live":             contractLive,
		"Tasks":            contractTasks,
		"TemplateAndRepo":  contractRepo,
		"Machines":         contractMachines,
		"TaskIDs":          contractTaskIDs,
		"Lease":            contractLease,
		"LeaseCancelledIn": contractLeaseCancel,
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, newRig(t))
		})
	}
}

func baseTask(id domain.TaskID, owner domain.Login, life domain.Life) domain.Task {
	return domain.Task{
		ID: id, Version: 1, Owner: owner, Kind: domain.KindInteractive,
		Spec:      domain.Spec{Repo: "acme/app", Also: []domain.RepoName{"acme/lib"}, Title: "t", Timeout: time.Hour},
		CreatedAt: contractT0, NextAsk: 9, AgentRun: 2, BoxGen: 2, Life: life,
	}
}

func queuedTask(id domain.TaskID, owner domain.Login) domain.Task {
	return baseTask(id, owner, domain.Queued{})
}

func closingTask(id domain.TaskID, until time.Time) domain.Task {
	return baseTask(id, "ana", domain.Closing{
		Want: domain.OutcomeDone, From: domain.StatusWorking, Step: domain.CloseSummary, Ask: 1, Until: until,
	})
}

func endedTask(id domain.TaskID, ensure domain.AskID) domain.Task {
	return baseTask(id, "ana", domain.Ended{Outcome: domain.OutcomeDone, At: contractT0, Ensure: ensure})
}

func insert(t *testing.T, s ports.Store, task domain.Task, entries ...domain.Entry) []ports.OutboxID {
	t.Helper()
	ids, err := s.Commit(context.Background(), ports.Commit{Task: task.Record(), Entries: entries})
	if err != nil {
		t.Fatalf("insert task %d: %v", task.ID, err)
	}
	return ids
}

func entry(i int) domain.Entry {
	return domain.Entry{At: contractT0.Add(time.Duration(i) * time.Second), Kind: domain.EntryPhase, Text: "step " + string(rune('a'+i))}
}

func contractCAS(t *testing.T, r StoreRig) {
	ctx := context.Background()
	task := queuedTask(1, "ana")
	insert(t, r.Store, task)
	if _, err := r.Store.Commit(ctx, ports.Commit{Task: task.Record()}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("inserting an id twice: %v", err)
	}
	next := closingTask(1, contractT0.Add(time.Minute))
	next.Version = 2
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 7, Task: next.Record()}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("a stale version: %v", err)
	}
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 5, Task: baseTask(5, "ana", domain.Queued{}).Record()}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("an update of a task that does not exist: %v", err)
	}
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: task.Record()}); err == nil || errors.Is(err, ports.ErrConflict) {
		t.Fatalf("a record that does not follow its predecessor: %v", err)
	}
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: next.Record()}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Store.Load(ctx, 1)
	if err != nil || got.Version != 2 || got.Status() != domain.StatusWorking || got.Spec.Also[0] != "acme/lib" || !got.CreatedAt.Equal(contractT0) {
		t.Fatalf("loaded %+v, %v", got, err)
	}
	if _, err := r.Store.Load(ctx, 99); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}

func contractConflictWritesNothing(t *testing.T, r StoreRig) {
	ctx := context.Background()
	insert(t, r.Store, queuedTask(1, "ana"), entry(0))
	bad := ports.Commit{
		Prev: 4, Task: closingTask(1, contractT0.Add(time.Minute)).Record(),
		Entries:  []domain.Entry{entry(1)},
		Outbox:   []ports.OutboxRow{{Kind: ports.RowStopBox, Target: ports.Target{Task: 1}}},
		Template: &ports.TemplateRow{Repo: "acme/app", Template: ports.Template{Handle: "img", Machine: "m1"}, At: contractT0},
	}
	bad.Task.Version = 5
	if _, err := r.Store.Commit(ctx, bad); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("got %v, want a conflict", err)
	}
	got, _ := r.Store.Load(ctx, 1)
	entries, _ := r.Store.Timeline(ctx, 1, 0)
	pending, _ := r.Store.Pending(ctx)
	repo, _ := r.Store.Repo(ctx, "acme/app")
	if got.Version != 1 || len(entries) != 1 || len(pending) != 0 || repo.ReadyImage != "" {
		t.Fatalf("a conflict wrote: version %d, %d entries, %d outbox rows, repo %+v", got.Version, len(entries), len(pending), repo)
	}
}

func contractRacingWriters(t *testing.T, r StoreRig) {
	ctx := context.Background()
	insert(t, r.Store, queuedTask(1, "ana"))
	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			next := closingTask(1, contractT0.Add(time.Duration(i+1)*time.Minute))
			next.Version = 2
			_, results[i] = r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: next.Record(), Entries: []domain.Entry{entry(i)}})
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ports.ErrConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	entries, _ := r.Store.Timeline(ctx, 1, 0)
	if wins != 1 || len(entries) != 1 {
		t.Fatalf("%d writers won and left %d entries, want 1 and 1", wins, len(entries))
	}
}

func contractTimeline(t *testing.T, r StoreRig) {
	ctx := context.Background()
	insert(t, r.Store, queuedTask(1, "ana"), entry(0), entry(1))
	next := closingTask(1, contractT0.Add(time.Minute))
	next.Version = 2
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: next.Record(), Entries: []domain.Entry{entry(2)}}); err != nil {
		t.Fatal(err)
	}
	all, _ := r.Store.Timeline(ctx, 1, 0)
	if len(all) != 3 || all[0].Text != "step a" || all[2].Text != "step c" || !all[1].At.Equal(entry(1).At) || all[1].Kind != domain.EntryPhase {
		t.Fatalf("timeline %+v", all)
	}
	for skip, want := range map[int]int{-1: 3, 1: 2, 2: 1, 3: 0, 9: 0} {
		if got, _ := r.Store.Timeline(ctx, 1, skip); len(got) != want {
			t.Errorf("skip %d: %d entries, want %d", skip, len(got), want)
		}
	}
	if got, _ := r.Store.Timeline(ctx, 42, 0); len(got) != 0 {
		t.Fatalf("an unknown task has a timeline: %+v", got)
	}
}

func contractOutbox(t *testing.T, r StoreRig) {
	ctx := context.Background()
	tgt := ports.Target{Task: 1, Machine: "m1", Run: 2, Gen: 3}
	rows := []ports.OutboxRow{
		{Kind: ports.RowType, Target: tgt, Text: "hello"},
		{Kind: ports.RowInterrupt, Target: tgt},
		{Kind: ports.RowRelease, Target: tgt},
	}
	task := queuedTask(1, "ana")
	ids, err := r.Store.Commit(ctx, ports.Commit{Task: task.Record(), Outbox: rows})
	if err != nil || len(ids) != 3 || !slices.IsSorted(ids) || ids[0] == ids[2] {
		t.Fatalf("ids %v, %v", ids, err)
	}
	pending, _ := r.Store.Pending(ctx)
	if len(pending) != 3 || pending[0] != (ports.OutboxRow{ID: ids[0], Kind: ports.RowType, Target: tgt, Text: "hello"}) || pending[2].Kind != ports.RowRelease {
		t.Fatalf("pending %+v", pending)
	}
	for range 2 {
		if err := r.Store.Done(ctx, ids[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ = r.Store.Pending(ctx)
	if len(pending) != 2 || pending[0].ID != ids[0] || pending[1].ID != ids[2] {
		t.Fatalf("after Done: %+v", pending)
	}
	outboxIDsKeepRising(t, r, ids[2], rows[:1])
}

func outboxIDsKeepRising(t *testing.T, r StoreRig, last ports.OutboxID, rows []ports.OutboxRow) {
	t.Helper()
	ctx := context.Background()
	next := closingTask(1, contractT0.Add(time.Minute))
	next.Version = 2
	more, _ := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: next.Record(), Outbox: rows})
	if len(more) != 1 || more[0] <= last {
		t.Fatalf("ids do not keep rising: %v after %v", more, last)
	}
}

func contractDue(t *testing.T, r StoreRig) {
	ctx := context.Background()
	if _, ok, err := r.Store.NextWake(ctx); ok || err != nil {
		t.Fatalf("empty store has a wake: %v %v", ok, err)
	}
	soon, later := contractT0.Add(time.Minute), contractT0.Add(time.Hour)
	insert(t, r.Store, closingTask(1, later))
	insert(t, r.Store, closingTask(2, soon))
	insert(t, r.Store, closingTask(3, soon))
	insert(t, r.Store, queuedTask(4, "ana"))
	if got, _ := r.Store.Due(ctx, soon.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("due before any wake: %v", got)
	}
	if got, _ := r.Store.Due(ctx, soon); !slices.Equal(got, []domain.TaskID{2, 3}) {
		t.Fatalf("due at the wake instant: %v", got)
	}
	if got, _ := r.Store.Due(ctx, later); !slices.Equal(got, []domain.TaskID{2, 3, 1}) {
		t.Fatalf("due later: %v", got)
	}
	if at, ok, _ := r.Store.NextWake(ctx); !ok || !at.Equal(soon) {
		t.Fatalf("next wake %v %v", at, ok)
	}
	ended := endedTask(2, 0)
	ended.Version = 2
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: ended.Record()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Due(ctx, later); !slices.Equal(got, []domain.TaskID{3, 1}) {
		t.Fatalf("an ended task with no wake is still due: %v", got)
	}
}

func contractLive(t *testing.T, r StoreRig) {
	ctx := context.Background()
	insert(t, r.Store, endedTask(1, 0))
	insert(t, r.Store, queuedTask(2, "ana"))
	insert(t, r.Store, endedTask(3, 4))
	insert(t, r.Store, closingTask(4, contractT0.Add(time.Minute)))
	live, err := r.Store.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []domain.TaskID
	for _, task := range live {
		ids = append(ids, task.ID)
	}
	if !slices.Equal(ids, []domain.TaskID{2, 3, 4}) {
		t.Fatalf("live tasks %v, want 2 3 4: an ended task is live while a wake answer is outstanding", ids)
	}
	answered := endedTask(3, 0)
	answered.Version = 2
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: answered.Record()}); err != nil {
		t.Fatal(err)
	}
	if live, _ = r.Store.Live(ctx); len(live) != 2 {
		t.Fatalf("task 3 is still live after its answer: %d live", len(live))
	}
}

func contractTasks(t *testing.T, r StoreRig) {
	ctx := context.Background()
	insert(t, r.Store, queuedTask(1, "ana"))
	insert(t, r.Store, queuedTask(2, "bob"))
	insert(t, r.Store, closingTask(3, contractT0.Add(time.Minute)))
	insert(t, r.Store, queuedTask(4, "ana"))
	check := func(name string, q ports.TaskQuery, want ...domain.TaskID) {
		t.Helper()
		got, err := r.Store.Tasks(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var ids []domain.TaskID
		for _, task := range got {
			ids = append(ids, task.ID)
		}
		if !slices.Equal(ids, want) {
			t.Errorf("%s: %v, want %v", name, ids, want)
		}
	}
	check("all, newest first", ports.TaskQuery{}, 4, 3, 2, 1)
	check("owner", ports.TaskQuery{Owner: "ana"}, 4, 3, 1)
	check("status", ports.TaskQuery{Status: domain.StatusQueued}, 4, 2, 1)
	check("owner and status", ports.TaskQuery{Owner: "ana", Status: domain.StatusQueued}, 4, 1)
	check("before", ports.TaskQuery{Before: 3}, 2, 1)
	check("limit", ports.TaskQuery{Limit: 2}, 4, 3)
	check("page", ports.TaskQuery{Before: 4, Limit: 1, Owner: "ana"}, 3)
}

func contractRepo(t *testing.T, r StoreRig) {
	ctx := context.Background()
	if got, err := r.Store.Repo(ctx, "acme/none"); err != nil || got != (domain.Repo{Name: "acme/none"}) {
		t.Fatalf("unknown repo: %+v %v", got, err)
	}
	r.SetRepo(domain.Repo{Name: "acme/app", Notes: "keep me", SetupTask: 7})
	tpl := &ports.TemplateRow{Repo: "acme/app", Template: ports.Template{Handle: "img:1", Machine: "m1"}, At: contractT0}
	task := queuedTask(1, "ana")
	if _, err := r.Store.Commit(ctx, ports.Commit{Task: task.Record(), Template: tpl}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store.Repo(ctx, "acme/app")
	if got.ReadyImage != "img:1" || got.ReadyOn != "m1" || !got.ReadyAt.Equal(contractT0) || got.Notes != "keep me" || got.SetupTask != 7 {
		t.Fatalf("repo after a template save: %+v", got)
	}
	tpl.Repo = "acme/new"
	next := closingTask(1, contractT0.Add(time.Minute))
	next.Version = 2
	if _, err := r.Store.Commit(ctx, ports.Commit{Prev: 1, Task: next.Record(), Template: tpl}); err != nil {
		t.Fatal(err)
	}
	if got, _ = r.Store.Repo(ctx, "acme/new"); got.Name != "acme/new" || got.ReadyImage != "img:1" {
		t.Fatalf("a template save for a repo with no row: %+v", got)
	}
}

func contractMachines(t *testing.T, r StoreRig) {
	ctx := context.Background()
	up := r.AddMachine(domain.Machine{ID: "m1", Name: "box-a", Owner: "ana", Role: domain.RoleSandbox, Slots: 2}, true)
	down := r.AddMachine(domain.Machine{ID: "m2", Name: "mac", Owner: "ana", Role: domain.RoleTool, Slots: 1, Exec: true}, false)
	got, err := r.Store.Machines(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("machines %+v, %v", got, err)
	}
	byName := map[string]ports.MachineState{}
	for _, m := range got {
		byName[m.Machine.Name] = m
	}
	a, b := byName["box-a"], byName["mac"]
	if a.Machine.ID != up || !a.Online || a.Machine.Slots != 2 || a.Machine.Role != domain.RoleSandbox || a.Machine.Owner != "ana" {
		t.Errorf("box-a: %+v", a)
	}
	if b.Machine.ID != down || b.Online || !b.Machine.Exec || b.Machine.Role != domain.RoleTool {
		t.Errorf("mac: %+v", b)
	}
}

func contractTaskIDs(t *testing.T, r StoreRig) {
	ctx := context.Background()
	const n = 16
	ids := make([]domain.TaskID, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id, err := r.Store.NewTaskID(ctx)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		})
	}
	wg.Wait()
	slices.Sort(ids)
	if ids[0] <= 0 || len(slices.Compact(slices.Clone(ids))) != n {
		t.Fatalf("ids are not positive and distinct: %v", ids)
	}
}

func contractLease(t *testing.T, r StoreRig) {
	ctx := context.Background()
	release, err := r.Store.Lease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		rel, err := r.Store.Lease(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		got <- rel
	}()
	select {
	case <-got:
		t.Fatal("a second holder got the lease while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	release()
	select {
	case rel := <-got:
		rel()
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never got the lease after the release")
	}
}

func contractLeaseCancel(t *testing.T, r StoreRig) {
	release, err := r.Store.Lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if rel, err := r.Store.Lease(ctx); err == nil {
		rel()
		t.Fatal("a cancelled wait got the lease")
	}
}
