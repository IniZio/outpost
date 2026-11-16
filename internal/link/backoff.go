package link

import (
	"context"
	"fmt"
	"time"
)

// Sleeper waits d or until ctx ends. Tests pass one that records d and returns at once.
type Sleeper func(ctx context.Context, d time.Duration) error

// sleep is the real Sleeper.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("link: sleep: %w", ctx.Err())
	}
}

// Backoff is the reconnect schedule of an agent, and of Hold. It is pure: no clock and no
// randomness of its own.
type Backoff struct {
	Base, Max time.Duration
	// StableAfter is how long a link must stay up before the attempt count resets. Resetting on
	// "connected once" lets a server that accepts and immediately drops (a duplicate agent being
	// replaced, a crash loop) pin the agent at one dial per Base.
	StableAfter time.Duration
	// RelinkMax caps the wait for RelinkWindow after the server asked for a relink, because a
	// deploy is then in progress and the replacement is seconds away.
	RelinkMax, RelinkWindow time.Duration
}

// DefaultBackoff is 1 s growing to 60 s, reset after a link has held for 30 s. For 120 s after a
// relink the cap is 2 s.
func DefaultBackoff() Backoff {
	return Backoff{
		Base: time.Second, Max: time.Minute, StableAfter: 30 * time.Second,
		RelinkMax: 2 * time.Second, RelinkWindow: 120 * time.Second,
	}
}

// Delay is the wait before attempt n (0-based). sinceRelink is the time since the server last asked
// for a relink, or a large value if it never did. It uses equal jitter: half the capped exponential
// plus a random share of the other half, r in [0,1), so a fleet that lost its server together does
// not come back together and no wait collapses toward zero.
func (b Backoff) Delay(attempt int, sinceRelink time.Duration, r float64) time.Duration {
	ceiling := b.Max
	if sinceRelink < b.RelinkWindow && b.RelinkMax < ceiling {
		ceiling = b.RelinkMax
	}
	d := ceiling
	if attempt < 31 && b.Base<<attempt > 0 && b.Base<<attempt < ceiling {
		d = b.Base << attempt
	}
	half := d / 2
	return half + time.Duration(min(max(r, 0), 0.999999)*float64(half))
}

// Next is the attempt count after a link that was up for upFor.
func (b Backoff) Next(attempt int, upFor time.Duration) int {
	if upFor >= b.StableAfter {
		return 0
	}
	return attempt + 1
}
