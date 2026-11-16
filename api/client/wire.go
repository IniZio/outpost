// Package client is the Go client for Outpost's HTTP API, and the API's wire types. The server's
// internal/api encodes these same types, so the two cannot drift.
package client

import (
	"fmt"
	"time"
)

// Task is a task as the API shows it. Access is "watch" or "control".
type Task struct {
	ID        int64     `json:"id"`
	Owner     string    `json:"owner"`
	Kind      string    `json:"kind"`
	Repo      string    `json:"repo"`
	Also      []string  `json:"also,omitempty"`
	Title     string    `json:"title,omitempty"`
	Prompt    string    `json:"prompt"`
	Status    string    `json:"status"`
	Phase     string    `json:"phase,omitempty"`
	Machine   string    `json:"machine,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Access    string    `json:"access"`
}

// Ended reports whether the task's status is final: done, failed or stopped.
func (t Task) Ended() bool {
	return t.Status == "done" || t.Status == "failed" || t.Status == "stopped"
}

// NewTask starts a task.
type NewTask struct {
	Repo       string   `json:"repo"`
	Also       []string `json:"also,omitempty"`
	Prompt     string   `json:"prompt"`
	Title      string   `json:"title,omitempty"`
	TimeoutMin int      `json:"timeout_min,omitempty"`
}

// Entry is one timeline entry. Seq counts from 1 and is the SSE event id.
type Entry struct {
	Seq  int       `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// Text is a message to a task's agent, or a secret's value.
type Text struct {
	Text string `json:"text"`
}

// Machine is a paired machine.
type Machine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Role   string `json:"role"`
	Slots  int    `json:"slots"`
	Exec   bool   `json:"exec"`
	Online bool   `json:"online"`
}

// PairRequest is what an agent sends to start pairing. Key is its ed25519 public key.
type PairRequest struct {
	Key   []byte `json:"key"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Slots int    `json:"slots"`
	Exec  bool   `json:"exec,omitempty"`
}

// PairTicket is the answer: the code a person confirms, and the device secret the agent collects
// with.
type PairTicket struct {
	UserCode    string    `json:"user_code"`
	DeviceCode  []byte    `json:"device_code"`
	IntervalSec int       `json:"interval_sec"`
	Expires     time.Time `json:"expires"`
}

// Collect asks whether a pairing was confirmed.
type Collect struct {
	DeviceCode []byte `json:"device_code"`
}

// Collected is "pending" until a person confirms, then "enrolled" with the machine's id.
type Collected struct {
	State     string `json:"state"`
	MachineID string `json:"machine_id,omitempty"`
}

// Confirm confirms a pairing code for the caller.
type Confirm struct {
	UserCode string `json:"user_code"`
}

// Repo is a repo Outpost works on.
type Repo struct {
	Name       string     `json:"name"`
	ReadyImage string     `json:"ready_image"`
	ReadyOn    string     `json:"ready_on"`
	ReadyAt    *time.Time `json:"ready_at,omitempty"`
	Notes      string     `json:"notes"`
}

// SignIn trades a GitHub token for an API token.
type SignIn struct {
	GitHubToken string `json:"github_token"`
}

// Session is a new API token and its login.
type Session struct {
	Token string `json:"token"`
	Login string `json:"login"`
}

// WebLink is a one-time sign-in link for a browser that only watches.
type WebLink struct {
	URL string `json:"url"`
}

// Me is who the caller is. Act is false for a watch-only web session.
type Me struct {
	Login string `json:"login"`
	Act   bool   `json:"act"`
}

// Status is the server's state, without secrets.
type Status struct {
	Leading bool `json:"leading"`
}

// Error is the API's one error shape.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message) }

// Schedule is a recurring task. Cron has five fields, read in TZ. The Last fields and Next are
// read-only: the server fills them.
type Schedule struct {
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	TZ          string     `json:"tz"`
	Repo        string     `json:"repo"`
	Also        []string   `json:"also,omitempty"`
	Prompt      string     `json:"prompt"`
	Title       string     `json:"title,omitempty"`
	TimeoutMin  int        `json:"timeout_min,omitempty"`
	Enabled     bool       `json:"enabled"`
	Next        *time.Time `json:"next,omitempty"`
	LastDue     *time.Time `json:"last_due,omitempty"`
	LastTask    int64      `json:"last_task,omitempty"`
	LastSkipped *time.Time `json:"last_skipped,omitempty"`
}
