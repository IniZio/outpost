package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// waiting is an interactive task whose first turn ended, so it waits for its owner.
func waiting(t *testing.T) *run {
	t.Helper()
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.report(domain.HCWorking, "")
	r.advance(time.Minute)
	r.report(domain.HCIdle, "")
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "Ready for review."})
	return r
}

func TestAWaitingTaskSleepsAndAMessageWakesIt(t *testing.T) {
	r := waiting(t)
	r.want(domain.StatusWaiting)

	fx := r.advanceTo(r.policy.IdleSleep)
	if kinds(fx) != "StopBox" || r.task.BoxWant() != domain.BoxAsleep || r.task.AgentRun != 2 {
		t.Fatalf("effects %q, box want %v, run %d: sleeping stops the box and moves the run on", kinds(fx), r.task.BoxWant(), r.task.AgentRun)
	}
	r.want(domain.StatusWaiting)
	if !strings.Contains(lastEntry(r).Text, "went to sleep after 30 idle minutes") {
		t.Errorf("last entry %+v", lastEntry(r))
	}

	r.dropped(domain.AgentExited{Run: 1, Code: 143}, domain.DropStaleKey)
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropStaleKey)
	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxDied}, domain.DropNoop)
	r.dropped(domain.Tick{}, domain.DropNoop)

	res := r.apply(domain.UserMessage{By: owner, Text: "add tests too"})
	ensure := latest[domain.EnsureRunning](r)
	if kinds(res.Effects) != "EnsureRunning" || ensure.Run != 2 || ensure.Gen != 1 {
		t.Fatalf("effects %q, ensure %+v: want one wake for run 2 in the same box", kinds(res.Effects), ensure)
	}
	r.want(domain.StatusWorking)
	r.apply(domain.UserMessage{By: owner, Text: "and a changelog"})
	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxStarted}, domain.DropNoop)
	r.dropped(domain.Tick{}, domain.DropNoop)

	res = r.apply(domain.Ensured{Ask: ensure.Ask, Revived: true})
	if kinds(res.Effects) != "ClearDoneMarker,SendToAgent,SendToAgent" {
		t.Fatalf("effects = %q", kinds(res.Effects))
	}
	if got := sent(res.Effects); got[0] != "add tests too" || got[1] != "and a changelog" {
		t.Errorf("sent %q, want both messages in order", got)
	}
	if r.task.BoxWant() != domain.BoxUp || lastEntry(r).Kind != domain.EntryMessage {
		t.Errorf("box want %v, last %+v", r.task.BoxWant(), lastEntry(r))
	}

	r.dropped(domain.BoxEvent{Gen: 1, Kind: domain.BoxStarted}, domain.DropNoop)
	r.dropped(domain.AgentReport{Run: 2, Status: domain.HCIdle}, domain.DropNoop)
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	r.want(domain.StatusWaiting)
}

func TestARealRestartAfterAWakeStillReplacesTheBox(t *testing.T) {
	r := waiting(t)
	r.advanceTo(r.policy.IdleSleep)
	r.apply(domain.UserMessage{By: owner, Text: "hi"})
	r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})

	r.advance(r.policy.BoxStartGrace)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxStarted})
	if r.task.BoxWant() != domain.BoxReplacing {
		t.Errorf("box want %v, want a replacement after a start the grace does not explain", r.task.BoxWant())
	}
}

func TestSleepRestartsWhenTheTaskLogsSomething(t *testing.T) {
	r := waiting(t)
	r.advance(20 * time.Minute)
	r.apply(domain.BoxEvent{Gen: 1, Kind: domain.BoxOOM, StillRunning: true})

	if fx := r.advanceTo(15 * time.Minute); len(fx) != 0 {
		t.Fatalf("effects = %q: the box slept 5 minutes after the event instead of 30", kinds(fx))
	}
	if fx := r.advanceTo(15 * time.Minute); kinds(fx) != "StopBox" {
		t.Errorf("effects = %q, want the box asleep 30 minutes after the last event", kinds(fx))
	}
}

func TestOnlyAnAttendedWaitingTaskSleeps(t *testing.T) {
	tests := []struct {
		name   string
		kind   domain.Kind
		report domain.HCStatus
		sleeps bool
	}{
		{"attended and waiting", domain.KindInteractive, domain.HCIdle, true},
		{"attended and asking", domain.KindInteractive, domain.HCBlocked, true},
		{"attended and working", domain.KindInteractive, domain.HCWorking, false},
		{"unattended and idle", domain.KindScheduled, domain.HCIdle, false},
		{"unattended and asking", domain.KindScheduled, domain.HCBlocked, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t, tc.kind, spec()).up()
			r.report(domain.HCWorking, "")
			r.report(tc.report, "")
			fx := r.advanceTo(3 * time.Hour)
			if got := has[domain.StopBox](fx); got != tc.sleeps {
				t.Errorf("box stopped = %v, want %v", got, tc.sleeps)
			}
		})
	}
}

func TestSleepIsOffWhenThePolicySaysSo(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.policy.IdleSleep = 0
	r.up()
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	if _, ok := r.task.NextDeadline(); ok {
		t.Error("a deadline is armed although sleep is off")
	}
}

func TestAFailedWakeFromSleepKeepsTheTaskWaiting(t *testing.T) {
	r := waiting(t)
	r.advanceTo(r.policy.IdleSleep)
	r.apply(domain.UserMessage{By: owner, Text: "wake up"})
	r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Err: "docker is down"})

	r.want(domain.StatusWaiting)
	if r.task.BoxWant() != domain.BoxAsleep || !strings.Contains(lastEntry(r).Text, "docker is down") {
		t.Errorf("box want %v, last %+v", r.task.BoxWant(), lastEntry(r))
	}
}

func TestMessagesAreOnlyForTheOwnerAndATaskThatCanTakeThem(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	mustRefuse(t, r.refused(domain.UserMessage{By: owner, Text: "hi"}), domain.ErrNotReady)
	r.apply(domain.SlotGranted{Machine: "m1"})
	mustRefuse(t, r.refused(domain.UserMessage{By: owner, Text: "hi"}), domain.ErrNotReady)
	mustRefuse(t, r.refused(domain.UserResume{By: owner}), domain.ErrNotReady)
	r.apply(domain.Started{Ask: latest[domain.StartTask](r).Ask})
	mustRefuse(t, r.refused(domain.UserMessage{By: stranger, Text: "hi"}), domain.ErrForbidden)
	mustRefuse(t, r.refused(domain.UserResume{By: stranger}), domain.ErrForbidden)

	r.apply(domain.UserStop{By: owner})
	mustRefuse(t, r.refused(domain.UserMessage{By: owner, Text: "hi"}), domain.ErrClosing)
	mustRefuse(t, r.refused(domain.UserResume{By: owner}), domain.ErrClosing)
}

func TestAMessageToAWorkingAgentIsTypedStraightIn(t *testing.T) {
	r := working(t, domain.KindInteractive)
	res := r.apply(domain.UserMessage{By: owner, Text: "also bump the lockfile"})
	if kinds(res.Effects) != "ClearDoneMarker,SendToAgent" || lastEntry(r).Kind != domain.EntryMessage {
		t.Errorf("effects %q, last %+v", kinds(res.Effects), lastEntry(r))
	}
	r.want(domain.StatusWorking)
	if a, _ := r.task.Life.(domain.Active); a.Turn.State != domain.TurnRunning {
		t.Errorf("turn = %v, want the running turn left alone", a.Turn.State)
	}
}

func TestAMessageToAWaitingAgentStartsAPendingTurn(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.policy.IdleSleep = 0
	r.up()
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	r.apply(domain.UserMessage{By: owner, Text: "one more thing"})
	r.want(domain.StatusWorking)

	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropNoop)
	r.report(domain.HCWorking, "")
	r.report(domain.HCIdle, "")
	r.want(domain.StatusWaiting)
}
