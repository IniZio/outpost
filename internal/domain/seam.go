package domain

// The types below are the seam between the state machine and the code that drives it. Each is a
// pure classification, so the lifecycle holds no rule: a new Effect or Event fails the sumtype check
// here and nowhere else.

// Command is an event a person sends, and the only kind the API, the CLI and /mcp may feed a task.
type Command interface {
	Event
	command()
}

func (UserMessage) command()           {}
func (UserStop) command()              {}
func (UserResume) command()            {}
func (SaveTemplateRequested) command() {}

// Signal is an event a machine observes about a box or the agent's terminal in it. A feed may
// deliver only these, so it cannot forge a command or an answer.
type Signal interface {
	Event
	signal()
}

func (AgentReport) signal() {}
func (AgentExited) signal() {}
func (BoxEvent) signal()    {}

// Delivery says what is owed to an effect if the process dies after the task was saved and before
// the effect ran.
type Delivery uint8

const (
	// DeliverAsked effects carry an Ask. The saved task is their record, and Recovered re-issues
	// them.
	DeliverAsked Delivery = iota + 1
	// DeliverReplay effects have no answer and are idempotent clean-up or input. They are stored
	// with the task and run again after a crash until marked done.
	DeliverReplay
	// DeliverOnce effects are meaningful only in this process: a start abort and a scheduler poke.
	DeliverOnce
)

// EffectMeta is what the lifecycle may know about an effect without switching on its type. Ask is
// zero for an effect that expects no answer. Abortable effects are the ones AbortStart cancels.
// Only a fresh start is: a wake is still answered after the stop that overtook it, because that
// answer is how the box gets stopped again.
type EffectMeta struct {
	Ask       AskID
	Delivery  Delivery
	Abortable bool
}

// Meta classifies an effect.
func Meta(e Effect) EffectMeta {
	switch e := e.(type) {
	case StartTask:
		return EffectMeta{Ask: e.Ask, Delivery: DeliverAsked, Abortable: true}
	case EnsureRunning:
		return EffectMeta{Ask: e.Ask, Delivery: DeliverAsked}
	case ProbeDoneMarker:
		return EffectMeta{Ask: e.Ask, Delivery: DeliverAsked}
	case ReadSummary:
		return EffectMeta{Ask: e.Ask, Delivery: DeliverAsked}
	case SaveTemplate:
		return EffectMeta{Ask: e.Ask, Delivery: DeliverAsked}
	case InspectBox:
		return EffectMeta{Ask: e.Ask, Delivery: DeliverAsked}
	case SendToAgent, InterruptAgent, KillAgent, ClearDoneMarker, StopBox, Release:
		return EffectMeta{Delivery: DeliverReplay}
	case AbortStart, RunScheduler:
		return EffectMeta{Delivery: DeliverOnce}
	}
	return EffectMeta{}
}

// FailedAnswer is the event that stands for an asked effect that could not be carried out, so
// every ask gets its one answer. Three answers have no Err field, and each default leans to the
// recoverable side: a probe that cannot be read finds no marker and the nudge ladder decides, an
// inspect that cannot find the box finds it not running and the loss path replaces it, and a
// summary that cannot be read is empty. ok is false for an effect that expects no answer.
func FailedAnswer(e Effect, why string) (ev Event, ok bool) {
	switch e := e.(type) {
	case StartTask:
		return StartFailed{Ask: e.Ask, Err: why}, true
	case EnsureRunning:
		return Ensured{Ask: e.Ask, Err: why}, true
	case SaveTemplate:
		return TemplateSaved{Ask: e.Ask, Err: why}, true
	case ReadSummary:
		return SummaryRead{Ask: e.Ask}, true
	case ProbeDoneMarker:
		return DoneMarker{Ask: e.Ask}, true
	case InspectBox:
		return BoxInspected{Ask: e.Ask}, true
	case AbortStart, SendToAgent, InterruptAgent, KillAgent, ClearDoneMarker, StopBox, Release, RunScheduler:
		return nil, false
	}
	return nil, false
}

// NeedsObservation reports whether Recovered for t is judged against what the agent's terminal
// looks like now. Only an Active task whose box is meant to be up reads it, so every other task can
// be recovered while its machine is offline.
func (t Task) NeedsObservation() bool {
	a, ok := t.Life.(Active)
	return ok && a.Box.Want == BoxUp
}

// Unfinished reports whether Recovered has anything to do for t after a restart: any task that has
// not ended, and an ended one still waiting for the answer to a wake.
func (t Task) Unfinished() bool {
	e, ok := t.Life.(Ended)
	return !ok || e.Ensure != 0
}
