package domain

import "fmt"

// onBoxEvent handles a sandbox event. It is judged against what Outpost intends for the box: a
// box that stops while we want it asleep or being replaced is our own doing, not an incident. The
// agent runs inside the box, so a lost box also takes the agent down, and the recovery is a new box
// plus the agent's own resume, never a task failure.
func (x *tx) onBoxEvent(ev BoxEvent) {
	a, ok := x.active()
	switch {
	case !ok:
		x.drop(DropStaleLife)
	case a.Box.Want != BoxUp:
		x.drop(DropNoop)
	default:
		x.boxEventUp(a, ev)
	}
}

// boxEventUp judges an event about a box that is meant to be up. An agent exit waiting on the
// box's state is settled by it: a lost box is the loss path, a box still up fails the run.
func (x *tx) boxEventUp(a Active, ev BoxEvent) {
	why := x.lossOf(a, ev)
	switch {
	case why != "":
		x.loseBox(a, why)
	case a.Asks[AskInspect].ID != 0:
		x.failExit(a.Asks[AskInspect].Code)
	case ev.Kind == BoxOOM:
		x.note(EntryWarning, "A process in the box ran out of memory and was killed. The box is still up.")
		x.emit(SendToAgent{Text: OOMNote})
	default:
		x.drop(DropNoop)
	}
}

// lossOf says why the event means the box was lost, or "" when the box is still up. A start soon
// after our own is its echo, and a later one is a restart that took the agent down.
func (x *tx) lossOf(a Active, ev BoxEvent) string {
	switch {
	case ev.Kind == BoxStarted && x.now.Before(a.Box.StartedAt.Add(x.p.BoxStartGrace)):
		return ""
	case ev.Kind == BoxStarted:
		return "restarted"
	case ev.StillRunning:
		return ""
	case ev.Kind == BoxOOM || ev.OOMKilled:
		return "ran out of memory"
	}
	return "stopped unexpectedly"
}

// loseBox starts the replacement of a lost box. The agent died with it, so the run and the box
// generation both move on: that is what makes the dead agent's late exit and the dead box's late
// events stale, whichever order they arrive in.
func (x *tx) loseBox(a Active, why string) {
	x.t.AgentRun++
	x.t.BoxGen++
	a.Box = Box{Want: BoxReplacing, Lost: why}
	a.Turn = Turn{State: TurnPending}
	a.Deadlines = a.Deadlines.Set(DlReplace, x.now.Add(x.p.ReplaceDelay))
	x.t.Life = a
	x.note(EntryWarning, fmt.Sprintf("The box %s. The agent and workspace are safe; bringing the box back.", why))
}

// onReplaceDue starts the replacement of a lost box.
func (x *tx) onReplaceDue() {
	a, ok := x.active()
	if !ok || a.Box.Want != BoxReplacing {
		return
	}
	x.t.Life = x.ensure(a, WhyReplace)
}

// ensure records an EnsureRunning question for the box and emits it.
func (x *tx) ensure(a Active, why Why) Active {
	ask := x.newAsk()
	a.Box.Want = BoxWaking
	a.Asks[AskEnsure] = Ask{ID: ask, Why: why}
	x.emit(EnsureRunning{Ask: ask, Run: x.t.AgentRun, Gen: x.t.BoxGen})
	return a
}

// onSleepDue puts an idle waiting task's box to sleep. The agent dies with the box, so the run
// moves on, and a message wakes it with the agent's own resume.
func (x *tx) onSleepDue() {
	a, ok := x.active()
	if !ok || a.Box.Want != BoxUp || a.status(x.t.Kind) != StatusWaiting {
		return
	}
	x.t.AgentRun++
	a.Box = Box{Want: BoxAsleep, StartedAt: a.Box.StartedAt}
	x.t.Life = a
	x.emit(StopBox{})
	x.note(EntryPhase, fmt.Sprintf("The box went to sleep after %d idle minutes. Send a message to wake it.", int(x.p.IdleSleep.Minutes())))
}

// onEnsured handles the answer to EnsureRunning, for a replaced box, a woken box, or a resume.
func (x *tx) onEnsured(ev Ensured) {
	switch l := x.t.Life.(type) {
	case Starting:
		x.ensuredWake(ev)
	case Active:
		x.ensuredBox(ev)
	case Closing:
		x.ensuredClosing(l, ev)
	case Ended:
		x.ensuredEnded(l, ev)
	case Queued:
		x.drop(DropStaleLife)
	}
}

// ensuredClosing takes the answer to a wake the finish window overtook. The window wants the box
// up, so there is nothing to undo yet; end stops the box.
func (x *tx) ensuredClosing(c Closing, ev Ensured) {
	if !x.orphan(c.Ensure, ev.Ask) {
		return
	}
	c.Ensure = 0
	x.t.Life = c
}

// ensuredEnded takes the answer to a wake the run ended during. The box may have started after
// it was stopped, so it is put back the way Ended wants it.
func (x *tx) ensuredEnded(e Ended, ev Ensured) {
	if !x.orphan(e.Ensure, ev.Ask) {
		return
	}
	e.Ensure = 0
	x.t.Life = e
	x.restoreBox()
}

// orphan reports whether ask answers the EnsureRunning a finished run is still waiting for.
func (x *tx) orphan(want, ask AskID) bool {
	switch {
	case want == 0:
		x.drop(DropStaleLife)
	case want != ask:
		x.drop(DropStaleAsk)
	default:
		return true
	}
	return false
}

// unwake gives a wake's Ended back. EnsureRunning may have started the box before it failed or was
// abandoned, so the box is put back the way Ended wants it. pending is the EnsureRunning still in
// flight, if any.
func (x *tx) unwake(s Starting, pending AskID) {
	e := *s.Resume
	e.Ensure = pending
	x.t.Life = e
	x.restoreBox()
}

// restoreBox emits what returns an ended task's box to its intended state.
func (x *tx) restoreBox() {
	switch x.t.BoxWant() {
	case BoxStopped:
		x.emit(StopBox{})
	case BoxReleased:
		x.emit(Release{})
	case BoxNone, BoxUp, BoxAsleep, BoxReplacing, BoxWaking:
	}
}

// ensuredWake finishes waking an ended task. A failed wake gives the old Ended back whole.
func (x *tx) ensuredWake(ev Ensured) {
	s, ok := x.starting(ev.Ask, false)
	if !ok {
		return
	}
	if ev.Err != "" {
		x.unwake(s, 0)
		x.note(EntryError, "Could not wake the task: "+ev.Err)
		return
	}
	a := Active{Since: x.now, Turn: Turn{State: TurnPaused}, Box: Box{Want: BoxUp, StartedAt: x.now}}
	if len(s.Messages) > 0 {
		a.Turn = Turn{State: TurnPending}
	}
	x.t.Life = a
	x.note(EntryPhase, "The task was resumed")
	x.deliver(s.Messages)
	if len(s.Messages) == 0 && x.t.Kind.Unattended() {
		x.t.Life = x.armLadder(a, 0)
	}
	x.replay(s.Early)
}

func (x *tx) ensuredBox(ev Ensured) {
	a, ask, ok := x.takeAsk(AskEnsure, ev.Ask)
	if !ok {
		return
	}
	switch {
	case ev.Err != "" && ask.Why == WhyReplace:
		x.replaceFailed(a, ev.Err)
		return
	case ev.Err != "":
		x.boxStayedAsleep(a, ev.Err)
		return
	}
	lost, parked := a.Box.Lost, a.Box.Parked
	a.Box = Box{Want: BoxUp, StartedAt: x.now}
	if ev.Revived {
		a.Turn = Turn{State: TurnPending}
	}
	x.t.Life = a
	if ask.Why == WhyReplace && ev.Revived {
		x.note(EntryPhase, "The box is back")
		x.emit(SendToAgent{Text: BoxBackNote(lost)})
	}
	if len(parked) > 0 {
		x.deliver(parked)
	}
}

// replaceFailed tries the replacement again after a backoff. Once the retries are spent, an
// unattended run fails, so it neither holds its slot nor waits for ever, while an interactive
// task's box stays asleep for the person watching it to retry with a message.
func (x *tx) replaceFailed(a Active, err string) {
	tries := a.Box.Tries
	if tries >= len(x.p.ReplaceBackoff) {
		if !x.t.Kind.Unattended() {
			x.boxStayedAsleep(a, fmt.Sprintf("%s (after %d tries; send a message to try again)", cut(err, maxErrText), tries+1))
			return
		}
		x.t.Life = a
		x.close(OutcomeFailed, fmt.Sprintf("Could not bring the box back after %d tries: %s", tries+1, cut(err, maxErrText)))
		return
	}
	wait := x.p.ReplaceBackoff[tries]
	a.Box.Want, a.Box.Tries = BoxReplacing, tries+1
	a.Deadlines = a.Deadlines.Set(DlReplace, x.now.Add(wait))
	x.t.Life = a
	x.note(EntryWarning, fmt.Sprintf("Could not bring the box back: %s. Trying again in %v.", cut(err, maxErrText), wait))
}

// boxStayedAsleep gives up waking a sleeping box. The parked messages are lost, which the
// timeline says, and a later message tries again.
func (x *tx) boxStayedAsleep(a Active, err string) {
	a.Box = Box{Want: BoxAsleep}
	if a.Turn.State == TurnPending {
		a.Turn = Turn{State: TurnPaused}
	}
	x.t.Life = a
	x.note(EntryError, "Could not bring the box back: "+err)
}
