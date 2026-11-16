package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

func TestASlotFiresOnceAndAManualFireClaimsNone(t *testing.T) {
	store := portstest.NewStore()
	g := newRig(t, store)
	defer g.close()
	s := domain.Schedule{ID: 4, Owner: "ana", Kind: domain.KindScheduled, Spec: domain.Spec{Repo: "acme/app", Prompt: "audit"}, Enabled: true}
	due := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	first, err := g.r.Fire(g.ctx, s, due)
	if err != nil || first.Spec.Schedule != 4 {
		t.Fatalf("the slot's task: %+v %v", first, err)
	}
	if _, err := g.r.Fire(g.ctx, s, due); !errors.Is(err, ports.ErrFired) {
		t.Fatalf("the same slot again: %v", err)
	}
	for range 2 {
		if _, err := g.r.Fire(g.ctx, s, time.Time{}); err != nil {
			t.Fatalf("a manual fire: %v", err)
		}
	}
	ts, err := store.Tasks(g.ctx, ports.TaskQuery{})
	if err != nil || len(ts) != 3 {
		t.Fatalf("one slot task and two manual ones: %d %v", len(ts), err)
	}
}
