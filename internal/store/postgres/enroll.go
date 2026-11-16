package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/domain"
)

// maxWaitingPairings bounds the codes that wait for a person at once, so an unauthenticated
// endpoint cannot fill the table. An attempt past its expiry no longer counts.
const maxWaitingPairings = 50

// keepEnrollments is how long a row stays after it expires, so a late poll still finds it.
const keepEnrollments = 24 * time.Hour

// Begin stores a waiting attempt. Only a hash of the device secret is kept. It is
// domain.ErrTooManyPairings when maxWaitingPairings unexpired attempts already wait.
func (s *Store) Begin(ctx context.Context, e domain.Enrollment) error {
	req, err := json.Marshal(e.Req)
	if err != nil {
		return fmt.Errorf("postgres: pair request: %w", err)
	}
	now := s.clock.Now()
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, enrollKey); err != nil {
			return fmt.Errorf("postgres: lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM enrollments WHERE expires < $1`, now.Add(-keepEnrollments)); err != nil {
			return fmt.Errorf("postgres: prune pairings: %w", err)
		}
		var waiting int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM enrollments WHERE state = $1 AND expires > $2`,
			domain.EnrollWaiting, now).Scan(&waiting); err != nil {
			return fmt.Errorf("postgres: count pairings: %w", err)
		}
		if waiting >= maxWaitingPairings {
			return domain.ErrTooManyPairings
		}
		hash := sha256.Sum256(e.Device[:])
		_, err := tx.Exec(ctx, `INSERT INTO enrollments (user_code, device_hash, req, state, expires)
			VALUES ($1,$2,$3,$4,$5)`, string(e.User), hash[:], req, e.State, e.Expires)
		return wrap(err, "postgres: begin pairing")
	})
}

// enrollKey serialises Begin's count-then-insert across server processes.
const enrollKey int64 = 0x656e726f6c6c // "enroll"

// Confirm runs domain.Enrollment.Confirm on the attempt for code under a row lock, writes the machine
// row and runs Enrolled, all in one transaction. A failed write rolls back, which leaves the attempt
// waiting as Failed would, so the person can confirm again. The returned Enrollment has no Device.
func (s *Store) Confirm(ctx context.Context, code domain.UserCode, owner domain.Login, now time.Time) (domain.Enrollment, error) {
	var out domain.Enrollment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		e, err := lockEnrollment(ctx, tx, `user_code = $1`, string(code))
		if err != nil {
			return err
		}
		if e, err = e.Confirm(owner, now); err != nil {
			out = e
			return fmt.Errorf("postgres: confirm: %w", err)
		}
		var id domain.MachineID
		row := e.Row()
		err = tx.QueryRow(ctx, `INSERT INTO machines (name, owner, role, slots, exec, key)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (owner, name) DO UPDATE SET role=$3, slots=$4, exec=$5, key=$6
			RETURNING id`, row.Name, string(row.Owner), row.Role, row.Slots, row.Exec, e.Req.Key[:]).Scan(&id)
		if err != nil {
			return fmt.Errorf("postgres: write machine: %w", err)
		}
		out = e.Enrolled(id)
		return saveEnrollment(ctx, tx, out)
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// Collect runs domain.Enrollment.Collect on the attempt for the device secret under a row lock.
// Two polls racing for an enrolled attempt get ok from exactly one of them.
func (s *Store) Collect(ctx context.Context, device [32]byte, now time.Time) (domain.Enrollment, bool, error) {
	hash := sha256.Sum256(device[:])
	var (
		out domain.Enrollment
		ok  bool
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		e, err := lockEnrollment(ctx, tx, `device_hash = $1`, hash[:])
		if err != nil {
			return err
		}
		e.Device = device
		if out, ok, err = e.Collect(now); err != nil {
			return fmt.Errorf("postgres: collect: %w", err)
		}
		if !ok {
			return nil
		}
		return saveEnrollment(ctx, tx, out)
	})
	return out, ok && err == nil, err
}

func lockEnrollment(ctx context.Context, tx pgx.Tx, where string, arg any) (domain.Enrollment, error) {
	var (
		e    domain.Enrollment
		req  []byte
		user string
	)
	err := tx.QueryRow(ctx, `SELECT user_code, req, state, owner, machine, expires FROM enrollments
		WHERE `+where+` FOR UPDATE`, arg).Scan(&user, &req, &e.State, &e.Owner, &e.Machine, &e.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Enrollment{}, domain.ErrUnknownPairing
	}
	if err != nil {
		return domain.Enrollment{}, fmt.Errorf("postgres: read pairing: %w", err)
	}
	e.User = domain.UserCode(user)
	if err := json.Unmarshal(req, &e.Req); err != nil {
		return domain.Enrollment{}, fmt.Errorf("postgres: pair request: %w", err)
	}
	return e, nil
}

func saveEnrollment(ctx context.Context, tx pgx.Tx, e domain.Enrollment) error {
	_, err := tx.Exec(ctx, `UPDATE enrollments SET state=$2, owner=$3, machine=$4 WHERE user_code=$1`,
		string(e.User), e.State, string(e.Owner), string(e.Machine))
	return wrap(err, "postgres: save pairing")
}
