package domain

import (
	"slices"
	"time"
)

// DeadlineKind names one deadline slot. The order is the priority: when several are due in one
// Tick they are handled in this order, so a timeout beats a nudge.
type DeadlineKind uint8

const (
	DlTimeout DeadlineKind = iota
	DlGiveUp
	DlNudge
	DlReplace
	DlSleep
	DlStall
)

const numDeadlines = int(DlStall) + 1

// Deadlines holds each armed deadline as an instant. The zero time means unarmed. It is an array,
// so copying an Active copies its deadlines and Apply cannot alias them.
//
// Deadlines are state, not callbacks. Nothing arms or cancels a timer. After each Apply the
// lifecycle asks Task.NextDeadline for one wake-up instant, and at that instant it feeds Tick.
// A restart needs no re-arming because the deadlines were persisted with the task.
type Deadlines [numDeadlines]time.Time

// Set returns the deadlines with k armed at at.
func (d Deadlines) Set(k DeadlineKind, at time.Time) Deadlines {
	d[k] = at
	return d
}

// Clear returns the deadlines with k unarmed.
func (d Deadlines) Clear(k DeadlineKind) Deadlines {
	d[k] = time.Time{}
	return d
}

// Armed reports whether k is armed.
func (d Deadlines) Armed(k DeadlineKind) bool { return !d[k].IsZero() }

// Due lists the armed deadlines at or before now, in priority order.
func (d Deadlines) Due(now time.Time) []DeadlineKind {
	var due []DeadlineKind
	for k := range DeadlineKind(numDeadlines) {
		if d.Armed(k) && !d[k].After(now) {
			due = append(due, k)
		}
	}
	return due
}

// Next returns the earliest armed deadline.
func (d Deadlines) Next() (time.Time, bool) {
	armed := slices.DeleteFunc(slices.Clone(d[:]), time.Time.IsZero)
	if len(armed) == 0 {
		return time.Time{}, false
	}
	return slices.MinFunc(armed, time.Time.Compare), true
}
