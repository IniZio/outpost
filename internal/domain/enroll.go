package domain

import (
	"errors"
	"time"
)

// UserCode is the XXXX-XXXX code a person checks on the confirm page, from a vowel-free alphabet.
type UserCode string

// EnrollState is where one pairing attempt stands.
type EnrollState uint8

const (
	EnrollWaiting   EnrollState = iota + 1 // code shown, nobody has confirmed it
	EnrollEnrolling                        // a person confirmed; the machine row is being written
	EnrollEnrolled                         // the row exists; the next poll hands it out
	EnrollCollected                        // the agent has its machine id
)

var (
	ErrEnrollExpired   = errors.New("pairing code expired")
	ErrEnrollUsed      = errors.New("pairing code already confirmed")
	ErrExecNotTool     = errors.New("only a tool machine may serve exec")
	ErrBadPairing      = errors.New("bad pairing request")
	ErrTooManyPairings = errors.New("too many pairings waiting")
	ErrUnknownPairing  = errors.New("unknown pairing code")
)

const (
	maxMachineNameLength = 64
	maxMachineSlots      = 64
)

// PairRequest is what `outpost-agent pair` sends. The agent makes the key and only its public half
// leaves the machine.
type PairRequest struct {
	Key   MachineKey
	Name  string
	Role  MachineRole
	Slots int
	Exec  bool
}

// Validate checks every bound of a pairing request.
func (r PairRequest) Validate() error {
	switch {
	case r.Key == MachineKey{}:
		return errors.Join(ErrBadPairing, errors.New("no key"))
	case r.Name == "" || len(r.Name) > maxMachineNameLength:
		return errors.Join(ErrBadPairing, errors.New("name must be 1 to 64 bytes"))
	case r.Role != RoleSandbox && r.Role != RoleTool:
		return errors.Join(ErrBadPairing, errors.New("unknown role"))
	case r.Slots < 1 || r.Slots > maxMachineSlots:
		return errors.Join(ErrBadPairing, errors.New("slots must be 1 to 64"))
	case r.Exec && r.Role != RoleTool:
		return ErrExecNotTool
	}
	return nil
}

// PairTicket is what the server answers a pairing request with. Device is the agent's secret for
// polling, and stores keep only a hash of it.
type PairTicket struct {
	Device   [32]byte
	User     UserCode
	Expires  time.Time
	Interval time.Duration
}

// Enrollment is one pairing attempt, from the agent's request to the poll that collects it. Its
// transitions are pure. The store runs them under its own lock and writes the machine row between
// Confirm and Enrolled.
type Enrollment struct {
	User    UserCode
	Device  [32]byte
	Req     PairRequest
	State   EnrollState
	Owner   Login
	Machine MachineID
	Expires time.Time
}

// Begin starts an attempt that waits for a person to confirm user until now plus ttl.
func (r PairRequest) Begin(device [32]byte, user UserCode, now time.Time, ttl time.Duration) (Enrollment, error) {
	if err := r.Validate(); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{User: user, Device: device, Req: r, State: EnrollWaiting, Expires: now.Add(ttl)}, nil
}

// Confirm moves a waiting attempt to enrolling for owner. It succeeds at most once per attempt.
func (e Enrollment) Confirm(owner Login, now time.Time) (Enrollment, error) {
	switch {
	case e.State == EnrollWaiting && now.After(e.Expires):
		return e, ErrEnrollExpired
	case e.State != EnrollWaiting:
		return e, ErrEnrollUsed
	}
	e.State, e.Owner = EnrollEnrolling, owner
	return e, nil
}

// Enrolled records the machine id the store wrote. It does nothing unless the attempt is enrolling.
// Pairing again under the same owner and name gets the existing id back, with the key replaced.
func (e Enrollment) Enrolled(id MachineID) Enrollment {
	if e.State == EnrollEnrolling {
		e.State, e.Machine = EnrollEnrolled, id
	}
	return e
}

// Failed returns an enrolling attempt to waiting after the store write failed, so the person can
// confirm again.
func (e Enrollment) Failed() Enrollment {
	if e.State == EnrollEnrolling {
		e.State, e.Owner = EnrollWaiting, ""
	}
	return e
}

// Collect hands out an enrolled attempt exactly once. ok is false while the attempt is waiting or
// enrolling, and after the first collection. A pending attempt that has expired is an error.
func (e Enrollment) Collect(now time.Time) (next Enrollment, ok bool, err error) {
	switch {
	case e.State == EnrollEnrolled:
		e.State = EnrollCollected
		return e, true, nil
	case e.State != EnrollCollected && now.After(e.Expires):
		return e, false, ErrEnrollExpired
	}
	return e, false, nil
}

// Row is the machine row a confirmed attempt creates. The store assigns the id.
func (e Enrollment) Row() Machine {
	return Machine{Name: e.Req.Name, Owner: e.Owner, Role: e.Req.Role, Slots: e.Req.Slots, Exec: e.Req.Exec}
}
