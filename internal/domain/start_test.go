package domain_test

import (
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

func TestAQueuedTaskStartsOnItsMachine(t *testing.T) {
	r := newRun(t, domain.KindInteractive, domain.Spec{Repo: "acme/app", Timeout: time.Hour})
	res := r.apply(domain.SlotGranted{Machine: "m1"})

	r.want(domain.StatusStarting)
	start := latest[domain.StartTask](r)
	if start.Run != 1 || start.Gen != 1 || r.task.Machine != "m1" {
		t.Errorf("StartTask %+v on machine %q: want run 1, gen 1 on m1", start, r.task.Machine)
	}
	if kinds(res.Effects) != "StartTask" || r.task.Phase() != "Preparing the computer" {
		t.Errorf("effects %q, phase %q", kinds(res.Effects), r.task.Phase())
	}
	if _, armed := r.task.NextDeadline(); armed {
		t.Error("a starting task has a deadline before its agent is up")
	}
}

func TestStartingReportsProgressThenTheAgentComesUp(t *testing.T) {
	r := newRun(t, domain.KindInteractive, domain.Spec{Repo: "acme/app", Timeout: time.Hour})
	r.apply(domain.SlotGranted{Machine: "m1"})
	ask := latest[domain.StartTask](r).Ask

	r.apply(domain.Progressed{Ask: ask, Phase: "Cloning acme/app", Note: "Cloning acme/app"})
	if r.task.Phase() != "Cloning acme/app" || lastEntry(r).Kind != domain.EntryPhase {
		t.Errorf("phase %q, last entry %+v", r.task.Phase(), lastEntry(r))
	}
	r.apply(domain.Progressed{Ask: ask, Phase: "Starting the agent"})
	if r.task.Phase() != "Starting the agent" || lastEntry(r).Text != "Cloning acme/app" {
		t.Errorf("a progress without a note must not log: phase %q, last %+v", r.task.Phase(), lastEntry(r))
	}

	r.advance(time.Minute)
	r.apply(domain.Started{Ask: ask})
	r.want(domain.StatusWorking)
	at, ok := r.task.NextDeadline()
	if want := t0.Add(time.Minute + time.Hour); !ok || !at.Equal(want) {
		t.Errorf("timeout armed at %v, want %v: it counts from the agent coming up", at, want)
	}
}

func TestStartingIgnoresAnswersToOtherQuestions(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.dropped(domain.Started{Ask: 1}, domain.DropStaleLife)
	r.apply(domain.SlotGranted{Machine: "m1"})
	r.dropped(domain.Started{Ask: 99}, domain.DropStaleAsk)
	r.dropped(domain.Progressed{Ask: 99, Phase: "x"}, domain.DropStaleAsk)
	r.dropped(domain.StartFailed{Ask: 99, Err: "x"}, domain.DropStaleAsk)
	r.dropped(domain.SlotGranted{Machine: "m2"}, domain.DropStaleLife)
	r.want(domain.StatusStarting)
}

func TestAFailedStartReleasesAtOnce(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.apply(domain.SlotGranted{Machine: "m1"})
	res := r.apply(domain.StartFailed{Ask: latest[domain.StartTask](r).Ask, Err: "clone failed"})

	r.want(domain.StatusFailed)
	if kinds(res.Effects) != "Release,RunScheduler" {
		t.Errorf("effects = %q, want the computer released and the scheduler kicked", kinds(res.Effects))
	}
	if lastEntry(r).Kind != domain.EntryError || lastEntry(r).Text != "clone failed" {
		t.Errorf("last entry = %+v", lastEntry(r))
	}
	if _, ok := r.task.NextDeadline(); ok || r.task.BoxWant() != domain.BoxReleased {
		t.Error("a failed start must be released, with nothing left to clean up later")
	}
}

func TestStoppingAQueuedTaskNeedsNoCleanup(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	mustRefuse(t, r.refused(domain.UserStop{By: stranger}), domain.ErrForbidden)

	res := r.apply(domain.UserStop{By: owner})
	r.want(domain.StatusStopped)
	if len(res.Effects) != 0 || r.task.BoxWant() != domain.BoxReleased {
		t.Errorf("effects %q, box want %v: a queued stop holds no slot and no computer", kinds(res.Effects), r.task.BoxWant())
	}
	r.dropped(domain.SlotGranted{Machine: "m1"}, domain.DropStaleLife)
	r.dropped(domain.UserStop{By: owner}, domain.DropNoop)
}

func TestStoppingAFreshStartAbortsAndReleases(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.apply(domain.SlotGranted{Machine: "m1"})
	ask := latest[domain.StartTask](r).Ask

	res := r.apply(domain.UserStop{By: owner})
	r.want(domain.StatusStopped)
	if kinds(res.Effects) != "AbortStart,Release,RunScheduler" {
		t.Errorf("effects = %q", kinds(res.Effects))
	}
	r.dropped(domain.Started{Ask: ask}, domain.DropStaleLife)
	r.dropped(domain.StartFailed{Ask: ask, Err: "aborted"}, domain.DropStaleLife)
	r.want(domain.StatusStopped)
}

func TestStoppingAWakeGivesTheOldRunBack(t *testing.T) {
	ended := domain.Ended{Outcome: domain.OutcomeDone, At: t0, Summary: "Opened a PR."}
	task := fixture(domain.KindInteractive, domain.Starting{Since: t0, Ask: 3, Resume: &ended})
	r := runOf(t, task)

	res := r.apply(domain.UserStop{By: owner})
	if kinds(res.Effects) != "AbortStart" {
		t.Errorf("effects = %q", kinds(res.Effects))
	}
	r.want(domain.StatusDone)
	if got, ok := r.task.Life.(domain.Ended); !ok || got.Summary != "Opened a PR." {
		t.Errorf("life = %+v, want the old Ended back whole", r.task.Life)
	}
}
