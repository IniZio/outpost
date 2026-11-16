package machine

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
)

// AgentConfig is a paired machine agent: who it is, where it dials, and its box provider.
type AgentConfig struct {
	URL     string // ws(s)://host/machines/link
	ID      domain.MachineID
	Key     ed25519.PrivateKey
	Machine Config
	Log     *slog.Logger
}

// RunAgent links to the server and serves the box ops until ctx ends. The link relinks on its own
// after a drop or a server deploy, so it returns only when ctx ends or the setup is wrong.
func RunAgent(ctx context.Context, cfg AgentConfig) error {
	m, err := New(cfg.Machine)
	if err != nil {
		return err
	}
	reg := link.NewRegistry(ops.Catalogue(), link.ServedByAgent, 0)
	m.Register(reg)
	a, err := link.NewAgent(link.AgentConfig{
		URL: cfg.URL, ID: cfg.ID, Key: cfg.Key, Build: cfg.Machine.Build, Reg: reg,
		Clock: cfg.Machine.Clock, Log: cfg.Log,
	})
	if err != nil {
		return fmt.Errorf("machine: %w", err)
	}
	// Either half ending early ends the other: an agent that cannot see its boxes must not link.
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() { defer cancel(); errs[0] = m.Run(run) })
	wg.Go(func() { defer cancel(); errs[1] = a.Run(run) })
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(errs...)
}
