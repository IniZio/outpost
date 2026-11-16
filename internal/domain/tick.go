package domain

import (
	"fmt"
	"time"
)

// onTick runs whatever has come due: the finish window's wait, the release of a finished task's
// computer, or the active task's deadlines.
func (x *tx) onTick() {
	switch l := x.t.Life.(type) {
	case Active:
		x.tickActive(l)
	case Closing:
		x.onCloseDue(l)
	case Ended:
		x.onReleaseDue(l)
	case Queued, Starting:
		x.drop(DropNoop)
	}
}

// onCloseDue gives up waiting for the answer the finish window asked for and ends without it.
func (x *tx) onCloseDue(c Closing) {
	if x.now.Before(c.Until) {
		x.drop(DropNoop)
		return
	}
	if c.Step == CloseSummary {
		x.onCloseSummary(SummaryRead{Ask: c.Ask})
		return
	}
	x.onTemplate(TemplateSaved{Ask: c.Ask, Err: "timed out"})
}

// onReleaseDue cleans up a finished task's computer once its keep time is over. This replaces
// the prototype's gc loop.
func (x *tx) onReleaseDue(e Ended) {
	if !e.releasable() || x.now.Before(e.ReleaseAt) {
		x.drop(DropNoop)
		return
	}
	e.ReleasedAt = x.now
	x.t.Life = e
	x.emit(Release{})
}

// tickActive runs each due deadline in priority order, so a timeout beats a nudge. A deadline is
// cleared before its handler runs, and the loop stops once a handler has ended the run.
func (x *tx) tickActive(a Active) {
	due := a.Deadlines.Due(x.now)
	if len(due) == 0 {
		x.drop(DropNoop)
		return
	}
	for _, k := range due {
		cur, ok := x.active()
		if !ok {
			return
		}
		cur.Deadlines = cur.Deadlines.Clear(k)
		x.t.Life = cur
		x.runDeadline(k)
	}
}

func (x *tx) runDeadline(k DeadlineKind) {
	switch k {
	case DlTimeout:
		x.onTimeout()
	case DlGiveUp:
		x.probeDue(WhyGiveUp)
	case DlNudge:
		x.probeDue(WhyNudge)
	case DlReplace:
		x.onReplaceDue()
	case DlSleep:
		x.onSleepDue()
	}
}

// onTimeout fails the run when the task's own timeout has passed.
func (x *tx) onTimeout() {
	x.emit(InterruptAgent{})
	x.close(OutcomeFailed, fmt.Sprintf("Timed out after %d minutes", int(x.t.Spec.Timeout/time.Minute)))
}

// probeDue asks whether the agent marked the run done before a nudge or the give-up acts on it.
func (x *tx) probeDue(why Why) {
	if a, ok := x.active(); ok && a.Turn.idle() {
		x.t.Life = x.askMarker(a, why)
	}
}
