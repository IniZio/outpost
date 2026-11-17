package ports

import (
	"context"
	"errors"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// ErrFired is what Commit and Skip return when the schedule's slot already has a row: another
// process, or an earlier pass, fired or skipped it.
var ErrFired = errors.New("store: that schedule slot already fired")

// FireRow claims one slot of a schedule. It rides in the Commit that inserts the slot's task, so a
// slot fires at most once and a crash before the commit claims nothing.
type FireRow struct {
	Schedule domain.ScheduleID
	Due      time.Time
}

// ScheduleRow is a schedule as stored: the domain part, its name and cron rule, and its slot
// bookkeeping. Since is when it was last written or enabled; no slot at or before it fires. The
// Last fields are read-only: the store fills them from the slots.
type ScheduleRow struct {
	domain.Schedule
	Name  string
	Cron  string
	TZ    string
	Since time.Time

	LastDue     time.Time     // the latest slot fired or skipped
	LastTask    domain.TaskID // the task of the latest slot, zero when it was skipped
	LastSkipped time.Time     // the latest skipped slot
}

// Schedules stores schedules and their skipped slots. Fired slots go through Commit.
type Schedules interface {
	// PutSchedule inserts or replaces the schedule of that owner and name and returns its id.
	PutSchedule(ctx context.Context, s ScheduleRow) (domain.ScheduleID, error)

	// Schedules lists every schedule in id order.
	Schedules(ctx context.Context) ([]ScheduleRow, error)

	// DeleteSchedule removes the owner's schedule of that name with its slots, and reports whether
	// it existed. Tasks it already fired stay.
	DeleteSchedule(ctx context.Context, owner domain.Login, name string) (bool, error)

	// Skip records a slot that was not fired. A slot that already has a row is ErrFired.
	Skip(ctx context.Context, id domain.ScheduleID, due, at time.Time) error
}
