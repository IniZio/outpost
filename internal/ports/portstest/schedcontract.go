package portstest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// contractSchedules checks schedules and their slots: two racing fires of one slot make one task,
// a slot is skipped at most once, and a put replaces by owner and name.
func contractSchedules(t *testing.T, r StoreRig) {
	ctx := context.Background()
	row := ports.ScheduleRow{
		Schedule: domain.Schedule{Owner: "ana", Kind: domain.KindScheduled, Spec: domain.Spec{Repo: "acme/app", Prompt: "audit", Timeout: time.Hour}},
		Name:     "audit", Cron: "0 6 * * 1,3,5", TZ: "Asia/Hong_Kong", Since: contractT0,
	}
	id, err := r.Schedules.PutSchedule(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	due := contractT0.Add(time.Hour)
	won := raceFire(t, r.Store, id, due)
	if err := r.Schedules.Skip(ctx, id, due, due); !errors.Is(err, ports.ErrFired) {
		t.Fatalf("skipping a fired slot: %v", err)
	}
	later := due.Add(48 * time.Hour)
	if err := r.Schedules.Skip(ctx, id, later, later); err != nil {
		t.Fatal(err)
	}
	row.Enabled, row.Spec.Prompt = true, "audit again"
	if again, err := r.Schedules.PutSchedule(ctx, row); err != nil || again != id {
		t.Fatalf("a put of the same name replaces: %d %v", again, err)
	}
	rows, err := r.Schedules.Schedules(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("schedules: %+v %v", rows, err)
	}
	got := rows[0]
	if !got.Enabled || got.Spec.Prompt != "audit again" || got.Spec.Timeout != time.Hour || got.TZ != row.TZ || got.Owner != "ana" ||
		!got.LastDue.Equal(later) || got.LastTask != 0 || !got.LastSkipped.Equal(later) {
		t.Fatalf("schedule row: %+v", got)
	}
	if _, err := r.Store.Load(ctx, won); err != nil {
		t.Fatalf("the winner's task: %v", err)
	}
}

// raceFire commits two tasks for one slot at once and returns the id of the one that won.
func raceFire(t *testing.T, s ports.Store, id domain.ScheduleID, due time.Time) domain.TaskID {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := queuedTask(domain.TaskID(100+i), "ana")
			_, errs[i] = s.Commit(context.Background(), ports.Commit{Task: task.Record(), Fire: &ports.FireRow{Schedule: id, Due: due}})
		}()
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case !errors.Is(err, ports.ErrFired):
			t.Fatalf("racing fires: %v", errs)
		}
	}
	if winner < 0 {
		t.Fatalf("no fire won: %v", errs)
	}
	loser := domain.TaskID(100 + 1 - winner)
	if _, err := s.Load(context.Background(), loser); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("the losing fire left task %d: %v", loser, err)
	}
	return domain.TaskID(100 + winner)
}
