package domain

import "time"

// Life is where a task is in its run. It is a sealed sum: exactly the five variants below, each
// carrying only the fields that make sense in it. "Working" and "waiting" are not variants. They
// flip on every agent report and are derived from Active.Turn (see Task.Status).
//
// A type switch over Life must cover all five. The gochecksumtype linter enforces that.
//
//sumtype:decl
type Life interface{ life() }

func (Queued) life()   {}
func (Starting) life() {}
func (Active) life()   {}
func (Closing) life()  {}
func (Ended) life()    {}

// Queued waits for a free slot. Nothing exists to clean up.
type Queued struct{}

// Starting is bringing the box and the agent up. Ask is the outstanding StartTask or
// EnsureRunning. For a fresh task the lifecycle runs one StartTask effect and answers with
// Progressed, then Started or StartFailed.
//
// When Resume is set the task is waking an ended one. The Ended value is kept so a failed wake
// puts the task back exactly as it was. Messages were typed by the owner to wake the task and are
// delivered once the agent is up. Early holds agent reports and exits that arrived before the
// answer and are replayed when it comes.
type Starting struct {
	Since    time.Time
	Phase    string
	Ask      AskID
	Resume   *Ended
	Messages []string
	Early    []Early
}

// Early is an agent report or exit that beat the answer to StartTask or EnsureRunning. Exactly
// one field is set.
type Early struct {
	Report *AgentReport
	Exit   *AgentExited
}

// same reports whether e buffers the same event as o.
func (e Early) same(o Early) bool {
	switch {
	case e.Report != nil && o.Report != nil:
		return *e.Report == *o.Report
	case e.Exit != nil && o.Exit != nil:
		return *e.Exit == *o.Exit
	}
	return false
}

// Event returns the buffered event.
func (e Early) Event() Event {
	if e.Report != nil {
		return *e.Report
	}
	return *e.Exit
}

// Active is a running task: the agent has a terminal in a box. Since is when this stretch
// began, and the timeout counts from it. Everything the run's sub-rules need sits here as small
// values, so Apply can compose them without any hidden state.
type Active struct {
	Since     time.Time
	Turn      Turn
	Box       Box
	Nudge     Nudge
	Summary   string
	Deadlines Deadlines
	Asks      Asks
}

// CloseStep is what a Closing task is waiting for.
type CloseStep uint8

const (
	CloseSummary  CloseStep = iota + 1 // reading the agent's last message
	CloseTemplate                      // saving the box as the repo's ready template
)

// Closing is the finish window. Every path that ends a run enters it, so a second finish finds
// Closing and is dropped. From is the status shown during the window, so users see no new status.
// Until bounds the wait for the answer.
type Closing struct {
	Want    Outcome
	Reason  string
	From    Status
	Step    CloseStep
	Ask     AskID
	Until   time.Time
	Summary string
}

// Ended is a finished run. It is resumable until IsReleased. BoxKept is set on a setup task
// whose template save failed: its box keeps running so the save can be retried. ReleaseAt is when
// the box and desk get cleaned up. ReleasedAt stays zero until then, and after it no resume works.
type Ended struct {
	Outcome    Outcome
	At         time.Time
	Error      string
	Summary    string
	BoxKept    bool
	ReleaseAt  time.Time
	ReleasedAt time.Time
}

// releasable reports whether a release is still due: it is scheduled and has not happened.
func (e Ended) releasable() bool { return !e.ReleaseAt.IsZero() && !e.IsReleased() }

// IsReleased reports whether the task's computer was cleaned up.
func (e Ended) IsReleased() bool { return !e.ReleasedAt.IsZero() }

// Asks holds at most one outstanding question per kind. The domain records every Effect that
// expects an answer here, and accepts only answers that match. That makes a late or duplicate
// answer harmless, and it lets a restart re-issue what was in flight (see Recovered).
type Asks [numAskKinds]Ask

// AskKind indexes Asks.
type AskKind uint8

const (
	AskDoneMarker AskKind = iota
	AskSummary
	AskEnsure
)

const numAskKinds = int(AskEnsure) + 1

// Ask is one outstanding question and why it was asked. The zero Ask is an empty slot.
type Ask struct {
	ID  AskID
	Why Why
}

// Why is the reason attached to a question, so the answer is handled in context. WhyTurnEnd,
// WhyNudge and WhyGiveUp tag done-marker probes. WhyReplace and WhyWake tag EnsureRunning: the
// box was lost, or a message needs a sleeping box.
type Why uint8

const (
	WhyTurnEnd Why = iota + 1
	WhyNudge
	WhyGiveUp
	WhyReplace
	WhyWake
)

// Nudge is the unattended run's ladder position. Sent counts nudges since the last stretch of
// real work.
type Nudge struct {
	Sent int
}

// BoxWant is the state Outpost intends for a task's box. Box events are judged against it: a box
// that stops while Outpost wants it asleep is not an incident. Task.BoxWant derives it for every
// Life. BoxNone is a queued task with no box yet. BoxUp is running and meant to be. BoxAsleep is
// stopped after idle, and a message wakes it. BoxReplacing means it was lost and a replacement
// starts at DlReplace. BoxWaking means an EnsureRunning is outstanding. BoxStopped is stopped
// because the run finished, and still resumable. BoxReleased is gone for good.
type BoxWant uint8

const (
	BoxNone BoxWant = iota + 1
	BoxUp
	BoxAsleep
	BoxReplacing
	BoxWaking
	BoxStopped
	BoxReleased
)

var boxWantNames = [...]string{"", "none", "up", "asleep", "replacing", "waking", "stopped", "released"}

// String names the intended box state.
func (w BoxWant) String() string {
	if int(w) >= len(boxWantNames) {
		return "unknown"
	}
	return boxWantNames[w]
}

// Box is what the domain knows about an active task's box. The agent runs inside it, so box up
// also means the agent's terminal can exist. Want is only ever Up, Asleep, Replacing or Waking
// here. Lost is why the box was lost, for the note to the agent. StartedAt is when Outpost last
// started it, so a start event soon after is our own echo. Parked holds messages waiting for the
// box to come back.
type Box struct {
	Want      BoxWant
	Lost      string
	StartedAt time.Time
	Parked    []string
}

// Turn is the agent's turn state as the domain sees it, built from hc's level reports.
// WorkingSince and Phase are set only in TurnRunning.
type Turn struct {
	State        TurnState
	WorkingSince time.Time
	Phase        string
}

// TurnState is where the agent is in its turn.
//
// TurnPending: a turn is expected but the agent has not reported working yet. Claude reports
// idle once at start, and an idle left over from before a message arrives late. Both land here
// and are ignored. This one state replaces the prototype's null agent_status.
// TurnPaused: the turn ended and the agent is idle, done or errored. TurnBlocked: it asks a
// question.
type TurnState uint8

const (
	TurnPending TurnState = iota + 1
	TurnRunning
	TurnPaused
	TurnBlocked
)

// idle reports whether the agent has stopped without finishing: its turn ended or it asks.
func (tn Turn) idle() bool { return tn.State == TurnPaused || tn.State == TurnBlocked }

// status derives the status shown for a running task.
//
// An unattended run keeps showing "working" while the agent is idle between nudges, because
// nobody is there to wait for. Blocked is waiting for everyone.
func (a Active) status(k Kind) Status {
	switch a.Turn.State {
	case TurnBlocked:
		return StatusWaiting
	case TurnPaused:
		if k.Unattended() {
			return StatusWorking
		}
		return StatusWaiting
	case TurnPending, TurnRunning:
		return StatusWorking
	}
	return StatusWorking
}
