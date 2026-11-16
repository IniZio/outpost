package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// run drives one task through domain.Apply the way the lifecycle will: it holds the task and the
// clock, feeds events in, and keeps what came out. It decides nothing, so a test reads as the
// sequence of events and what Apply made of each.
type run struct {
	t        *testing.T
	task     domain.Task
	now      time.Time
	policy   domain.Policy
	effects  []domain.Effect
	timeline []domain.Entry
	drops    []domain.Drop
}

func newRun(t *testing.T, kind domain.Kind, sp domain.Spec) *run {
	t.Helper()
	return &run{t: t, task: newTask(1, kind, sp), now: t0, policy: domain.DefaultPolicy()}
}

func runOf(t *testing.T, task domain.Task) *run {
	t.Helper()
	return &run{t: t, task: task, now: t0, policy: domain.DefaultPolicy()}
}

// apply feeds one event and fails the test if Apply refuses it.
func (r *run) apply(ev domain.Event) domain.Result {
	r.t.Helper()
	res, err := domain.Apply(r.task, ev, r.now, r.policy)
	if err != nil {
		r.t.Fatalf("Apply(%T) refused: %v", ev, err)
	}
	r.keep(res)
	return res
}

// refused feeds one event that Apply must refuse, and checks the task did not change.
func (r *run) refused(ev domain.Event) error {
	r.t.Helper()
	before := r.task.Record()
	res, err := domain.Apply(r.task, ev, r.now, r.policy)
	if err == nil {
		r.t.Fatalf("Apply(%T) = %v, want a refusal", ev, res.Dropped)
	}
	if after := r.task.Record(); fmt.Sprint(before) != fmt.Sprint(after) {
		r.t.Fatalf("a refused %T changed the task", ev)
	}
	return fmt.Errorf("apply %T: %w", ev, err)
}

func (r *run) keep(res domain.Result) {
	r.task = res.Task
	r.drops = append(r.drops, res.Dropped)
	r.effects = append(r.effects, res.Effects...)
	r.timeline = append(r.timeline, res.Timeline...)
}

// dropped feeds one event and requires Apply to ignore it, with the task untouched.
func (r *run) dropped(ev domain.Event, why domain.Drop) {
	r.t.Helper()
	before := r.task
	res := r.apply(ev)
	if res.Dropped != why {
		r.t.Fatalf("Apply(%T) dropped = %v, want %v", ev, res.Dropped, why)
	}
	if len(res.Effects) != 0 || len(res.Timeline) != 0 || res.Task.Version != before.Version {
		r.t.Fatalf("a dropped %T still produced output: %+v", ev, res)
	}
}

func (r *run) advance(d time.Duration) { r.now = r.now.Add(d) }

// advanceTo moves the clock and feeds Tick for every deadline that falls due on the way, as the
// lifecycle's one wake-up per task does. It returns the effects those ticks produced.
func (r *run) advanceTo(d time.Duration) []domain.Effect {
	r.t.Helper()
	target := r.now.Add(d)
	var out []domain.Effect
	for {
		at, ok := r.task.NextDeadline()
		if !ok || at.After(target) {
			break
		}
		if at.After(r.now) {
			r.now = at
		}
		res := r.apply(domain.Tick{})
		if res.Dropped != domain.Applied {
			r.t.Fatalf("Tick at %v dropped: %v", at, res.Dropped)
		}
		out = append(out, res.Effects...)
	}
	r.now = target
	return out
}

func (r *run) want(s domain.Status) {
	r.t.Helper()
	if got := r.task.Status(); got != s {
		r.t.Fatalf("status = %v, want %v", got, s)
	}
}

// up takes a queued task through SlotGranted and Started, so it is Active with a pending turn.
func (r *run) up() *run {
	r.t.Helper()
	r.apply(domain.SlotGranted{Machine: "m1"})
	r.apply(domain.Started{Ask: latest[domain.StartTask](r).Ask})
	return r
}

// report feeds an hc report from the current agent run.
func (r *run) report(s domain.HCStatus, phase string) domain.Result {
	r.t.Helper()
	return r.apply(domain.AgentReport{Run: r.task.AgentRun, Status: s, Phase: phase})
}

// latest returns the most recent effect of type T the run produced.
func latest[T domain.Effect](r *run) T {
	r.t.Helper()
	for i := len(r.effects) - 1; i >= 0; i-- {
		if fx, ok := r.effects[i].(T); ok {
			return fx
		}
	}
	var zero T
	r.t.Fatalf("no %T effect was emitted", zero)
	return zero
}

// kinds names a list of effects, so a test can compare them as one string.
func kinds(fx []domain.Effect) string {
	names := make([]string, len(fx))
	for i, e := range fx {
		names[i] = strings.TrimPrefix(fmt.Sprintf("%T", e), "domain.")
	}
	return strings.Join(names, ",")
}

// sent lists the text of every SendToAgent in the list.
func sent(fx []domain.Effect) []string {
	var out []string
	for _, e := range fx {
		if s, ok := e.(domain.SendToAgent); ok {
			out = append(out, s.Text)
		}
	}
	return out
}

// has reports whether any effect in the list is of type T.
func has[T domain.Effect](fx []domain.Effect) bool {
	for _, e := range fx {
		if _, ok := e.(T); ok {
			return true
		}
	}
	return false
}

func mustRefuse(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func lastEntry(r *run) domain.Entry {
	r.t.Helper()
	if len(r.timeline) == 0 {
		r.t.Fatal("the timeline is empty")
	}
	return r.timeline[len(r.timeline)-1]
}
