package domain_test

import (
	"reflect"
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

// boxLost is a scheduled task whose box died of memory with Docker's event first.
func boxLost(t *testing.T) *run {
	t.Helper()
	r := working(t, domain.KindScheduled)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied, OOMKilled: true})
	r.dropped(domain.AgentExited{Run: 1, Code: 137}, domain.DropStaleKey)
	return r
}

// requireSameLoss checks that two runs ended up in the same place after the same box loss.
func requireSameLoss(t *testing.T, got, want *run) {
	t.Helper()
	if !reflect.DeepEqual(got.task.Life, want.task.Life) || got.task.AgentRun != want.task.AgentRun || got.task.BoxGen != want.task.BoxGen {
		t.Fatalf("exit first: run %d gen %d life %+v\nbox first: run %d gen %d life %+v",
			got.task.AgentRun, got.task.BoxGen, got.task.Life, want.task.AgentRun, want.task.BoxGen, want.task.Life)
	}
	if lastEntry(got).Text != lastEntry(want).Text {
		t.Errorf("last entry %q, want %q", lastEntry(got).Text, lastEntry(want).Text)
	}
}

func TestAnExitBeforeTheBoxEventInspectsTheBoxInsteadOfFailing(t *testing.T) {
	r := working(t, domain.KindScheduled)
	res := r.apply(domain.AgentExited{Run: 1, Code: 137})
	if kinds(res.Effects) != "InspectBox" {
		t.Fatalf("effects = %q, want the box inspected", kinds(res.Effects))
	}
	r.want(domain.StatusWorking)
	if _, ok := r.task.Life.(domain.Active); !ok {
		t.Fatalf("life = %+v, want the run still active", r.task.Life)
	}
	r.dropped(domain.AgentExited{Run: 1, Code: 137}, domain.DropNoop)
}

func TestAnExitBeforeTheBoxEventEndsLikeTheBoxEventFirst(t *testing.T) {
	t.Run("the inspect finds the box down", func(t *testing.T) {
		r := working(t, domain.KindScheduled)
		r.apply(domain.AgentExited{Run: 1, Code: 137})
		inspect := latest[domain.InspectBox](r)
		r.apply(domain.BoxInspected{Ask: inspect.Ask, OOMKilled: true})
		requireSameLoss(t, r, boxLost(t))
		r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied, OOMKilled: true}, domain.DropStaleKey)
		r.dropped(domain.BoxInspected{Ask: inspect.Ask, OOMKilled: true}, domain.DropStaleAsk)
	})
	t.Run("the box event lands before the inspect answer", func(t *testing.T) {
		r := working(t, domain.KindScheduled)
		r.apply(domain.AgentExited{Run: 1, Code: 137})
		inspect := latest[domain.InspectBox](r)
		r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied, OOMKilled: true})
		requireSameLoss(t, r, boxLost(t))
		r.dropped(domain.BoxInspected{Ask: inspect.Ask, OOMKilled: true}, domain.DropStaleAsk)
	})
}

func TestAnExitWithTheBoxStillUpFailsTheRun(t *testing.T) {
	tests := []struct {
		name    string
		resolve func(r *run, ask domain.AskID) domain.Event
	}{
		{"inspect says running", func(_ *run, ask domain.AskID) domain.Event {
			return domain.BoxInspected{Ask: ask, Running: true}
		}},
		{"a process OOM event with the box up", func(_ *run, _ domain.AskID) domain.Event {
			return domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, StillRunning: true}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := working(t, domain.KindInteractive)
			r.apply(domain.AgentExited{Run: 1, Code: 2})
			res := r.apply(tc.resolve(r, latest[domain.InspectBox](r).Ask))
			closing, ok := r.task.Life.(domain.Closing)
			if !ok || closing.Want != domain.OutcomeFailed || closing.Reason != "The agent exited with code 2" || kinds(res.Effects) != "ReadSummary" {
				t.Errorf("life %+v, effects %q: want a failed close naming the exit code", r.task.Life, kinds(res.Effects))
			}
		})
	}
}

func TestARestartWhileInspectingAsksAgain(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.apply(domain.AgentExited{Run: 1, Code: 137})
	inspect := latest[domain.InspectBox](r)
	res := restarted(t, r, domain.AgentObservation{Present: true, Finished: true, ExitCode: 137})
	if kinds(res.Effects) != "InspectBox" || latest[domain.InspectBox](r).Ask != inspect.Ask {
		t.Fatalf("effects %q: want the same inspect asked again, not a failure", kinds(res.Effects))
	}
	r.apply(domain.BoxInspected{Ask: inspect.Ask, OOMKilled: true})
	requireSameLoss(t, r, boxLost(t))
}

func TestARestartThatFindsTheAgentExitedInspectsTheBox(t *testing.T) {
	r := working(t, domain.KindScheduled)
	res := restarted(t, r, domain.AgentObservation{Present: true, Finished: true, ExitCode: 137})
	if kinds(res.Effects) != "InspectBox" {
		t.Fatalf("effects = %q, want the box inspected before failing", kinds(res.Effects))
	}
}

func TestAnInspectAnswerAfterTheBoxSleptIsStale(t *testing.T) {
	r := waiting(t)
	r.apply(domain.AgentExited{Run: 1, Code: 1})
	inspect := latest[domain.InspectBox](r)
	r.advanceTo(r.policy.IdleSleep)
	if r.task.BoxWant() != domain.BoxAsleep {
		t.Fatalf("box want %v", r.task.BoxWant())
	}
	r.dropped(domain.BoxInspected{Ask: inspect.Ask}, domain.DropStaleAsk)
}
