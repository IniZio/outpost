package domain

import "time"

// TurnSignal is what a report meant for the run.
type TurnSignal uint8

const (
	SigNone TurnSignal = iota
	SigStarted
	SigPhase
	SigEnded
	SigBlocked
)

// TurnStep is the result of feeding one report to a Turn. Worked is how long the turn ran,
// set with SigEnded and SigBlocked.
type TurnStep struct {
	Turn   Turn
	Signal TurnSignal
	Worked time.Duration
}

// onReport is the turn machine. It is pure and knows nothing of kinds, boxes or nudges. hc
// reports a level, so a turn starts and ends where the level changes.
//
//	state     report              next      signal
//	Pending   working             Running   Started
//	Pending   idle done error     Pending   none      (initial or leftover idle)
//	Running   working             Running   Phase if the text changed, else none
//	Running   idle done error     Paused    Ended
//	Paused    working             Running   Started
//	Paused    idle done error     Paused    none
//	Blocked   working             Running   Started
//	Blocked   idle done error     Paused    Ended     (the question went away)
//	any       blocked             Blocked   Blocked, or none when already Blocked
func (tn Turn) onReport(r AgentReport, now time.Time) TurnStep {
	switch r.Status {
	case HCWorking:
		return tn.working(r.Phase, now)
	case HCBlocked:
		return tn.blocked(now)
	case HCIdle, HCDone, HCError:
		return tn.paused(now)
	}
	return TurnStep{Turn: tn}
}

func (tn Turn) working(phase string, now time.Time) TurnStep {
	if tn.State == TurnRunning {
		sig := SigNone
		if tn.Phase != phase {
			sig = SigPhase
		}
		tn.Phase = phase
		return TurnStep{Turn: tn, Signal: sig}
	}
	tn.State, tn.WorkingSince, tn.Phase = TurnRunning, now, phase
	return TurnStep{Turn: tn, Signal: SigStarted}
}

func (tn Turn) blocked(now time.Time) TurnStep {
	if tn.State == TurnBlocked {
		return TurnStep{Turn: tn}
	}
	worked := tn.worked(now)
	tn.State, tn.WorkingSince, tn.Phase = TurnBlocked, time.Time{}, ""
	return TurnStep{Turn: tn, Signal: SigBlocked, Worked: worked}
}

func (tn Turn) paused(now time.Time) TurnStep {
	if tn.State != TurnRunning && tn.State != TurnBlocked {
		return TurnStep{Turn: tn}
	}
	worked := tn.worked(now)
	tn.State, tn.WorkingSince, tn.Phase = TurnPaused, time.Time{}, ""
	return TurnStep{Turn: tn, Signal: SigEnded, Worked: worked}
}

// worked is how long the running turn has run, zero when it is not running.
func (tn Turn) worked(now time.Time) time.Duration {
	if tn.State != TurnRunning {
		return 0
	}
	return now.Sub(tn.WorkingSince)
}
