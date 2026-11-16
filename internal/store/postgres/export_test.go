package postgres

import "github.com/jackc/pgx/v5/pgxpool"

// Pool lets tests reach the tables.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
