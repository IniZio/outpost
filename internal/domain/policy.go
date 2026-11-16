package domain

import "time"

// Policy holds every number the rules use. Callers build it once from config. Tests pass small
// values. Nothing in the domain reads a global. A zero Timeout, IdleSleep or KeepFinished turns
// that rule off.
type Policy struct {
	NudgeAfter    []time.Duration
	RealWork      time.Duration
	GiveUp        time.Duration
	IdleSleep     time.Duration
	ReplaceDelay  time.Duration
	BoxStartGrace time.Duration
	CloseGrace    time.Duration
	KeepFinished  time.Duration
}

// DefaultPolicy is the prototype's numbers: nudges after 3, 10 and 10 minutes, two minutes of
// real work resets the ladder, give up 30 minutes after the third nudge, sleep the box after 30
// idle minutes, wait 4 s before replacing a lost box, call a box start more than 30 s after our
// own start a restart, wait 2 minutes for the summary, and keep a finished task's computer a day.
func DefaultPolicy() Policy {
	return Policy{
		NudgeAfter:    []time.Duration{3 * time.Minute, 10 * time.Minute, 10 * time.Minute},
		RealWork:      2 * time.Minute,
		GiveUp:        30 * time.Minute,
		IdleSleep:     30 * time.Minute,
		ReplaceDelay:  4 * time.Second,
		BoxStartGrace: 30 * time.Second,
		CloseGrace:    2 * time.Minute,
		KeepFinished:  24 * time.Hour,
	}
}
