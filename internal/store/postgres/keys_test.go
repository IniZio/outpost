package postgres_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/store/postgres"
)

func sandbox(name string) domain.Machine {
	return domain.Machine{Name: name, Owner: "ana", Role: domain.RoleSandbox, Slots: 2}
}

func TestLookupFindsAPairedMachineAndNotARevokedOne(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, _, err := s.Lookup(ctx, "nope"); !errors.Is(err, domain.ErrUnknownMachine) {
		t.Fatalf("unknown id: %v", err)
	}
	id := addMachine(t, s, sandbox("a"))
	m, key, err := s.Lookup(ctx, id)
	if err != nil || m.ID != id || m.Name != "a" || m.Owner != "ana" || m.Slots != 2 || key == (domain.MachineKey{}) {
		t.Fatalf("lookup: %+v %v %v", m, key, err)
	}
	if ok, err := s.DeleteMachine(ctx, id); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if _, _, err := s.Lookup(ctx, id); !errors.Is(err, domain.ErrUnknownMachine) {
		t.Fatalf("after revoke: %v", err)
	}
	if ok, _ := s.DeleteMachine(ctx, id); ok {
		t.Fatal("deleted a machine twice")
	}
}

func TestLinkGenerationsOrderLinksAndStaleClosesChangeNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id := addMachine(t, s, sandbox("a"))
	if on, since, err := s.Seen(ctx, id); on || !since.IsZero() || err != nil {
		t.Fatalf("never linked: %v %v %v", on, since, err)
	}
	g1, err := s.Linked(ctx, id, "build-1")
	if err != nil || g1 != 1 {
		t.Fatalf("first link: %d %v", g1, err)
	}
	g2, _ := s.Linked(ctx, id, "build-2")
	if g2 != 2 {
		t.Fatalf("second link: %d", g2)
	}
	_, linkedAt, _ := s.Seen(ctx, id)
	unlink(t, s, id, g1)
	on, since, _ := s.Seen(ctx, id)
	if !on || !since.Equal(linkedAt) {
		t.Fatalf("a superseded link's close took the machine offline or moved since: %v %v", on, since)
	}
	unlink(t, s, id, g2)
	on, since, _ = s.Seen(ctx, id)
	if on || since.Before(linkedAt) || !recent(since) {
		t.Fatalf("after the current link closed: %v %v", on, since)
	}
}

func unlink(t *testing.T, s *postgres.Store, id domain.MachineID, gen domain.LinkGen) {
	t.Helper()
	if err := s.Unlinked(context.Background(), id, gen); err != nil {
		t.Fatal(err)
	}
}

func recent(at time.Time) bool {
	now := domain.SystemClock{}.Now()
	return at.After(now.Add(-time.Minute)) && at.Before(now.Add(time.Minute))
}

func TestPresenceOfAnUnknownMachine(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Linked(ctx, "nope", ""); !errors.Is(err, domain.ErrUnknownMachine) {
		t.Fatalf("link of an unknown machine: %v", err)
	}
	if err := s.Unlinked(ctx, "nope", 1); err != nil {
		t.Fatalf("unlink of an unknown machine: %v", err)
	}
	if on, since, err := s.Seen(ctx, "nope"); on || !since.IsZero() || err != nil {
		t.Fatalf("seen: %v %v %v", on, since, err)
	}
}

func TestTwoLinksRacingGetDistinctGenerationsAndOnlyTheLastClosesIt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	id := addMachine(t, s, sandbox("a"))
	const links = 8
	gens := raceLinks(t, s, id, links)
	for i, g := range gens {
		if g != domain.LinkGen(i+1) {
			t.Fatalf("generations %v, want 1..%d", gens, links)
		}
	}
	for _, g := range gens[:links-1] {
		if err := s.Unlinked(ctx, id, g); err != nil {
			t.Fatal(err)
		}
	}
	if on, _, _ := s.Seen(ctx, id); !on {
		t.Fatal("the machine went offline while its newest link was open")
	}
	if err := s.Unlinked(ctx, id, gens[links-1]); err != nil {
		t.Fatal(err)
	}
	if on, _, _ := s.Seen(ctx, id); on {
		t.Fatal("the machine stayed online after its newest link closed")
	}
}

// raceLinks links id n times at once and returns the generations handed out, sorted.
func raceLinks(t *testing.T, s *postgres.Store, id domain.MachineID, n int) []domain.LinkGen {
	gens := make([]domain.LinkGen, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			g, err := s.Linked(context.Background(), id, "b")
			if err != nil {
				t.Error(err)
			}
			gens[i] = g
		})
	}
	wg.Wait()
	slices.Sort(gens)
	return gens
}
