// Package schedule fires cron schedules. A schedule's slots are the instants its cron rule names
// in its time zone. Only the process that drives tasks fires, and the store's slot key makes each
// slot fire at most once, even across a deploy's overlap. A slot missed by more than the catch-up
// window is recorded as skipped instead, so a 06:00 audit does not start at 14:00 after an outage.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // schedules name zones; the server image may have no zoneinfo

	"github.com/robfig/cron/v3"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Row is ports.ScheduleRow under schedule's name, because internal/api may not import ports.
type Row = ports.ScheduleRow

// ErrNotFound is a schedule name the owner does not have.
var ErrNotFound = errors.New("schedule: no such schedule")

// Fire creates the task of one slot: lifecycle.Runner.Fire. A zero due is a manual firing.
type Fire func(ctx context.Context, s domain.Schedule, due time.Time) (domain.Task, error)

// Config wires a Service.
type Config struct {
	Store ports.Schedules
	Fire  Fire
	Clock domain.Clock
	// Catchup is how late a slot may still fire. Default one hour.
	Catchup time.Duration
	// Idle is the longest pause between passes, so another process's edits are seen. Default one
	// minute.
	Idle time.Duration
	Log  *slog.Logger
}

// Service keeps and fires schedules.
type Service struct {
	cfg  Config
	poke chan struct{}
}

// New returns a service. Run fires; the other methods work without it.
func New(cfg Config) *Service {
	if cfg.Clock == nil {
		cfg.Clock = domain.SystemClock{}
	}
	if cfg.Catchup == 0 {
		cfg.Catchup = time.Hour
	}
	if cfg.Idle == 0 {
		cfg.Idle = time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg, poke: make(chan struct{}, 1)}
}

// Entry is a schedule as people see it, with its next slot when it is enabled.
type Entry struct {
	Row
	Next time.Time
}

// Input is what a person sets on a schedule.
type Input struct {
	Cron    string
	TZ      string
	Spec    domain.Spec
	Enabled bool
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// parse reads a five-field cron rule in zone tz.
func parse(expr, tz string) (cron.Schedule, error) {
	if strings.Contains(expr, "TZ=") {
		return nil, fmt.Errorf("%w: put the time zone in tz, not in the rule", domain.ErrInvalid)
	}
	if _, err := time.LoadLocation(tz); err != nil || tz == "" {
		return nil, fmt.Errorf("%w: unknown time zone %q", domain.ErrInvalid, tz)
	}
	rule, err := cron.ParseStandard("CRON_TZ=" + tz + " " + expr)
	if err != nil {
		return nil, fmt.Errorf("%w: cron rule %q: %w", domain.ErrInvalid, expr, err)
	}
	return rule, nil
}

// Put creates or replaces the owner's schedule of that name. No slot at or before now fires.
func (s *Service) Put(ctx context.Context, owner domain.Login, name string, in Input) (Entry, error) {
	if !validName.MatchString(name) {
		return Entry{}, fmt.Errorf("%w: a schedule name is letters, digits, '.', '_' and '-'", domain.ErrInvalid)
	}
	if in.Spec.Repo == "" || in.Spec.Prompt == "" {
		return Entry{}, fmt.Errorf("%w: a schedule needs a repo and a prompt", domain.ErrInvalid)
	}
	if _, err := parse(in.Cron, in.TZ); err != nil {
		return Entry{}, err
	}
	in.Spec.Schedule = 0
	row := Row{
		Schedule: domain.Schedule{Owner: owner, Kind: domain.KindScheduled, Spec: in.Spec, Enabled: in.Enabled},
		Name:     name, Cron: in.Cron, TZ: in.TZ, Since: s.cfg.Clock.Now(),
	}
	return s.store(ctx, row)
}

func (s *Service) store(ctx context.Context, row Row) (Entry, error) {
	id, err := s.cfg.Store.PutSchedule(ctx, row)
	if err != nil {
		return Entry{}, fmt.Errorf("schedule: put %s: %w", row.Name, err)
	}
	row.ID = id
	s.Poke()
	return s.entry(row), nil
}

func (s *Service) entry(row Row) Entry {
	e := Entry{Row: row}
	if rule, err := parse(row.Cron, row.TZ); err == nil && row.Enabled {
		e.Next = rule.Next(s.cfg.Clock.Now())
	}
	return e
}

// List returns the owner's schedules.
func (s *Service) List(ctx context.Context, owner domain.Login) ([]Entry, error) {
	rows, err := s.cfg.Store.Schedules(ctx)
	if err != nil {
		return nil, fmt.Errorf("schedule: list: %w", err)
	}
	var out []Entry
	for _, row := range rows {
		if row.Owner == owner {
			out = append(out, s.entry(row))
		}
	}
	return out, nil
}

// Get returns the owner's schedule of that name.
func (s *Service) Get(ctx context.Context, owner domain.Login, name string) (Entry, error) {
	all, err := s.List(ctx, owner)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range all {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// SetEnabled turns a schedule on or off. Turning it on fires no slot that is already past.
func (s *Service) SetEnabled(ctx context.Context, owner domain.Login, name string, on bool) (Entry, error) {
	e, err := s.Get(ctx, owner, name)
	if err != nil || e.Enabled == on {
		return e, err
	}
	e.Enabled = on
	if on {
		e.Since = s.cfg.Clock.Now()
	}
	return s.store(ctx, e.Row)
}

// FireNow starts the schedule's task now. It claims no slot, so it neither blocks nor is blocked
// by the cron.
func (s *Service) FireNow(ctx context.Context, owner domain.Login, name string) (domain.Task, error) {
	e, err := s.Get(ctx, owner, name)
	if err != nil {
		return domain.Task{}, err
	}
	t, err := s.cfg.Fire(ctx, e.Schedule, time.Time{})
	if err != nil {
		return domain.Task{}, fmt.Errorf("schedule: fire %s: %w", name, err)
	}
	return t, nil
}

// Poke makes Run pass again now, after a schedule changed.
func (s *Service) Poke() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Run waits for ready, which is closed once this process drives tasks, then passes over the
// schedules until ctx ends: at each due slot, on a Poke, and at least every Idle.
func (s *Service) Run(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
	case <-ctx.Done():
		return nil
	}
	for {
		wait := max(min(s.Pass(ctx).Sub(s.cfg.Clock.Now()), s.cfg.Idle), 0)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		case <-s.poke:
			t.Stop()
		}
	}
}

// Pass fires or skips the latest unhandled slot of every enabled schedule and returns when the
// next slot of any of them is due.
func (s *Service) Pass(ctx context.Context) time.Time {
	now := s.cfg.Clock.Now()
	next := now.Add(s.cfg.Idle)
	rows, err := s.cfg.Store.Schedules(ctx)
	if err != nil {
		s.cfg.Log.WarnContext(ctx, "schedules: list", "err", err)
		return next
	}
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		rule, err := parse(row.Cron, row.TZ)
		if err != nil {
			s.cfg.Log.WarnContext(ctx, "schedules: bad rule", "schedule", row.Name, "err", err)
			continue
		}
		s.settle(ctx, row, rule, now)
		if n := rule.Next(now); n.Before(next) {
			next = n
		}
	}
	return next
}

// settle handles the latest slot after the schedule's floor that is due by now.
func (s *Service) settle(ctx context.Context, row Row, rule cron.Schedule, now time.Time) {
	due, ok := latest(rule, laterOf(row.Since, row.LastDue), now)
	if !ok {
		return
	}
	log := s.cfg.Log.With("schedule", row.Name, "due", due)
	if now.Sub(due) > s.cfg.Catchup {
		if err := s.cfg.Store.Skip(ctx, row.ID, due, now); err != nil && !errors.Is(err, ports.ErrFired) {
			log.WarnContext(ctx, "schedules: record a skipped slot", "err", err)
			return
		}
		log.InfoContext(ctx, "schedules: skipped a slot missed by more than the catch-up window")
		return
	}
	t, err := s.cfg.Fire(ctx, row.Schedule, due)
	switch {
	case errors.Is(err, ports.ErrFired):
	case err != nil:
		log.WarnContext(ctx, "schedules: fire", "err", err)
	default:
		log.InfoContext(ctx, "schedules: fired", "task", t.ID)
	}
}

// maxSlots bounds the walk from a schedule's floor to now.
const maxSlots = 100_000

// latest is the last slot after floor and at or before now.
func latest(rule cron.Schedule, floor, now time.Time) (time.Time, bool) {
	var due time.Time
	for t, n := rule.Next(floor), 0; !t.IsZero() && !t.After(now) && n < maxSlots; t, n = rule.Next(t), n+1 {
		due = t
	}
	return due, !due.IsZero()
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
