package domain

// settle runs once at the end of every applied step and enforces what must follow from the new
// state, so no handler clears a deadline by hand and handlers arm only the ones a step starts.
// It touches only an Active task.
func (x *tx) settle() {
	a, ok := x.active()
	if !ok {
		return
	}
	if !a.Turn.idle() {
		a.Asks[AskDoneMarker] = Ask{} // the agent went back to work, so a late probe answer is stale
	}
	if a.Box.Want != BoxUp {
		a.Asks[AskInspect] = Ask{} // the box went down another way, which settled the exit
	}
	a.Deadlines = x.settled(a)
	x.t.Life = a
}

func (x *tx) settled(a Active) Deadlines {
	d := a.Deadlines
	if x.t.Spec.Timeout > 0 {
		d = d.Set(DlTimeout, a.Since.Add(x.t.Spec.Timeout))
	} else {
		d = d.Clear(DlTimeout)
	}
	if !x.t.Kind.Unattended() || !a.Turn.idle() {
		d = d.Clear(DlNudge).Clear(DlGiveUp)
	}
	if a.Box.Want != BoxReplacing {
		d = d.Clear(DlReplace)
	}
	switch {
	case !a.stalls(x.t.Kind):
		d = d.Clear(DlStall)
	case !d.Armed(DlStall):
		d = d.Set(DlStall, x.now.Add(x.p.StallAfter))
	}
	return x.sleepDeadline(a, d)
}

// sleepDeadline keeps an attended, waiting task's box on a sleep timer that restarts whenever the
// task changes or logs anything, which is the prototype's "sleep keys on the last task event".
func (x *tx) sleepDeadline(a Active, d Deadlines) Deadlines {
	sleeps := !x.t.Kind.Unattended() && a.status(x.t.Kind) == StatusWaiting &&
		a.Box.Want == BoxUp && x.p.IdleSleep > 0
	switch {
	case !sleeps:
		return d.Clear(DlSleep)
	case !d.Armed(DlSleep) || x.was != StatusWaiting || len(x.tl) > 0:
		return d.Set(DlSleep, x.now.Add(x.p.IdleSleep))
	}
	return d
}
