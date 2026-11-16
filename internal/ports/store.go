package ports

import (
	"context"
	"errors"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// ErrConflict is what Commit returns when the stored version is not Commit.Prev.
var ErrConflict = errors.New("store: task changed underneath")

// ErrNotFound is what Load returns for an unknown task.
var ErrNotFound = errors.New("store: no such task")

// RowKind is one of the effects that is done once and not answered. Only these go through the
// outbox. An effect with an Ask is recorded in the task itself, and Recovered re-issues it, so the
// task stays the single record of what is outstanding.
type RowKind uint8

const (
	RowType RowKind = iota + 1
	RowInterrupt
	RowKill
	RowClearDone
	RowStopBox
	RowRelease
)

// OutboxID orders a task's outbox rows. The store hands it out in Commit.
type OutboxID int64

// OutboxRow is one effect to carry out once. Text is set for RowType only. It is flat so the
// store keeps plain columns and never decodes a domain sum type.
type OutboxRow struct {
	ID     OutboxID
	Kind   RowKind
	Target Target
	Text   string
}

// TemplateRow records a saved ready template on its repo. It rides in the Commit that applies the
// TemplateSaved answer, so the repo row changes only when the domain accepted the save: a save that
// finishes after the finish window timed it out changes nothing.
type TemplateRow struct {
	Repo     domain.RepoName
	Template Template
	At       time.Time
}

// Commit is one applied event, written in one transaction: the task as a Record, its new timeline
// entries, the outbox rows its effects produced in order, and a template row when the event was an
// accepted save. Prev is the version the event was applied to. Zero inserts a new task.
type Commit struct {
	Prev     uint64
	Task     domain.Record
	Entries  []domain.Entry
	Outbox   []OutboxRow
	Template *TemplateRow
}

// MachineState is a machine row and whether it is linked now, from link.Presence's online flag.
type MachineState struct {
	Machine domain.Machine
	Online  bool
}

// TaskQuery selects tasks for the API's lists. A zero field does not filter.
type TaskQuery struct {
	Owner  domain.Login
	Status domain.Status
	Before domain.TaskID
	Limit  int
}

// Store is the server's durable state as lifecycle needs it. internal/store/postgres implements
// it. Tasks are rows of domain.Record, and the wake index is an index on Record.Wake.
type Store interface {
	// Lease blocks until this process is the one that drives tasks, and holds that until ctx
	// ends or the returned release is called. In Postgres it is an advisory lock on a dedicated
	// connection, so a process that dies lets go.
	Lease(ctx context.Context) (release func(), err error)

	// NewTaskID reserves an id for domain.New.
	NewTaskID(ctx context.Context) (domain.TaskID, error)

	// Load returns the task, parsed with domain.Restore.
	Load(ctx context.Context, id domain.TaskID) (domain.Task, error)

	// Commit writes c atomically, compare-and-swap on c.Prev, and returns the ids it gave the
	// outbox rows, in order. A version mismatch is ErrConflict and writes nothing.
	Commit(ctx context.Context, c Commit) ([]OutboxID, error)

	// Tasks lists tasks for the API, newest first.
	Tasks(ctx context.Context, q TaskQuery) ([]domain.Task, error)

	// Timeline returns a task's entries after the first skip, oldest first.
	Timeline(ctx context.Context, id domain.TaskID, skip int) ([]domain.Entry, error)

	// Live lists every task for which Task.Unfinished is true, in id order: those not yet ended,
	// and ended ones still waiting for a wake's answer. The scheduler and the restart read it.
	Live(ctx context.Context) ([]domain.Task, error)

	// Due lists the tasks whose wake is set and at or before now, earliest first.
	Due(ctx context.Context, now time.Time) ([]domain.TaskID, error)

	// NextWake is the earliest wake of any task, and false when no task has one.
	NextWake(ctx context.Context) (time.Time, bool, error)

	// Pending lists the outbox rows not yet marked done, in id order.
	Pending(ctx context.Context) ([]OutboxRow, error)

	// Done marks an outbox row as run.
	Done(ctx context.Context, id OutboxID) error

	// Machines lists every machine row with its online flag.
	Machines(ctx context.Context) ([]MachineState, error)

	// Repo returns the repo row, the zero Repo with only Name set when none exists.
	Repo(ctx context.Context, name domain.RepoName) (domain.Repo, error)
}
