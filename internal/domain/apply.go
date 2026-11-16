package domain

import "time"

// Apply is the task state machine's single entry point. It is pure: same inputs, same Result,
// no clock, no I/O. now and p are arguments so tests control them.
//
// Idempotence: an agent or box event from an earlier run or generation, an event for a Life the
// task already left, an answer that does not match the outstanding Ask, and a Tick with nothing
// due are all dropped with the task untouched. Replaying events therefore converges.
//
// The returned error is a refused command (ErrForbidden and its siblings). The lifecycle passes
// it to the API caller. Every other outcome is a Result.
func Apply(t Task, ev Event, now time.Time, p Policy) (Result, error) {
	if k, ok := ev.(keyed); ok && k.staleFor(t) {
		return Result{Task: t, Dropped: DropStaleKey}, nil
	}
	x := &tx{t: t, was: t.Status(), now: now, p: p}
	if err := x.dispatch(ev); err != nil {
		return Result{}, err
	}
	if x.dropped != Applied {
		return Result{Task: t, Dropped: x.dropped}, nil
	}
	x.settle()
	x.t.Version++
	return Result{Task: x.t, Effects: x.fx, Timeline: x.tl}, nil
}

// keyed is an event that carries the run or generation of its source.
type keyed interface{ staleFor(t Task) bool }

func (e AgentReport) staleFor(t Task) bool { return e.Run < t.AgentRun }
func (e AgentExited) staleFor(t Task) bool { return e.Run < t.AgentRun }
func (e BoxEvent) staleFor(t Task) bool    { return e.Gen < t.BoxGen }

// tx is one Apply in progress. It owns a private copy of the task. Handlers read and write it
// and append effects and timeline entries. Nothing outside Apply sees a tx.
type tx struct {
	t       Task
	was     Status
	now     time.Time
	p       Policy
	fx      []Effect
	tl      []Entry
	dropped Drop
}

// dispatch is the only switch over Event. Each case is one call into a handler that lives in the
// file named for its concern: command.go, start.go, agent.go, box.go, close.go, tick.go.
func (x *tx) dispatch(ev Event) error {
	switch ev := ev.(type) {
	case UserStop:
		return x.onStop(ev)
	case SaveTemplateRequested:
		return x.onSaveTemplate(ev)
	case SlotGranted:
		x.onSlotGranted(ev)
	case Tick:
		x.onTick()
	case Progressed:
		x.onProgressed(ev)
	case Started:
		x.onStarted(ev)
	case StartFailed:
		x.onStartFailed(ev)
	case SummaryRead:
		x.onSummary(ev)
	case TemplateSaved:
		x.onTemplate(ev)
	}
	return nil
}

func (x *tx) emit(fx ...Effect) { x.fx = append(x.fx, fx...) }

func (x *tx) note(k EntryKind, text string) {
	x.tl = append(x.tl, Entry{At: x.now, Kind: k, Text: text})
}

func (x *tx) drop(why Drop) { x.dropped = why }

func (x *tx) newAsk() AskID {
	x.t.NextAsk++
	return x.t.NextAsk
}

func (x *tx) authorize(by Login) error {
	if x.t.AccessFor(by, false) != AccessControl {
		return ErrForbidden
	}
	return nil
}

func (x *tx) active() (Active, bool) {
	a, ok := x.t.Life.(Active)
	return a, ok
}

// replay feeds buffered agent events back through dispatch once the agent is up. A replayed event
// that turns out to change nothing must not cancel the step that replays it, so the drop it
// records is cleared.
func (x *tx) replay(early []Early) {
	for _, e := range early {
		_ = x.dispatch(e.Event()) // only reports and exits are buffered, and they never refuse
		x.dropped = Applied
	}
}

// takeAsk returns the Active life with the slot of kind k cleared, when id is that slot's
// outstanding question. Otherwise it records why the answer is stale.
func (x *tx) takeAsk(k AskKind, id AskID) (Active, Ask, bool) {
	a, ok := x.active()
	switch {
	case !ok:
		x.drop(DropStaleLife)
	case id == 0 || a.Asks[k].ID != id:
		x.drop(DropStaleAsk)
	default:
		ask := a.Asks[k]
		a.Asks[k] = Ask{}
		return a, ask, true
	}
	return Active{}, Ask{}, false
}
