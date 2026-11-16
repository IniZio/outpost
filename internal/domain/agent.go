package domain

import (
	"fmt"
	"slices"
	"time"
)

// onReport folds one hc report into the task. A report that arrives while the agent is still
// starting is kept and replayed once it is up, because the first working report can beat Started.
func (x *tx) onReport(ev AgentReport) {
	switch l := x.t.Life.(type) {
	case Starting:
		x.bufferEarly(l, Early{Report: &ev})
	case Active:
		x.reportActive(l, ev)
	case Queued, Closing, Ended:
		x.drop(DropStaleLife)
	}
}

func (x *tx) bufferEarly(s Starting, e Early) {
	s.Early = append(slices.Clone(s.Early), e)
	x.t.Life = s
}

func (x *tx) reportActive(a Active, ev AgentReport) {
	step := a.Turn.onReport(ev, x.now)
	if step.Signal == SigNone {
		x.drop(DropNoop)
		return
	}
	a.Turn = step.Turn
	switch step.Signal {
	case SigBlocked:
		x.note(EntryWaiting, "The agent is asking a question")
		x.t.Life = a
		if x.t.Kind.Unattended() {
			x.t.Life = x.armLadder(a, step.Worked)
		}
	case SigEnded:
		x.turnEnded(a, step.Worked)
	case SigStarted, SigPhase, SigNone:
		x.t.Life = a // settle clears the nudge, give-up, sleep and the probe answer
	}
}

// turnEnded is the only place a turn end is interpreted. An unattended run arms the nudge ladder
// and asks whether the agent marked the run done. An attended one waits for its owner and reads
// the agent's last message.
func (x *tx) turnEnded(a Active, worked time.Duration) {
	if x.t.Kind.Unattended() {
		x.t.Life = x.armLadder(a, worked)
		return
	}
	x.note(EntryWaiting, "The agent is waiting for you")
	ask := x.newAsk()
	a.Asks[AskSummary] = Ask{ID: ask}
	x.t.Life = a
	x.emit(ReadSummary{Ask: ask})
}

// armLadder arms the next rung of the nudge ladder, or the give-up, and probes the done marker.
func (x *tx) armLadder(a Active, worked time.Duration) Active {
	n, kind, after := a.Nudge.nudgeStep(worked, x.p)
	a.Nudge = n
	a.Deadlines = a.Deadlines.Clear(DlNudge).Clear(DlGiveUp).Set(kind, x.now.Add(after))
	return x.askMarker(a, WhyTurnEnd)
}

// askMarker records a done-marker probe and emits it.
func (x *tx) askMarker(a Active, why Why) Active {
	ask := x.newAsk()
	a.Asks[AskDoneMarker] = Ask{ID: ask, Why: why}
	x.emit(ProbeDoneMarker{Ask: ask})
	return a
}

// onExit handles the agent's terminal ending. Exit 0 is done, anything else is failed. The agent
// lives in the box, so an exit while the box is not up is the box's doing, not the agent's.
func (x *tx) onExit(ev AgentExited) {
	switch l := x.t.Life.(type) {
	case Starting:
		x.bufferEarly(l, Early{Exit: &ev})
	case Active:
		x.exitActive(l, ev)
	case Queued, Closing, Ended:
		x.drop(DropStaleLife)
	}
}

func (x *tx) exitActive(a Active, ev AgentExited) {
	switch {
	case a.Box.Want != BoxUp:
		x.drop(DropStaleLife)
	case ev.Code == 0:
		x.close(OutcomeDone, "")
	default:
		x.close(OutcomeFailed, fmt.Sprintf("The agent exited with code %d", ev.Code))
	}
}

// onDoneMarker handles the answer to a done-marker probe. The probe is answered only while the
// agent is idle: settle clears the question when it goes back to work, so a late answer is stale.
func (x *tx) onDoneMarker(ev DoneMarker) {
	a, ask, ok := x.takeAsk(AskDoneMarker, ev.Ask)
	if !ok {
		return
	}
	x.t.Life = a
	if ev.Present {
		x.close(OutcomeDone, "")
		return
	}
	x.onMarkerAbsent(a, ask.Why)
}

func (x *tx) onMarkerAbsent(a Active, why Why) {
	switch why {
	case WhyNudge:
		a.Nudge.Sent++
		x.t.Life = a
		x.note(EntryPhase, "The agent paused without marking the run done; nudged it")
		x.emit(SendToAgent{Text: NudgeText})
	case WhyGiveUp:
		x.note(EntryWarning, "The agent stayed idle without marking the run done, so it was finished as is.")
		x.close(OutcomeDone, "")
	case WhyTurnEnd, WhyReplace, WhyWake:
	}
}
