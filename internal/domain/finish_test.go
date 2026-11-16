package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

func TestStoppingARunningTaskKillsTheAgentThenEnds(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.report(domain.HCWorking, "Editing")
	mustRefuse(t, r.refused(domain.UserStop{By: stranger}), domain.ErrForbidden)

	res := r.apply(domain.UserStop{By: owner})
	if kinds(res.Effects) != "KillAgent,ReadSummary" {
		t.Fatalf("effects = %q", kinds(res.Effects))
	}
	r.want(domain.StatusWorking)
	r.dropped(domain.UserStop{By: owner}, domain.DropNoop)

	res = r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask, Text: "Stopped midway."})
	r.want(domain.StatusStopped)
	if kinds(res.Effects) != "RunScheduler,StopBox" || lastEntry(r).Kind != domain.EntryStopped {
		t.Errorf("effects %q, last entry %+v", kinds(res.Effects), lastEntry(r))
	}
	ended, _ := r.task.Life.(domain.Ended)
	if ended.Summary != "Stopped midway." || r.task.BoxWant() != domain.BoxStopped {
		t.Errorf("ended = %+v, box want %v", ended, r.task.BoxWant())
	}
}

func TestAnAnswerToAClosedWindowIsStale(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.apply(domain.UserStop{By: owner})
	ask := latest[domain.ReadSummary](r).Ask

	r.dropped(domain.SummaryRead{Ask: ask + 5, Text: "x"}, domain.DropStaleAsk)
	r.dropped(domain.TemplateSaved{Ask: ask, Tag: "t"}, domain.DropStaleAsk)
	r.apply(domain.SummaryRead{Ask: ask})
	r.dropped(domain.SummaryRead{Ask: ask, Text: "again"}, domain.DropStaleLife)
	r.dropped(domain.TemplateSaved{Ask: ask, Tag: "t"}, domain.DropStaleLife)
	r.want(domain.StatusStopped)
}

func TestTheFinishWindowGivesUpWaitingForTheSummary(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.apply(domain.UserStop{By: owner})

	r.dropped(domain.Tick{}, domain.DropNoop)
	r.advanceTo(r.policy.CloseGrace)
	r.want(domain.StatusStopped)
	if ended, _ := r.task.Life.(domain.Ended); ended.Summary != "" {
		t.Errorf("summary = %q, want none", ended.Summary)
	}
}

func TestAFinishedComputerIsReleasedAfterTheKeepTime(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.apply(domain.UserStop{By: owner})
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})

	r.dropped(domain.Tick{}, domain.DropNoop)
	fx := r.advanceTo(r.policy.KeepFinished)
	if kinds(fx) != "Release" || r.task.BoxWant() != domain.BoxReleased {
		t.Errorf("effects %q, box want %v", kinds(fx), r.task.BoxWant())
	}
	if _, ok := r.task.NextDeadline(); ok {
		t.Error("a released task still wants a wake-up")
	}
	r.dropped(domain.Tick{}, domain.DropNoop)
	r.want(domain.StatusStopped)
}

func TestKeepFinishedZeroKeepsTheComputerUntilAskedOtherwise(t *testing.T) {
	r := newRun(t, domain.KindInteractive, spec()).up()
	r.policy.KeepFinished = 0
	r.apply(domain.UserStop{By: owner})
	r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
	if _, ok := r.task.NextDeadline(); ok {
		t.Error("a release is scheduled although KeepFinished is zero")
	}
}

// closingSetup is a setup task that decided to finish as done and is reading the agent's summary.
func closingSetup(t *testing.T) *run {
	t.Helper()
	task := fixture(domain.KindSetup, domain.Closing{
		Want: domain.OutcomeDone, From: domain.StatusWorking, Step: domain.CloseSummary,
		Ask: 7, Until: t0.Add(time.Hour), Summary: "Prepared.",
	})
	task.NextAsk = 7
	return runOf(t, task)
}

func TestASetupTaskSavesItsTemplateWhenDone(t *testing.T) {
	r := closingSetup(t)
	res := r.apply(domain.SummaryRead{Ask: 7, Text: "Prepared and tested."})
	r.want(domain.StatusWorking)
	if kinds(res.Effects) != "SaveTemplate" {
		t.Fatalf("effects = %q, want the template save before ending", kinds(res.Effects))
	}

	res = r.apply(domain.TemplateSaved{Ask: latest[domain.SaveTemplate](r).Ask, Tag: "ready-1"})
	r.want(domain.StatusDone)
	ended, _ := r.task.Life.(domain.Ended)
	if kinds(res.Effects) != "RunScheduler,StopBox" || ended.Summary != "Prepared and tested." {
		t.Errorf("effects %q, ended %+v", kinds(res.Effects), ended)
	}
	if !strings.Contains(r.timeline[0].Text, "ready-1") || r.timeline[0].Kind != domain.EntryReady {
		t.Errorf("timeline = %+v, want the saved tag announced", r.timeline)
	}
}

func TestAFailedTemplateSaveFailsTheTaskAndKeepsTheBox(t *testing.T) {
	r := closingSetup(t)
	r.apply(domain.SummaryRead{Ask: 7})
	res := r.apply(domain.TemplateSaved{Ask: latest[domain.SaveTemplate](r).Ask, Err: strings.Repeat("x", 900)})

	r.want(domain.StatusFailed)
	ended, _ := r.task.Life.(domain.Ended)
	if !ended.BoxKept || kinds(res.Effects) != "RunScheduler" || r.task.BoxWant() != domain.BoxUp {
		t.Errorf("ended %+v, effects %q: the box must keep running", ended, kinds(res.Effects))
	}
	if !strings.Contains(ended.Error, "retry saving") || len(ended.Error) > 800 || ended.Summary != "Prepared." {
		t.Errorf("error %q (%d bytes), summary %q", ended.Error, len(ended.Error), ended.Summary)
	}
}

func keptBox(t *testing.T) *run {
	t.Helper()
	task := fixture(domain.KindSetup, domain.Ended{
		Outcome: domain.OutcomeFailed, At: t0, Error: "Could not save", BoxKept: true,
		Summary: "Prepared.", ReleaseAt: t0.Add(time.Hour),
	})
	return runOf(t, task)
}

func TestTheOwnerCanRetryAFailedTemplateSave(t *testing.T) {
	r := keptBox(t)
	mustRefuse(t, r.refused(domain.SaveTemplateRequested{By: stranger}), domain.ErrForbidden)

	res := r.apply(domain.SaveTemplateRequested{By: owner})
	r.want(domain.StatusFailed)
	if kinds(res.Effects) != "SaveTemplate" {
		t.Fatalf("effects = %q", kinds(res.Effects))
	}
	r.dropped(domain.SaveTemplateRequested{By: owner}, domain.DropNoop)

	res = r.apply(domain.TemplateSaved{Ask: latest[domain.SaveTemplate](r).Ask, Tag: "ready-2"})
	r.want(domain.StatusDone)
	if kinds(res.Effects) != "RunScheduler,StopBox" {
		t.Errorf("effects = %q", kinds(res.Effects))
	}
}

func TestARetriedTemplateSaveThatFailsAgainReturnsToFailed(t *testing.T) {
	r := keptBox(t)
	r.apply(domain.SaveTemplateRequested{By: owner})
	r.advanceTo(r.policy.CloseGrace)

	r.want(domain.StatusFailed)
	ended, _ := r.task.Life.(domain.Ended)
	if !ended.BoxKept || !strings.Contains(ended.Error, "timed out") {
		t.Errorf("ended = %+v, want the box kept and a timeout error", ended)
	}
}

func TestOnlyAKeptSetupBoxCanRetrySaving(t *testing.T) {
	plain := runOf(t, fixture(domain.KindInteractive, domain.Ended{Outcome: domain.OutcomeFailed, At: t0}))
	plain.dropped(domain.SaveTemplateRequested{By: owner}, domain.DropNoop)

	gone := keptBox(t)
	ended, _ := gone.task.Life.(domain.Ended)
	ended.ReleasedAt = t0
	gone.task.Life = ended
	gone.dropped(domain.SaveTemplateRequested{By: owner}, domain.DropNoop)
}
