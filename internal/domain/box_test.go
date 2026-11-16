package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// working is an interactive or scheduled task whose agent is mid-turn.
func working(t *testing.T, kind domain.Kind) *run {
	t.Helper()
	r := newRun(t, kind, spec()).up()
	r.report(domain.HCWorking, "Editing")
	r.advance(10 * time.Minute)
	return r
}

func TestAnOOMKilledBoxMovesTheRunAndGenerationOn(t *testing.T) {
	r := working(t, domain.KindScheduled)

	res := r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, OOMKilled: true})
	r.want(domain.StatusWorking)
	if kinds(res.Effects) != "" || lastEntry(r).Kind != domain.EntryWarning || !strings.Contains(lastEntry(r).Text, "ran out of memory") {
		t.Fatalf("effects %q, last %+v", kinds(res.Effects), lastEntry(r))
	}
	if r.task.AgentRun != 2 || r.task.BoxGen != 2 || r.task.BoxWant() != domain.BoxReplacing {
		t.Fatalf("run %d, gen %d, want %v: loss must move on both keys", r.task.AgentRun, r.task.BoxGen, r.task.BoxWant())
	}

	r.dropped(domain.AgentExited{Run: 1, Code: 137}, domain.DropStaleKey)
	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied}, domain.DropStaleKey)
	r.dropped(domain.AgentExited{Run: 2, Code: 137}, domain.DropStaleLife)
	r.dropped(domain.BoxEvent{Gen: 2, Kind: domain.BoxDied}, domain.DropNoop)
	r.dropped(domain.Tick{}, domain.DropNoop)
}

func TestTheReplacementBoxIsStartedAndTheAgentResumes(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, OOMKilled: true})

	fx := r.advanceTo(r.policy.ReplaceDelay)
	ensure := latest[domain.EnsureRunning](r)
	if kinds(fx) != "EnsureRunning" || ensure.Run != 2 || ensure.Gen != 2 {
		t.Fatalf("effects %q, ensure %+v: want one EnsureRunning for run 2, gen 2", kinds(fx), ensure)
	}
	r.want(domain.StatusWorking)

	res := r.apply(domain.Ensured{Ask: ensure.Ask, Revived: true})
	if got := sent(res.Effects); len(got) != 1 || got[0] != domain.BoxBackNote("ran out of memory") {
		t.Fatalf("sent %q, want the box-back note", got)
	}
	if r.task.BoxWant() != domain.BoxUp || lastEntry(r).Text != "The box is back" {
		t.Errorf("box want %v, last %+v", r.task.BoxWant(), lastEntry(r))
	}

	r.dropped(domain.BoxEvent{Gen: 2, Kind: domain.BoxStarted}, domain.DropNoop)
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropStaleKey)
	r.dropped(domain.AgentReport{Run: 2, Status: domain.HCIdle}, domain.DropNoop)
	r.report(domain.HCWorking, "Restarting the build")
	r.report(domain.HCIdle, "")
	r.dropped(domain.Ensured{Ask: ensure.Ask, Revived: true}, domain.DropStaleAsk)
	if err := r.task.Validate(); err != nil {
		t.Error(err)
	}
}

func TestTheReplacementBoxCarriesTheNewGenerationSoItsOwnLossCounts(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, OOMKilled: true})
	r.advanceTo(r.policy.ReplaceDelay)
	r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})
	r.advance(time.Minute)

	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, OOMKilled: true}, domain.DropStaleKey)
	r.apply(domain.BoxEvent{Gen: 2, Kind: domain.BoxOOM, OOMKilled: true})
	if r.task.BoxWant() != domain.BoxReplacing || r.task.BoxGen != 3 || r.task.AgentRun != 3 {
		t.Errorf("box want %v, gen %d, run %d: the second box's loss must be replaced too", r.task.BoxWant(), r.task.BoxGen, r.task.AgentRun)
	}
}

func TestAProcessOOMWithTheBoxUpOnlyWarns(t *testing.T) {
	r := working(t, domain.KindScheduled)
	before := r.task

	res := r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, StillRunning: true})
	if got := sent(res.Effects); len(got) != 1 || got[0] != domain.OOMNote {
		t.Fatalf("sent %q, want the OOM note", got)
	}
	if lastEntry(r).Kind != domain.EntryWarning || r.task.AgentRun != before.AgentRun || r.task.BoxWant() != domain.BoxUp {
		t.Errorf("last %+v, run %d, box want %v: no state change expected", lastEntry(r), r.task.AgentRun, r.task.BoxWant())
	}
}

func TestABoxThatDiesIsReplacedWithTheRightReason(t *testing.T) {
	tests := []struct {
		name string
		ev   domain.BoxEvent
		note string
	}{
		{"died", domain.BoxEvent{Gen: 1, Kind: domain.BoxDied}, "stopped unexpectedly"},
		{"died of memory", domain.BoxEvent{Gen: 1, Kind: domain.BoxDied, OOMKilled: true}, "ran out of memory"},
		{"restarted by docker", domain.BoxEvent{Gen: 1, Kind: domain.BoxStarted}, "restarted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := working(t, domain.KindInteractive)
			r.apply(tc.ev)
			if r.task.BoxWant() != domain.BoxReplacing || !strings.Contains(lastEntry(r).Text, tc.note) {
				t.Errorf("box want %v, last %+v", r.task.BoxWant(), lastEntry(r))
			}
			r.advanceTo(r.policy.ReplaceDelay)
			r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})
			if got := sent(r.effects); len(got) != 1 || !strings.Contains(got[0], tc.note) {
				t.Errorf("sent %q, want one note naming %q", got, tc.note)
			}
		})
	}
}

func TestOurOwnBoxStartIsNotARestart(t *testing.T) {
	r := working(t, domain.KindInteractive)
	r.task.Life = withBoxStart(r.task, r.now.Add(-time.Second))
	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxStarted}, domain.DropNoop)
	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied, StillRunning: true}, domain.DropNoop)

	r.task.Life = withBoxStart(r.task, r.now.Add(-r.policy.BoxStartGrace))
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxStarted})
	if r.task.BoxWant() != domain.BoxReplacing {
		t.Errorf("box want %v: a start after the grace is a restart", r.task.BoxWant())
	}
}

func withBoxStart(t domain.Task, at time.Time) domain.Life {
	a := t.Life.(domain.Active)
	a.Box.StartedAt = at
	return a
}

func TestBoxEventsOutsideARunningTaskAreStale(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.dropped(domain.BoxEvent{Kind: domain.BoxDied}, domain.DropStaleLife)
	r.up()
	r.apply(domain.UserStop{By: owner})
	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied}, domain.DropStaleLife)
}

func TestAReplaceDueAfterTheBoxCameBackDoesNothing(t *testing.T) {
	a := active(domain.TurnPending)
	a.Deadlines = a.Deadlines.Set(domain.DlReplace, t0)
	r := runOf(t, fixture(domain.KindInteractive, a))
	if res := r.apply(domain.Tick{}); len(res.Effects) != 0 {
		t.Errorf("effects = %q, want none", kinds(res.Effects))
	}
}

func TestAnEnsuredAnswerForAReplacementThatWasNotNeeded(t *testing.T) {
	r := working(t, domain.KindInteractive)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied})
	r.advanceTo(r.policy.ReplaceDelay)
	ask := latest[domain.EnsureRunning](r).Ask

	res := r.apply(domain.Ensured{Ask: ask, Revived: false})
	if len(sent(res.Effects)) != 0 || r.task.BoxWant() != domain.BoxUp {
		t.Errorf("sent %q, box want %v: nothing was started, so there is nothing to tell the agent", sent(res.Effects), r.task.BoxWant())
	}
}

// failReplacement answers the replacement that is due next with an error.
func failReplacement(r *run, wait time.Duration) domain.Result {
	r.t.Helper()
	if fx := r.advanceTo(wait); kinds(fx) != "EnsureRunning" {
		r.t.Fatalf("effects = %q, want a replacement attempt after %v", kinds(fx), wait)
	}
	return r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Err: "no space left"})
}

// spendRetries fails every retry the policy allows, checking each is armed with its backoff.
func spendRetries(r *run) {
	r.t.Helper()
	for i, wait := range r.policy.ReplaceBackoff {
		if r.task.BoxWant() != domain.BoxReplacing || !strings.Contains(lastEntry(r).Text, "no space left") {
			r.t.Fatalf("try %d: box want %v, last %+v: want another try", i+1, r.task.BoxWant(), lastEntry(r))
		}
		if at, ok := r.task.NextDeadline(); !ok || at.Sub(r.now) != wait {
			r.t.Fatalf("try %d: next deadline in %v (%v), want %v", i+1, at.Sub(r.now), ok, wait)
		}
		failReplacement(r, wait)
	}
}

func TestAFailedReplacementRetriesThenFailsAnUnattendedRun(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied})
	failReplacement(r, r.policy.ReplaceDelay)
	spendRetries(r)
	closing, ok := r.task.Life.(domain.Closing)
	if !ok || closing.Want != domain.OutcomeFailed || !strings.Contains(closing.Reason, "no space left") {
		t.Fatalf("life = %+v, want a failed close once the retries are spent", r.task.Life)
	}
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
	r.want(domain.StatusFailed)
	if lastEntry(r).Kind != domain.EntryFailed || !strings.Contains(lastEntry(r).Text, "no space left") {
		t.Errorf("last entry %+v", lastEntry(r))
	}
}

func TestAFailedReplacementLeavesAnInteractiveBoxAsleepForAMessage(t *testing.T) {
	r := working(t, domain.KindInteractive)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied})
	failReplacement(r, r.policy.ReplaceDelay)
	spendRetries(r)
	r.want(domain.StatusWaiting)
	if r.task.BoxWant() != domain.BoxAsleep || !strings.Contains(lastEntry(r).Text, "send a message") {
		t.Fatalf("box want %v, last %+v: want the box asleep and the person told", r.task.BoxWant(), lastEntry(r))
	}
	if _, ok := r.task.NextDeadline(); ok {
		t.Errorf("a deadline is armed; a person decides what happens next")
	}
	fx := r.apply(domain.UserMessage{By: owner, Text: "try again"}).Effects
	if kinds(fx) != "EnsureRunning" {
		t.Errorf("effects = %q, want the message to wake the box", kinds(fx))
	}
}

func TestAReplacementThatWorksOnARetryCarriesOn(t *testing.T) {
	r := working(t, domain.KindScheduled)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied})
	r.apply(domain.UserMessage{By: owner, Text: "are you there?"})
	failReplacement(r, r.policy.ReplaceDelay)
	r.advanceTo(r.policy.ReplaceBackoff[0])
	res := r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})
	if got := sent(res.Effects); len(got) != 2 || got[1] != "are you there?" {
		t.Errorf("sent %q, want the box-back note and the parked message", got)
	}
	r.apply(domain.BoxEvent{Gen: 2, Kind: domain.BoxDied})
	failReplacement(r, r.policy.ReplaceDelay)
	if at, _ := r.task.NextDeadline(); at.Sub(r.now) != r.policy.ReplaceBackoff[0] {
		t.Errorf("next try in %v: a new loss starts the retries over", at.Sub(r.now))
	}
}
