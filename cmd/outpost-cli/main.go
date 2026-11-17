// Command outpost-cli starts, lists, watches and steers runs from a laptop.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const usage = `usage: outpost-cli <command>

  login SERVER [TOKEN]       sign in; with no TOKEN, reads a GitHub token on stdin
                             (gh auth token | outpost-cli login http://host:7400)
  run [-also a/b,c/d] [-title T] [-d] REPO PROMPT...
                             start a task and watch it (-d: print its id and return)
  ls [STATUS]                list your tasks
  watch [-screen] ID         follow a task's timeline, or redraw its screen each second
  msg ID TEXT...             type to the task's agent; resumes an ended task
  stop ID                    stop a task
  machine confirm CODE       pair the machine showing CODE to you
  machines                   list your machines
  weblink                    print a one-time link that signs a browser in to watch
  schedule ls|put|import|fire|enable|disable
                             manage your schedules (outpost-cli schedule for details)
  repo setup put|ls|log|build
                             manage a repo's setup revisions (outpost-cli repo for details)

Settings: OUTPOST_SERVER and OUTPOST_TOKEN override the saved login in ~/.config/outpost/cli.json.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], env{get: os.Getenv, in: os.Stdin, out: os.Stdout})
	stop()
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "outpost-cli:", err)
		os.Exit(1)
	}
}

// env is the CLI's world: its settings, stdin and stdout.
type env struct {
	get func(string) string
	in  io.Reader
	out io.Writer
}

type command func(ctx context.Context, e env, args []string) error

var commands = map[string]command{
	"login":    login,
	"run":      runTask,
	"ls":       list,
	"watch":    watch,
	"msg":      message,
	"stop":     stopTask,
	"machine":  confirmMachine,
	"machines": machines,
	"weblink":  webLink,
	"schedule": schedules,
	"repo":     repoCmd,
}

func run(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		return errors.New(strings.TrimSpace(usage))
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	return cmd(ctx, e, args[1:])
}
