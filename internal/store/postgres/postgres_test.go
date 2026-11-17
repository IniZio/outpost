package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
	"github.com/IniZio/outpost/internal/store/postgres"
)

const urlEnv = "OUTPOST_TEST_DATABASE_URL"

// newDB makes a database of its own for the test, so tests run in parallel and each one's advisory
// locks are its own. The URL must be in the postgres:// form.
func newDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(urlEnv)
	if admin == "" {
		t.Skipf("%s is not set", urlEnv)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "outpost_t_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `DROP DATABASE `+name+` WITH (FORCE)`)
		_ = conn.Close(context.WithoutCancel(ctx))
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func openAt(t *testing.T, dsn string, opts ...postgres.Option) *postgres.Store {
	t.Helper()
	s, err := postgres.Open(context.Background(), dsn, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func newStore(t *testing.T, opts ...postgres.Option) *postgres.Store {
	t.Helper()
	s := openAt(t, newDB(t), opts...)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

var t0 = time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)

func pairKey() domain.MachineKey {
	var k domain.MachineKey
	_, _ = rand.Read(k[:])
	return k
}

// addMachine enrolls a machine through the real pairing path and returns its id.
func addMachine(t *testing.T, s *postgres.Store, m domain.Machine) domain.MachineID {
	t.Helper()
	ctx := context.Background()
	var device [32]byte
	_, _ = rand.Read(device[:])
	req := domain.PairRequest{Key: pairKey(), Name: m.Name, Role: m.Role, Slots: m.Slots, Exec: m.Exec}
	code := domain.UserCode(hex.EncodeToString(device[:4]))
	e, err := req.Begin(device, code, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Begin(ctx, e); err != nil {
		t.Fatal(err)
	}
	done, err := s.Confirm(ctx, code, m.Owner, t0)
	if err != nil {
		t.Fatal(err)
	}
	return done.Machine
}

func TestPostgresMeetsTheStoreContract(t *testing.T) {
	portstest.StoreContract(t, func(t *testing.T) portstest.StoreRig {
		s := newStore(t)
		return portstest.StoreRig{
			Store:     s,
			Schedules: s,
			Setups:    s,
			AddMachine: func(m domain.Machine, online bool) domain.MachineID {
				id := addMachine(t, s, m)
				if online {
					if _, err := s.Linked(context.Background(), id, "test"); err != nil {
						t.Fatal(err)
					}
				}
				return id
			},
			SetRepo: func(r domain.Repo) {
				if err := s.SetRepo(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func TestMigrateIsIdempotentAndSafeToRaceAndRecordsVersions(t *testing.T) {
	ctx := context.Background()
	dsn := newDB(t)
	a, b := openAt(t, dsn), openAt(t, dsn)
	errs := make(chan error, 2)
	for _, s := range []*postgres.Store{a, b} {
		go func() { errs <- s.Migrate(ctx) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var n, max int
	if err := a.Pool().QueryRow(ctx, `SELECT count(*), max(version) FROM schema_migrations`).Scan(&n, &max); err != nil || n < 2 || n != max {
		t.Fatalf("versions: %d rows, max %d, %v", n, max, err)
	}
}

func TestTheLeaseFreesWhenTheHoldersConnectionDies(t *testing.T) {
	ctx := context.Background()
	dsn := newDB(t)
	holder, other := openAt(t, dsn), openAt(t, dsn)
	if err := holder.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Lease(ctx); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		rel, err := other.Lease(ctx)
		if err == nil {
			defer rel()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("the lease was free while the holder lived")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := other.Pool().Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks l
		JOIN pg_stat_activity a USING (pid) WHERE l.locktype = 'advisory' AND l.granted`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lease stayed held after its connection was killed")
	}
}

func TestACommitOfAnUnrestorableRecordWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	bad := domain.Record{ID: 1, Version: 1, Owner: "ana", Kind: domain.KindInteractive, Phase: "nonsense"}
	if _, err := s.Commit(ctx, ports.Commit{Task: bad}); err == nil {
		t.Fatal("committed a corrupt record")
	}
	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows, %v", n, err)
	}
}
