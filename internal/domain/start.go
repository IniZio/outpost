package domain

// starting returns the Starting life when ask is its outstanding question and its kind matches:
// fresh for StartTask answers, not fresh for a resume. Otherwise it records why the event is stale.
func (x *tx) starting(ask AskID, fresh bool) (Starting, bool) {
	s, ok := x.t.Life.(Starting)
	switch {
	case !ok || (s.Resume == nil) != fresh:
		x.drop(DropStaleLife)
	case s.Ask != ask:
		x.drop(DropStaleAsk)
	default:
		return s, true
	}
	return Starting{}, false
}

// onSlotGranted starts a queued task. The first agent is run 1 in box generation 1.
func (x *tx) onSlotGranted(ev SlotGranted) {
	if _, ok := x.t.Life.(Queued); !ok {
		x.drop(DropStaleLife)
		return
	}
	ask := x.newAsk()
	x.t.Machine = ev.Machine
	x.t.AgentRun++
	x.t.BoxGen++
	x.t.Life = Starting{Since: x.now, Phase: phasePreparing, Ask: ask}
	x.note(EntryPhase, phasePreparing)
	x.emit(StartTask{Ask: ask, Run: x.t.AgentRun, Gen: x.t.BoxGen})
}

// onProgressed records start progress for the UI and the timeline.
func (x *tx) onProgressed(ev Progressed) {
	s, ok := x.starting(ev.Ask, true)
	if !ok {
		return
	}
	s.Phase = ev.Phase
	x.t.Life = s
	if ev.Note != "" {
		x.note(EntryPhase, ev.Note)
	}
}

// onStarted finishes a fresh start and replays what the agent reported before it.
func (x *tx) onStarted(ev Started) {
	s, ok := x.starting(ev.Ask, true)
	if !ok {
		return
	}
	x.t.Life = Active{
		Since: x.now,
		Turn:  Turn{State: TurnPending},
		Box:   Box{Want: BoxUp, StartedAt: x.now},
	}
	x.note(EntryPhase, "The agent is working")
	x.replay(s.Early)
}

// onStartFailed fails a start. A failed start releases at once: nothing is worth resuming.
func (x *tx) onStartFailed(ev StartFailed) {
	if _, ok := x.starting(ev.Ask, true); !ok {
		return
	}
	x.note(EntryError, ev.Err)
	x.t.Life = Ended{Outcome: OutcomeFailed, At: x.now, Error: ev.Err, ReleasedAt: x.now}
	x.emit(Release{}, RunScheduler{})
}
