package domain_test

import (
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// turnFixture is an active task whose agent is in the given turn state.
func turnFixture(t *testing.T, kind domain.Kind, state domain.TurnState, phase string) *run {
	a := active(state)
	if state == domain.TurnRunning {
		a.Turn.WorkingSince, a.Turn.Phase = t0, phase
	}
	return runOf(t, fixture(kind, a))
}

func TestTheTurnMachineFollowsHCReports(t *testing.T) {
	const (
		work = domain.HCWorking
		idle = domain.HCIdle
		done = domain.HCDone
		fail = domain.HCError
		ask  = domain.HCBlocked
	)
	tests := []struct {
		name    string
		kind    domain.Kind
		from    domain.TurnState
		report  domain.HCStatus
		phase   string
		drop    domain.Drop
		status  domain.Status
		effects string
	}{
		{"claude's first idle is ignored", domain.KindInteractive, domain.TurnPending, idle, "", domain.DropNoop, domain.StatusWorking, ""},
		{"a done report before the first turn is ignored", domain.KindInteractive, domain.TurnPending, done, "", domain.DropNoop, domain.StatusWorking, ""},
		{"an error report before the first turn is ignored", domain.KindInteractive, domain.TurnPending, fail, "", domain.DropNoop, domain.StatusWorking, ""},
		{"the first working report starts the turn", domain.KindInteractive, domain.TurnPending, work, "Editing", domain.Applied, domain.StatusWorking, ""},
		{"a question before the first turn", domain.KindInteractive, domain.TurnPending, ask, "", domain.Applied, domain.StatusWaiting, ""},
		{"same phase while running changes nothing", domain.KindInteractive, domain.TurnRunning, work, "Editing", domain.DropNoop, domain.StatusWorking, ""},
		{"a new phase while running is kept", domain.KindInteractive, domain.TurnRunning, work, "Testing", domain.Applied, domain.StatusWorking, ""},
		{"idle ends an attended turn", domain.KindInteractive, domain.TurnRunning, idle, "", domain.Applied, domain.StatusWaiting, "ReadSummary"},
		{"done ends an attended turn", domain.KindInteractive, domain.TurnRunning, done, "", domain.Applied, domain.StatusWaiting, "ReadSummary"},
		{"error ends an attended turn", domain.KindInteractive, domain.TurnRunning, fail, "", domain.Applied, domain.StatusWaiting, "ReadSummary"},
		{"idle ends an unattended turn", domain.KindScheduled, domain.TurnRunning, idle, "", domain.Applied, domain.StatusWorking, "ProbeDoneMarker"},
		{"a question while running", domain.KindInteractive, domain.TurnRunning, ask, "", domain.Applied, domain.StatusWaiting, ""},
		{"working restarts a paused turn", domain.KindInteractive, domain.TurnPaused, work, "Editing", domain.Applied, domain.StatusWorking, ""},
		{"idle again while paused changes nothing", domain.KindInteractive, domain.TurnPaused, idle, "", domain.DropNoop, domain.StatusWaiting, ""},
		{"a question while paused", domain.KindInteractive, domain.TurnPaused, ask, "", domain.Applied, domain.StatusWaiting, ""},
		{"the same question again changes nothing", domain.KindInteractive, domain.TurnBlocked, ask, "", domain.DropNoop, domain.StatusWaiting, ""},
		{"working answers a question", domain.KindInteractive, domain.TurnBlocked, work, "Editing", domain.Applied, domain.StatusWorking, ""},
		{"idle after a question ends the attended turn", domain.KindInteractive, domain.TurnBlocked, idle, "", domain.Applied, domain.StatusWaiting, "ReadSummary"},
		{"idle after a question ends the unattended turn", domain.KindScheduled, domain.TurnBlocked, idle, "", domain.Applied, domain.StatusWorking, "ProbeDoneMarker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := turnFixture(t, tc.kind, tc.from, "Editing")
			res := r.report(tc.report, tc.phase)
			if res.Dropped != tc.drop || r.task.Status() != tc.status || kinds(res.Effects) != tc.effects {
				t.Errorf("dropped %v, status %v, effects %q; want %v, %v, %q",
					res.Dropped, r.task.Status(), kinds(res.Effects), tc.drop, tc.status, tc.effects)
			}
			if err := r.task.Validate(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestThePhaseShowsWhileTheAgentWorks(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.report(domain.HCWorking, "Editing main.go")
	if r.task.Phase() != "Editing main.go" {
		t.Errorf("phase = %q", r.task.Phase())
	}
	r.report(domain.HCWorking, "Running tests")
	if r.task.Phase() != "Running tests" {
		t.Errorf("phase = %q", r.task.Phase())
	}
	r.report(domain.HCIdle, "")
	if r.task.Phase() != "" {
		t.Errorf("phase = %q after the turn ended", r.task.Phase())
	}
}

func TestAnInteractiveTurnEndWaitsForTheRequester(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.report(domain.HCIdle, "")
	r.dropped(domain.AgentReport{Run: r.task.AgentRun, Status: domain.HCIdle}, domain.DropNoop)
	r.report(domain.HCWorking, "")
	res := r.report(domain.HCIdle, "")

	r.want(domain.StatusWaiting)
	if lastEntry(r).Kind != domain.EntryWaiting || kinds(res.Effects) != "ReadSummary" {
		t.Errorf("last entry %+v, effects %q", lastEntry(r), kinds(res.Effects))
	}
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "Done for now."})
	r.dropped(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "again"}, domain.DropStaleAsk)
	if a, _ := r.task.Life.(domain.Active); a.Summary != "Done for now." {
		t.Errorf("summary = %q", a.Summary)
	}
	if at, ok := r.task.NextDeadline(); !ok || !at.Equal(t0.Add(r.policy.IdleSleep)) {
		t.Errorf("next deadline = %v, want the box to sleep after %v idle", at, r.policy.IdleSleep)
	}
}

func TestReportsFromAnOlderRunAreStale(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.task.AgentRun = 3
	r.dropped(domain.AgentReport{Run: 2, Status: domain.HCWorking}, domain.DropStaleKey)
	r.dropped(domain.AgentExited{Run: 2, Code: 1}, domain.DropStaleKey)
	r.want(domain.StatusWorking)
}

func TestReportsAreDroppedOnceTheTaskIsNotRunning(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.dropped(domain.AgentReport{Status: domain.HCWorking}, domain.DropStaleLife)
	r.dropped(domain.AgentExited{Code: 0}, domain.DropStaleLife)
	r.up()
	r.apply(domain.UserStop{By: owner})
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCWorking}, domain.DropStaleLife)
	r.dropped(domain.AgentExited{Run: 1, Code: 143}, domain.DropStaleLife)
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCIdle}, domain.DropStaleLife)
	r.dropped(domain.AgentExited{Run: 1, Code: 0}, domain.DropStaleLife)
	r.want(domain.StatusStopped)
}

func TestAReportThatBeatsStartedIsReplayed(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.apply(domain.SlotGranted{Machine: "m1"})
	ask := latest[domain.StartTask](r).Ask
	r.apply(domain.AgentReport{Run: 1, Status: domain.HCIdle})
	r.apply(domain.AgentReport{Run: 1, Status: domain.HCWorking, Phase: "Editing"})
	r.want(domain.StatusStarting)

	res := r.apply(domain.Started{Ask: ask})
	if res.Dropped != domain.Applied {
		t.Fatalf("Started dropped (%v): the replayed initial idle cancelled it", res.Dropped)
	}
	if r.task.Phase() != "Editing" {
		t.Errorf("phase = %q, want the early working report replayed", r.task.Phase())
	}
	r.report(domain.HCIdle, "")
	r.want(domain.StatusWaiting)
}

func TestAnExitThatBeatsStartedEndsTheRunWhenItIsUp(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.apply(domain.SlotGranted{Machine: "m1"})
	r.apply(domain.AgentExited{Run: 1, Code: 2})
	r.apply(domain.Started{Ask: latest[domain.StartTask](r).Ask})
	r.boxStillUp()

	r.want(domain.StatusWorking)
	closing, ok := r.task.Life.(domain.Closing)
	if !ok || closing.Want != domain.OutcomeFailed || closing.Reason != "The agent exited with code 2" {
		t.Errorf("life = %+v, want a failed close for the early exit", r.task.Life)
	}
}

func TestAgentExitDecidesDoneOrFailed(t *testing.T) {
	tests := []struct {
		code   int
		status domain.Status
		reason string
	}{
		{0, domain.StatusDone, ""},
		{1, domain.StatusFailed, "The agent exited with code 1"},
		{137, domain.StatusFailed, "The agent exited with code 137"},
	}
	for _, tc := range tests {
		r := newRun(t, domain.KindInteractive, spec()).up()
		res := r.exitStands(r.apply(domain.AgentExited{Run: 1, Code: tc.code}))
		if kinds(res.Effects) != "ReadSummary" {
			t.Errorf("exit %d: effects %q, want the summary read", tc.code, kinds(res.Effects))
		}
		r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "bye"})
		ended, _ := r.task.Life.(domain.Ended)
		if r.task.Status() != tc.status || ended.Error != tc.reason || ended.Summary != "bye" {
			t.Errorf("exit %d: status %v, ended %+v", tc.code, r.task.Status(), ended)
		}
	}
}

func TestATimeoutFailsAWorkingOrWaitingTask(t *testing.T) {
	for _, turn := range []string{"working", "waiting"} {
		r := newRun(t, domain.KindInteractive, domain.Spec{Repo: "acme/app", Timeout: time.Hour}).up()
		r.policy.IdleSleep = 0
		r.report(domain.HCWorking, "")
		if turn == "waiting" {
			r.report(domain.HCIdle, "")
		}
		fx := r.advanceTo(time.Hour)
		if kinds(fx) != "InterruptAgent,ReadSummary" {
			t.Fatalf("%s: effects = %q", turn, kinds(fx))
		}
		r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
		ended, _ := r.task.Life.(domain.Ended)
		if r.task.Status() != domain.StatusFailed || ended.Error != "Timed out after 60 minutes" {
			t.Errorf("%s: status %v, error %q", turn, r.task.Status(), ended.Error)
		}
	}
}

func TestATimeoutBeatsANudgeDueAtTheSameInstant(t *testing.T) {
	r := newRun(t, domain.KindScheduled, domain.Spec{Repo: "acme/app", Timeout: 10 * time.Minute}).up()
	r.report(domain.HCWorking, "")
	r.advance(7 * time.Minute)
	r.report(domain.HCIdle, "")
	if got, _ := r.task.NextDeadline(); !got.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("next deadline = %v, want the timeout and the nudge both at +10m", got)
	}
	fx := r.advanceTo(3 * time.Minute)
	if has[domain.ProbeDoneMarker](fx) || kinds(fx) != "InterruptAgent,ReadSummary" {
		t.Errorf("effects = %q, want the timeout to win and the nudge to be skipped", kinds(fx))
	}
}

func TestADuplicateEarlyReportIsDropped(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec())
	r.apply(domain.SlotGranted{Machine: "m1"})
	r.apply(domain.AgentReport{Run: 1, Status: domain.HCWorking, Phase: "Editing"})
	r.dropped(domain.AgentReport{Run: 1, Status: domain.HCWorking, Phase: "Editing"}, domain.DropNoop)
	r.apply(domain.AgentExited{Run: 1, Code: 1})
	r.dropped(domain.AgentExited{Run: 1, Code: 1}, domain.DropNoop)
	r.apply(domain.AgentReport{Run: 1, Status: domain.HCIdle})
	r.apply(domain.AgentReport{Run: 1, Status: domain.HCWorking, Phase: "Editing"})
}
