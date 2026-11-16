package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Sessions = (*Store)(nil)

// PutSession stores a session.
func (s *Store) PutSession(ctx context.Context, x ports.Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (hash, login, kind, name, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		x.Hash, string(x.Login), int16(x.Kind), x.Name, x.Expires)
	return wrap(err, "postgres: put session")
}

// Session returns the unexpired, unspent session for hash.
func (s *Store) Session(ctx context.Context, hash []byte, now time.Time) (ports.Session, error) {
	return s.oneSession(ctx, `SELECT hash, login, kind, name, expires_at FROM sessions
		WHERE hash = $1 AND expires_at > $2 AND spent_at IS NULL`, hash, now)
}

// Spend marks a link spent and returns it. Only one caller wins a race.
func (s *Store) Spend(ctx context.Context, hash []byte, now time.Time) (ports.Session, error) {
	return s.oneSession(ctx, `UPDATE sessions SET spent_at = $2
		WHERE hash = $1 AND kind = `+fmt.Sprint(int(ports.SessionLink))+` AND expires_at > $2 AND spent_at IS NULL
		RETURNING hash, login, kind, name, expires_at`, hash, now)
}

// DeleteSession removes a session. A missing one is no error.
func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE hash = $1`, hash)
	return wrap(err, "postgres: delete session")
}

func (s *Store) oneSession(ctx context.Context, q string, hash []byte, now time.Time) (ports.Session, error) {
	var (
		x    ports.Session
		kind int16
	)
	err := s.pool.QueryRow(ctx, q, hash, now).Scan(&x.Hash, &x.Login, &kind, &x.Name, &x.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.Session{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.Session{}, fmt.Errorf("postgres: session: %w", err)
	}
	x.Kind = ports.SessionKind(kind)
	return x, nil
}
