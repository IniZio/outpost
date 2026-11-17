package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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
	data, err := dataDir(env)
	if err != nil {
		return err
	}
	sb, err := docker.New(docker.Config{
		Tools: or(env("OUTPOST_TOOLS_IMAGE"), "ob-tools:dev"), Prefix: env("OUTPOST_BOX_PREFIX"),
		Label: env("OUTPOST_BOX_LABEL"), Network: env("OUTPOST_BOX_NETWORK"), Memory: env("OUTPOST_BOX_MEMORY"),
		Data: data, CacheMax: env("OUTPOST_SETUP_CACHE_MAX"),
	})
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	log.Info("linking", "server", id.Link, "machine", id.MachineID)
	return machine.RunAgent(ctx, machine.AgentConfig{ //nolint:wrapcheck // machine wraps its own
		URL: id.Link, ID: domain.MachineID(id.MachineID), Key: key, Log: log,
		Machine: machine.Config{Sandbox: sb, Builder: sb, Build: build},
	})
}

// dataDir is where the agent keeps repo mirrors, inner image tars and setup build work dirs.
func dataDir(env func(string) string) (string, error) {
	if d := env("OUTPOST_AGENT_DATA"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory; set OUTPOST_AGENT_DATA: %w", err)
	}
	return filepath.Join(home, ".local", "share", "outpost-agent"), nil
}
