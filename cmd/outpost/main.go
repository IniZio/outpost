// Command outpost is the Outpost server and its admin commands. serve runs the server. The admin
// commands talk to Postgres directly, so there is a way in before anyone can sign in.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: outpost <command>

  serve                      run the server (settings: OUTPOST_* environment variables)
  migrate                    apply database migrations
  key                        print a new OUTPOST_SECRET_KEY
  token create LOGIN [NAME]  print a new API token for LOGIN
  secret set LOGIN NAME      store a secret for LOGIN, read from stdin
  repo set OWNER/NAME IMAGE MACHINE [NOTES]
                             register a ready image held on MACHINE for a repo`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "outpost:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, env func(string) string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("no command\n%s", usage)
	}
	if args[0] == "key" {
		_, err := fmt.Fprintln(out, newKey())
		return err //nolint:wrapcheck // a write to stdout
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	if args[0] == "serve" {
		return serve(ctx, cfg)
	}
	return admin(ctx, cfg, args, in, out)
}

func serve(ctx context.Context, cfg config) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	s, err := newServer(ctx, cfg, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		s.store.Close()
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("serving", "addr", ln.Addr().String(), "public", cfg.PublicURL)
	return s.serve(ctx, ln)
}
