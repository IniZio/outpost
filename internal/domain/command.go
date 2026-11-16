package domain

// onStop handles a stop from the owner. A queued task stops at once with nothing to clean up. A
// starting one aborts and releases. A running one kills its agent and finishes as stopped.
func (x *tx) onStop(ev UserStop) error {
	if err := x.authorize(ev.By); err != nil {
		return err
	}
	switch l := x.t.Life.(type) {
	case Queued:
		x.t.Life = Ended{Outcome: OutcomeStopped, At: x.now, ReleasedAt: x.now}
		x.note(EntryStopped, "Stopped before it started")
	case Starting:
		x.stopStarting(l)
	case Active:
		x.emit(KillAgent{})
		x.close(OutcomeStopped, "")
	case Closing, Ended:
		x.drop(DropNoop)
	}
	return nil
}

// stopStarting aborts a start. A fresh start has nothing worth resuming, so it releases at once.
// A wake gives the old Ended back whole.
func (x *tx) stopStarting(s Starting) {
	x.emit(AbortStart{})
	if s.Resume != nil {
		x.t.Life = *s.Resume
		x.note(EntryPhase, "Stopped waking the task")
		return
	}
	x.t.Life = Ended{Outcome: OutcomeStopped, At: x.now, ReleasedAt: x.now}
	x.note(EntryStopped, "Stopped")
	x.emit(Release{}, RunScheduler{})
}

// onSaveTemplate handles the owner's retry of a failed template save on a setup task whose box
// was kept. Success ends the task as done. A second failure puts it back as failed with the new
// error.
func (x *tx) onSaveTemplate(ev SaveTemplateRequested) error {
	if err := x.authorize(ev.By); err != nil {
		return err
	}
	e, ok := x.t.Life.(Ended)
	if !ok || !e.BoxKept || e.IsReleased() {
		x.drop(DropNoop)
		return nil
	}
	ask := x.newAsk()
	x.t.Life = Closing{
		Want: OutcomeDone, From: StatusFailed, Step: CloseTemplate, Ask: ask,
		Until: x.now.Add(x.p.CloseGrace), Summary: e.Summary,
	}
	x.emit(SaveTemplate{Ask: ask})
	return nil
}
