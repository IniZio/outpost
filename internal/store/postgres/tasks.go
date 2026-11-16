package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

const taskColumns = `id, version, owner, kind, machine, created_at, next_ask, agent_run, box_gen,
	spec, phase, status, wake, payload`

// NewTaskID reserves the next id from a sequence. A reserved id that is never committed leaves a gap.
func (s *Store) NewTaskID(ctx context.Context) (domain.TaskID, error) {
	var id domain.TaskID
	if err := s.pool.QueryRow(ctx, `SELECT nextval('task_ids')`).Scan(&id); err != nil {
		return 0, fmt.Errorf("postgres: new task id: %w", err)
	}
	return id, nil
}

// Load returns the task, parsed with domain.Restore.
func (s *Store) Load(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id)
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, fmt.Errorf("task %d: %w", id, ports.ErrNotFound)
	}
	return t, err
}

func scanTask(row pgx.Row) (domain.Task, error) {
	var (
		r    domain.Record
		spec []byte
		wake *time.Time
	)
	if err := row.Scan(&r.ID, &r.Version, &r.Owner, &r.Kind, &r.Machine, &r.CreatedAt, &r.NextAsk,
		&r.AgentRun, &r.BoxGen, &spec, &r.Phase, &r.Status, &wake, &r.Payload); err != nil {
		return domain.Task{}, fmt.Errorf("postgres: scan task: %w", err)
	}
	if err := json.Unmarshal(spec, &r.Spec); err != nil {
		return domain.Task{}, fmt.Errorf("postgres: task %d spec: %w", r.ID, err)
	}
	if wake != nil {
		r.Wake = *wake
	}
	t, err := domain.Restore(r)
	if err != nil {
		return domain.Task{}, fmt.Errorf("postgres: %w", err)
	}
	return t, nil
}

func scanTasks(rows pgx.Rows) ([]domain.Task, error) {
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read tasks: %w", err)
	}
	return out, nil
}

// Tasks lists tasks newest first. A zero field of q does not filter, and a zero Limit is no limit.
func (s *Store) Tasks(ctx context.Context, q ports.TaskQuery) ([]domain.Task, error) {
	limit := any(nil)
	if q.Limit > 0 {
		limit = q.Limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE ($1 = '' OR owner = $1) AND ($2 = 0 OR status = $2) AND ($3 = 0 OR id < $3)
		ORDER BY id DESC LIMIT $4`, string(q.Owner), int16(q.Status), int64(q.Before), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: tasks: %w", err)
	}
	return scanTasks(rows)
}

// Live lists the unfinished tasks in id order.
func (s *Store) Live(ctx context.Context) ([]domain.Task, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE unfinished ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: live: %w", err)
	}
	return scanTasks(rows)
}

// Due lists the tasks whose wake is at or before now, earliest first and then by id.
func (s *Store) Due(ctx context.Context, now time.Time) ([]domain.TaskID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM tasks WHERE wake <= $1 ORDER BY wake, id`, now)
	if err != nil {
		return nil, fmt.Errorf("postgres: due: %w", err)
	}
	defer rows.Close()
	var ids []domain.TaskID
	for rows.Next() {
		var id domain.TaskID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: scan due: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, wrap(rows.Err(), "postgres: read due")
}

// NextWake is the earliest wake of any task.
func (s *Store) NextWake(ctx context.Context) (time.Time, bool, error) {
	var wake *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(wake) FROM tasks`).Scan(&wake); err != nil {
		return time.Time{}, false, fmt.Errorf("postgres: next wake: %w", err)
	}
	if wake == nil {
		return time.Time{}, false, nil
	}
	return *wake, true, nil
}

// Timeline returns a task's entries after the first skip, oldest first.
func (s *Store) Timeline(ctx context.Context, id domain.TaskID, skip int) ([]domain.Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT at, kind, text FROM timeline
		WHERE task_id = $1 AND seq >= $2 ORDER BY seq`, id, max(skip, 0))
	if err != nil {
		return nil, fmt.Errorf("postgres: timeline: %w", err)
	}
	defer rows.Close()
	var out []domain.Entry
	for rows.Next() {
		var e domain.Entry
		if err := rows.Scan(&e.At, &e.Kind, &e.Text); err != nil {
			return nil, fmt.Errorf("postgres: scan entry: %w", err)
		}
		out = append(out, e)
	}
	return out, wrap(rows.Err(), "postgres: read timeline")
}
