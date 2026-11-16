package domain

import "time"

// MachineRole says what a machine offers. A sandbox machine runs boxes. A tool machine, such as a
// Mac, serves slots that a task's box reaches through the machine agent.
type MachineRole uint8

const (
	RoleSandbox MachineRole = iota + 1
	RoleTool
)

// Machine is a computer attached through the machine agent. Slots is how many boxes a sandbox
// machine runs at once, or how many claims a tool machine serves. A machine runs only its owner's
// boxes. Exec is the opt-in a tool machine's owner gave at pairing to run commands on the machine
// itself. A sandbox machine never has it.
type Machine struct {
	ID    MachineID
	Name  string
	Owner Login
	Role  MachineRole
	Slots int
	Exec  bool
}

// CanHost is the one place the owner-only rule lives.
func (m Machine) CanHost(t Task) bool {
	return m.Role == RoleSandbox && m.Owner == t.Owner
}

// Repo is a repo Outpost works on. ReadyImage is the saved template new tasks start from.
type Repo struct {
	Name       RepoName
	ReadyImage string
	ReadyAt    time.Time
	Notes      string
	SetupTask  TaskID
}

// StartImage picks the image a new task starts from. A setup task always starts fresh.
func (r Repo) StartImage(k Kind, base string) string {
	if k == KindSetup || r.ReadyImage == "" {
		return base
	}
	return r.ReadyImage
}

// Schedule is a recurring task definition. The cron rule lives in internal/schedule. This is the
// part the domain needs to turn a firing into a task.
type Schedule struct {
	ID      ScheduleID
	Owner   Login
	Kind    Kind
	Spec    Spec
	Enabled bool
}

// Fire builds the task one firing creates.
func (s Schedule) Fire(id TaskID, now time.Time) (Result, error) {
	spec := s.Spec
	spec.Schedule = s.ID
	return New(id, s.Owner, s.Kind, spec, now)
}
