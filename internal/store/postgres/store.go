package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IniZio/outpost/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Advisory lock keys: one for the lease that makes a process the driver of tasks, one that
// serialises concurrent migrators. They are arbitrary and only need to differ.
const closeTimeout = 5 * time.Second

const (
	leaseKey   int64 = 0x6f7574706f7374 // "outpost"
	migrateKey int64 = 0x6d696772617465 // "migrate"
)

// Store is the server's durable state on Postgres. It implements ports.Store and ports.Enrollments,
// and link.KeyStore and link.Presence structurally, so it imports none of link.
type Store struct {
	pool  *pgxpool.Pool
	clock domain.Clock
}

// Option adjusts a Store.
type Option func(*Store)

// WithClock sets the clock that judges pairing expiry. The default is the system clock. Link times
// are always the database's own clock, so every server process agrees on them.
func WithClock(c domain.Clock) Option { return func(s *Store) { s.clock = c } }

// Open connects to dsn. It does not migrate: call Migrate once at start.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	s := &Store{pool: pool, clock: domain.SystemClock{}}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close closes the pool. A held lease is released with its connection.
func (s *Store) Close() { s.pool.Close() }

// Migrate applies, in file order, every migration not yet recorded in schema_migrations. Each file
// runs in its own transaction under an advisory lock, so servers starting together migrate once.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("postgres: list migrations: %w", err)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := s.apply(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, name string) error {
	version, err := versionOf(name)
	if err != nil {
		return err
	}
	body, err := migrationFiles.ReadFile(name)
	if err != nil {
		return fmt.Errorf("postgres: read %s: %w", name, err)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error { return runMigration(ctx, tx, name, version, body) })
}

func runMigration(ctx context.Context, tx pgx.Tx, name string, version int, body []byte) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateKey); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("version table: %w", err)
	}
	seen, err := recorded(ctx, tx, version)
	if err != nil || seen {
		return err
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
	return wrap(err, "record version")
}

func recorded(ctx context.Context, tx pgx.Tx, version int) (bool, error) {
	var seen bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&seen)
	return seen, wrap(err, "check version")
}

// versionOf reads the number a migration file starts with, as in migrations/0001_init.sql.
func versionOf(name string) (int, error) {
	base := name[strings.LastIndex(name, "/")+1:]
	v, _, _ := strings.Cut(base, "_")
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("postgres: migration %s has no numeric prefix: %w", name, err)
	}
	return n, nil
}

// inTx runs f in a transaction and commits when it returns nil.
func (s *Store) inTx(ctx context.Context, f func(pgx.Tx) error) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = f(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// Lease holds a session-level advisory lock on a connection of its own, taken out of the pool. A
// process that dies, or whose connection drops, lets go because Postgres ends the session. release
// closes the connection, which is what drops the lock, so a lock-holding connection never goes back
// to the pool.
func (s *Store) Lease(ctx context.Context) (func(), error) {
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: lease: %w", err)
	}
	conn := pc.Hijack()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, leaseKey); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("postgres: lease: %w", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-stop:
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}, nil
}
