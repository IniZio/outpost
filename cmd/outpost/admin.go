package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/secrets"
	"github.com/IniZio/outpost/internal/store/postgres"
)

func newKey() string { return secrets.NewKey() }

// admin runs the commands that work on the database directly. Each one migrates first, so the
// first command on an empty database works.
func admin(ctx context.Context, cfg config, args []string, in io.Reader, out io.Writer) error {
	cmd := strings.Join(args[:min(2, len(args))], " ")
	if args[0] == "migrate" {
		cmd = "migrate"
	}
	do, ok := adminCommands[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q\n%s", strings.Join(args, " "), usage)
	}
	cfg.Migrate = true
	st, err := openStore(ctx, cfg, domain.SystemClock{})
	if err != nil {
		return err
	}
	defer st.Close()
	return do(ctx, adminEnv{cfg: cfg, store: st, in: in, out: out}, args[min(2, len(args)):])
}

type adminEnv struct {
	cfg   config
	store *postgres.Store
	in    io.Reader
	out   io.Writer
}

var adminCommands = map[string]func(context.Context, adminEnv, []string) error{
	"migrate": func(context.Context, adminEnv, []string) error { return nil },
	"token create": func(ctx context.Context, e adminEnv, args []string) error {
		if len(args) < 1 || len(args) > 2 {
			return fmt.Errorf("usage: outpost token create LOGIN [NAME]")
		}
		name := "admin"
		if len(args) == 2 {
			name = args[1]
		}
		tok, err := auth.New(auth.Config{Sessions: e.store, Clock: domain.SystemClock{}}).Mint(ctx, domain.Login(args[0]), name)
		if err != nil {
			return err //nolint:wrapcheck // auth wraps its own
		}
		_, err = fmt.Fprintln(e.out, tok)
		return err //nolint:wrapcheck // a write to stdout
	},
	"secret set": func(ctx context.Context, e adminEnv, args []string) error {
		if len(args) != 2 {
			return fmt.Errorf("usage: outpost secret set LOGIN NAME < value")
		}
		key, err := secrets.ParseKey(e.cfg.SecretKey)
		if err != nil {
			return fmt.Errorf("OUTPOST_SECRET_KEY: %w", err)
		}
		v, err := secrets.NewVault(key, e.store, domain.SystemClock{})
		if err != nil {
			return err //nolint:wrapcheck // the vault wraps its own
		}
		value, err := io.ReadAll(io.LimitReader(e.in, 64<<10))
		if err != nil {
			return fmt.Errorf("read the secret: %w", err)
		}
		return vault{v}.SetSecret(ctx, domain.Login(args[0]), args[1], strings.TrimSpace(string(value)))
	},
	"repo set": func(ctx context.Context, e adminEnv, args []string) error {
		if len(args) < 3 || len(args) > 4 || !strings.Contains(args[0], "/") {
			return fmt.Errorf("usage: outpost repo set OWNER/NAME IMAGE MACHINE [NOTES]")
		}
		r, err := e.store.Repo(ctx, domain.RepoName(args[0]))
		if err != nil {
			return err //nolint:wrapcheck // the store wraps its own
		}
		r.ReadyImage, r.ReadyOn, r.ReadyAt = args[1], domain.MachineID(args[2]), domain.SystemClock{}.Now()
		if len(args) == 4 {
			r.Notes = args[3]
		}
		return e.store.SetRepo(ctx, r) //nolint:wrapcheck // the store wraps its own
	},
}
