package domain

import (
	"fmt"
	"time"
)

// EntryKind is the timeline entry kind. The strings are the values the prototype stored in task_events.kind.
type EntryKind string

const (
	EntryQueued  EntryKind = "queued"
	EntryPhase   EntryKind = "phase"
	EntryWarning EntryKind = "warning"
	EntryError   EntryKind = "error"
	EntryWaiting EntryKind = "waiting"
	EntryMessage EntryKind = "message"
	EntryDone    EntryKind = "done"
	EntryFailed  EntryKind = "failed"
	EntryStopped EntryKind = "stopped"
	EntryReady   EntryKind = "ready"
)

// Entry is one line of the task's timeline. Apply stamps At from its now argument. Every text
// the UI shows for a state change is built in this package, so the lifecycle composes none.
type Entry struct {
	At   time.Time
	Kind EntryKind
	Text string
}

// Notes to the agent are rules too: the unattended nudge, the box-was-replaced note, the OOM
// warning. They live here with the rules that trigger them.
const NudgeText = "[outpost] You stopped without marking this run as done. Nobody is watching this run. If the whole task is complete, run `touch /run/outpost/done` and then write your final summary. If you are waiting for something, keep going."

const OOMNote = "[outpost] The kernel killed a process in your box because memory ran out (the box limit is reached). The box is still up, but what that process was doing is gone. Check what died, restart it, and keep memory under the box limit."

// BoxBackNote tells the agent its box was replaced. why is "ran out of memory", "stopped
// unexpectedly" or "restarted".
func BoxBackNote(why string) string {
	return fmt.Sprintf("[outpost] Your box (the computer your commands run on) %s and was replaced with a fresh one. The workspace is intact, but processes you started there (servers, emulators, compose stacks) are gone. Restart what you need and carry on.", why)
}

const phasePreparing = "Preparing the computer"
