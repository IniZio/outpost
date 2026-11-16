package domain

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Spec is what the requester asked for. It never changes after creation. A zero Timeout means
// none. Schedule is zero unless a schedule created the task.
type Spec struct {
	Title    string
	Repo     RepoName
	Also     []RepoName // extra repos cloned beside Repo, each pushable
	Prompt   string
	Timeout  time.Duration
	Schedule ScheduleID
}

// Task is one run of an agent. It is a plain value: Apply takes one and returns the next, and
// nothing in this package mutates a Task it was handed. The store keeps it. Version is the
// store's compare-and-swap token, so two writers cannot both win.
//
// The run's progress is the single field Life. The seven-valued Status is derived from it.
// AgentRun and BoxGen live here, not in Active, because they must outlive a finished run: a
// resume continues the count, which is what makes the old agent's late reports stale.
type Task struct {
	ID        TaskID
	Version   uint64
	Owner     Login
	Kind      Kind
	Spec      Spec
	Machine   MachineID // the machine whose box runs this task; set when it starts
	CreatedAt time.Time
	NextAsk   AskID // last AskID handed out
	AgentRun  AgentRun
	BoxGen    BoxGen
	Life      Life
}

// Errors for a task that cannot be built or stored.
var (
	ErrInvalid = errors.New("domain: invalid task")
	ErrCorrupt = errors.New("domain: corrupt task")
)

// New builds a queued task and the timeline entry that says so. It validates the boundary facts
// once, so nothing inside re-checks them.
func New(id TaskID, owner Login, kind Kind, spec Spec, now time.Time) (Result, error) {
	switch {
	case owner == "":
		return Result{}, fmt.Errorf("%w: no owner", ErrInvalid)
	case spec.Repo == "":
		return Result{}, fmt.Errorf("%w: no repo", ErrInvalid)
	case !kind.Valid():
		return Result{}, fmt.Errorf("%w: unknown kind %d", ErrInvalid, kind)
	case spec.Timeout < 0:
		return Result{}, fmt.Errorf("%w: negative timeout", ErrInvalid)
	case kind == KindScheduled && spec.Schedule == 0:
		return Result{}, fmt.Errorf("%w: a scheduled task needs a schedule", ErrInvalid)
	case kind == KindInteractive && spec.Schedule != 0:
		return Result{}, fmt.Errorf("%w: an interactive task has no schedule", ErrInvalid)
	}
	t := Task{
		ID: id, Version: 1, Owner: owner, Kind: kind, Spec: spec, CreatedAt: now, Life: Queued{},
	}
	return Result{
		Task:     t,
		Effects:  []Effect{RunScheduler{}},
		Timeline: []Entry{{At: now, Kind: EntryQueued, Text: "Queued"}},
	}, nil
}

// Status derives the status shown to users. Same Task, same answer. A task with no Life has no
// status and returns zero, which Validate rejects.
func (t Task) Status() Status {
	switch l := t.Life.(type) {
	case Queued:
		return StatusQueued
	case Starting:
		if l.Resume != nil {
			return l.Resume.Outcome.Status() // a wake is quiet: still "done" until the agent is up
		}
		return StatusStarting
	case Active:
		return l.status(t.Kind)
	case Closing:
		return l.From
	case Ended:
		return l.Outcome.Status()
	}
	return 0
}

// BoxWant derives what Outpost intends for the task's box, whatever the Life.
func (t Task) BoxWant() BoxWant {
	switch l := t.Life.(type) {
	case Queued:
		return BoxNone
	case Starting:
		if l.Resume != nil {
			return BoxWaking
		}
		return BoxUp
	case Active:
		return l.Box.Want
	case Closing:
		return BoxUp
	case Ended:
		return l.boxWant()
	}
	return BoxNone
}

func (e Ended) boxWant() BoxWant {
	switch {
	case e.IsReleased():
		return BoxReleased
	case e.BoxKept:
		return BoxUp
	}
	return BoxStopped
}

// Busy reports whether the task holds one of the global running slots. A fresh start and a
// working task hold one. A waiting, ended or waking task does not.
func (t Task) Busy() bool {
	s := t.Status()
	return s == StatusStarting || s == StatusWorking
}

// Phase is the short progress text shown beside the status: "Cloning x/y", the agent's own phase
// message while it works, or empty.
func (t Task) Phase() string {
	switch l := t.Life.(type) {
	case Starting:
		return l.Phase
	case Active:
		return l.Turn.Phase
	case Queued, Closing, Ended:
		return ""
	}
	return ""
}

// NextDeadline is the earliest instant at which Apply(t, Tick{}) would do something. The lifecycle
// keeps one wake-up per task at that instant. There is nothing to cancel: deadlines are state,
// not callbacks, so a stale timer cannot exist.
func (t Task) NextDeadline() (time.Time, bool) {
	switch l := t.Life.(type) {
	case Active:
		return l.Deadlines.Next()
	case Closing:
		return l.Until, true
	case Ended:
		if !l.releasable() {
			return time.Time{}, false
		}
		return l.ReleaseAt, true
	case Starting:
		return l.Until, !l.Until.IsZero()
	case Queued:
		return time.Time{}, false
	}
	return time.Time{}, false
}

// holdsBox reports whether the task has a box on its machine.
func (t Task) holdsBox() bool {
	switch t.Life.(type) {
	case Starting, Active, Closing:
		return true
	case Queued, Ended:
		return false
	}
	return false
}

// Access says what a login may do with a task. Authorization of the other kind, whether a
// teammate can read all the run's repos, needs GitHub and stays outside; the caller passes the answer.
type Access uint8

const (
	AccessNone Access = iota
	AccessWatch
	AccessControl
)

// AccessFor is the only place the owner-types, teammates-watch rule lives.
func (t Task) AccessFor(who Login, canReadAllRepos bool) Access {
	switch {
	case who == t.Owner:
		return AccessControl
	case canReadAllRepos:
		return AccessWatch
	}
	return AccessNone
}

// Capacity is the global cap on running tasks. Hold is set while box images are building.
type Capacity struct {
	Max  int
	Hold bool
}

// Placement says which queued task starts on which machine.
type Placement struct {
	Task    TaskID
	Machine MachineID
}

// PickStarts returns the queued tasks to start now, lowest id first, each with a machine that may
// host it, up to the free slots. A task goes to the machine holding its repo's ready template when
// that machine has room, and otherwise to any machine, where it starts from the base image. The
// lifecycle feeds each result SlotGranted. Two callers racing on the same id is safe: the second
// SlotGranted finds the task no longer Queued and is dropped.
func PickStarts(tasks []Task, machines []Machine, repos []Repo, c Capacity) []Placement {
	if c.Hold {
		return nil
	}
	free := c.Max
	var queued []Task
	used := map[MachineID]int{}
	for _, t := range tasks {
		if t.Busy() {
			free--
		}
		if t.holdsBox() {
			used[t.Machine]++
		}
		if _, ok := t.Life.(Queued); ok {
			queued = append(queued, t)
		}
	}
	slices.SortFunc(queued, func(a, b Task) int { return cmp.Compare(a.ID, b.ID) })
	return place(queued, machines, readyOn(repos), used, free)
}

func readyOn(repos []Repo) map[RepoName]MachineID {
	ready := make(map[RepoName]MachineID, len(repos))
	for _, r := range repos {
		ready[r.Name] = r.ReadyOn
	}
	return ready
}

// place pairs queued tasks with hosting machines until the free slots or the machines run out. A
// task no machine can host stays queued and does not use a free slot.
func place(queued []Task, machines []Machine, ready map[RepoName]MachineID, used map[MachineID]int, free int) []Placement {
	var out []Placement
	for _, t := range queued {
		if free <= 0 {
			break
		}
		i := hostFor(t, machines, ready[t.Spec.Repo], used)
		if i < 0 {
			continue
		}
		used[machines[i].ID]++
		free--
		out = append(out, Placement{Task: t.ID, Machine: machines[i].ID})
	}
	return out
}

// hostFor returns the index of the machine that should host t: the one holding its ready template
// if it can take the task, else the first that can. A setup task starts fresh, so it has no
// preference.
func hostFor(t Task, machines []Machine, readyOn MachineID, used map[MachineID]int) int {
	fits := func(m Machine) bool { return m.CanHost(t) && used[m.ID] < m.Slots }
	if readyOn != "" && t.Kind != KindSetup {
		if i := slices.IndexFunc(machines, func(m Machine) bool { return m.ID == readyOn && fits(m) }); i >= 0 {
			return i
		}
	}
	return slices.IndexFunc(machines, fits)
}
