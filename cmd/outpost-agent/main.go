// Command outpost-agent is the machine agent. It runs on each attached machine, dials out to the
// server and runs the server's boxes in Docker. pair enrolls the machine once; run serves.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const usage = `usage: outpost-agent <command>

  pair SERVER NAME [SLOTS]   pair this machine with the server at SERVER (http://host:7400)
  run                        link to the server and run its boxes

Settings: OUTPOST_AGENT_DIR (default ~/.config/outpost-agent), OUTPOST_TOOLS_IMAGE,
OUTPOST_BOX_PREFIX, OUTPOST_BOX_LABEL, OUTPOST_BOX_NETWORK, OUTPOST_BOX_MEMORY, OUTPOST_LOG.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "outpost-agent:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, env func(string) string, out io.Writer) error {
	dir := env("OUTPOST_AGENT_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("no home directory; set OUTPOST_AGENT_DIR: %w", err)
		}
		dir = filepath.Join(home, ".config", "outpost-agent")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(or(env("OUTPOST_LOG"), "info"))); err != nil {
		return fmt.Errorf("OUTPOST_LOG: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	switch {
	case len(args) >= 3 && len(args) <= 4 && args[0] == "pair":
		return pair(ctx, dir, args[1:], out)
	case len(args) == 1 && args[0] == "run":
		return serve(ctx, dir, env, log)
	}
	return fmt.Errorf("%s", usage)
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
