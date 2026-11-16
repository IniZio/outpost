package domain

// onTick runs whatever has come due: the finish window's wait, the release of a finished task's
// computer, or the active task's deadlines.
func (x *tx) onTick() {
	switch l := x.t.Life.(type) {
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
