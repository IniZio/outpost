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

func (x *tx) boxEventUp(a Active, ev BoxEvent) {
	switch {
	case ev.Kind == BoxOOM && ev.StillRunning:
		x.note(EntryWarning, "A process in the box ran out of memory and was killed. The box is still up.")
		x.emit(SendToAgent{Text: OOMNote})
	case ev.Kind == BoxDied && ev.StillRunning:
		x.drop(DropNoop)
	case ev.Kind == BoxStarted:
		x.boxStarted(a)
	case ev.Kind == BoxOOM || ev.OOMKilled:
		x.loseBox(a, "ran out of memory")
	default:
		x.loseBox(a, "stopped unexpectedly")
	}
}

// boxStarted treats a start soon after our own as its echo, and a later one as a restart that
// took the agent down.
func (x *tx) boxStarted(a Active) {
	if x.now.Before(a.Box.StartedAt.Add(x.p.BoxStartGrace)) {
		x.drop(DropNoop)
		return
	}
	x.loseBox(a, "restarted")
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
	switch x.t.Life.(type) {
	case Starting:
		x.ensuredWake(ev)
	case Active:
		x.ensuredBox(ev)
	case Queued, Closing, Ended:
		x.drop(DropStaleLife)
	}
}

// ensuredWake finishes waking an ended task. A failed wake gives the old Ended back whole.
func (x *tx) ensuredWake(ev Ensured) {
	s, ok := x.starting(ev.Ask, false)
	if !ok {
		return
	}
	if ev.Err != "" {
		x.t.Life = *s.Resume
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
	if ev.Err != "" {
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

// boxStayedAsleep gives up bringing the box back. The parked messages are lost, which the
// timeline says, and a later message tries again.
func (x *tx) boxStayedAsleep(a Active, err string) {
	a.Box = Box{Want: BoxAsleep}
	if a.Turn.State == TurnPending {
		a.Turn = Turn{State: TurnPaused}
	}
	x.t.Life = a
	x.note(EntryError, "Could not bring the box back: "+err)
}
