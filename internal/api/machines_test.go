package api_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/api"
)

func TestAMachinePairsByDeviceCode(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	pub, _, _ := ed25519.GenerateKey(nil)
	agent := client.New(g.srv.URL, "")
	ticket, err := agent.Pair(c, client.PairRequest{Key: pub, Name: "laptop", Role: "sandbox", Slots: 1})
	if err != nil || len(ticket.UserCode) != 9 || len(ticket.DeviceCode) != 32 || ticket.IntervalSec != 2 {
		t.Fatalf("%+v %v", ticket, err)
	}
	if got, err := agent.Collect(c, ticket.DeviceCode); err != nil || got.State != "pending" {
		t.Fatalf("before confirm: %+v %v", got, err)
	}
	m, err := g.owner.Confirm(c, ticket.UserCode)
	if err != nil || m.Name != "laptop" || m.Owner != "ana" || m.ID == "" {
		t.Fatalf("confirm: %+v %v", m, err)
	}
	if _, err := g.owner.Confirm(c, ticket.UserCode); code(err) != "used" {
		t.Fatalf("a code confirms once: %v", err)
	}
	if got, err := agent.Collect(c, ticket.DeviceCode); err != nil || got != (client.Collected{State: "enrolled", MachineID: m.ID}) {
		t.Fatalf("collect: %+v %v", got, err)
	}
}

func TestBadPairingsAreRefused(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	pub, _, _ := ed25519.GenerateKey(nil)
	agent := client.New(g.srv.URL, "")
	for _, bad := range []client.PairRequest{
		{Key: []byte("short"), Name: "x", Role: "sandbox", Slots: 1},
		{Key: pub, Name: "x", Role: "router", Slots: 1},
	} {
		if _, err := agent.Pair(c, bad); code(err) != "invalid" {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, err := agent.Collect(c, []byte("nope")); code(err) != "not_found" {
		t.Fatalf("an unknown device: %v", err)
	}
	if _, err := g.owner.Confirm(c, "ZZZZ-ZZZZ"); code(err) != "not_found" {
		t.Fatalf("an unknown code: %v", err)
	}
}

func TestOwnersListAndRevokeTheirMachines(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	ms, err := g.owner.Machines(c)
	if err != nil || len(ms) != 1 || ms[0] != (client.Machine{ID: "m1", Name: "box1", Owner: "ana", Role: "sandbox", Slots: 2, Online: true}) {
		t.Fatalf("%+v %v", ms, err)
	}
	if ms, err := g.as("bob").Machines(c); err != nil || len(ms) != 0 {
		t.Fatalf("bob has none: %+v %v", ms, err)
	}
	if err := g.as("bob").DeleteMachine(c, "m1"); code(err) != "not_found" {
		t.Fatalf("bob cannot revoke ana's machine: %v", err)
	}
	if err := g.owner.DeleteMachine(c, "m1"); err != nil {
		t.Fatal(err)
	}
}

func TestReposAndSecrets(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	r, err := g.owner.SetRepo(c, client.Repo{Name: "o/r", ReadyImage: "ready:1", ReadyOn: "m1", Notes: "warm gradle"})
	if err != nil || r.ReadyAt == nil || r.ReadyImage != "ready:1" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := g.owner.SetRepo(c, client.Repo{Name: "o/r", ReadyImage: "ready:2"}); code(err) != "bad_request" {
		t.Fatalf("an image needs its machine: %v", err)
	}
	if rs, err := g.as("bob").Repos(c); err != nil || len(rs) != 1 || rs[0].Notes != "warm gradle" {
		t.Fatalf("%+v %v", rs, err)
	}
	if err := g.owner.SetSecret(c, "github", "gho_x"); err != nil || g.secrets.got["ana/github"] != "gho_x" {
		t.Fatalf("%v %v", err, g.secrets.got)
	}
	if err := g.owner.SetSecret(c, "github", ""); code(err) != "bad_request" {
		t.Fatalf("an empty secret: %v", err)
	}
	none := newRig(t, func(cfg *api.Config) { cfg.Secrets = nil })
	if err := none.owner.SetSecret(c, "github", "x"); code(err) != "not_built" {
		t.Fatalf("no store yet: %v", err)
	}
}
