package lifecycle

import (
	"errors"
	"slices"
	"testing"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

// restarted is a new process over what the old one committed, leading after boot.
func restarted(t *testing.T, store *portstest.Store) *rig {
	t.Helper()
	h := newRig(t, store.Crash())
	if err := h.r.recoverAll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	return h
}

// The server dies while a start is half way. The new process puts the task back in the queue,
// releases the half-built box, and starts it again as a new run and generation.
func TestRestartMidStart(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.created(domain.KindScheduled)
	g.boxes.Hold(id)
	if err := g.r.schedule(g.ctx); err != nil {
		t.Fatal(err)
	}
	<-g.boxes.Entered()

	h := restarted(t, store)
	h.wantStatus(id, domain.StatusQueued)
	h.pass()
	if got := methods(h.boxes); !slices.Equal(got, []string{"Release", "Converge", "ConvergeReturned"}) {
		t.Fatalf("calls %v: want the half-built box released, then a fresh start", got)
	}
	rel, cv := calls(h.boxes, "Release")[0], calls(h.boxes, "Converge")[0]
	if rel.Target.Gen != 1 || cv.Plan.Target.Gen != 2 || cv.Plan.Target.Run != 2 || !cv.Plan.Fresh {
		t.Fatalf("release %+v, start %+v", rel.Target, cv.Plan.Target)
	}
	h.wantStatus(id, domain.StatusWorking)
}

// A crash between saving a command and running it loses nothing: the new process runs the row
// first, before anything Recovered emits.
func TestRestartReplaysTheOutboxFirst(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindInteractive)
	g.boxes.Stall("Kill")
	if _, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	<-g.boxes.Entered()

	h := restarted(t, store)
	got := methods(h.boxes)
	if len(got) < 2 || got[0] != "Kill" {
		t.Fatalf("pending kill must run first, got %v", got)
	}
	h.wantStatus(id, domain.StatusStopped)
	if rows, _ := h.store.Pending(h.ctx); len(rows) != 0 {
		t.Fatalf("pending %+v", rows)
	}
}

// A StopBox lost with the crash is never asked again by Recovered, so the replay is what stops it.
func TestRestartStopsTheBoxALostStopLeftUp(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindScheduled)
	g.boxes.Touch(id, domain.DoneMarkerPath)
	g.boxes.Stall("Stop")
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	<-g.boxes.Entered()
	g.wantStatus(id, domain.StatusDone)

	h := restarted(t, store)
	stops := calls(h.boxes, "Stop")
	if len(stops) != 1 || stops[0].Target.Gen != 1 || stops[0].Target.Machine != machine {
		t.Fatalf("stops %+v", stops)
	}
	if got := calls(h.boxes, "Converge"); len(got) != 0 {
		t.Fatalf("an ended task was started again: %+v", got)
	}
}

// A crash while a stopped task's wake was abandoned: the wake's answer is asked again, and its
// answer stops the box it may have started.
func TestRestartDuringAnAbandonedWake(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindInteractive)
	if _, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	g.boxes.Hold(id)
	if _, err := g.r.Do(g.ctx, id, domain.UserMessage{By: owner, Text: "one more thing"}); err != nil {
		t.Fatal(err)
	}
	<-g.boxes.Entered()
	g.boxes.Stall("Stop")
	if _, err := g.r.Do(g.ctx, id, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	<-g.boxes.Entered()

	h := restarted(t, store)
	if got := calls(h.boxes, "Converge"); len(got) != 1 || got[0].Plan.Fresh {
		t.Fatalf("the wake must be asked again as a resume: %+v", got)
	}
	if e := h.task(id).Life.(domain.Ended); e.Ensure != 0 {
		t.Fatalf("the wake's answer was never applied: %+v", e)
	}
	want := []string{"Stop", "Converge", "ConvergeReturned", "Stop"}
	if got := methods(h.boxes); !slices.Equal(got, want) {
		t.Fatalf("calls %v, want the replayed stop, the wake asked again, and its answer stopping the box again", got)
	}
}

// An active task is judged against its agent only once its machine is online.
func TestRestartWaitsForAnOfflineMachine(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	id := g.started(domain.KindInteractive)

	crashed := store.Crash()
	crashed.SetOnline(machine, false)
	h := newRig(t, crashed)
	if err := h.r.recoverAll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.wantStatus(id, domain.StatusWorking)
	if got := calls(h.boxes, "Observe"); len(got) != 0 {
		t.Fatalf("a machine that is away was asked: %+v", got)
	}

	crashed.SetOnline(machine, true)
	h.boxes.Fail("Observe", errors.New("link not up yet"))
	h.r.retryRecovery(h.ctx)
	h.settle()
	h.wantStatus(id, domain.StatusWorking)

	h.boxes.Fail("Observe", nil)
	h.r.retryRecovery(h.ctx)
	h.settle()
	h.wantStatus(id, domain.StatusFailed)
	if got := calls(h.boxes, "Observe"); len(got) != 2 || got[0].Target.Run != 1 || got[0].Target.Machine != machine {
		t.Fatalf("observations %+v", got)
	}
	h.r.retryRecovery(h.ctx)
	if got := calls(h.boxes, "Observe"); len(got) != 2 {
		t.Fatalf("a recovered task was observed again: %+v", got)
	}
}

// An agent that is still running after a restart changes nothing, and one that finished is done.
func TestRestartTrustsWhatTheMachineReports(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	running := g.started(domain.KindInteractive)
	finished := g.started(domain.KindInteractive)

	crashed := store.Crash()
	h := newRig(t, crashed)
	h.boxes.SetObservation(running, domain.AgentObservation{Present: true})
	h.boxes.SetObservation(finished, domain.AgentObservation{Present: true, Finished: true})
	if err := h.r.recoverAll(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.wantStatus(running, domain.StatusWorking)
	h.wantStatus(finished, domain.StatusDone)
}

// Stopping a task that is still queued needs no box, so a restart has nothing to do for it.
func TestRestartLeavesQueuedAndEndedTasksAlone(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	queued := g.created(domain.KindInteractive)
	ended := g.created(domain.KindInteractive)
	if _, err := g.r.Do(g.ctx, ended, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	g.settle()

	h := restarted(t, store)
	h.wantStatus(queued, domain.StatusQueued)
	h.wantStatus(ended, domain.StatusStopped)
	if got := h.boxes.Calls(); len(got) != 0 {
		t.Fatalf("calls %+v", got)
	}
}
