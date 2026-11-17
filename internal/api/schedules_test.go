package api_test

import (
	"errors"
	"testing"

	"github.com/IniZio/outpost/api/client"
)

var auditSchedule = client.Schedule{
	Name: "audit", Cron: "0 6 * * 1,3,5", TZ: "Asia/Hong_Kong", Repo: "acme/app", Also: []string{"acme/lib"},
	Prompt: "audit", TimeoutMin: 120,
}

func TestSchedulesArePutSwitchedAndFired(t *testing.T) {
	g := newRig(t)
	got, err := g.owner.PutSchedule(ctx(t), auditSchedule)
	if err != nil || got.Enabled || got.Next != nil || got.TimeoutMin != 120 || len(got.Also) != 1 {
		t.Fatalf("put: %+v %v", got, err)
	}
	if got, err = g.owner.SetScheduleEnabled(ctx(t), "audit", true); err != nil || !got.Enabled || got.Next == nil {
		t.Fatalf("enable: %+v %v", got, err)
	}
	task, err := g.owner.FireSchedule(ctx(t), "audit")
	if err != nil || task.Kind != "scheduled" || task.Repo != "acme/app" {
		t.Fatalf("fire: %+v %v", task, err)
	}
	if got, err = g.owner.SetScheduleEnabled(ctx(t), "audit", false); err != nil || got.Enabled {
		t.Fatalf("disable: %+v %v", got, err)
	}
}

func TestSchedulesAreTheOwnersAndChecked(t *testing.T) {
	g := newRig(t)
	if _, err := g.owner.PutSchedule(ctx(t), auditSchedule); err != nil {
		t.Fatal(err)
	}
	if ss, err := g.owner.Schedules(ctx(t)); err != nil || len(ss) != 1 || ss[0].Name != "audit" {
		t.Fatalf("ls: %+v %v", ss, err)
	}
	if ss, err := g.as("bob").Schedules(ctx(t)); err != nil || len(ss) != 0 {
		t.Fatalf("another person sees none: %+v %v", ss, err)
	}
	if _, err := g.as("bob").FireSchedule(ctx(t), "audit"); code(err) != "not_found" {
		t.Fatalf("another person's schedule: %v", err)
	}
	bad := auditSchedule
	bad.Cron = "whenever"
	if _, err := g.owner.PutSchedule(ctx(t), bad); code(err) != "invalid" {
		t.Fatalf("a bad rule: %v", err)
	}
}

func TestAScheduleIsReadDeletedAndItsRunsListed(t *testing.T) {
	g := newRig(t)
	other := auditSchedule
	other.Name = "other"
	if err := errors.Join(errOf(g.owner.PutSchedule(ctx(t), auditSchedule)), errOf(g.owner.PutSchedule(ctx(t), other))); err != nil {
		t.Fatal(err)
	}
	got, err := g.owner.GetSchedule(ctx(t), "audit")
	if err != nil || got.ID == 0 || got.Repo != "acme/app" {
		t.Fatalf("get: %+v %v", got, err)
	}
	fired, err := g.owner.FireSchedule(ctx(t), "audit")
	if err != nil || fired.ScheduleID != got.ID {
		t.Fatalf("fire: %+v %v", fired, err)
	}
	if _, err := g.owner.FireSchedule(ctx(t), "other"); err != nil {
		t.Fatal(err)
	}
	checkScheduleRuns(t, g, fired.ID)
	checkScheduleDelete(t, g)
}

func checkScheduleRuns(t *testing.T, g *rig, want int64) {
	t.Helper()
	if runs, err := g.owner.ScheduleRuns(ctx(t), "audit"); err != nil || len(runs) != 1 || runs[0].ID != want {
		t.Fatalf("runs of audit: %+v %v", runs, err)
	}
	if _, err := g.owner.ScheduleRuns(ctx(t), "nope"); code(err) != "not_found" {
		t.Fatalf("runs of an unknown schedule: %v", err)
	}
}

func checkScheduleDelete(t *testing.T, g *rig) {
	t.Helper()
	if err := g.as("bob").DeleteSchedule(ctx(t), "audit"); code(err) != "not_found" {
		t.Fatalf("another person's delete: %v", err)
	}
	if err := g.owner.DeleteSchedule(ctx(t), "audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.owner.GetSchedule(ctx(t), "audit"); code(err) != "not_found" {
		t.Fatalf("after delete: %v", err)
	}
	if ss, _ := g.owner.Schedules(ctx(t)); len(ss) != 1 || ss[0].Name != "other" {
		t.Fatalf("the other stays: %+v", ss)
	}
}
