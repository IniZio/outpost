package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// stopped is a task that ran, was stopped, and finished: resumable, run 1.
func stopped(t *testing.T) *run {
	t.Helper()
	r := working(t, domain.KindInteractive)
	r.apply(domain.UserStop{By: owner})
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "Stopped midway."})
	r.want(domain.StatusStopped)
	return r
}

func TestAStaleReportAfterStopCannotEndTheNextTurnAfterAResume(t *testing.T) {
	r := stopped(t)
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropStaleLife)

	res := r.apply(domain.UserMessage{By: owner, Text: "carry on"})
	wake := latest[domain.EnsureRunning](r)
	if kinds(res.Effects) != "EnsureRunning" || wake.Run != 2 {
		t.Fatalf("effects %q, wake %+v: a resume is a new agent launch, run 2", kinds(res.Effects), wake)
	}
	r.want(domain.StatusStopped)
	if r.task.Busy() {
		t.Error("a task being woken holds no slot")
	}
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCWorking}, domain.DropStaleKey)
	r.dropped(domain.AgentExited{Run: 1, Code: 143}, domain.DropStaleKey)

	res = r.apply(domain.Ensured{Ask: wake.Ask, Revived: true})
	if kinds(res.Effects) != "ClearDoneMarker,SendToAgent" || sent(res.Effects)[0] != "carry on" {
		t.Fatalf("effects = %q", kinds(res.Effects))
	}
	r.want(domain.StatusWorking)
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropStaleKey)
	r.dropped(domain.AgentReport{Run: 2, Status: domain.HCIdle}, domain.DropNoop)
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	r.want(domain.StatusWaiting)
}

func TestAWakeBuffersWhatTheNewAgentReportsBeforeTheAnswer(t *testing.T) {
	r := stopped(t)
	r.apply(domain.UserMessage{By: owner, Text: "carry on"})
	r.apply(domain.UserMessage{By: owner, Text: "and be quick"})
	r.dropped(domain.UserResume{By: owner}, domain.DropNoop)
	r.apply(domain.AgentReport{Run: 2, Status: domain.HCIdle})
	r.apply(domain.AgentReport{Run: 2, Status: domain.HCWorking, Phase: "Resuming"})
	r.want(domain.StatusStopped)

	res := r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})
	if got := sent(res.Effects); len(got) != 2 || got[1] != "and be quick" {
		t.Errorf("sent %q, want both messages", got)
	}
	if res.Dropped != domain.Applied || r.task.Phase() != "Resuming" {
		t.Errorf("dropped %v, phase %q: the replayed idle must not cancel the wake", res.Dropped, r.task.Phase())
	}
}

func TestAFailedWakeGivesTheEndedRunBack(t *testing.T) {
	r := stopped(t)
	r.apply(domain.UserMessage{By: owner, Text: "carry on"})
	r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Err: "disk full"})

	r.want(domain.StatusStopped)
	ended, ok := r.task.Life.(domain.Ended)
	if !ok || ended.Summary != "Stopped midway." || !strings.Contains(lastEntry(r).Text, "disk full") {
		t.Errorf("life %+v, last %+v", r.task.Life, lastEntry(r))
	}
	res := r.apply(domain.UserResume{By: owner})
	if kinds(res.Effects) != "EnsureRunning" {
		t.Errorf("effects = %q, want a second attempt to be possible", kinds(res.Effects))
	}
}

func TestAResumeWithoutTextWaitsForTheOwner(t *testing.T) {
	r := stopped(t)
	r.apply(domain.UserResume{By: owner})
	res := r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})

	r.want(domain.StatusWaiting)
	if kinds(res.Effects) != "ClearDoneMarker" {
		t.Errorf("effects = %q: clear the marker so an old one cannot end the run again", kinds(res.Effects))
	}
}

func TestAResumedUnattendedTaskIsNudgedEvenWithoutAMessage(t *testing.T) {
	task := fixture(domain.KindScheduled, domain.Ended{Outcome: domain.OutcomeDone, At: t0, ReleaseAt: t0.Add(time.Hour)})
	r := runOf(t, task)
	r.apply(domain.UserResume{By: owner})
	res := r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})

	if kinds(res.Effects) != "ClearDoneMarker,ProbeDoneMarker" {
		t.Fatalf("effects = %q, want the marker cleared before it is probed", kinds(res.Effects))
	}
	if at, ok := r.task.NextDeadline(); !ok || at.Sub(r.now) != 3*time.Minute {
		t.Errorf("next deadline %v (%v), want the first nudge in 3m", at.Sub(r.now), ok)
	}
}

func TestAResumedTasksTimeoutCountsFromTheResume(t *testing.T) {
	task := fixture(domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeFailed, At: t0})
	task.Spec.Timeout = time.Hour
	r := runOf(t, task)
	r.policy.IdleSleep = 0
	r.advance(5 * time.Hour)
	r.apply(domain.UserResume{By: owner})
	r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})
	if at, ok := r.task.NextDeadline(); !ok || !at.Equal(r.now.Add(time.Hour)) {
		t.Errorf("deadline %v, want %v", at, r.now.Add(time.Hour))
	}
}

func TestAReleasedTaskCannotBeResumed(t *testing.T) {
	r := stopped(t)
	r.advanceTo(r.policy.KeepFinished)
	mustRefuse(t, r.refused(domain.UserMessage{By: owner, Text: "hi"}), domain.ErrReleased)
	mustRefuse(t, r.refused(domain.UserResume{By: owner}), domain.ErrReleased)
}

func TestResumingAKeptBoxMovesTheRunOnToo(t *testing.T) {
	r := keptBox(t)
	r.apply(domain.UserMessage{By: owner, Text: "try the save by hand"})
	wake := latest[domain.EnsureRunning](r)
	if wake.Run != 3 {
		t.Fatalf("wake run = %d, want 3: the live agent is replaced by one under the new run", wake.Run)
	}
	r.apply(domain.Ensured{Ask: wake.Ask, Revived: true})
	r.dropped(domain.AgentReport{Run: 2, Status: domain.HCIdle}, domain.DropStaleKey)
	r.report(domain.HCWorking, "")
}

func TestWakeAnswersForOtherQuestionsAreStale(t *testing.T) {
	r := stopped(t)
	r.dropped(domain.Ensured{Ask: 99}, domain.DropStaleLife)
	r.apply(domain.UserMessage{By: owner, Text: "go"})
	r.dropped(domain.Ensured{Ask: 99}, domain.DropStaleAsk)
	r.dropped(domain.Started{Ask: latest[domain.EnsureRunning](r).Ask}, domain.DropStaleLife)
	r.dropped(domain.Progressed{Ask: latest[domain.EnsureRunning](r).Ask}, domain.DropStaleLife)
}
