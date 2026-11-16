package main

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/ports"
)

// pairAgent pairs a machine the way outpost-agent pair does, confirmed by c's login.
func pairAgent(ctx context.Context, t *testing.T, l *live, c *client.Client) (domain.MachineID, ed25519.PrivateKey) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(nil)
	anon := client.New(l.url, "")
	ticket, err := anon.Pair(ctx, client.PairRequest{Key: pub, Name: "it", Role: "sandbox", Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Confirm(ctx, ticket.UserCode); err != nil {
		t.Fatal(err)
	}
	got, err := anon.Collect(ctx, ticket.DeviceCode)
	if err != nil || got.State != "enrolled" {
		t.Fatalf("%+v %v", got, err)
	}
	return domain.MachineID(got.MachineID), key
}

// startAgent runs a paired agent in the test over sb until the test ends.
func startAgent(t *testing.T, l *live, id domain.MachineID, key ed25519.PrivateKey, sb ports.Sandbox, tune func(*machine.Config)) {
	t.Helper()
	cfg := machine.AgentConfig{URL: l.env["OUTPOST_AGENT_URL"], ID: id, Key: key, Machine: machine.Config{Sandbox: sb, Redial: 20 * time.Millisecond}}
	if tune != nil {
		tune(&cfg.Machine)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- machine.RunAgent(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent: %v", err)
		}
	})
}

func waitFor(ctx context.Context, t *testing.T, what string, ok func() bool) {
	t.Helper()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatalf("never: %s", what)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func machineOnline(ctx context.Context, c *client.Client, id domain.MachineID) bool {
	ms, err := c.Machines(ctx)
	return err == nil && len(ms) == 1 && ms[0].ID == string(id) && ms[0].Online
}

func status(ctx context.Context, c *client.Client, id int64, want ...string) bool {
	task, err := c.Task(ctx, id)
	if err != nil {
		return false
	}
	for _, w := range want {
		if task.Status == w {
			return true
		}
	}
	return false
}
