package domain

import "fmt"

const maxErrText = 600

// close enters the finish window. Every path that ends a run goes through here: stop, exit,
// timeout, done marker, give-up. A second finish finds Closing, so callers need no guard of
// their own.
func (x *tx) close(want Outcome, reason string) {
	a, ok := x.active()
	if !ok {
		return
	}
	ask := x.newAsk()
	x.t.Life = Closing{
		Want: want, Reason: reason, From: x.t.Status(), Step: CloseSummary,
		Ask: ask, Until: x.now.Add(x.p.CloseGrace), Summary: a.Summary, Ensure: a.Asks[AskEnsure].ID,
	}
	x.emit(ReadSummary{Ask: ask})
}

// closing returns the Closing life when ask is its outstanding question at step. Otherwise it
// records why the event is stale.
func (x *tx) closing(ask AskID, step CloseStep) (Closing, bool) {
	c, ok := x.t.Life.(Closing)
	switch {
	case !ok:
		x.drop(DropStaleLife)
	case c.Step != step || c.Ask != ask:
		x.drop(DropStaleAsk)
	default:
		return c, true
	}
	return Closing{}, false
}

// onSummary handles the answer to ReadSummary, for an attended turn end or for the finish window.
func (x *tx) onSummary(ev SummaryRead) {
	switch x.t.Life.(type) {
	case Active:
		x.onTurnSummary(ev)
	case Closing:
		x.onCloseSummary(ev)
	case Queued, Starting, Ended:
		x.drop(DropStaleLife)
	}
}

// onTurnSummary keeps the agent's last message from an attended turn end.
func (x *tx) onTurnSummary(ev SummaryRead) {
	a, _, ok := x.takeAsk(AskSummary, ev.Ask)
	if !ok {
		return
	}
	if ev.Text != "" {
		a.Summary = ev.Text
	}
	x.t.Life = a
}

func (x *tx) onCloseSummary(ev SummaryRead) {
	c, ok := x.closing(ev.Ask, CloseSummary)
	if !ok {
		return
	}
	if ev.Text != "" {
		c.Summary = ev.Text
	}
	if c.Want != OutcomeDone || !x.t.Kind.SavesTemplate() {
		x.end(c, false)
		return
	}
	c.Step, c.Ask, c.Until = CloseTemplate, x.newAsk(), x.now.Add(x.p.SaveBudget)
	x.t.Life = c
	x.emit(SaveTemplate{Ask: c.Ask})
}

// onTemplate handles the answer to SaveTemplate. A failed save fails the run but keeps the box
// running so the owner can retry.
func (x *tx) onTemplate(ev TemplateSaved) {
	c, ok := x.closing(ev.Ask, CloseTemplate)
	if !ok {
		return
	}
	if ev.Err == "" {
		x.note(EntryReady, fmt.Sprintf("Saved %s: new tasks on %s start from it", ev.Tag, x.t.Spec.Repo))
		x.end(c, false)
		return
	}
	c.Want = OutcomeFailed
	c.Reason = "Could not save the ready image: " + cut(ev.Err, maxErrText) +
		" The box is kept running so you can retry saving."
	x.end(c, true)
}

// end leaves the finish window for Ended and schedules the computer's clean-up.
func (x *tx) end(c Closing, boxKept bool) {
	e := Ended{
		Outcome: c.Want, At: x.now, Error: c.Reason, Summary: c.Summary, BoxKept: boxKept,
		Ensure: c.Ensure,
	}
	if x.p.KeepFinished > 0 {
		e.ReleaseAt = x.now.Add(x.p.KeepFinished)
	}
	x.t.Life = e
	x.noteEnd(e)
	x.emit(RunScheduler{})
	if !boxKept {
		x.emit(StopBox{})
	}
}

func (x *tx) noteEnd(e Ended) {
	switch e.Outcome {
	case OutcomeDone:
		x.note(EntryDone, "Finished")
	case OutcomeStopped:
		x.note(EntryStopped, "Stopped")
	case OutcomeFailed:
		x.note(EntryFailed, orElse(e.Error, "Failed"))
	}
}

func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
