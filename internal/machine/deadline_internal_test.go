package machine

import (
	"context"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

func TestExecTimeoutsStayWithinWhatHCAccepts(t *testing.T) {
	m := &Machine{cfg: Config{Clock: domain.NewManualClock(time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC))}}
	now := m.cfg.Clock.Now()
	for budget, want := range map[time.Duration]float64{40 * time.Minute: maxExec, 90 * time.Second: 90, -time.Second: 1} {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(budget))
		if got := m.deadline(ctx); got != want {
			t.Errorf("a %s budget: %v, want %v", budget, got, want)
		}
		cancel()
	}
	if got := m.deadline(context.Background()); got != 0 {
		t.Errorf("no deadline: %v", got)
	}
}
