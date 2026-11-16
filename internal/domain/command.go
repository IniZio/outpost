package domain

import "slices"

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
// A wake gives the old Ended back, still waiting for the wake's answer.
func (x *tx) stopStarting(s Starting) {
	x.emit(AbortStart{})
	if s.Resume != nil {
		x.unwake(s, s.Ask)
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
		Until: x.now.Add(x.p.CloseGrace), Summary: e.Summary, Ensure: e.Ensure,
	}
	x.emit(SaveTemplate{Ask: ask})
	return nil
}

// onMessage handles text typed by the owner. A message to an ended task wakes it. A message to a
// sleeping box wakes the box and waits there until the box is back. A message to a task that is
// not up yet or is finishing is refused.
func (x *tx) onMessage(ev UserMessage) error {
	if err := x.authorize(ev.By); err != nil {
		return err
	}
	switch l := x.t.Life.(type) {
	case Queued:
		return ErrNotReady
	case Starting:
		return x.messageStarting(l, ev.Text)
	case Active:
		x.messageActive(l, ev.Text)
	case Closing:
		return ErrClosing
	case Ended:
		return x.wake(l, ev.Text)
	}
	return nil
}

func (x *tx) messageStarting(s Starting, text string) error {
	if s.Resume == nil {
		return ErrNotReady
	}
	s.Messages = append(slices.Clone(s.Messages), text)
	x.t.Life = s
	return nil
}

// messageActive types the text into the agent, or parks it while the box is away. Either way the
// agent has a turn coming, so an idle turn becomes pending: a leftover idle report cannot end it.
func (x *tx) messageActive(a Active, text string) {
	if a.Turn.idle() {
		a.Turn = Turn{State: TurnPending}
	}
	if a.Box.Want == BoxUp {
		x.t.Life = a
		x.deliver([]string{text})
		return
	}
	a.Box.Parked = append(slices.Clone(a.Box.Parked), text)
	if a.Box.Want == BoxAsleep {
		x.note(EntryPhase, "Waking the box")
		a = x.ensure(a, WhyWake)
	}
	x.t.Life = a
}

// onResume handles a resume with no text: the task comes back and waits for the owner.
func (x *tx) onResume(ev UserResume) error {
	if err := x.authorize(ev.By); err != nil {
		return err
	}
	switch l := x.t.Life.(type) {
	case Queued:
		return ErrNotReady
	case Starting:
		if l.Resume == nil {
			return ErrNotReady
		}
		x.drop(DropNoop)
	case Active:
		x.drop(DropNoop)
	case Closing:
		return ErrClosing
	case Ended:
		return x.wake(l, "")
	}
	return nil
}

// wake brings an ended task back: the box is started and the agent resumed from its own session.
// It is a new agent launch, so the run moves on and the old agent's late reports are stale.
func (x *tx) wake(e Ended, text string) error {
	if e.IsReleased() {
		return ErrReleased
	}
	var messages []string
	if text != "" {
		messages = []string{text}
	}
	ask := x.newAsk()
	x.t.AgentRun++
	x.t.Life = Starting{Since: x.now, Until: x.startUntil(), Phase: "Waking the box", Ask: ask, Resume: &e, Messages: messages}
	x.note(EntryPhase, "Waking the box")
	x.emit(EnsureRunning{Ask: ask, Run: x.t.AgentRun, Gen: x.t.BoxGen})
	return nil
}

// deliver clears the done marker, so an old one cannot end the run the message restarts, and
// types each text into the agent.
func (x *tx) deliver(texts []string) {
	x.emit(ClearDoneMarker{})
	for _, text := range texts {
		x.emit(SendToAgent{Text: text})
		x.note(EntryMessage, text)
	}
}
