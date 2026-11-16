package domain

// onRecovered runs once per unfinished task when the server starts. Deadlines and asks were
// persisted with the task, so the work is to re-issue what was in flight and to reconcile with
// what the world looks like now.
func (x *tx) onRecovered(ev Recovered) {
	switch l := x.t.Life.(type) {
	case Starting:
		x.recoverStarting(l)
	case Active:
		x.recoverActive(l, ev.Agent)
	case Closing:
		x.recoverClosing(l)
	case Queued, Ended:
		x.drop(DropNoop)
	}
}

// recoverStarting never resumes a start half way: a fresh task goes back to the queue with its
// half-built computer released, and a wake gives the ended run back.
func (x *tx) recoverStarting(s Starting) {
	const text = "Outpost restarted while this computer was starting; starting over"
	if s.Resume != nil {
		x.unwake(s, 0)
		x.note(EntryPhase, "Outpost restarted while waking the task; send a message to try again")
		return
	}
	x.t.Life = Queued{}
	x.note(EntryPhase, text)
	x.emit(Release{}, RunScheduler{})
}

func (x *tx) recoverActive(a Active, obs AgentObservation) {
	if a.Box.Want == BoxUp && a.Asks[AskInspect].ID == 0 && (!obs.Present || obs.Finished) {
		x.agentGone(a, obs)
		return
	}
	if x.reissue(a) == 0 {
		x.drop(DropNoop)
	}
}

// agentGone handles a run whose agent is not running although its box is meant to be up. A failed
// exit is the same race as a live one, so it inspects the box too.
func (x *tx) agentGone(a Active, obs AgentObservation) {
	switch {
	case !obs.Present:
		x.close(OutcomeFailed, "The agent was lost while Outpost was down")
	case obs.ExitCode == 0:
		x.close(OutcomeDone, "")
	default:
		x.inspect(a, obs.ExitCode)
	}
}

// reissue emits again every effect whose answer is outstanding, and re-runs the turn-end check
// of an unattended run that sat idle through the restart, unless its agent already exited. It returns how many effects it emitted.
func (x *tx) reissue(a Active) int {
	for k, ask := range a.Asks {
		if ask.ID != 0 {
			x.emit(x.askEffect(AskKind(k), ask.ID))
		}
	}
	idle := x.t.Kind.Unattended() && a.Turn.idle() && a.Asks[AskDoneMarker].ID == 0 && a.Asks[AskInspect].ID == 0
	switch {
	case idle && !a.Deadlines.Armed(DlNudge) && !a.Deadlines.Armed(DlGiveUp):
		x.t.Life = x.armLadder(a, 0)
	case idle:
		x.t.Life = x.askMarker(a, WhyTurnEnd)
	}
	return len(x.fx)
}

func (x *tx) askEffect(k AskKind, id AskID) Effect {
	switch k {
	case AskDoneMarker:
		return ProbeDoneMarker{Ask: id}
	case AskSummary:
		return ReadSummary{Ask: id}
	case AskEnsure:
		return EnsureRunning{Ask: id, Run: x.t.AgentRun, Gen: x.t.BoxGen}
	case AskInspect:
		return InspectBox{Ask: id, Gen: x.t.BoxGen}
	}
	return nil
}

func (x *tx) recoverClosing(c Closing) {
	if c.Step == CloseTemplate {
		x.emit(SaveTemplate{Ask: c.Ask})
		return
	}
	x.emit(ReadSummary{Ask: c.Ask})
}
