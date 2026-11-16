package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/sandbox/docker"
)

// build is what machine.status reports. Release builds set it with -ldflags.
var build = "dev"

func serve(ctx context.Context, dir string, env func(string) string, log *slog.Logger) error {
	id, key, err := load(dir)
	if err != nil {
		return err
	}
	sb, err := docker.New(docker.Config{
		Tools: or(env("OUTPOST_TOOLS_IMAGE"), "ob-tools:dev"), Prefix: env("OUTPOST_BOX_PREFIX"),
		Label: env("OUTPOST_BOX_LABEL"), Network: env("OUTPOST_BOX_NETWORK"), Memory: env("OUTPOST_BOX_MEMORY"),
	})
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	log.Info("linking", "server", id.Link, "machine", id.MachineID)
	return machine.RunAgent(ctx, machine.AgentConfig{ //nolint:wrapcheck // machine wraps its own
		URL: id.Link, ID: domain.MachineID(id.MachineID), Key: key, Log: log,
		Machine: machine.Config{Sandbox: sb, Build: build},
	})
}
