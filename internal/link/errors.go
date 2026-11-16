package link

import (
	"errors"
	"fmt"
)

// Code classifies a failure that crossed the link. It is part of the wire format, so a code is
// never renumbered and never reused.
type Code string

const (
	CodeUnknownOp  Code = "unknown_op"  // the peer does not serve that op (a version skew)
	CodeBadRequest Code = "bad_request" // undecodable, unknown field, or Validate failed
	CodeForbidden  Code = "forbidden"   // the machine lacks the capability the op needs
	CodeBusy       Code = "busy"        // too many ops in flight on this session
	CodeDraining   Code = "draining"    // the session is retiring; use the newer one
	CodeCancelled  Code = "cancelled"   // the caller gave up
	CodeRefused    Code = "refused"     // the handler declined, with a reason a person can read
	CodeInternal   Code = "internal"    // the handler failed
)

// Error is a failure the peer reported. Handlers return one through Errorf to choose the code.
type Error struct {
	Code Code   `json:"code"`
	Msg  string `json:"msg"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Msg }

// Is makes errors.Is(err, &link.Error{Code: c}) match on the code alone.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Errorf builds an *Error for a handler to return.
func Errorf(c Code, format string, a ...any) error {
	return &Error{Code: c, Msg: fmt.Sprintf(format, a...)}
}

// Sentinels for failures that are about the link and not about the op.
var (
	// ErrOffline: no session to that machine exists, and none arrived within the link wait.
	// Nothing was sent.
	ErrOffline = errors.New("link: machine offline")
	// ErrDraining: this server is shutting down and takes no new ops. The next server process
	// takes the machine over.
	ErrDraining = errors.New("link: server draining")
	// ErrLinkLost: the session died after the request was sent. The op may have run. The package
	// never retries it: whether a replay is safe is the caller's knowledge.
	ErrLinkLost = errors.New("link: session lost mid-operation")
)
