package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.SecretRows = (*Store)(nil)

// PutSecret writes a sealed secret, replacing one of the same owner and name.
func (s *Store) PutSecret(ctx context.Context, x ports.SealedSecret) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO secrets (owner, name, sealed, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner, name) DO UPDATE SET sealed = EXCLUDED.sealed, updated_at = EXCLUDED.updated_at`,
		string(x.Owner), x.Name, x.Sealed, x.Updated)
	return wrap(err, "postgres: put secret")
}

// Secret returns one sealed secret.
func (s *Store) Secret(ctx context.Context, owner domain.Login, name string) (ports.SealedSecret, error) {
	x := ports.SealedSecret{Owner: owner, Name: name}
	err := s.pool.QueryRow(ctx, `SELECT sealed, updated_at FROM secrets WHERE owner = $1 AND name = $2`,
		string(owner), name).Scan(&x.Sealed, &x.Updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.SealedSecret{}, ports.ErrNotFound
	}
	if err != nil {
		return ports.SealedSecret{}, fmt.Errorf("postgres: secret: %w", err)
	}
	return x, nil
}

// SecretList returns the owner's sealed secrets by name.
func (s *Store) SecretList(ctx context.Context, owner domain.Login) ([]ports.SealedSecret, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, sealed, updated_at FROM secrets WHERE owner = $1 ORDER BY name`, string(owner))
	if err != nil {
		return nil, fmt.Errorf("postgres: secrets: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ports.SealedSecret, error) {
		x := ports.SealedSecret{Owner: owner}
		return x, r.Scan(&x.Name, &x.Sealed, &x.Updated)
	})
	return out, wrap(err, "postgres: secrets")
}

// DeleteSecret removes a sealed secret. A missing one is no error.
func (s *Store) DeleteSecret(ctx context.Context, owner domain.Login, name string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM secrets WHERE owner = $1 AND name = $2`, string(owner), name)
	return wrap(err, "postgres: delete secret")
}
