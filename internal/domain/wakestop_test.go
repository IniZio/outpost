package domain_test

import (
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

func requireBoxStopped(t *testing.T, res domain.Result) {
	t.Helper()
	if !has[domain.StopBox](res.Effects) {
		t.Fatalf("effects = %q, want the box stopped", kinds(res.Effects))
	}
}

func TestAStopDuringAWakeLeavesTheBoxStoppedInEitherOrder(t *testing.T) {
	t.Run("stop first", func(t *testing.T) {
		r := stopped(t)
		r.apply(domain.UserMessage{By: owner, Text: "carry on"})
		wake := latest[domain.EnsureRunning](r)
		requireBoxStopped(t, r.apply(domain.UserStop{By: owner}))
		r.want(domain.StatusStopped)

		requireBoxStopped(t, r.apply(domain.Ensured{Ask: wake.Ask, Revived: true}))
		r.want(domain.StatusStopped)
		r.dropped(domain.Ensured{Ask: wake.Ask, Revived: true}, domain.DropStaleLife)
	})
	t.Run("answer first", func(t *testing.T) {
		r := stopped(t)
		r.apply(domain.UserMessage{By: owner, Text: "carry on"})
		r.apply(domain.Ensured{Ask: latest[domain.EnsureRunning](r).Ask, Revived: true})
		r.apply(domain.UserStop{By: owner})
		requireBoxStopped(t, r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask}))
		r.want(domain.StatusStopped)
	})
}

func TestAStopWhileASleepingBoxWakesLeavesItStopped(t *testing.T) {
	asleepAndWaking := func(t *testing.T) (*run, domain.EnsureRunning) {
		t.Helper()
		r := waiting(t)
		r.advanceTo(r.policy.IdleSleep)
		r.apply(domain.UserMessage{By: owner, Text: "add tests too"})
		wake := latest[domain.EnsureRunning](r)
		r.apply(domain.UserStop{By: owner})
		return r, wake
	}
	t.Run("answer during the finish window", func(t *testing.T) {
		r, wake := asleepAndWaking(t)
		r.apply(domain.Ensured{Ask: wake.Ask, Revived: true})
		requireBoxStopped(t, r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask}))
		r.want(domain.StatusStopped)
	})
	t.Run("answer after the end", func(t *testing.T) {
		r, wake := asleepAndWaking(t)
		r.apply(domain.SummaryRead{Ask: latest[domain.ReadSummary](r).Ask})
		r.want(domain.StatusStopped)
		requireBoxStopped(t, r.apply(domain.Ensured{Ask: wake.Ask, Revived: true}))
		r.dropped(domain.Ensured{Ask: wake.Ask, Revived: true}, domain.DropStaleLife)
	})
}

func TestALateWakeAnswerLeavesAKeptBoxRunning(t *testing.T) {
	r := keptBox(t)
	r.apply(domain.UserMessage{By: owner, Text: "try the save by hand"})
	wake := latest[domain.EnsureRunning](r)
	if res := r.apply(domain.UserStop{By: owner}); has[domain.StopBox](res.Effects) {
		t.Fatalf("effects = %q: the kept box stays up for a retry", kinds(res.Effects))
	}
	if res := r.apply(domain.Ensured{Ask: wake.Ask, Revived: true}); has[domain.StopBox](res.Effects) {
		t.Errorf("effects = %q: the kept box stays up for a retry", kinds(res.Effects))
	}
}
