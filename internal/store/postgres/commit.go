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

// Commit writes c in one transaction. The task row's version is the compare-and-swap token: an
// insert that finds the id taken, or an update whose version is not c.Prev, writes nothing and is
// ErrConflict, and it wins over a record whose version does not follow Prev, as in the fake. The record is restored first, so a record domain.Restore rejects never reaches a row.
func (s *Store) Commit(ctx context.Context, c ports.Commit) ([]ports.OutboxID, error) {
	task, err := domain.Restore(c.Task)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	var ids []ports.OutboxID
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		first, err := writeTask(ctx, tx, c, task.Unfinished(), len(c.Entries))
		if err != nil {
			return err
		}
		if c.Task.Version != c.Prev+1 {
			return fmt.Errorf("postgres: record version %d does not follow %d", c.Task.Version, c.Prev)
		}
		ids, err = writeRows(ctx, tx, c, first)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// writeRows writes what a commit adds beside the task row.
func writeRows(ctx context.Context, tx pgx.Tx, c ports.Commit, first int) ([]ports.OutboxID, error) {
	if err := insertEntries(ctx, tx, c, first); err != nil {
		return nil, err
	}
	ids, err := insertOutbox(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if err := insertFire(ctx, tx, c); err != nil {
		return nil, err
	}
	return ids, upsertTemplate(ctx, tx, c.Template)
}

// writeTask inserts or updates the task row and returns the sequence number of the first new entry.
func writeTask(ctx context.Context, tx pgx.Tx, c ports.Commit, unfinished bool, added int) (int, error) {
	r := c.Task
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return 0, fmt.Errorf("postgres: spec: %w", err)
	}
	var wake *time.Time
	if !r.Wake.IsZero() {
		wake = &r.Wake
	}
	fields := []any{
		r.ID, r.Version, string(r.Owner), r.Kind, string(r.Machine), r.CreatedAt, r.NextAsk,
		r.AgentRun, r.BoxGen, spec, string(r.Phase), r.Status, wake, unfinished, r.Payload, added,
	}
	var total int
	if c.Prev == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO tasks (id, version, owner, kind, machine, created_at, next_ask,
			agent_run, box_gen, spec, phase, status, wake, unfinished, payload, entries)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (id) DO NOTHING RETURNING entries`, fields...).Scan(&total)
	} else {
		err = tx.QueryRow(ctx, `UPDATE tasks SET version=$2, owner=$3, kind=$4, machine=$5, created_at=$6,
			next_ask=$7, agent_run=$8, box_gen=$9, spec=$10, phase=$11, status=$12, wake=$13,
			unfinished=$14, payload=$15, entries=entries+$16
			WHERE id=$1 AND version=$17 RETURNING entries`, append(fields, c.Prev)...).Scan(&total)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ports.ErrConflict
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: write task: %w", err)
	}
	return total - added, nil
}

func insertEntries(ctx context.Context, tx pgx.Tx, c ports.Commit, first int) error {
	if len(c.Entries) == 0 {
		return nil
	}
	var b pgx.Batch
	for i, e := range c.Entries {
		b.Queue(`INSERT INTO timeline (task_id, seq, at, kind, text) VALUES ($1,$2,$3,$4,$5)`,
			c.Task.ID, first+i, e.At, string(e.Kind), e.Text)
	}
	return runBatch(ctx, tx, &b, "entries")
}

func insertOutbox(ctx context.Context, tx pgx.Tx, c ports.Commit) ([]ports.OutboxID, error) {
	if len(c.Outbox) == 0 {
		return nil, nil
	}
	var b pgx.Batch
	for _, row := range c.Outbox {
		t := row.Target
		b.Queue(`INSERT INTO outbox (task_id, kind, machine, run, gen, text) VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id`, t.Task, row.Kind, string(t.Machine), t.Run, t.Gen, row.Text)
	}
	br := tx.SendBatch(ctx, &b)
	ids := make([]ports.OutboxID, len(c.Outbox))
	for i := range ids {
		if err := br.QueryRow().Scan(&ids[i]); err != nil {
			_ = br.Close()
			return nil, fmt.Errorf("postgres: outbox: %w", err)
		}
	}
	return ids, wrap(br.Close(), "postgres: outbox")
}

func runBatch(ctx context.Context, tx pgx.Tx, b *pgx.Batch, what string) error {
	br := tx.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("postgres: %s: %w", what, err)
		}
	}
	return wrap(br.Close(), "postgres: "+what)
}

func upsertTemplate(ctx context.Context, tx pgx.Tx, t *ports.TemplateRow) error {
	if t == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO repos (name, ready_image, ready_on, ready_at, notes) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (name) DO UPDATE SET ready_image = $2, ready_on = $3, ready_at = $4,
			notes = CASE WHEN $5 = '' THEN repos.notes ELSE $5 END`,
		string(t.Repo), t.Template.Handle, string(t.Template.Machine), t.At, t.Notes)
	return wrap(err, "postgres: template")
}

// Pending lists the outbox rows not yet marked done, in id order.
func (s *Store) Pending(ctx context.Context) ([]ports.OutboxRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, task_id, machine, run, gen, text FROM outbox
		WHERE NOT done ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: pending: %w", err)
	}
	defer rows.Close()
	var out []ports.OutboxRow
	for rows.Next() {
		var r ports.OutboxRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Target.Task, &r.Target.Machine, &r.Target.Run, &r.Target.Gen, &r.Text); err != nil {
			return nil, fmt.Errorf("postgres: scan outbox: %w", err)
		}
		out = append(out, r)
	}
	return out, wrap(rows.Err(), "postgres: read outbox")
}

// Done marks an outbox row as run. Marking a row twice, or an unknown row, is not an error.
func (s *Store) Done(ctx context.Context, id ports.OutboxID) error {
	_, err := s.pool.Exec(ctx, `UPDATE outbox SET done = true WHERE id = $1`, id)
	return wrap(err, "postgres: done")
}
