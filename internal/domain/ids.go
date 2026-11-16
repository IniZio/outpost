package domain

// Branded ids. Distinct types keep a TaskID from being passed where a MachineID is wanted.
type (
	TaskID     int64
	ScheduleID int64
	Login      string
	RepoName   string
	MachineID  string

	// AskID names one outstanding question the domain put to the outside world through an Effect.
	// Ids are unique per task and never reused, so an answer to a forgotten question is detectable.
	AskID uint64

	// AgentRun counts agent launches within a task. It goes up on every launch, every sleep and
	// every box loss, and agent events carry the run of the terminal they came from. An event from
	// an older run is stale, which is how a late report from a replaced agent is recognised.
	AgentRun uint32

	// BoxGen counts boxes within a task. It goes up on every box replacement, and box events carry
	// the generation of the container they came from.
	BoxGen uint32
)

// Kind is what started a task. It decides who watches the run.
type Kind uint8

const (
	KindInteractive Kind = iota + 1 // a person's task; waits for them at each turn end
	KindScheduled                   // from a schedule; nobody watches
	KindSetup                       // prepares a repo's ready template; nobody watches
)

// Valid reports whether k is one of the three kinds.
func (k Kind) Valid() bool { return k >= KindInteractive && k <= KindSetup }

// Unattended reports whether nobody watches the run, so it needs the done marker and nudges.
func (k Kind) Unattended() bool { return k == KindScheduled || k == KindSetup }

// SavesTemplate reports whether finishing as done also saves the box as the repo's ready template.
func (k Kind) SavesTemplate() bool { return k == KindSetup }

// Status is the seven-valued status the UI, the API and the queue query show. It is derived from a
// Task's Life and Turn by Task.Status. It is never stored as the source of truth.
type Status uint8

const (
	StatusQueued Status = iota + 1
	StatusStarting
	StatusWorking
	StatusWaiting
	StatusDone
	StatusFailed
	StatusStopped
)

var statusNames = [...]string{"", "queued", "starting", "working", "waiting", "done", "failed", "stopped"}

// String names the status as the UI and the queue query spell it.
func (s Status) String() string {
	if int(s) >= len(statusNames) {
		return "unknown"
	}
	return statusNames[s]
}

// Terminal reports whether the status is one of the three resumable end states.
func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusStopped
}

// Outcome is how a run ended.
type Outcome uint8

const (
	OutcomeDone Outcome = iota + 1
	OutcomeFailed
	OutcomeStopped
)

// Valid reports whether o is one of the three outcomes.
func (o Outcome) Valid() bool { return o >= OutcomeDone && o <= OutcomeStopped }

// Status maps an outcome to its terminal status.
func (o Outcome) Status() Status {
	switch o {
	case OutcomeDone:
		return StatusDone
	case OutcomeStopped:
		return StatusStopped
	case OutcomeFailed:
		return StatusFailed
	}
	return 0
}
