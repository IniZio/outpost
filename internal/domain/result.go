package domain

import "errors"

// Commands a person may not issue, or that the task cannot take now. The texts are the prototype's.
var (
	ErrForbidden = errors.New("only the task's owner can do that")
	ErrReleased  = errors.New("this task's computer was already cleaned up; start a new task")
	ErrNotReady  = errors.New("this task's computer isn't ready yet; try again once it has started")
	ErrClosing   = errors.New("this task is finishing; try again in a moment")
)

// Drop says why Apply ignored an event. Applied is the zero value.
type Drop uint8

const (
	Applied       Drop = iota
	DropStaleLife      // the task is in a Life this event does not apply to
	DropStaleAsk       // an answer to a question that is no longer outstanding
	DropStaleKey       // an agent or box event from an earlier run or generation
	DropNoop           // the event changes nothing
)

// Result is everything one Apply produced. When Dropped is not Applied, Task is the input
// unchanged and both lists are empty: nothing to save, nothing to run.
type Result struct {
	Task     Task
	Effects  []Effect
	Timeline []Entry
	Dropped  Drop
}
