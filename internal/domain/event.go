package domain

// Event is everything that can happen to a task. The lifecycle turns each thing it observes into
// one Event and calls Apply. Events are plain values with no wire types: adapters parse hc
// output, docker events and API calls into these at the boundary.
//
// The set is sealed. Only this package defines events, so the one switch in Apply can be checked
// for completeness. Machine-tool claims get their own events here when internal/toolmachine lands.
//
//sumtype:decl
type Event interface{ event() }

func (UserMessage) event()           {}
func (UserStop) event()              {}
func (UserResume) event()            {}
func (SaveTemplateRequested) event() {}
func (SlotGranted) event()           {}
func (Tick) event()                  {}
func (Recovered) event()             {}
func (AgentReport) event()           {}
func (AgentExited) event()           {}
func (BoxEvent) event()              {}
func (Progressed) event()            {}
func (Started) event()               {}
func (StartFailed) event()           {}
func (DoneMarker) event()            {}
func (SummaryRead) event()           {}
func (TemplateSaved) event()         {}
func (Ensured) event()               {}
func (BoxInspected) event()          {}

// UserMessage, UserStop, UserResume and SaveTemplateRequested are commands from a person. Apply
// refuses them with ErrForbidden unless By owns the task. A message to an ended task resumes it.
type (
	UserMessage struct {
		By   Login
		Text string
	}
	UserStop              struct{ By Login }
	UserResume            struct{ By Login }
	SaveTemplateRequested struct{ By Login }
)

// SlotGranted moves a queued task to starting on Machine. The lifecycle sends one for each
// Placement PickStarts returns.
type SlotGranted struct{ Machine MachineID }

// Tick means the wake-up instant from NextDeadline has come. It carries nothing: Apply reads
// the task's own deadlines against now. A duplicate Tick finds nothing due and is dropped.
type Tick struct{}

// Recovered is fed once per unfinished task when the server starts. Agent is what the lifecycle
// found when it asked hc about the agent's terminal.
type (
	Recovered        struct{ Agent AgentObservation }
	AgentObservation struct {
		Present  bool
		Finished bool
		ExitCode int
	}
)

// HCStatus is what hc reports about the agent. The report is a level, not an edge: the domain
// derives turn starts and ends from changes between reports.
type HCStatus uint8

const (
	HCWorking HCStatus = iota + 1
	HCIdle
	HCDone
	HCBlocked
	HCError
)

// AgentReport is one hc status report. Run is the AgentRun of the terminal it came from, which
// the lifecycle reads from the label it gave the terminal.
type AgentReport struct {
	Run    AgentRun
	Status HCStatus
	Phase  string
}

// AgentExited means the terminal of Run ended with Code.
type AgentExited struct {
	Run  AgentRun
	Code int
}

// BoxEventKind is what the sandbox saw. BoxEvent carries the facts the prototype read back from
// docker inside its handler: whether the box is still running and whether the kernel killed it
// for memory. The adapter inspects once and puts the answers on the event, so the decision
// stays pure.
type BoxEventKind uint8

const (
	BoxStarted BoxEventKind = iota + 1
	BoxDied
	BoxOOM
)

// BoxEvent is one sandbox event about the box of generation Gen. Whether it matters is judged
// against what Outpost intends for the box (see BoxWant), so an echo of our own stop or start is
// no incident.
type BoxEvent struct {
	Gen          BoxGen
	Kind         BoxEventKind
	StillRunning bool
	OOMKilled    bool
}

// Progressed, Started and StartFailed answer StartTask. A resume is answered by Ensured instead.
type (
	Progressed struct {
		Ask         AskID
		Phase, Note string
	}
	Started     struct{ Ask AskID }
	StartFailed struct {
		Ask AskID
		Err string
	}
)

// DoneMarker answers ProbeDoneMarker, SummaryRead answers ReadSummary, TemplateSaved answers
// SaveTemplate, Ensured answers EnsureRunning and BoxInspected answers InspectBox. Each carries
// the AskID it answers. Apply drops an answer whose Ask is not the outstanding one, which covers
// late, duplicate and orphaned answers.
type (
	DoneMarker struct {
		Ask     AskID
		Present bool
	}
	SummaryRead struct {
		Ask  AskID
		Text string
	}
	TemplateSaved struct {
		Ask AskID
		Tag string
		Err string
	}
	Ensured struct {
		Ask     AskID
		Revived bool
		Err     string
	}
	BoxInspected struct {
		Ask       AskID
		Running   bool
		OOMKilled bool
	}
)
