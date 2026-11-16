package domain_test

import (
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// restarted stores the task, restores it as the server would after a restart, and feeds Recovered.
func restarted(t *testing.T, r *run, obs domain.AgentObservation) domain.Result {
	t.Helper()
	restored, err := domain.Restore(r.task.Record())
	if err != nil {
		t.Fatalf("restore after restart: %v", err)
	}
	r.task = restored
	return r.apply(domain.Recovered{Agent: obs})
}

var (
	agentUp   = domain.AgentObservation{Present: true}
	agentGone = domain.AgentObservation{}
)

func TestARestartDuringAFreshStartQueuesTheTaskAgain(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.apply(domain.SlotGranted{Machine: "m1"})
	ask := latest[domain.StartTask](r).Ask

	res := restarted(t, r, agentGone)
	r.want(domain.StatusQueued)
	if kinds(res.Effects) != "Release,RunScheduler" {
		t.Errorf("effects = %q: a start is never resumed half way", kinds(res.Effects))
	}
	r.dropped(domain.Started{Ask: ask}, domain.DropStaleLife)

	r.apply(domain.SlotGranted{Machine: "m1"})
	if start := latest[domain.StartTask](r); start.Run != 2 || start.Gen != 2 || start.Ask == ask {
		t.Errorf("second start %+v: keys and asks must never be reused", start)
	}
}

func TestARestartDuringAWakeGivesTheEndedRunBack(t *testing.T) {
	r := stopped(t)
	r.apply(domain.UserMessage{By: owner, Text: "carry on"})
	res := restarted(t, r, agentGone)
	r.want(domain.StatusStopped)
	if !has[domain.StopBox](res.Effects) {
		t.Errorf("effects = %q: the wake may have started the box", kinds(res.Effects))
	}
	if _, ok := r.task.Life.(domain.Ended); !ok {
		t.Errorf("life = %+v", r.task.Life)
	}
}

func TestARestartLeavesAQueuedOrEndedTaskAlone(t *testing.T) {
	queued := newRun(t, domain.KindInteractive, spec())
	queued.dropped(domain.Recovered{}, domain.DropNoop)
	ended := runOf(t, fixture(domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeDone, At: t0}))
	ended.dropped(domain.Recovered{}, domain.DropNoop)
}

func TestARestartWithTheAgentStillRunningChangesNothing(t *testing.T) {
	r := working(t, domain.KindInteractive)
	restored, err := domain.Restore(r.task.Record())
	if err != nil {
		t.Fatal(err)
	}
	r.task = restored
	r.dropped(domain.Recovered{Agent: agentUp}, domain.DropNoop)
	r.want(domain.StatusWorking)
}

func TestARestartFinishesARunWhoseAgentIsGone(t *testing.T) {
	tests := []struct {
		name   string
		obs    domain.AgentObservation
		status domain.Status
		reason string
	}{
		{"lost", agentGone, domain.StatusFailed, "The agent was lost while Outpost was down"},
		{"exited cleanly", domain.AgentObservation{Present: true, Finished: true}, domain.StatusDone, ""},
		{"exited with an error", domain.AgentObservation{Present: true, Finished: true, ExitCode: 3}, domain.StatusFailed, "The agent exited with code 3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := working(t, domain.KindInteractive)
			res := restarted(t, r, tc.obs)
			if kinds(res.Effects) != "ReadSummary" {
				t.Fatalf("effects = %q", kinds(res.Effects))
			}
			r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
			ended, _ := r.task.Life.(domain.Ended)
			if r.task.Status() != tc.status || ended.Error != tc.reason {
				t.Errorf("status %v, error %q", r.task.Status(), ended.Error)
			}
		})
	}
}

func TestARestartReissuesTheProbeAnUnattendedRunWasWaitingOn(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.report(domain.HCIdle, "")
	ask := latest[domain.ProbeDoneMarker](r).Ask

	res := restarted(t, r, agentUp)
	if kinds(res.Effects) != "ProbeDoneMarker" || latest[domain.ProbeDoneMarker](r).Ask != ask {
		t.Fatalf("effects %q: want the same probe asked again", kinds(res.Effects))
	}
	res = r.apply(domain.DoneMarker{Ask: ask, Present: true})
	if kinds(res.Effects) != "ReadSummary" {
		t.Errorf("effects = %q: the answer to the re-issued probe must still count", kinds(res.Effects))
	}
}

func TestARestartRerunsTheTurnEndCheckOfAnIdleUnattendedRun(t *testing.T) {
	a := active(domain.TurnPaused)
	a.Deadlines = a.Deadlines.Set(domain.DlNudge, t0.Add(3*time.Minute))
	r := runOf(t, fixture(domain.KindScheduled, a))

	res := restarted(t, r, agentUp)
	if kinds(res.Effects) != "ProbeDoneMarker" {
		t.Fatalf("effects = %q", kinds(res.Effects))
	}
	if at, _ := r.task.NextDeadline(); !at.Equal(t0.Add(3 * time.Minute)) {
		t.Errorf("deadline moved to %v: the persisted nudge must be kept", at)
	}
}

func TestARestartArmsTheLadderWhenAnIdleUnattendedRunHadNone(t *testing.T) {
	r := runOf(t, fixture(domain.KindScheduled, active(domain.TurnPaused)))
	res := restarted(t, r, agentUp)
	if kinds(res.Effects) != "ProbeDoneMarker" {
		t.Fatalf("effects = %q", kinds(res.Effects))
	}
	if at, ok := r.task.NextDeadline(); !ok || at.Sub(r.now) != 3*time.Minute {
		t.Errorf("deadline in %v (%v), want the first nudge in 3m", at.Sub(r.now), ok)
	}
}

func TestARestartKeepsTheDeadlinesAndTheLadderGoesOn(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.report(domain.HCIdle, "")
	marked(r, false)
	restarted(t, r, agentUp)
	marked(r, false)

	fx := r.advanceTo(3 * time.Minute)
	if kinds(fx) != "ProbeDoneMarker" {
		t.Fatalf("effects = %q, want the nudge to fall due after the restart", kinds(fx))
	}
	res := marked(r, false)
	if got := sent(res.Effects); len(got) != 1 || got[0] != domain.NudgeText {
		t.Errorf("sent %q, want the nudge", got)
	}
}

func TestARestartDoesNotCloseARunWhoseBoxIsAsleepOrBeingReplaced(t *testing.T) {
	r := waiting(t)
	r.advanceTo(r.policy.IdleSleep)
	r.dropped(domain.Recovered{Agent: agentGone}, domain.DropNoop)

	r.apply(domain.UserMessage{By: owner, Text: "wake"})
	ask := latest[domain.EnsureRunning](r).Ask
	res := restarted(t, r, agentGone)
	if again := latest[domain.EnsureRunning](r); kinds(res.Effects) != "EnsureRunning" || again.Ask != ask || again.Run != 2 {
		t.Errorf("effects %q, ensure %+v: want the same wake asked again", kinds(res.Effects), again)
	}
	r.apply(domain.Ensured{Ask: ask, Revived: true})
	r.want(domain.StatusWorking)
}

func TestARestartReissuesTheFinishWindowsQuestion(t *testing.T) {
	r := working(t, domain.KindInteractive)
	r.apply(domain.UserStop{By: owner})
	ask := latest[domain.ReadSummary](r).Ask
	res := restarted(t, r, agentGone)
	if kinds(res.Effects) != "ReadSummary" || latest[domain.ReadSummary](r).Ask != ask {
		t.Errorf("effects = %q", kinds(res.Effects))
	}

	setup := closingSetup(t)
	setup.apply(domain.SummaryRead{Ask: 7})
	res = restarted(t, setup, agentGone)
	if kinds(res.Effects) != "SaveTemplate" {
		t.Errorf("effects = %q", kinds(res.Effects))
	}
}
