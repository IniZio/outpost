package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

func TestTheNudgeLadderArmsTheNextRungAtTurnEnd(t *testing.T) {
	tests := []struct {
		name   string
		sent   int
		worked time.Duration
		delay  time.Duration
	}{
		{"first turn end", 0, 20 * time.Second, 3 * time.Minute},
		{"after one nudge", 1, 20 * time.Second, 10 * time.Minute},
		{"after two nudges", 2, 20 * time.Second, 10 * time.Minute},
		{"after three nudges it gives up", 3, 20 * time.Second, 30 * time.Minute},
		{"two minutes of work resets the ladder", 3, 2 * time.Minute, 3 * time.Minute},
		{"a long turn resets a half-way ladder", 2, 40 * time.Minute, 3 * time.Minute},
		{"just under two minutes does not reset", 3, 2*time.Minute - time.Second, 30 * time.Minute},
		{"a short turn after the first nudge", 1, 0, 10 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := active(domain.TurnRunning)
			a.Turn.WorkingSince = t0.Add(-tc.worked)
			a.Nudge.Sent = tc.sent
			r := runOf(t, fixture(domain.KindScheduled, a))
			r.report(domain.HCIdle, "")
			if at, ok := r.task.NextDeadline(); !ok || at.Sub(t0) != tc.delay {
				t.Errorf("next deadline in %v (%v), want %v", at.Sub(t0), ok, tc.delay)
			}
		})
	}
}

// marked answers the latest done-marker probe.
func marked(r *run, present bool) domain.Result {
	r.t.Helper()
	return r.apply(domain.DoneMarker{Ask: latest[domain.ProbeDoneMarker](r).Ask, Present: present})
}

func TestAnUnattendedRunEndsAtTurnEndWhenTheMarkerExists(t *testing.T) {
	r := newRun(t, domain.KindScheduled, domain.Spec{Repo: "acme/app", Timeout: 3 * time.Hour})
	r.apply(domain.SlotGranted{Machine: "m1"})
	r.apply(domain.Started{Ask: latest[domain.StartTask](r).Ask})

	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropNoop)
	r.report(domain.HCWorking, "Editing")
	r.advance(7 * time.Minute)
	res := r.report(domain.HCIdle, "")
	if kinds(res.Effects) != "ProbeDoneMarker" {
		t.Fatalf("turn end effects = %q", kinds(res.Effects))
	}
	r.want(domain.StatusWorking)

	res = marked(r, true)
	if kinds(res.Effects) != "ReadSummary" {
		t.Fatalf("effects = %q, want the summary read", kinds(res.Effects))
	}
	r.want(domain.StatusWorking)

	res = r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "Opened a PR."})
	r.want(domain.StatusDone)
	ended, _ := r.task.Life.(domain.Ended)
	if kinds(res.Effects) != "RunScheduler,StopBox" || ended.Summary != "Opened a PR." || lastEntry(r).Kind != domain.EntryDone {
		t.Errorf("effects %q, ended %+v, last %+v", kinds(res.Effects), ended, lastEntry(r))
	}
	if at, ok := r.task.NextDeadline(); !ok || !at.Equal(ended.ReleaseAt) {
		t.Errorf("next deadline %v, want the release at %v", at, ended.ReleaseAt)
	}
}

func TestAnUnattendedRunWithoutTheMarkerIsNudgedUntilItGivesUp(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	r.report(domain.HCWorking, "")
	r.advance(6 * time.Minute)

	r.report(domain.HCIdle, "")
	staleProbe := latest[domain.ProbeDoneMarker](r).Ask
	r.report(domain.HCWorking, "")
	r.dropped(domain.DoneMarker{Ask: staleProbe, Present: true}, domain.DropStaleAsk)
	r.want(domain.StatusWorking)
	if _, ok := r.task.NextDeadline(); ok {
		t.Fatal("a working agent must leave no nudge armed")
	}

	r.advance(30 * time.Second)
	r.report(domain.HCIdle, "")
	marked(r, false)
	for rung, wait := range []time.Duration{3 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		fx := r.advanceTo(wait)
		if kinds(fx) != "ProbeDoneMarker" {
			t.Fatalf("rung %d: effects = %q, want a probe before nudging", rung+1, kinds(fx))
		}
		res := marked(r, false)
		if got := sent(res.Effects); len(got) != 1 || got[0] != domain.NudgeText {
			t.Fatalf("rung %d: sent %q, want the nudge", rung+1, got)
		}
		r.want(domain.StatusWorking)
		r.report(domain.HCWorking, "")
		r.advance(20 * time.Second)
		r.report(domain.HCIdle, "")
		marked(r, false)
	}

	fx := r.advanceTo(30 * time.Minute)
	if kinds(fx) != "ProbeDoneMarker" {
		t.Fatalf("give-up effects = %q", kinds(fx))
	}
	res := marked(r, false)
	if kinds(res.Effects) != "ReadSummary" {
		t.Fatalf("effects = %q, want the run closed", kinds(res.Effects))
	}
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
	r.want(domain.StatusDone)
	if !hasWarning(r, "so it was finished as is") {
		t.Errorf("timeline %+v lacks the give-up warning", r.timeline)
	}
}

func hasWarning(r *run, text string) bool {
	for _, e := range r.timeline {
		if e.Kind == domain.EntryWarning && strings.Contains(e.Text, text) {
			return true
		}
	}
	return false
}

func TestNudgeMarkerPresentAtNudgeTimeFinishesInsteadOfNudging(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	marked(r, false)
	r.advanceTo(3 * time.Minute)
	res := marked(r, true)
	if len(sent(res.Effects)) != 0 || kinds(res.Effects) != "ReadSummary" {
		t.Errorf("effects = %q, want a finish and no nudge", kinds(res.Effects))
	}
}

func TestANudgeDueWhileTheAgentWorksProbesNothing(t *testing.T) {
	a := active(domain.TurnRunning)
	a.Turn.WorkingSince = t0
	a.Deadlines = a.Deadlines.Set(domain.DlNudge, t0)
	task := fixture(domain.KindScheduled, a)
	r := runOf(t, task)
	res := r.apply(domain.Tick{})
	if len(res.Effects) != 0 {
		t.Errorf("effects = %q, want none", kinds(res.Effects))
	}
}

func TestAnUnattendedQuestionArmsTheLadderLikeATurnEnd(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	r.advance(5 * time.Minute)
	r.report(domain.HCWorking, "")
	r.advance(5 * time.Minute)
	res := r.report(domain.HCBlocked, "")

	r.want(domain.StatusWaiting)
	if kinds(res.Effects) != "ProbeDoneMarker" || lastEntry(r).Kind != domain.EntryWaiting {
		t.Fatalf("effects %q, last %+v", kinds(res.Effects), lastEntry(r))
	}
	if at, _ := r.task.NextDeadline(); at.Sub(r.now) != 3*time.Minute {
		t.Errorf("first nudge in %v, want 3m: five minutes of work reset the ladder", at.Sub(r.now))
	}

	r.report(domain.HCIdle, "")
	r.want(domain.StatusWorking)
	if got := r.advanceTo(3 * time.Minute); kinds(got) != "ProbeDoneMarker" {
		t.Errorf("effects = %q, want the ladder to carry on after the question went away", kinds(got))
	}
}

func TestAnAttendedQuestionJustWaits(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.report(domain.HCWorking, "")
	res := r.report(domain.HCBlocked, "")
	r.want(domain.StatusWaiting)
	if len(res.Effects) != 0 || lastEntry(r).Text != "The agent is asking a question" {
		t.Errorf("effects %q, last %+v", kinds(res.Effects), lastEntry(r))
	}
}

func TestASilentAgentStillReachesTheGiveUp(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	marked(r, false)
	for rung := range len(r.policy.NudgeAfter) {
		if _, ok := r.task.NextDeadline(); !ok {
			t.Fatalf("rung %d: no deadline armed although the agent never answered the last nudge", rung+1)
		}
		r.advanceTo(r.policy.NudgeAfter[rung])
		if got := sent(marked(r, false).Effects); len(got) != 1 || got[0] != domain.NudgeText {
			t.Fatalf("rung %d: sent %q, want the nudge", rung+1, got)
		}
	}
	if at, ok := r.task.NextDeadline(); !ok || at.Sub(r.now) != r.policy.GiveUp {
		t.Fatalf("next deadline in %v (%v), want the give-up in %v", at.Sub(r.now), ok, r.policy.GiveUp)
	}
	r.advanceTo(r.policy.GiveUp)
	marked(r, false)
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
	r.want(domain.StatusDone)
}

func TestAnUnattendedAgentThatNeverReportsATurnIsCheckedOnAndEnds(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	if at, ok := r.task.NextDeadline(); !r.task.Busy() || !ok {
		t.Fatalf("busy %v, deadline %v (%v): a pending agent holds a slot, so something must end it", r.task.Busy(), at, ok)
	}
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropNoop)
	fx := r.advanceTo(r.policy.StallAfter)
	if kinds(fx) != "ProbeDoneMarker" {
		t.Fatalf("effects = %q, want the done marker checked as at a turn end", kinds(fx))
	}
	r.want(domain.StatusWorking)
	marked(r, false)
	if at, ok := r.task.NextDeadline(); !ok || at.Sub(r.now) != r.policy.NudgeAfter[0] {
		t.Errorf("next deadline in %v (%v), want the nudge ladder to take over", at.Sub(r.now), ok)
	}
}

func TestAPendingAgentThatMarkedTheRunDoneFinishesAtTheStall(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	r.advanceTo(r.policy.StallAfter)
	if kinds(marked(r, true).Effects) != "ReadSummary" {
		t.Errorf("want the run closed: a turn too short for hc to report still counts")
	}
}

func TestAMessageToAPendingUnattendedRunKeepsTheStallDeadline(t *testing.T) {
	r := newRun(t, domain.KindScheduled, spec()).up()
	want, _ := r.task.NextDeadline()
	r.advance(time.Minute)
	r.apply(domain.UserMessage{By: owner, Text: "go on"})
	if got, _ := r.task.NextDeadline(); !got.Equal(want) {
		t.Errorf("deadline %v, want %v", got, want)
	}
	r.report(domain.HCWorking, "")
	if _, ok := r.task.NextDeadline(); ok {
		t.Error("a working agent must leave no stall armed")
	}
}
