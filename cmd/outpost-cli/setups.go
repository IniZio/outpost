package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/IniZio/outpost/api/client"
)

const repoUsage = `usage: outpost-cli repo setup put REPO DIR     store DIR's files as REPO's next setup revision
       outpost-cli repo setup ls REPO          list REPO's setup revisions
       outpost-cli repo setup log REPO REV     print a revision's build log
       outpost-cli repo setup build [-machine NAME] REPO REV
                                               build a revision on one of your machines`

func repoCmd(ctx context.Context, e env, args []string) error {
	if len(args) < 3 || args[0] != "setup" {
		return errors.New(repoUsage)
	}
	sub, rest := args[1], args[2:]
	run := map[string]func(context.Context, env, *client.Client, []string) error{
		"put": putSetup, "ls": listSetups, "log": setupLog, "build": buildSetup,
	}[sub]
	if run == nil {
		return errors.New(repoUsage)
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	return run(ctx, e, c, rest)
}

// setupFiles reads the regular files directly in dir, by name.
func setupFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	files := map[string]string{}
	for _, en := range entries {
		if !en.Type().IsRegular() || strings.HasPrefix(en.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, en.Name())) //nolint:gosec // the user's own dir
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", en.Name(), err)
		}
		files[en.Name()] = string(b)
	}
	return files, nil
}

func putSetup(ctx context.Context, e env, c *client.Client, args []string) error {
	if len(args) != 2 {
		return errors.New(repoUsage)
	}
	files, err := setupFiles(args[1])
	if err != nil {
		return err
	}
	r, err := c.PutSetup(ctx, args[0], files)
	if err != nil {
		return fmt.Errorf("put setup: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "Stored %s setup revision %d: %s.\n", r.Repo, r.Rev, strings.Join(r.Files, ", "))
	return nil
}

func listSetups(ctx context.Context, e env, c *client.Client, args []string) error {
	if len(args) != 1 {
		return errors.New(repoUsage)
	}
	revs, err := c.Setups(ctx, args[0])
	if err != nil {
		return fmt.Errorf("setups: %w", err)
	}
	w := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "REV\tSTATUS\tMACHINE\tIMAGE\tCOMMIT\tBY")
	for _, r := range revs {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", r.Rev, r.Status, r.Machine, r.Image, firstLine(r.Commit, 12), r.CreatedBy)
	}
	return w.Flush() //nolint:wrapcheck // stdout
}

func revArg(args []string) (int, error) {
	if len(args) != 2 {
		return 0, errors.New(repoUsage)
	}
	rev, err := strconv.Atoi(args[1])
	if err != nil || rev <= 0 {
		return 0, fmt.Errorf("bad revision %q", args[1])
	}
	return rev, nil
}

func setupLog(ctx context.Context, e env, c *client.Client, args []string) error {
	rev, err := revArg(args)
	if err != nil {
		return err
	}
	revs, err := c.Setups(ctx, args[0])
	if err != nil {
		return fmt.Errorf("setups: %w", err)
	}
	for _, r := range revs {
		if r.Rev == rev {
			_, _ = io.WriteString(e.out, r.Log+"\n")
			return nil
		}
	}
	return fmt.Errorf("%s has no revision %d", args[0], rev)
}

func buildSetup(ctx context.Context, e env, c *client.Client, args []string) error {
	fs := flag.NewFlagSet("repo setup build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	machine := fs.String("machine", "", "the machine to build on")
	if err := fs.Parse(args); err != nil {
		return errors.New(repoUsage)
	}
	rev, err := revArg(fs.Args())
	if err != nil {
		return err
	}
	r, err := c.BuildSetup(ctx, fs.Arg(0), rev, *machine)
	if err != nil {
		return fmt.Errorf("build setup: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "Building %s setup revision %d on %s. See it with: outpost-cli repo setup ls %s\n", r.Repo, r.Rev, r.Machine, r.Repo)
	return nil
}
