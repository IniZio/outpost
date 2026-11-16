package api_test

import (
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
