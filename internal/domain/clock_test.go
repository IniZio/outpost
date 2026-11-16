package domain_test

import (
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

var (
	_ domain.Clock = domain.SystemClock{}
	_ domain.Clock = (*domain.ManualClock)(nil)
)

func TestManualClockMovesOnlyWhenAdvanced(t *testing.T) {
	start := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	c := domain.NewManualClock(start)

	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}

	c.Advance(90 * time.Minute)

	if got, want := c.Now(), start.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", got, want)
	}
}

func TestManualClockIsSafeAcrossGoroutines(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	c := domain.NewManualClock(start)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			c.Advance(time.Second)
			_ = c.Now()
		})
	}
	wg.Wait()

	if got, want := c.Now(), start.Add(100*time.Second); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}
