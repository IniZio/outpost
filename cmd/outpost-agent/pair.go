package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IniZio/outpost/api/client"
)

// identity is what pair writes and run reads: agent.json, and the private key beside it (0600).
type identity struct {
	Server    string `json:"server"`
	Link      string `json:"link"`
	MachineID string `json:"machine_id"`
}

// pair makes a key, asks the server for a code, shows it, and waits for a person to confirm it
// with `outpost-cli machine confirm CODE` while signed in.
func pair(ctx context.Context, dir string, args []string, out io.Writer) error {
	server, name, slots := strings.TrimRight(args[0], "/"), args[1], 1
	if len(args) == 3 {
		n, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("slots must be a number: %w", err)
		}
		slots = n
	}
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return fmt.Errorf("make a key: %w", err)
	}
	c := client.New(server, "")
	t, err := c.Pair(ctx, client.PairRequest{Key: pub, Name: name, Role: "sandbox", Slots: slots})
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Confirm this machine while signed in:\n\n  outpost-cli machine confirm %s\n\nThe code works until %s.\n",
		t.UserCode, t.Expires.Local().Format(time.Kitchen))
	id, err := collect(ctx, c, t)
	if err != nil {
		return err
	}
	link := "ws" + strings.TrimPrefix(server, "http") + "/machines/link"
	if err := save(dir, identity{Server: server, Link: link, MachineID: id}, key); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Paired as %s. Start the agent with: outpost-agent run\n", id)
	return nil
}

func collect(ctx context.Context, c *client.Client, t client.PairTicket) (string, error) {
	every := time.Duration(max(t.IntervalSec, 1)) * time.Second
	for {
		got, err := c.Collect(ctx, t.DeviceCode)
		if err != nil {
			return "", fmt.Errorf("wait for the confirmation: %w", err)
		}
		if got.State == "enrolled" {
			return got.MachineID, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("pairing: %w", ctx.Err())
		case <-time.After(every):
		}
	}
}

func save(dir string, id identity, key ed25519.PrivateKey) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key"), key.Seed(), 0o600); err != nil {
		return fmt.Errorf("write the key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), b, 0o600); err != nil {
		return fmt.Errorf("write agent.json: %w", err)
	}
	return nil
}

func load(dir string) (identity, ed25519.PrivateKey, error) {
	var id identity
	b, err := os.ReadFile(filepath.Join(dir, "agent.json"))
	if err != nil {
		return id, nil, fmt.Errorf("not paired yet (run outpost-agent pair): %w", err)
	}
	if err := json.Unmarshal(b, &id); err != nil || id.MachineID == "" || id.Link == "" {
		return id, nil, fmt.Errorf("agent.json is damaged; pair again: %w", errors.Join(err, errors.New("missing fields")))
	}
	seed, err := os.ReadFile(filepath.Join(dir, "key"))
	if err != nil || len(seed) != ed25519.SeedSize {
		return id, nil, fmt.Errorf("the key is missing or damaged; pair again: %w", errors.Join(err, errors.New("bad key")))
	}
	return id, ed25519.NewKeyFromSeed(seed), nil
}
