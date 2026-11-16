package domain

// Effect is something the domain wants done in the world. Apply returns them in order. The
// lifecycle persists the new Task first, then runs the effects in order through the ports. An
// effect that carries an Ask is answered by an Event with the same AskID. The rest are best
// effort: the lifecycle logs a failure and moves on, as the prototype's .catch(() => {}) did.
//
// Because every effect with an answer is recorded in the task before it runs, a restart can
// re-issue them from the persisted task (see Recovered).
//
//sumtype:decl
type Effect interface{ effect() }

func (StartTask) effect()       {}
func (AbortStart) effect()      {}
func (EnsureRunning) effect()   {}
func (SendToAgent) effect()     {}
func (InterruptAgent) effect()  {}
func (KillAgent) effect()       {}
func (ClearDoneMarker) effect() {}
func (ProbeDoneMarker) effect() {}
func (ReadSummary) effect()     {}
func (SaveTemplate) effect()    {}
func (StopBox) effect()         {}
func (Release) effect()         {}
func (RunScheduler) effect()    {}

// StartTask runs the whole fresh start: desk, box, clones, agent profile, agent launch. The
// lifecycle reports Progressed along the way and ends with Started or StartFailed. Run and Gen
// are the keys to label the agent terminal and the box with, so their events can be recognised.
type StartTask struct {
	Ask AskID
	Run AgentRun
	Gen BoxGen
}

// AbortStart cancels a StartTask in flight. Its late answers are dropped by Apply.
type AbortStart struct{}

// EnsureRunning is idempotent. It leaves a box labelled Gen running, replacing a lost box of an
// older generation with a new container, and one agent terminal running under Run, launched with
// the agent's own resume and replacing a terminal still alive under an older run. The workspace
// and the agent's home live on volumes outside the box, so a replacement loses no work. Labelling
// matters: the domain drops box and agent events that carry an older Gen or Run. Ensured.Revived
// is false only when a terminal of Run was already alive, as when the same ask is retried.
type EnsureRunning struct {
	Ask AskID
	Run AgentRun
	Gen BoxGen
}

// SendToAgent types text into the agent's terminal. InterruptAgent presses escape. KillAgent
// ends the terminal.
type (
	SendToAgent    struct{ Text string }
	InterruptAgent struct{}
	KillAgent      struct{}
)

// ClearDoneMarker removes the done marker. ProbeDoneMarker reads whether it exists.
type (
	ClearDoneMarker struct{}
	ProbeDoneMarker struct{ Ask AskID }
)

// ReadSummary reads the agent's last message. The adapter retries inside the effect, so the
// domain gets one SummaryRead, possibly empty.
type ReadSummary struct{ Ask AskID }

// SaveTemplate commits the box as the repo's ready template.
type SaveTemplate struct{ Ask AskID }

// StopBox stops the box without deleting it.
type StopBox struct{}

// Release cleans up the desk, the box and the hc machine for good.
type Release struct{}

// RunScheduler tells the lifecycle a slot freed, so it should run PickStarts.
type RunScheduler struct{}
