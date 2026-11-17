package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Schedules = (*Store)(nil)

// PutSchedule inserts or replaces the schedule of that owner and name.
func (s *Store) PutSchedule(ctx context.Context, row ports.ScheduleRow) (domain.ScheduleID, error) {
	spec, err := json.Marshal(row.Spec)
	if err != nil {
		return 0, fmt.Errorf("postgres: schedule spec: %w", err)
	}
	var id domain.ScheduleID
	err = s.pool.QueryRow(ctx, `INSERT INTO schedules (owner, name, kind, cron, tz, spec, enabled, since)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (owner, name) DO UPDATE SET kind=$3, cron=$4, tz=$5, spec=$6, enabled=$7, since=$8
		RETURNING id`, string(row.Owner), row.Name, row.Kind, row.Cron, row.TZ, spec, row.Enabled, row.Since).Scan(&id)
	return id, wrap(err, "postgres: put schedule")
}

// Schedules lists every schedule with its latest slot, the task of that slot, and its latest
// skipped slot.
func (s *Store) Schedules(ctx context.Context) ([]ports.ScheduleRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id, s.owner, s.name, s.kind, s.cron, s.tz, s.spec, s.enabled, s.since,
		last.due, last.task_id, (SELECT max(due) FROM schedule_fires f WHERE f.schedule_id = s.id AND f.task_id = 0)
		FROM schedules s LEFT JOIN LATERAL (SELECT due, task_id FROM schedule_fires f
			WHERE f.schedule_id = s.id ORDER BY due DESC LIMIT 1) last ON true
		ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: schedules: %w", err)
	}
	defer rows.Close()
	var out []ports.ScheduleRow
	for rows.Next() {
		row, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, wrap(rows.Err(), "postgres: read schedules")
}

func scanSchedule(rows pgx.Rows) (ports.ScheduleRow, error) {
	var (
		r            ports.ScheduleRow
		owner        string
		spec         []byte
		due, skipped *time.Time
		task         *int64
	)
	if err := rows.Scan(&r.ID, &owner, &r.Name, &r.Kind, &r.Cron, &r.TZ, &spec, &r.Enabled, &r.Since, &due, &task, &skipped); err != nil {
		return r, fmt.Errorf("postgres: scan schedule: %w", err)
	}
	if err := json.Unmarshal(spec, &r.Spec); err != nil {
		return r, fmt.Errorf("postgres: schedule %d spec: %w", r.ID, err)
	}
	r.Owner = domain.Login(owner)
	if due != nil {
		r.LastDue, r.LastTask = *due, domain.TaskID(*task)
	}
	if skipped != nil {
		r.LastSkipped = *skipped
	}
	return r, nil
}

// Skip records a slot that was not fired.
func (s *Store) Skip(ctx context.Context, id domain.ScheduleID, due, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO schedule_fires (schedule_id, due, task_id, at) VALUES ($1,$2,0,$3)`, id, due, at)
	return slotErr(err, "postgres: skip")
}

func insertFire(ctx context.Context, tx pgx.Tx, c ports.Commit) error {
	if c.Fire == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO schedule_fires (schedule_id, due, task_id, at) VALUES ($1,$2,$3,$4)`,
		c.Fire.Schedule, c.Fire.Due, c.Task.ID, c.Task.CreatedAt)
	return slotErr(err, "postgres: fire")
}

// slotErr turns a duplicate slot key into ErrFired.
func slotErr(err error, what string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ports.ErrFired
	}
	return wrap(err, what)
}

// DeleteSchedule removes the owner's schedule of that name. Its slots go with it.
func (s *Store) DeleteSchedule(ctx context.Context, owner domain.Login, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM schedules WHERE owner=$1 AND name=$2`, string(owner), name)
	return tag.RowsAffected() > 0, wrap(err, "postgres: delete schedule")
}
