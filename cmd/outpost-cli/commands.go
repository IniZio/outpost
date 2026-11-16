package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/IniZio/outpost/api/client"
)

// saved is the login kept in ~/.config/outpost/cli.json.
type saved struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath(e env) (string, error) {
	if p := e.get("OUTPOST_CLI_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no config directory: %w", err)
	}
	return filepath.Join(dir, "outpost", "cli.json"), nil
}

// connect returns a client for the saved login, with OUTPOST_SERVER and OUTPOST_TOKEN on top.
func connect(e env) (*client.Client, error) {
	var s saved
	if p, err := configPath(e); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &s)
		}
	}
	if v := e.get("OUTPOST_SERVER"); v != "" {
		s.Server = v
	}
	if v := e.get("OUTPOST_TOKEN"); v != "" {
		s.Token = v
	}
	if s.Server == "" || s.Token == "" {
		return nil, errors.New("not signed in; run: gh auth token | outpost-cli login http://host:7400")
	}
	return client.New(s.Server, s.Token), nil
}

func login(ctx context.Context, e env, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: outpost-cli login SERVER [TOKEN]")
	}
	s := saved{Server: strings.TrimRight(args[0], "/")}
	if len(args) == 2 {
		s.Token = args[1]
	} else {
		gh, err := bufio.NewReader(e.in).ReadString('\n')
		if gh = strings.TrimSpace(gh); gh == "" {
			return fmt.Errorf("pipe a GitHub token in: gh auth token | outpost-cli login %s: %w", s.Server, errors.Join(err, io.ErrUnexpectedEOF))
		}
		sess, err := client.New(s.Server, "").SignIn(ctx, gh)
		if err != nil {
			return fmt.Errorf("sign in: %w", err)
		}
		s.Token = sess.Token
	}
	me, err := client.New(s.Server, s.Token).Me(ctx)
	if err != nil {
		return fmt.Errorf("check the token: %w", err)
	}
	if err := save(e, s); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.out, "Signed in to %s as %s.\n", s.Server, me.Login)
	return nil
}

func save(e env, s saved) error {
	p, err := configPath(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("make %s: %w", filepath.Dir(p), err)
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	return nil
}

func runTask(ctx context.Context, e env, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	also := fs.String("also", "", "more repos, comma separated")
	title := fs.String("title", "", "a title")
	detach := fs.Bool("d", false, "print the id and return")
	if err := fs.Parse(args); err != nil || fs.NArg() < 2 {
		return errors.New("usage: outpost-cli run [-also a/b,c/d] [-title T] [-d] REPO PROMPT")
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	n := client.NewTask{Repo: fs.Arg(0), Prompt: strings.Join(fs.Args()[1:], " "), Title: *title}
	if *also != "" {
		n.Also = strings.Split(*also, ",")
	}
	t, err := c.CreateTask(ctx, n)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "Task %d is %s.\n", t.ID, t.Status)
	if *detach {
		return nil
	}
	return follow(ctx, e, c, t.ID)
}

func list(ctx context.Context, e env, args []string) error {
	c, err := connect(e)
	if err != nil {
		return err
	}
	status := ""
	if len(args) > 0 {
		status = args[0]
	}
	ts, err := c.Tasks(ctx, status, 50)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	w := tabwriter.NewWriter(e.out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tREPO\tCREATED\tTITLE")
	for _, t := range ts {
		title := t.Title
		if title == "" {
			title = firstLine(t.Prompt, 50)
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", t.ID, t.Status, t.Repo, t.CreatedAt.Local().Format("Jan 2 15:04"), title)
	}
	return w.Flush() //nolint:wrapcheck // a write to stdout
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

func taskID(args []string, want int, use string) (int64, error) {
	if len(args) < want {
		return 0, errors.New("usage: outpost-cli " + use)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("a task id is a number: %w", err)
	}
	return id, nil
}

func message(ctx context.Context, e env, args []string) error {
	id, err := taskID(args, 2, "msg ID TEXT...")
	if err != nil {
		return err
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	t, err := c.Message(ctx, id, strings.Join(args[1:], " "))
	if err != nil {
		return fmt.Errorf("message: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "Sent. Task %d is %s.\n", t.ID, t.Status)
	return nil
}

func stopTask(ctx context.Context, e env, args []string) error {
	id, err := taskID(args, 1, "stop ID")
	if err != nil {
		return err
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	t, err := c.Stop(ctx, id)
	if err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "Task %d is %s.\n", t.ID, t.Status)
	return nil
}

func confirmMachine(ctx context.Context, e env, args []string) error {
	if len(args) != 2 || args[0] != "confirm" {
		return errors.New("usage: outpost-cli machine confirm CODE")
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	m, err := c.Confirm(ctx, strings.ToUpper(args[1]))
	if err != nil {
		return fmt.Errorf("confirm: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "Paired %s (%s). It links once outpost-agent run starts.\n", m.Name, m.ID)
	return nil
}

func machines(ctx context.Context, e env, _ []string) error {
	c, err := connect(e)
	if err != nil {
		return err
	}
	ms, err := c.Machines(ctx)
	if err != nil {
		return fmt.Errorf("machines: %w", err)
	}
	w := tabwriter.NewWriter(e.out, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tROLE\tSLOTS\tONLINE")
	for _, m := range ms {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%v\n", m.ID, m.Name, m.Role, m.Slots, m.Online)
	}
	return w.Flush() //nolint:wrapcheck // a write to stdout
}

func webLink(ctx context.Context, e env, _ []string) error {
	c, err := connect(e)
	if err != nil {
		return err
	}
	l, err := c.WebLink(ctx)
	if err != nil {
		return fmt.Errorf("web link: %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "%s\nIt works once, within two minutes.\n", l.URL)
	return nil
}

func watch(ctx context.Context, e env, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	screen := fs.Bool("screen", false, "redraw the agent's screen each second")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: outpost-cli watch [-screen] ID")
	}
	id, err := taskID(fs.Args(), 1, "watch [-screen] ID")
	if err != nil {
		return err
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	if *screen {
		return showScreen(ctx, e, c, id)
	}
	return follow(ctx, e, c, id)
}

// pollEvery is how often watch reads the task's status and screen.
var pollEvery = time.Second

// follow prints the timeline until the task waits for its owner or ends. The stream reconnects
// from the last entry it printed.
func follow(ctx context.Context, e env, c *client.Client, id int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go untilSettled(ctx, cancel, e, c, id)
	after := 0
	for ctx.Err() == nil {
		var err error
		after, err = c.Follow(ctx, id, after, func(en client.Entry) error {
			_, err := fmt.Fprintf(e.out, "%s  %-8s %s\n", en.At.Local().Format("15:04:05"), en.Kind, en.Text)
			return err //nolint:wrapcheck // a write to stdout
		})
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("follow: %w", err)
		}
	}
	return nil
}

// untilSettled ends a follow once the task waits for its owner or has ended, a beat later so the
// last entries arrive first.
func untilSettled(ctx context.Context, cancel context.CancelFunc, e env, c *client.Client, id int64) {
	for ctx.Err() == nil {
		t, err := c.Task(ctx, id)
		time.Sleep(pollEvery)
		if err == nil && (t.Ended() || t.Status == "waiting") {
			_, _ = fmt.Fprintf(e.out, "Task %d is %s.\n", id, t.Status)
			cancel()
			return
		}
	}
}

// showScreen clears the terminal and prints the agent's screen each second until the task ends.
func showScreen(ctx context.Context, e env, c *client.Client, id int64) error {
	for {
		s, err := c.Screen(ctx, id, 0)
		if err != nil {
			return fmt.Errorf("screen: %w", err)
		}
		_, _ = fmt.Fprintf(e.out, "\033[H\033[2J%s\n", s)
		if t, err := c.Task(ctx, id); err == nil && t.Ended() {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollEvery):
		}
	}
}
