package link

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackoffDelayStaysInsideItsBounds(t *testing.T) {
	b := DefaultBackoff()
	never := time.Duration(1 << 62)
	prev := time.Duration(0)
	for n := 0; n < 40; n++ {
		lo, hi := b.Delay(n, never, 0), b.Delay(n, never, 1)
		if lo <= 0 || hi > b.Max || lo > hi || hi < prev/2 {
			t.Fatalf("attempt %d: %v..%v", n, lo, hi)
		}
		prev = hi
	}
	if got := b.Delay(0, never, 0); got != b.Base/2 {
		t.Fatalf("first wait floor is half the base: %v", got)
	}
	if got := b.Delay(99, never, 0.5); got < b.Max/2 || got > b.Max {
		t.Fatalf("a huge attempt count caps at Max: %v", got)
	}
}

func TestBackoffJitterSpreadsAFleet(t *testing.T) {
	b := DefaultBackoff()
	never := time.Duration(1 << 62)
	seen := map[time.Duration]bool{}
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 0.99} {
		seen[b.Delay(4, never, r)] = true
	}
	if len(seen) != 5 {
		t.Fatalf("jitter must change the wait: %v", seen)
	}
	if b.Delay(4, never, -3) != b.Delay(4, never, 0) || b.Delay(4, never, 7) != b.Delay(4, never, 1) {
		t.Fatal("jitter outside [0,1) is clamped")
	}
}

func TestBackoffCapsShortlyAfterARelink(t *testing.T) {
	b := DefaultBackoff()
	if got := b.Delay(10, time.Second, 1); got > b.RelinkMax {
		t.Fatalf("inside the relink window the cap is RelinkMax: %v", got)
	}
	if got := b.Delay(10, b.RelinkWindow, 1); got <= b.RelinkMax {
		t.Fatalf("outside the window the cap is Max: %v", got)
	}
}

func TestBackoffResetsOnlyAfterAStableLink(t *testing.T) {
	b := DefaultBackoff()
	if b.Next(5, time.Second) != 6 || b.Next(5, b.StableAfter) != 0 {
		t.Fatal("the attempt count resets only after StableAfter")
	}
}

func TestSleepEndsWithItsContext(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
