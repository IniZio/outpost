package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// ErrNotLeading is returned while this process does not hold the lease, as during the overlap of a
// deploy. The API answers 503 and the client retries.
var ErrNotLeading = errors.New("lifecycle: this server is not driving tasks right now")

// ErrNotFound is returned for an unknown task.
var ErrNotFound = ports.ErrNotFound

// ErrNotRunning is a screen asked of a box that cannot be reached right now.
var ErrNotRunning = ports.ErrNotRunning

// Budgets bound each effect. They are mechanism, not rules: they guarantee that every ask gets its
// answer and that a stuck machine cannot hold a lane. A zero field takes the default. Start and
// Template sit above the domain's StartBudget and SaveBudget, so the domain decides when a start or
// a save has taken too long and the budget only backstops a lost tick.
type Budgets struct {
	Command  time.Duration
	Release  time.Duration
	Start    time.Duration
	Ensure   time.Duration
	Probe    time.Duration
	Summary  time.Duration
	Template time.Duration
	Observe  time.Duration
}

// DefaultBudgets are the production bounds.
func DefaultBudgets() Budgets {
	return Budgets{
		Command: 30 * time.Second, Release: 5 * time.Minute, Start: 40 * time.Minute,
		Ensure: 10 * time.Minute, Probe: 30 * time.Second, Summary: 2 * time.Minute,
		Template: 50 * time.Minute, Observe: 15 * time.Second,
	}
}

func (b Budgets) filled() Budgets {
	d := DefaultBudgets()
	for _, f := range []struct{ got, def *time.Duration }{
		{&b.Command, &d.Command},
		{&b.Release, &d.Release},
		{&b.Start, &d.Start},
		{&b.Ensure, &d.Ensure},
		{&b.Probe, &d.Probe},
		{&b.Summary, &d.Summary},
		{&b.Template, &d.Template},
		{&b.Observe, &d.Observe},
	} {
		if *f.got == 0 {
			*f.got = *f.def
		}
	}
	return b
}

// Config is everything a Runner needs. cmd/outpost builds it once.
type Config struct {
	Store     ports.Store
	Boxes     ports.Boxes
	Agent     ports.Agent
	Secrets   ports.Secrets
	Clock     domain.Clock
	Policy    domain.Policy
	BaseImage string

	// Capacity is read on every scheduler pass, so a hold while box images build takes effect at
	// once.
	Capacity func() domain.Capacity

	Budgets Budgets

	// PollEvery is the longest the wake loop and the scheduler loop sleep. Zero takes a minute.
	PollEvery time.Duration

	// Retry is the wait before each retry of a failed store write. The last value repeats. Nil
	// takes one, five and thirty seconds.
	Retry []time.Duration

	// After is how the loops wait. Nil takes time.After. A test passes a channel that is ready.
	After func(time.Duration) <-chan time.Time

	Log *slog.Logger
}

func (c Config) filled() (Config, error) {
	switch {
	case c.Store == nil || c.Boxes == nil || c.Agent == nil || c.Secrets == nil:
		return c, errors.New("lifecycle: Store, Boxes, Agent and Secrets are required")
	case c.Clock == nil || c.Capacity == nil:
		return c, errors.New("lifecycle: Clock and Capacity are required")
	}
	c.Budgets = c.Budgets.filled()
	if c.PollEvery == 0 {
		c.PollEvery = time.Minute
	}
	if len(c.Retry) == 0 {
		c.Retry = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	}
	if c.After == nil {
		c.After = time.After
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// Runner drives every task. There is one per server process, and only the one holding the lease
// applies events.
type Runner struct {
	cfg  Config
	lead atomic.Bool

	mu       sync.Mutex
	cells    map[domain.TaskID]*cell
	idle     []chan struct{}
	watchers map[domain.TaskID]map[chan struct{}]struct{}
	poke     chan struct{}
	nudge    chan struct{}
	armed    atomic.Int64
	feeds    map[domain.MachineID]bool
	waiting  map[domain.TaskID]struct{}
	schedMu  sync.Mutex
	started  chan struct{}
	bg       sync.WaitGroup

	base   context.Context
	jobCtx context.Context
	stop   context.CancelCauseFunc
}

// New checks cfg and fills its defaults.
func New(cfg Config) (*Runner, error) {
	cfg, err := cfg.filled()
	if err != nil {
		return nil, err
	}
	jobCtx, stop := context.WithCancelCause(context.Background())
	return &Runner{
		cfg:      cfg,
		cells:    map[domain.TaskID]*cell{},
		watchers: map[domain.TaskID]map[chan struct{}]struct{}{},
		poke:     make(chan struct{}, 1),
		nudge:    make(chan struct{}, 1),
		feeds:    map[domain.MachineID]bool{},
		waiting:  map[domain.TaskID]struct{}{},
		started:  make(chan struct{}),
		base:     context.Background(),
		jobCtx:   jobCtx,
		stop:     stop,
	}, nil
}

// quiesce waits until no step runs and no lane or job holds a task. Run calls it when draining,
// and in-package tests call it after each action. A job blocked in a fake's gate keeps it waiting,
// so a test of a slow start waits on the fake's entry signal instead.
func (r *Runner) quiesce(ctx context.Context) error {
	r.mu.Lock()
	if len(r.cells) == 0 {
		r.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	r.idle = append(r.idle, ch)
	r.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("lifecycle: tasks still busy: %w", ctx.Err())
	}
}

// retry runs fn until it succeeds, the process stops leading, ctx ends, or attempts runs out (zero
// means no limit), waiting out the configured backoff between tries.
func (r *Runner) retry(ctx context.Context, attempts int, what string, fn func() error) error {
	var err error
	for i := 0; attempts == 0 || i < attempts; i++ {
		if err = fn(); err == nil || errors.Is(err, ErrNotLeading) {
			return err
		}
		r.cfg.Log.WarnContext(ctx, "retrying", "what", what, "attempt", i+1, "err", err)
		wait := r.cfg.Retry[min(i, len(r.cfg.Retry)-1)]
		select {
		case <-r.cfg.After(wait):
		case <-ctx.Done():
			return err
		}
	}
	return err
}
