package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/store/postgres"
)

func begin(t *testing.T, s *postgres.Store, device byte, code domain.UserCode, name string) domain.Enrollment {
	t.Helper()
	req := domain.PairRequest{Key: pairKey(), Name: name, Role: domain.RoleSandbox, Slots: 2}
	e, err := req.Begin([32]byte{device}, code, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Begin(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAPairingRunsWaitingEnrolledCollected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, postgres.WithClock(domain.NewManualClock(t0)))
	begin(t, s, 1, "AAAA-BBBB", "laptop")
	if _, ok, err := s.Collect(ctx, [32]byte{1}, t0); ok || err != nil {
		t.Fatalf("collect before confirm: %v %v", ok, err)
	}
	done, err := s.Confirm(ctx, "AAAA-BBBB", "ana", t0)
	if err != nil || done.State != domain.EnrollEnrolled || done.Machine == "" || done.Owner != "ana" {
		t.Fatalf("confirm: %+v %v", done, err)
	}
	got, ok, err := s.Collect(ctx, [32]byte{1}, t0)
	if err != nil || !ok || got.Machine != done.Machine || got.State != domain.EnrollCollected {
		t.Fatalf("collect: %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.Collect(ctx, [32]byte{1}, t0); ok || err != nil {
		t.Fatalf("second collect: %v %v", ok, err)
	}
	m, key, err := s.Lookup(ctx, done.Machine)
	if err != nil || m.Name != "laptop" || m.Owner != "ana" || key == (domain.MachineKey{}) {
		t.Fatalf("the machine row: %+v %v", m, err)
	}
	deviceIsStoredHashed(t, s, [32]byte{1})
}

func deviceIsStoredHashed(t *testing.T, s *postgres.Store, device [32]byte) {
	t.Helper()
	var stored []byte
	if err := s.Pool().QueryRow(context.Background(), `SELECT device_hash FROM enrollments`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(device[:]); string(stored) != string(want[:]) {
		t.Fatal("the device secret is not stored as its hash")
	}
}

func TestPairingTheSameNameAgainKeepsTheMachineAndTakesTheNewKey(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, postgres.WithClock(domain.NewManualClock(t0)))
	begin(t, s, 1, "AAAA-BBBB", "laptop")
	first, _ := s.Confirm(ctx, "AAAA-BBBB", "ana", t0)
	_, oldKey, _ := s.Lookup(ctx, first.Machine)
	e := begin(t, s, 2, "CCCC-DDDD", "laptop")
	second, err := s.Confirm(ctx, "CCCC-DDDD", "ana", t0)
	if err != nil || second.Machine != first.Machine {
		t.Fatalf("re-pair: %+v %v", second, err)
	}
	if _, key, _ := s.Lookup(ctx, first.Machine); key != e.Req.Key || key == oldKey {
		t.Fatal("the key was not replaced")
	}
	begin(t, s, 3, "EEEE-FFFF", "laptop")
	other, _ := s.Confirm(ctx, "EEEE-FFFF", "bob", t0)
	if other.Machine == first.Machine {
		t.Fatal("another owner's machine shares an id")
	}
	if all, _ := s.Machines(ctx); len(all) != 2 {
		t.Fatalf("%d machines, want 2", len(all))
	}
}

func TestConfirmErrors(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, postgres.WithClock(domain.NewManualClock(t0)))
	if _, err := s.Confirm(ctx, "ZZZZ-ZZZZ", "ana", t0); !errors.Is(err, domain.ErrUnknownPairing) {
		t.Fatalf("unknown code: %v", err)
	}
	if _, _, err := s.Collect(ctx, [32]byte{9}, t0); !errors.Is(err, domain.ErrUnknownPairing) {
		t.Fatalf("unknown device: %v", err)
	}
	begin(t, s, 1, "AAAA-BBBB", "laptop")
	if _, err := s.Confirm(ctx, "AAAA-BBBB", "ana", t0.Add(2*time.Hour)); !errors.Is(err, domain.ErrEnrollExpired) {
		t.Fatalf("expired: %v", err)
	}
	if all, _ := s.Machines(ctx); len(all) != 0 {
		t.Fatal("an expired code made a machine")
	}
	if _, _, err := s.Collect(ctx, [32]byte{1}, t0.Add(2*time.Hour)); !errors.Is(err, domain.ErrEnrollExpired) {
		t.Fatalf("collect after expiry: %v", err)
	}
	if _, err := s.Confirm(ctx, "AAAA-BBBB", "ana", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, "AAAA-BBBB", "bob", t0); !errors.Is(err, domain.ErrEnrollUsed) {
		t.Fatalf("second confirm: %v", err)
	}
}

func TestAFailedMachineWriteLeavesTheCodeWaiting(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, postgres.WithClock(domain.NewManualClock(t0)))
	begin(t, s, 1, "AAAA-BBBB", "laptop")
	if _, err := s.Pool().Exec(ctx, `ALTER TABLE machines ADD CONSTRAINT no_ana CHECK (owner <> 'ana')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, "AAAA-BBBB", "ana", t0); err == nil {
		t.Fatal("the write should have failed")
	}
	if _, err := s.Pool().Exec(ctx, `ALTER TABLE machines DROP CONSTRAINT no_ana`); err != nil {
		t.Fatal(err)
	}
	if done, err := s.Confirm(ctx, "AAAA-BBBB", "ana", t0); err != nil || done.State != domain.EnrollEnrolled {
		t.Fatalf("confirming again: %+v %v", done, err)
	}
}

func TestConfirmAndCollectHappenOnceUnderConcurrency(t *testing.T) {
	s := newStore(t, postgres.WithClock(domain.NewManualClock(t0)))
	begin(t, s, 1, "AAAA-BBBB", "laptop")
	const racers = 12
	var confirmed, used, collected atomic.Int32
	race(racers, func() { countConfirm(t, s, &confirmed, &used) })
	race(racers, func() { countCollect(t, s, &collected) })
	if confirmed.Load() != 1 || used.Load() != racers-1 || collected.Load() != 1 {
		t.Fatalf("confirmed %d, refused %d, collected %d", confirmed.Load(), used.Load(), collected.Load())
	}
	if all, _ := s.Machines(context.Background()); len(all) != 1 {
		t.Fatalf("%d machines, want 1", len(all))
	}
}

func TestTooManyWaitingPairingsAreRefusedUntilSomeExpire(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewManualClock(t0)
	s := newStore(t, postgres.WithClock(clock))
	codes := []domain.UserCode{}
	for i := range 50 {
		code := domain.UserCode("AAAA-" + string(rune('A'+i/26)) + string(rune('A'+i%26)) + "XX")
		codes = append(codes, code)
		begin(t, s, byte(i+1), code, "m"+string(code))
	}
	req := domain.PairRequest{Key: pairKey(), Name: "one-too-many", Role: domain.RoleSandbox, Slots: 1}
	e, _ := req.Begin([32]byte{200}, "ZZZZ-ZZZZ", t0, time.Hour)
	if err := s.Begin(ctx, e); !errors.Is(err, domain.ErrTooManyPairings) {
		t.Fatalf("got %v, want too many pairings", err)
	}
	if _, err := s.Confirm(ctx, codes[0], "ana", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Begin(ctx, e); err != nil {
		t.Fatalf("a confirmed attempt should free a place: %v", err)
	}
	clock.Advance(3 * time.Hour)
	e2, _ := req.Begin([32]byte{201}, "YYYY-YYYY", clock.Now(), time.Hour)
	if err := s.Begin(ctx, e2); err != nil {
		t.Fatalf("expired attempts should not count: %v", err)
	}
}

func race(n int, f func()) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(f)
	}
	wg.Wait()
}

func countConfirm(t *testing.T, s *postgres.Store, confirmed, used *atomic.Int32) {
	switch _, err := s.Confirm(context.Background(), "AAAA-BBBB", "ana", t0); {
	case err == nil:
		confirmed.Add(1)
	case errors.Is(err, domain.ErrEnrollUsed):
		used.Add(1)
	default:
		t.Error(err)
	}
}

func countCollect(t *testing.T, s *postgres.Store, collected *atomic.Int32) {
	_, ok, err := s.Collect(context.Background(), [32]byte{1}, t0)
	if err != nil {
		t.Error(err)
	}
	if ok {
		collected.Add(1)
	}
}
