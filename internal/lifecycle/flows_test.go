package lifecycle

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

// The person's message reaches the agent, and a stranger's is refused with the domain's error.
func TestUserMessageFromTheAPI(t *testing.T) {
	g := newRig(t, newStore())
	id := g.started(domain.KindInteractive)
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	g.settle()

	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: "mallory", Text: "hi"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("stranger's message: %v", err)
	}
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "also bump go"}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	got := g.boxes.Calls()
	n := len(got)
	if got[n-2].Method != "Remove" || got[n-1].Method != "Type" || got[n-1].Text != "also bump go" {
		t.Fatalf("want the marker cleared then the text typed, got %+v", got[n-2:])
	}
	if got[n-1].Target.Run != 1 || got[n-1].Target.Gen != 1 {
		t.Fatalf("typed under %+v", got[n-1].Target)
	}
}

// A stop while the start is slow ends the task at once. The start is cancelled, and the release
// runs only after the start has returned.
func TestStopDuringSlowStart(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindInteractive)
	g.boxes.Hold(id)
	g.grant(id)
	<-g.boxes.Entered()

	tk, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner})
	if err != nil || tk.Status() != domain.StatusStopped {
		t.Fatalf("stop returned %v, %v", tk.Status(), err)
	}
	g.settle()
	got := methods(g.boxes)
	if want := []string{"Converge", "ConvergeReturned", "Release"}; !slices.Equal(got, want) {
		t.Fatalf("the release must wait for the cancelled converge to return, got %v", got)
	}
	g.wantStatus(id, domain.StatusStopped)
	if rel := calls(g.boxes, "Release"); len(rel) != 1 || rel[0].Target.Gen != 1 {
		t.Fatalf("releases %+v", rel)
	}
}

// A stop while an ended task wakes gives the ended run back. The EnsureRunning still in flight is
// answered when it returns, and that answer stops the box it may have started.
func TestStopDuringWake(t *testing.T) {
	g := newRig(t, newStore())
	id := g.started(domain.KindInteractive)
	if _, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	g.wantStatus(id, domain.StatusStopped)
	before := len(calls(g.boxes, "Stop"))

	g.boxes.Hold(id)
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "one more thing"}); err != nil {
		t.Fatal(err)
	}
	<-g.boxes.Entered()
	if _, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	g.boxes.Let(id, nil)
	g.settle()

	if e := g.task(id).Life.(domain.Ended); e.Ensure != 0 {
		t.Fatalf("the wake's answer was never applied: %+v", e)
	}
	if stops := len(calls(g.boxes, "Stop")) - before; stops != 2 {
		t.Fatalf("want the box stopped when the wake was abandoned and again when its answer came, got %d stops", stops)
	}
}

// A setup task saves its box as the repo's ready template when it is done. A task that starts
// later uses it on the machine that holds it, and starts from the base image elsewhere.
func TestTemplateSave(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	store.AddMachine(domain.Machine{ID: "m2", Owner: owner, Role: domain.RoleSandbox, Slots: 4}, true)
	id := g.started(domain.KindSetup)
	g.boxes.Touch(id, domain.DoneMarkerPath)
	g.boxes.Touch(id, domain.NotesPath)
	g.agent.SetSummary("Prepared and tested.")
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	g.settle()

	tk := g.task(id)
	if tk.Status() != domain.StatusDone || tk.Life.(domain.Ended).Summary != "Prepared and tested." {
		t.Fatalf("ended as %v with %+v", tk.Status(), tk.Life)
	}
	repo, _ := store.Repo(g.ctx, "acme/app")
	if repo.ReadyImage != "ready-1" || repo.ReadyOn != machine || repo.Notes != domain.NotesPath {
		t.Fatalf("repo %+v: the template and its machine must be recorded", repo)
	}

	onTemplate := g.created(domain.KindInteractive)
	g.grant(onTemplate)
	other := g.created(domain.KindInteractive)
	if _, err := g.r.step(g.ctx, other, domain.SlotGranted{Machine: "m2"}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	images := map[domain.TaskID]string{}
	for _, c := range calls(g.boxes, "Converge") {
		images[c.Target.Task] = c.Plan.Image
	}
	if images[id] != "base:1" || images[onTemplate] != "ready-1" || images[other] != "base:1" {
		t.Fatalf("images %v", images)
	}
}

// A template save that fails fails the task and keeps the box, and the save is not recorded.
func TestTemplateSaveFailure(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindSetup)
	g.boxes.Touch(id, domain.DoneMarkerPath)
	g.boxes.Fail("SaveTemplate", errors.New("disk full"))
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	g.settle()

	tk := g.task(id)
	if e := tk.Life.(domain.Ended); tk.Status() != domain.StatusFailed || !e.BoxKept {
		t.Fatalf("ended as %v with %+v", tk.Status(), e)
	}
	if repo, _ := store.Repo(g.ctx, "acme/app"); repo.ReadyImage != "" {
		t.Fatalf("repo %+v", repo)
	}
}

// A port that fails still gets its one answer, and the domain takes the safe path.
func TestAPortErrorIsAnswered(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*rig)
		want   domain.Status
	}{
		{"the start", func(g *rig) { g.boxes.Fail("Converge", errors.New("no space")) }, domain.StatusFailed},
		{"the agent launch", func(g *rig) { g.agent.FailLaunch(errors.New("no profile")) }, domain.StatusFailed},
		{"the placeholders", func(g *rig) { g.secrets.Fail(errors.New("vault down")) }, domain.StatusFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, newStore())
			tc.break_(g)
			id := g.created(domain.KindInteractive)
			g.grant(id)
			g.settle()
			g.wantStatus(id, tc.want)
			if len(calls(g.boxes, "Release")) != 1 {
				t.Fatalf("a failed start must release its box: %v", methods(g.boxes))
			}
		})
	}
}

// An agent that exits badly while the box state cannot be read is judged by the neutral answer: the
// box is taken as lost and replaced.
func TestAnUnreadableBoxIsTakenAsLost(t *testing.T) {
	g := newRig(t, newStore())
	id := g.started(domain.KindScheduled)
	g.boxes.Fail("Inspect", errors.New("machine busy"))
	g.feed(id, domain.AgentExited{Run: 1, Code: 1})
	g.settle()

	tk := g.task(id)
	a := tk.Life.(domain.Active)
	if tk.Status() != domain.StatusWorking || a.Box.Want != domain.BoxReplacing || tk.BoxGen != 2 {
		t.Fatalf("task %v, box %+v, gen %d", tk.Status(), a.Box, tk.BoxGen)
	}
}

// An unreadable marker reads as absent, so the nudge ladder decides.
func TestAnUnreadableMarkerReadsAsAbsent(t *testing.T) {
	g := newRig(t, newStore())
	id := g.started(domain.KindScheduled)
	g.boxes.Fail("Exists", errors.New("machine busy"))
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	g.settle()

	g.wantStatus(id, domain.StatusWorking)
	if a := g.task(id).Life.(domain.Active); a.Asks[domain.AskDoneMarker].ID != 0 || a.Nudge.Sent != 0 {
		t.Fatalf("active %+v", a)
	}
}

// A budget that runs out answers the ask, so a stuck machine cannot hold a task.
func TestABudgetThatRunsOutAnswersTheAsk(t *testing.T) {
	g := newRigWith(t, newStore(), func(c *Config) { c.Budgets.Probe = 20_000_000 })
	id := g.started(domain.KindScheduled)
	g.boxes.Stall("Exists")
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	<-g.boxes.Entered()
	g.settle()

	if a := g.task(id).Life.(domain.Active); a.Asks[domain.AskDoneMarker].ID != 0 {
		t.Fatalf("the probe was never answered: %+v", a.Asks)
	}
}

// Losing the compare-and-swap reloads and applies again, and gives up after a few tries.
func TestALostCompareAndSwapIsRetried(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindInteractive)
	store.ConflictNext(casTries - 1)
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "hi"}); err != nil {
		t.Fatalf("two lost swaps: %v", err)
	}
	store.ConflictNext(casTries)
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "again"}); err == nil {
		t.Fatal("a swap lost every time succeeded")
	}
	g.settle()
	if got := calls(g.boxes, "Type"); len(got) != 1 || got[0].Text != "hi" {
		t.Fatalf("typed %+v", got)
	}
}

// An answer whose own commit fails is committed again, so the ask is not left to the next start.
func TestAnAnswerWhoseCommitFailsIsRetried(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.created(domain.KindInteractive)
	g.grant(id)
	store.FailCommits(2, errors.New("db down"))
	g.settle()
	g.wantStatus(id, domain.StatusWorking)
}

// A row the store will not mark done keeps the lane waiting, and is marked when the store recovers.
func TestACommandIsMarkedDoneOnceTheStoreTakesIt(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindInteractive)
	store.FailDones(3, errors.New("db down"))
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	if rows, _ := store.Pending(g.ctx); len(rows) != 0 {
		t.Fatalf("pending %+v", rows)
	}
}

// A command that fails is logged and its row is still done, because it is best effort.
func TestAFailedCommandIsNotRetried(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindInteractive)
	g.boxes.Fail("Type", errors.New("terminal gone"))
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	if rows, _ := store.Pending(g.ctx); len(rows) != 0 || len(calls(g.boxes, "Type")) != 1 {
		t.Fatalf("pending %+v, typed %d times", rows, len(calls(g.boxes, "Type")))
	}
}

// Progressed applies while the start runs, and a stop in the middle still ends it.
func TestProgressApplyingWhileTheStartIsHeld(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindInteractive)
	g.boxes.Hold(id)
	g.grant(id)
	<-g.boxes.Entered()
	if got := g.task(id).Phase(); got != "Starting the agent" {
		t.Fatalf("phase %q while the start is held", got)
	}
	g.boxes.Let(id, nil)
	g.settle()
	g.wantStatus(id, domain.StatusWorking)
}

func TestTheAPISurface(t *testing.T) {
	g := newRig(t, newStore())
	if _, err := g.r.Create(g.ctx, owner, domain.KindInteractive, domain.Spec{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a task with no repo: %v", err)
	}
	id := g.created(domain.KindInteractive)
	g.created(domain.KindScheduled)
	mine, err := g.r.List(g.ctx, TaskQuery{Owner: owner, Limit: 1})
	if err != nil || len(mine) != 1 || mine[0].ID != id+1 {
		t.Fatalf("list %+v, %v", mine, err)
	}
	if _, err := g.r.Task(g.ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := g.r.Do(g.ctx, 99, domain.UserStop{By: owner}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stop of an unknown task: %v", err)
	}
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "hi"}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("message to a queued task: %v", err)
	}
	g.r.lead.Store(false)
	if _, err := g.r.Create(g.ctx, owner, domain.KindInteractive, domain.Spec{Repo: "a/b"}); !errors.Is(err, ErrNotLeading) {
		t.Fatalf("create while not leading: %v", err)
	}
	if _, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner}); !errors.Is(err, ErrNotLeading) {
		t.Fatalf("do while not leading: %v", err)
	}
}

// first takes the first n rows Follow yields after the given one.
func (g *rig) first(id domain.TaskID, after, n int, between func(Row)) []Row {
	g.t.Helper()
	var rows []Row
	for row, err := range g.r.Follow(g.ctx, id, after) {
		if err != nil {
			g.t.Fatal(err)
		}
		rows = append(rows, row)
		if between != nil {
			between(row)
		}
		if len(rows) == n {
			break
		}
	}
	return rows
}

func texts(rows []Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Text)
	}
	return out
}

func TestFollowStreamsTheTimelineThenNewEntries(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindInteractive)
	granted := func(row Row) {
		if row.Seq == 1 {
			g.grant(id)
		}
	}
	got := texts(g.first(id, 0, 2, granted))
	if !slices.Equal(got, []string{"Queued", "Preparing the computer"}) {
		t.Fatalf("timeline %v", got)
	}
	g.settle()
	rows := g.first(id, 2, 2, nil)
	if rows[0].Seq != 3 || rows[1].Seq != 4 {
		t.Fatalf("rows after the second: %+v", rows)
	}
}

func TestFollowOfAnUnknownTaskFails(t *testing.T) {
	g := newRig(t, newStore())
	for _, err := range g.r.Follow(g.ctx, 99, 0) {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("follow of an unknown task: %v", err)
		}
		return
	}
	t.Fatal("follow yielded nothing")
}

func TestFollowEndsWhenItsContextDoes(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindInteractive)
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	n := 0
	for range g.r.Follow(ctx, id, 0) {
		n++
		cancel()
	}
	if n != 1 {
		t.Fatalf("%d rows before the end", n)
	}
}

func TestScreenReadsTheCurrentRunUntilRelease(t *testing.T) {
	g := newRig(t, newStore())
	queued, err := g.r.Create(g.ctx, owner, domain.KindInteractive, domain.Spec{Repo: "o/r", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := g.r.Screen(g.ctx, queued.ID, 5); err != nil || s != "" {
		t.Fatalf("a queued task has no screen: %q %v", s, err)
	}
	id := g.started(domain.KindInteractive)
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	g.settle()
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "on screen"}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	if s, err := g.r.Screen(g.ctx, id, 5); err != nil || s != "on screen" {
		t.Fatalf("the screen shows the run's terminal: %q %v", s, err)
	}
	if _, err := g.r.Screen(g.ctx, 999, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown task: %v", err)
	}
	if !g.r.Leading() {
		t.Fatal("the rig leads")
	}
}
