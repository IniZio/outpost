package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

func TestNewRejectsWhatCannotRun(t *testing.T) {
	tests := []struct {
		name  string
		owner domain.Login
		kind  domain.Kind
		spec  domain.Spec
	}{
		{"no owner", "", domain.KindInteractive, domain.Spec{Repo: "acme/app"}},
		{"no repo", owner, domain.KindInteractive, domain.Spec{}},
		{"unknown kind", owner, 0, domain.Spec{Repo: "acme/app"}},
		{"negative timeout", owner, domain.KindInteractive, domain.Spec{Repo: "acme/app", Timeout: -time.Second}},
		{"scheduled without a schedule", owner, domain.KindScheduled, domain.Spec{Repo: "acme/app"}},
		{"interactive with a schedule", owner, domain.KindInteractive, domain.Spec{Repo: "acme/app", Schedule: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.New(5, tc.owner, tc.kind, tc.spec, t0); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNewBuildsAQueuedTask(t *testing.T) {
	specs := map[domain.Kind]domain.Spec{
		domain.KindInteractive: {Repo: "acme/app"},
		domain.KindScheduled:   {Repo: "acme/app", Schedule: 3},
		domain.KindSetup:       {Repo: "acme/app"},
	}
	for kind, sp := range specs {
		res, err := domain.New(5, owner, kind, sp, t0)
		if err != nil {
			t.Fatal(err)
		}
		if res.Task.Status() != domain.StatusQueued || res.Task.Validate() != nil {
			t.Errorf("kind %v: status %v, validate %v", kind, res.Task.Status(), res.Task.Validate())
		}
		if len(res.Effects) != 1 || len(res.Timeline) != 1 || res.Timeline[0].Kind != domain.EntryQueued {
			t.Errorf("effects %v, timeline %v: want the scheduler kick and one queued entry", res.Effects, res.Timeline)
		}
	}
}

func TestScheduleFireStampsTheSchedule(t *testing.T) {
	s := domain.Schedule{ID: 7, Owner: owner, Kind: domain.KindScheduled, Spec: domain.Spec{Repo: "acme/app"}, Enabled: true}
	res, err := s.Fire(12, t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.ID != 12 || res.Task.Spec.Schedule != 7 || res.Task.Kind != domain.KindScheduled || res.Task.Owner != owner {
		t.Errorf("fired task = %+v", res.Task)
	}
}

func TestStatusIsDerivedFromLife(t *testing.T) {
	resume := func(o domain.Outcome) domain.Life { return domain.Starting{Resume: &domain.Ended{Outcome: o, At: t0}} }
	tests := []struct {
		name string
		kind domain.Kind
		life domain.Life
		want domain.Status
		busy bool
	}{
		{"queued", domain.KindInteractive, domain.Queued{}, domain.StatusQueued, false},
		{"starting", domain.KindInteractive, domain.Starting{}, domain.StatusStarting, true},
		{"waking a done task", domain.KindInteractive, resume(domain.OutcomeDone), domain.StatusDone, false},
		{"waking a failed task", domain.KindInteractive, resume(domain.OutcomeFailed), domain.StatusFailed, false},
		{"waking a stopped task", domain.KindInteractive, resume(domain.OutcomeStopped), domain.StatusStopped, false},
		{"pending turn", domain.KindInteractive, active(domain.TurnPending), domain.StatusWorking, true},
		{"running turn", domain.KindInteractive, active(domain.TurnRunning), domain.StatusWorking, true},
		{"paused turn, attended", domain.KindInteractive, active(domain.TurnPaused), domain.StatusWaiting, false},
		{"paused turn, unattended", domain.KindScheduled, active(domain.TurnPaused), domain.StatusWorking, true},
		{"blocked, attended", domain.KindInteractive, active(domain.TurnBlocked), domain.StatusWaiting, false},
		{"blocked, unattended", domain.KindScheduled, active(domain.TurnBlocked), domain.StatusWaiting, false},
		{"closing a working task", domain.KindInteractive, domain.Closing{From: domain.StatusWorking}, domain.StatusWorking, true},
		{"closing a waiting task", domain.KindInteractive, domain.Closing{From: domain.StatusWaiting}, domain.StatusWaiting, false},
		{"ended done", domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeDone}, domain.StatusDone, false},
		{"ended failed", domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeFailed}, domain.StatusFailed, false},
		{"ended stopped", domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeStopped}, domain.StatusStopped, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := fixture(tc.kind, tc.life)
			got := [3]any{task.Status(), task.Busy(), task.Status().Terminal()}
			want := [3]any{tc.want, tc.busy, tc.want >= domain.StatusDone}
			if got != want {
				t.Errorf("status, busy, terminal = %v, want %v", got, want)
			}
		})
	}
}

func TestATaskWithNoLifeHasNoStatus(t *testing.T) {
	if got := (domain.Task{}).Status(); got != 0 {
		t.Errorf("Status() = %v", got)
	}
}

func TestBoxWantFollowsLife(t *testing.T) {
	released := domain.Ended{Outcome: domain.OutcomeDone, At: t0, ReleasedAt: t0}
	asleep := active(domain.TurnPaused)
	asleep.Box.Want = domain.BoxAsleep
	tests := []struct {
		name string
		life domain.Life
		want domain.BoxWant
	}{
		{"queued", domain.Queued{}, domain.BoxNone},
		{"starting", domain.Starting{}, domain.BoxUp},
		{"waking", domain.Starting{Resume: &domain.Ended{}}, domain.BoxWaking},
		{"active", active(domain.TurnRunning), domain.BoxUp},
		{"asleep", asleep, domain.BoxAsleep},
		{"closing", domain.Closing{}, domain.BoxUp},
		{"ended", domain.Ended{Outcome: domain.OutcomeDone, At: t0}, domain.BoxStopped},
		{"ended with the box kept", domain.Ended{Outcome: domain.OutcomeFailed, At: t0, BoxKept: true}, domain.BoxUp},
		{"released", released, domain.BoxReleased},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fixture(domain.KindInteractive, tc.life).BoxWant(); got != tc.want {
				t.Errorf("BoxWant() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPhaseAndNextDeadline(t *testing.T) {
	soon, later := t0.Add(time.Minute), t0.Add(time.Hour)
	run := active(domain.TurnRunning)
	run.Turn.WorkingSince, run.Turn.Phase = t0, "Editing"
	run.Deadlines = run.Deadlines.Set(domain.DlNudge, later).Set(domain.DlTimeout, soon)
	tests := []struct {
		name      string
		life      domain.Life
		phase     string
		deadline  time.Time
		hasWakeUp bool
	}{
		{"queued", domain.Queued{}, "", time.Time{}, false},
		{"starting", domain.Starting{Phase: "Cloning"}, "Cloning", time.Time{}, false},
		{"running with two deadlines", run, "Editing", soon, true},
		{"closing", domain.Closing{Until: later}, "", later, true},
		{"ended, to be released", domain.Ended{ReleaseAt: soon}, "", soon, true},
		{"ended, released", domain.Ended{ReleaseAt: soon, ReleasedAt: t0}, "", time.Time{}, false},
		{"ended, no release", domain.Ended{}, "", time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := fixture(domain.KindScheduled, tc.life)
			if got := task.Phase(); got != tc.phase {
				t.Errorf("Phase() = %q, want %q", got, tc.phase)
			}
			at, ok := task.NextDeadline()
			if ok != tc.hasWakeUp || !at.Equal(tc.deadline) {
				t.Errorf("NextDeadline() = %v, %v, want %v, %v", at, ok, tc.deadline, tc.hasWakeUp)
			}
		})
	}
}

func TestOnlyTheOwnerControlsATask(t *testing.T) {
	task := newTask(1, domain.KindInteractive, spec())
	tests := []struct {
		who  domain.Login
		read bool
		want domain.Access
	}{
		{owner, false, domain.AccessControl},
		{stranger, true, domain.AccessWatch},
		{stranger, false, domain.AccessNone},
	}
	for _, tc := range tests {
		if got := task.AccessFor(tc.who, tc.read); got != tc.want {
			t.Errorf("AccessFor(%q, %v) = %v, want %v", tc.who, tc.read, got, tc.want)
		}
	}
}

func TestMachineHostsOnlyItsOwnersSandboxTasks(t *testing.T) {
	task := newTask(1, domain.KindInteractive, spec())
	tests := []struct {
		name string
		m    domain.Machine
		want bool
	}{
		{"owner's sandbox machine", domain.Machine{Owner: owner, Role: domain.RoleSandbox, Slots: 1}, true},
		{"someone else's machine", domain.Machine{Owner: stranger, Role: domain.RoleSandbox, Slots: 1}, false},
		{"owner's tool machine", domain.Machine{Owner: owner, Role: domain.RoleTool, Slots: 1}, false},
	}
	for _, tc := range tests {
		if got := tc.m.CanHost(task); got != tc.want {
			t.Errorf("%s: CanHost = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRepoStartImage(t *testing.T) {
	ready := domain.Repo{Name: "acme/app", ReadyImage: "ready:1"}
	tests := []struct {
		repo domain.Repo
		kind domain.Kind
		want string
	}{
		{ready, domain.KindInteractive, "ready:1"},
		{ready, domain.KindSetup, "base"},
		{domain.Repo{Name: "acme/app"}, domain.KindInteractive, "base"},
	}
	for _, tc := range tests {
		if got := tc.repo.StartImage(tc.kind, "base"); got != tc.want {
			t.Errorf("StartImage(%v) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func queuedTask(id domain.TaskID, who domain.Login) domain.Task {
	task := newTask(id, domain.KindInteractive, spec())
	task.Owner = who
	return task
}

func busyTask(id domain.TaskID, machine domain.MachineID, turn domain.TurnState) domain.Task {
	task := fixture(domain.KindInteractive, active(turn))
	task.ID, task.Machine = id, machine
	return task
}

func TestPickStarts(t *testing.T) {
	box := []domain.Machine{{ID: "m1", Owner: owner, Role: domain.RoleSandbox, Slots: 2}}
	tests := []struct {
		name     string
		tasks    []domain.Task
		machines []domain.Machine
		cap      domain.Capacity
		want     []domain.Placement
	}{
		{
			name:     "lowest id first, up to the cap",
			tasks:    []domain.Task{queuedTask(9, owner), queuedTask(4, owner), queuedTask(7, owner)},
			machines: []domain.Machine{{ID: "m1", Owner: owner, Role: domain.RoleSandbox, Slots: 9}},
			cap:      domain.Capacity{Max: 2},
			want:     []domain.Placement{{Task: 4, Machine: "m1"}, {Task: 7, Machine: "m1"}},
		},
		{
			name:     "working tasks use the cap, waiting ones do not",
			tasks:    []domain.Task{busyTask(1, "m1", domain.TurnRunning), busyTask(2, "m1", domain.TurnPaused), queuedTask(3, owner), queuedTask(4, owner)},
			machines: []domain.Machine{{ID: "m1", Owner: owner, Role: domain.RoleSandbox, Slots: 9}},
			cap:      domain.Capacity{Max: 2},
			want:     []domain.Placement{{Task: 3, Machine: "m1"}},
		},
		{
			name:     "a machine's slots count its waiting tasks too",
			tasks:    []domain.Task{busyTask(1, "m1", domain.TurnPaused), busyTask(2, "m1", domain.TurnPaused), queuedTask(3, owner)},
			machines: box,
			cap:      domain.Capacity{Max: 5},
		},
		{
			name:     "a task nobody can host stays queued and keeps the slot for the next",
			tasks:    []domain.Task{queuedTask(1, stranger), queuedTask(2, owner)},
			machines: box,
			cap:      domain.Capacity{Max: 1},
			want:     []domain.Placement{{Task: 2, Machine: "m1"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.PickStarts(tc.tasks, tc.machines, tc.cap); !slices.Equal(got, tc.want) {
				t.Errorf("PickStarts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPickStartsStartsNothing(t *testing.T) {
	box := []domain.Machine{{ID: "m1", Owner: owner, Role: domain.RoleSandbox, Slots: 2}}
	tests := []struct {
		name     string
		tasks    []domain.Task
		machines []domain.Machine
		cap      domain.Capacity
	}{
		{
			name:     "a full cap starts nothing",
			tasks:    []domain.Task{busyTask(1, "m1", domain.TurnRunning), queuedTask(3, owner)},
			machines: box,
			cap:      domain.Capacity{Max: 1},
		},
		{
			name:     "a hold starts nothing",
			tasks:    []domain.Task{queuedTask(3, owner)},
			machines: box,
			cap:      domain.Capacity{Max: 5, Hold: true},
		},
		{
			name:     "no machines",
			tasks:    []domain.Task{queuedTask(1, owner)},
			machines: nil,
			cap:      domain.Capacity{Max: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.PickStarts(tc.tasks, tc.machines, tc.cap); len(got) != 0 {
				t.Errorf("PickStarts = %v, want nothing", got)
			}
		})
	}
}

func TestPickStartsSpreadsAcrossMachinesOfTheOwner(t *testing.T) {
	machines := []domain.Machine{
		{ID: "m1", Owner: owner, Role: domain.RoleSandbox, Slots: 1},
		{ID: "m2", Owner: owner, Role: domain.RoleSandbox, Slots: 1},
	}
	got := domain.PickStarts([]domain.Task{queuedTask(1, owner), queuedTask(2, owner), queuedTask(3, owner)}, machines, domain.Capacity{Max: 9})
	want := []domain.Placement{{Task: 1, Machine: "m1"}, {Task: 2, Machine: "m2"}}
	if !slices.Equal(got, want) {
		t.Errorf("PickStarts = %v, want %v", got, want)
	}
}

func TestRecordRoundTripsEveryLife(t *testing.T) {
	asleep := active(domain.TurnPaused)
	asleep.Box = domain.Box{Want: domain.BoxAsleep, StartedAt: t0}
	working := active(domain.TurnRunning)
	working.Turn.WorkingSince, working.Turn.Phase = t0, "Editing"
	working.Asks[domain.AskSummary] = domain.Ask{ID: 4}
	lives := map[string]domain.Life{
		"queued": domain.Queued{},
		"starting": domain.Starting{Since: t0, Phase: "Cloning", Ask: 3, Early: []domain.Early{
			{Report: &domain.AgentReport{Run: 2, Status: domain.HCWorking, Phase: "x"}},
			{Exit: &domain.AgentExited{Run: 2, Code: 1}},
		}},
		"waking":  domain.Starting{Since: t0, Ask: 3, Messages: []string{"hi"}, Resume: &domain.Ended{Outcome: domain.OutcomeDone, At: t0, Summary: "ok"}},
		"working": working,
		"asleep":  asleep,
		"closing": domain.Closing{Want: domain.OutcomeDone, From: domain.StatusWorking, Step: domain.CloseSummary, Ask: 5, Until: t0.Add(time.Minute)},
		"ended":   domain.Ended{Outcome: domain.OutcomeFailed, At: t0, Error: "boom", ReleaseAt: t0.Add(time.Hour)},
	}
	for name, life := range lives {
		t.Run(name, func(t *testing.T) {
			task := fixture(domain.KindInteractive, life)
			rec := task.Record()
			got, err := domain.Restore(rec)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := json.Marshal(got.Record())
			orig, _ := json.Marshal(rec)
			if string(again) != string(orig) {
				t.Errorf("round trip changed the record:\n got %s\nwant %s", again, orig)
			}
		})
	}
}

func TestRecordCarriesTheQueryColumns(t *testing.T) {
	run := active(domain.TurnPaused)
	run.Deadlines = run.Deadlines.Set(domain.DlSleep, t0.Add(30*time.Minute))
	rec := fixture(domain.KindInteractive, run).Record()
	if rec.Status != domain.StatusWaiting || rec.Phase != domain.PhaseActive || !rec.Wake.Equal(t0.Add(30*time.Minute)) {
		t.Errorf("record = status %v, phase %v, wake %v", rec.Status, rec.Phase, rec.Wake)
	}
	if got := fixture(domain.KindInteractive, domain.Queued{}).Record().Wake; !got.IsZero() {
		t.Errorf("a queued task wakes at %v", got)
	}
}

func TestRestoreRejectsCorruptRows(t *testing.T) {
	good := fixture(domain.KindInteractive, active(domain.TurnPaused)).Record()
	tests := []struct {
		name   string
		mutate func(r *domain.Record)
	}{
		{"unknown phase", func(r *domain.Record) { r.Phase = "limbo" }},
		{"payload is not json", func(r *domain.Record) { r.Payload = json.RawMessage(`{`) }},
		{"status column disagrees", func(r *domain.Record) { r.Status = domain.StatusDone }},
		{"version is zero", func(r *domain.Record) { r.Version = 0 }},
		{"unknown kind", func(r *domain.Record) { r.Kind = 0 }},
		{"no owner", func(r *domain.Record) { r.Owner = "" }},
		{"no agent run on a running task", func(r *domain.Record) { r.AgentRun = 0 }},
		{"working since set while paused", func(r *domain.Record) {
			r.Payload = json.RawMessage(`{"Turn":{"State":3,"WorkingSince":"2026-11-18T06:00:00Z"},"Box":{"Want":2}}`)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := good
			tc.mutate(&rec)
			if _, err := domain.Restore(rec); !errors.Is(err, domain.ErrCorrupt) {
				t.Errorf("Restore err = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestValidateAcceptsAReplacementInProgress(t *testing.T) {
	waking := active(domain.TurnPending)
	waking.Box = domain.Box{Want: domain.BoxWaking, Lost: "restarted"}
	waking.Asks[domain.AskEnsure] = domain.Ask{ID: 4, Why: domain.WhyReplace}
	if err := fixture(domain.KindInteractive, waking).Validate(); err != nil {
		t.Errorf("Validate() = %v: the lost reason is kept until the box is back", err)
	}
}

func TestValidateNamesEachBrokenInvariant(t *testing.T) {
	soon := t0.Add(time.Minute)
	withDeadline := func(k domain.DeadlineKind, turn domain.TurnState) domain.Active {
		a := active(turn)
		a.Deadlines = a.Deadlines.Set(k, soon)
		return a
	}
	box := func(want domain.BoxWant, mutate func(*domain.Active)) domain.Active {
		a := active(domain.TurnPaused)
		a.Box.Want = want
		mutate(&a)
		return a
	}
	tests := []struct {
		name string
		kind domain.Kind
		life domain.Life
	}{
		{"no life", domain.KindInteractive, nil},
		{"nudge while the agent works", domain.KindScheduled, withDeadline(domain.DlNudge, domain.TurnRunning)},
		{"nudge on an attended run", domain.KindInteractive, withDeadline(domain.DlNudge, domain.TurnPaused)},
		{"sleep on an unattended run", domain.KindScheduled, withDeadline(domain.DlSleep, domain.TurnBlocked)},
		{"sleep while working", domain.KindInteractive, withDeadline(domain.DlSleep, domain.TurnRunning)},
		{"replace deadline without replacing", domain.KindInteractive, withDeadline(domain.DlReplace, domain.TurnPaused)},
		{"replacing without a deadline", domain.KindInteractive, box(domain.BoxReplacing, func(*domain.Active) {})},
		{"timeout deadline without a spec timeout", domain.KindInteractive, withDeadline(domain.DlTimeout, domain.TurnPaused)},
		{"stopped box on an active task", domain.KindInteractive, box(domain.BoxStopped, func(*domain.Active) {})},
		{"lost reason on a live box", domain.KindInteractive, box(domain.BoxUp, func(a *domain.Active) { a.Box.Lost = "x" })},
		{"parked message on a live box", domain.KindInteractive, box(domain.BoxUp, func(a *domain.Active) { a.Box.Parked = []string{"x"} })},
		{"ask never issued", domain.KindInteractive, box(domain.BoxUp, func(a *domain.Active) { a.Asks[domain.AskSummary].ID = 99 })},
		{"waking without an ask", domain.KindInteractive, box(domain.BoxWaking, func(*domain.Active) {})},
		{"probe outstanding while working", domain.KindScheduled, func() domain.Active {
			a := active(domain.TurnRunning)
			a.Turn.WorkingSince = t0
			a.Asks[domain.AskDoneMarker].ID = 3
			return a
		}()},
		{"starting with an unissued ask", domain.KindInteractive, domain.Starting{Ask: 99}},
		{"starting with an empty buffered event", domain.KindInteractive, domain.Starting{Ask: 3, Early: []domain.Early{{}}}},
		{"parked message without a resume", domain.KindInteractive, domain.Starting{Ask: 3, Messages: []string{"hi"}}},
		{"resume of a released run", domain.KindInteractive, domain.Starting{Ask: 3, Resume: &domain.Ended{Outcome: domain.OutcomeDone, At: t0, ReleasedAt: t0}}},
		{"closing without a deadline", domain.KindInteractive, domain.Closing{Want: domain.OutcomeDone, From: domain.StatusWorking, Step: domain.CloseSummary, Ask: 3}},
		{"template step on a task that saves none", domain.KindInteractive, domain.Closing{Want: domain.OutcomeDone, From: domain.StatusWorking, Step: domain.CloseTemplate, Ask: 3, Until: soon}},
		{"closing with an unknown outcome", domain.KindInteractive, domain.Closing{From: domain.StatusWorking, Step: domain.CloseSummary, Ask: 3, Until: soon}},
		{"ended with an unknown outcome", domain.KindInteractive, domain.Ended{At: t0}},
		{"ended without a time", domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeDone}},
		{"box kept on an interactive task", domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeFailed, At: t0, BoxKept: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := fixture(tc.kind, tc.life).Validate(); !errors.Is(err, domain.ErrCorrupt) {
				t.Errorf("Validate() = %v, want ErrCorrupt", err)
			}
		})
	}
}
