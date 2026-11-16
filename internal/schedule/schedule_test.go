package schedule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

type rig struct {
	t     *testing.T
	clock *domain.ManualClock
	store *portstest.Store
	svc   *Service
}

// fire stands in for lifecycle.Runner.Fire: it commits the slot's task and claims the slot together.
func fire(store *portstest.Store, clock domain.Clock) Fire {
	return func(ctx context.Context, s domain.Schedule, due time.Time) (domain.Task, error) {
		id, err := store.NewTaskID(ctx)
		if err != nil {
			return domain.Task{}, fmt.Errorf("id: %w", err)
		}
		res, err := s.Fire(id, clock.Now())
		if err != nil {
			return domain.Task{}, fmt.Errorf("fire: %w", err)
		}
		c := ports.Commit{Task: res.Task.Record()}
		if !due.IsZero() {
			c.Fire = &ports.FireRow{Schedule: s.ID, Due: due}
		}
		if _, err := store.Commit(ctx, c); err != nil {
			return domain.Task{}, fmt.Errorf("commit: %w", err)
		}
		return res.Task, nil
	}
}

func newRig(t *testing.T, at string) *rig {
	clock := domain.NewManualClock(must(time.Parse(time.RFC3339, at)))
	store := portstest.NewStore()
	r := &rig{t: t, clock: clock, store: store}
	r.svc = r.service()
	return r
}

func (r *rig) service() *Service {
	return New(Config{Store: r.store, Fire: fire(r.store, r.clock), Clock: r.clock})
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var audit = Input{
	Cron: "0 6 * * *", TZ: "UTC", Enabled: true,
	Spec: domain.Spec{Repo: "acme/app", Prompt: "audit the dependencies", Timeout: 2 * time.Hour},
}

func (r *rig) put(name string, in Input) Entry {
	r.t.Helper()
	e, err := r.svc.Put(context.Background(), "ana", name, in)
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

func (r *rig) at(s string) {
	r.t.Helper()
	to := must(time.Parse(time.RFC3339, s))
	r.clock.Advance(to.Sub(r.clock.Now()))
}

func (r *rig) tasks() []domain.Task {
	r.t.Helper()
	ts, err := r.store.Tasks(context.Background(), ports.TaskQuery{})
	if err != nil {
		r.t.Fatal(err)
	}
	return ts
}

func (r *rig) entry(name string) Entry {
	r.t.Helper()
	e, err := r.svc.Get(context.Background(), "ana", name)
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

func TestASlotFiresOnceEvenWithTwoServers(t *testing.T) {
	r := newRig(t, "2026-11-18T05:00:00Z")
	if e := r.put("audit", audit); !e.Next.Equal(must(time.Parse(time.RFC3339, "2026-11-18T06:00:00Z"))) {
		t.Fatalf("next slot: %v", e.Next)
	}
	r.svc.Pass(context.Background())
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("nothing is due before 06:00: %d tasks", n)
	}
	r.at("2026-11-18T06:00:20Z")
	other := r.service()
	var wg sync.WaitGroup
	for _, svc := range []*Service{r.svc, other, r.svc} {
		wg.Add(1)
		go func() { defer wg.Done(); svc.Pass(context.Background()) }()
	}
	wg.Wait()
	ts := r.tasks()
	if len(ts) != 1 || ts[0].Kind != domain.KindScheduled || ts[0].Spec.Schedule == 0 || ts[0].Spec.Timeout != 2*time.Hour {
		t.Fatalf("one scheduled task: %+v", ts)
	}
	if e := r.entry("audit"); e.LastTask != ts[0].ID || !e.Next.Equal(must(time.Parse(time.RFC3339, "2026-11-19T06:00:00Z"))) {
		t.Fatalf("bookkeeping: %+v", e)
	}
}

func TestEnablingLateDoesNotFireAPastSlot(t *testing.T) {
	r := newRig(t, "2026-11-18T05:00:00Z")
	off := audit
	off.Enabled = false
	r.put("audit", off)
	r.at("2026-11-18T06:30:00Z")
	r.svc.Pass(context.Background())
	if _, err := r.svc.SetEnabled(context.Background(), "ana", "audit", true); err != nil {
		t.Fatal(err)
	}
	r.svc.Pass(context.Background())
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("the 06:00 slot passed before the schedule was on: %d tasks", n)
	}
	r.at("2026-11-19T06:00:01Z")
	r.svc.Pass(context.Background())
	if n := len(r.tasks()); n != 1 {
		t.Fatalf("the next slot fires: %d tasks", n)
	}
}

func TestASlotMissedPastTheCatchUpIsSkippedAndShown(t *testing.T) {
	r := newRig(t, "2026-11-18T05:00:00Z")
	r.put("audit", audit)
	r.at("2026-11-18T07:30:00Z")
	r.svc.Pass(context.Background())
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("a slot 90 minutes late is skipped: %d tasks", n)
	}
	if e := r.entry("audit"); !e.LastSkipped.Equal(must(time.Parse(time.RFC3339, "2026-11-18T06:00:00Z"))) {
		t.Fatalf("the skip is recorded: %+v", e)
	}
	r.at("2026-11-19T06:45:00Z")
	r.svc.Pass(context.Background())
	if n := len(r.tasks()); n != 1 {
		t.Fatalf("a slot 45 minutes late still fires: %d tasks", n)
	}
}

func TestAManualFireClaimsNoSlot(t *testing.T) {
	r := newRig(t, "2026-11-18T05:59:00Z")
	r.put("audit", audit)
	r.at("2026-11-18T06:00:00Z")
	if _, err := r.svc.FireNow(context.Background(), "ana", "audit"); err != nil {
		t.Fatal(err)
	}
	r.svc.Pass(context.Background())
	if n := len(r.tasks()); n != 2 {
		t.Fatalf("the manual run and the slot's run: %d tasks", n)
	}
	if _, err := r.svc.FireNow(context.Background(), "ana", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown schedule: %v", err)
	}
}

func TestSlotsFollowTheScheduleTimeZone(t *testing.T) {
	r := newRig(t, "2026-11-17T21:00:00Z")
	hk := audit
	hk.Cron, hk.TZ = "0 6 * * 1,3,5", "Asia/Hong_Kong"
	if e := r.put("audit", hk); !e.Next.Equal(must(time.Parse(time.RFC3339, "2026-11-17T22:00:00Z"))) {
		t.Fatalf("06:00 on Wednesday in Hong Kong is 22:00 UTC on Tuesday: %v", e.Next)
	}
}

func TestPutRefusesWhatCannotRun(t *testing.T) {
	r := newRig(t, "2026-11-18T05:00:00Z")
	for name, in := range map[string]Input{
		"bad-cron": {Cron: "every day", TZ: "UTC", Spec: audit.Spec},
		"bad-zone": {Cron: "0 6 * * *", TZ: "Mars/Olympus", Spec: audit.Spec},
		"no-zone":  {Cron: "0 6 * * *", Spec: audit.Spec},
		"tz-rule":  {Cron: "CRON_TZ=UTC 0 6 * * *", TZ: "UTC", Spec: audit.Spec},
		"no-repo":  {Cron: "0 6 * * *", TZ: "UTC", Spec: domain.Spec{Prompt: "x"}},
		"a/b":      audit,
	} {
		if _, err := r.svc.Put(context.Background(), "ana", name, in); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRunWaitsForTheLeaseAndFiresWhenDue(t *testing.T) {
	r := newRig(t, "2026-11-18T05:00:00Z")
	r.put("audit", audit)
	r.at("2026-11-18T06:00:05Z")
	svc := New(Config{Store: r.store, Fire: fire(r.store, r.clock), Clock: r.clock, Idle: time.Millisecond})
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx, ready) }()
	time.Sleep(20 * time.Millisecond)
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("nothing fires before the lease: %d tasks", n)
	}
	close(ready)
	for range 500 {
		if len(r.tasks()) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || len(r.tasks()) != 1 {
		t.Fatalf("run: %v, %d tasks", err, len(r.tasks()))
	}
}
