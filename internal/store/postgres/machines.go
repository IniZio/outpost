package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Machines lists every machine row, by id, with its online flag.
func (s *Store) Machines(ctx context.Context) ([]ports.MachineState, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, owner, role, slots, exec, online FROM machines ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: machines: %w", err)
	}
	defer rows.Close()
	var out []ports.MachineState
	for rows.Next() {
		var m ports.MachineState
		d := &m.Machine
		if err := rows.Scan(&d.ID, &d.Name, &d.Owner, &d.Role, &d.Slots, &d.Exec, &m.Online); err != nil {
			return nil, fmt.Errorf("postgres: scan machine: %w", err)
		}
		out = append(out, m)
	}
	return out, wrap(rows.Err(), "postgres: read machines")
}

// DeleteMachine revokes a machine: its next handshake finds no row. It reports whether a row existed.
// The caller kicks a live link with Hub.Kick.
func (s *Store) DeleteMachine(ctx context.Context, id domain.MachineID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM machines WHERE id = $1`, string(id))
	if err != nil {
		return false, fmt.Errorf("postgres: delete machine: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Lookup is link.KeyStore. An unknown or revoked id is domain.ErrUnknownMachine.
func (s *Store) Lookup(ctx context.Context, id domain.MachineID) (domain.Machine, domain.MachineKey, error) {
	var (
		m   domain.Machine
		key []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT id, name, owner, role, slots, exec, key FROM machines WHERE id = $1`,
		string(id)).Scan(&m.ID, &m.Name, &m.Owner, &m.Role, &m.Slots, &m.Exec, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Machine{}, domain.MachineKey{}, domain.ErrUnknownMachine
	}
	if err != nil {
		return domain.Machine{}, domain.MachineKey{}, fmt.Errorf("postgres: lookup: %w", err)
	}
	var k domain.MachineKey
	if copy(k[:], key) != len(k) || len(key) != len(k) {
		return domain.Machine{}, domain.MachineKey{}, fmt.Errorf("postgres: machine %s: %w: key is %d bytes", id, domain.ErrCorrupt, len(key))
	}
	return m, k, nil
}

// Linked is link.Presence. It bumps the generation and marks the machine online in one statement,
// so two links racing for a machine get distinct generations in the order Postgres serialised them.
func (s *Store) Linked(ctx context.Context, id domain.MachineID, build string) (domain.LinkGen, error) {
	var gen domain.LinkGen
	err := s.pool.QueryRow(ctx, `UPDATE machines SET link_gen = link_gen + 1, online = true,
		linked_at = now(), build = $2 WHERE id = $1 RETURNING link_gen`, string(id), build).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrUnknownMachine
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: linked: %w", err)
	}
	return gen, nil
}

// Unlinked is link.Presence. It marks the machine offline only while gen is the stored generation,
// so the close of a superseded link changes nothing. An unknown machine is not an error.
func (s *Store) Unlinked(ctx context.Context, id domain.MachineID, gen domain.LinkGen) error {
	_, err := s.pool.Exec(ctx, `UPDATE machines SET online = false, linked_at = now()
		WHERE id = $1 AND link_gen = $2`, string(id), gen)
	return wrap(err, "postgres: unlinked")
}

// Seen is link.Presence. since is the database's time of the last Linked, or of the last applied
// Unlinked. A machine that never linked, or is unknown, is (false, zero time).
func (s *Store) Seen(ctx context.Context, id domain.MachineID) (bool, time.Time, error) {
	var (
		online bool
		at     *time.Time
	)
	err := s.pool.QueryRow(ctx, `SELECT online, linked_at FROM machines WHERE id = $1`, string(id)).Scan(&online, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("postgres: seen: %w", err)
	}
	if at == nil {
		return online, time.Time{}, nil
	}
	return online, *at, nil
}

// Repo returns the repo row, or the zero Repo with only Name set when none exists.
func (s *Store) Repo(ctx context.Context, name domain.RepoName) (domain.Repo, error) {
	r := domain.Repo{Name: name}
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT ready_image, ready_on, ready_at, notes, setup_task
		FROM repos WHERE name = $1`, string(name)).Scan(&r.ReadyImage, &r.ReadyOn, &at, &r.Notes, &r.SetupTask)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return domain.Repo{}, fmt.Errorf("postgres: repo: %w", err)
	}
	if at != nil {
		r.ReadyAt = *at
	}
	return r, nil
}

// SetRepo adds or replaces a repo row.
func (s *Store) SetRepo(ctx context.Context, r domain.Repo) error {
	var at *time.Time
	if !r.ReadyAt.IsZero() {
		at = &r.ReadyAt
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO repos (name, ready_image, ready_on, ready_at, notes, setup_task)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (name) DO UPDATE SET ready_image=$2, ready_on=$3,
		ready_at=$4, notes=$5, setup_task=$6`,
		string(r.Name), r.ReadyImage, string(r.ReadyOn), at, r.Notes, r.SetupTask)
	return wrap(err, "postgres: set repo")
}

// Repos lists every repo row, by name.
func (s *Store) Repos(ctx context.Context) ([]domain.Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, ready_image, ready_on, ready_at, notes, setup_task FROM repos ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("postgres: repos: %w", err)
	}
	defer rows.Close()
	var out []domain.Repo
	for rows.Next() {
		var (
			r  domain.Repo
			at *time.Time
		)
		if err := rows.Scan(&r.Name, &r.ReadyImage, &r.ReadyOn, &at, &r.Notes, &r.SetupTask); err != nil {
			return nil, fmt.Errorf("postgres: scan repo: %w", err)
		}
		if at != nil {
			r.ReadyAt = *at
		}
		out = append(out, r)
	}
	return out, wrap(rows.Err(), "postgres: read repos")
}

// MarkAllOffline clears every online flag. The server calls it at start, before any agent can link,
// because a flag left by a process that died is stale. It bumps no link generation, so a late
// Unlinked from the dead process still matches nothing newer.
func (s *Store) MarkAllOffline(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE machines SET online = false, linked_at = now() WHERE online`)
	return wrap(err, "postgres: mark machines offline")
}
