package domain

import "time"

// Policy holds every number the rules use. Callers build it once from config. Tests pass small
// values. Nothing in the domain reads a global. A zero Timeout, IdleSleep or KeepFinished turns
// that rule off. StallAfter is how long an unattended agent may stay pending with its box up
// before the run treats its turn as ended. ReplaceBackoff is the wait before each retry of a
// failed box replacement, so its length is the number of retries. CloseGrace bounds the summary
// read of the finish window, StartBudget a start or wake, and SaveBudget a template save.
type Policy struct {
	NudgeAfter     []time.Duration
	RealWork       time.Duration
	GiveUp         time.Duration
	StallAfter     time.Duration
	IdleSleep      time.Duration
	ReplaceDelay   time.Duration
	ReplaceBackoff []time.Duration
	BoxStartGrace  time.Duration
	CloseGrace     time.Duration
	StartBudget    time.Duration
	SaveBudget     time.Duration
	KeepFinished   time.Duration
}

// DefaultPolicy is the prototype's numbers: nudges after 3, 10 and 10 minutes, two minutes of
// real work resets the ladder, give up 30 minutes after the third nudge, check on an unattended
// agent that has not reported a turn after 10 minutes, sleep the box after 30 idle minutes, wait
// 4 s before replacing a lost box and retry a failed replacement after 30 s, 2 and 5 minutes, call
// a box start more than 30 s after our own start a restart, wait 2 minutes for the summary, give
// up on a start after 30 minutes, wait 45 for a template save and keep a finished task's computer
// a day.
func DefaultPolicy() Policy {
	return Policy{
		NudgeAfter:     []time.Duration{3 * time.Minute, 10 * time.Minute, 10 * time.Minute},
		RealWork:       2 * time.Minute,
		GiveUp:         30 * time.Minute,
		StallAfter:     10 * time.Minute,
		IdleSleep:      30 * time.Minute,
		ReplaceDelay:   4 * time.Second,
		ReplaceBackoff: []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute},
		BoxStartGrace:  30 * time.Second,
		CloseGrace:     2 * time.Minute,
		StartBudget:    30 * time.Minute,
		SaveBudget:     45 * time.Minute,
		KeepFinished:   24 * time.Hour,
	}
}

// nudgeStep is the nudge ladder for unattended runs, as a pure function. A turn that ran
// RealWork or longer resets the count. Under the ladder's length the next nudge is armed, and
// at the end the give-up deadline is armed. The caller arms the returned deadline on turn end,
// and settle clears it whenever the agent is no longer idle, so "any return to working
// cancels the pending timer" needs no code of its own.
func (n Nudge) nudgeStep(worked time.Duration, p Policy) (Nudge, DeadlineKind, time.Duration) {
	if worked >= p.RealWork {
		n.Sent = 0
	}
	if n.Sent >= len(p.NudgeAfter) {
		return n, DlGiveUp, p.GiveUp
	}
	return n, DlNudge, p.NudgeAfter[n.Sent]
}
