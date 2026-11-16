package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"text/tabwriter"
	"time"

	"github.com/IniZio/outpost/api/client"
)

const scheduleUsage = `usage: outpost-cli schedule ls
       outpost-cli schedule put FILE        create or replace from a JSON schedule (- reads stdin)
       outpost-cli schedule import FILE     add the prototype's exported workflows, all disabled
       outpost-cli schedule fire NAME       start its task now; claims no slot
       outpost-cli schedule enable NAME
       outpost-cli schedule disable NAME`

var scheduleCommands = map[string]func(ctx context.Context, e env, c *client.Client, name string) error{
	"put":    putSchedule,
	"import": importWorkflows,
	"fire":   fireSchedule,
	"enable": func(ctx context.Context, e env, c *client.Client, name string) error {
		return enable(ctx, e, c, name, true)
	},
	"disable": func(ctx context.Context, e env, c *client.Client, name string) error {
		return enable(ctx, e, c, name, false)
	},
}

func schedules(ctx context.Context, e env, args []string) error {
	ls := len(args) == 1 && args[0] == "ls"
	var sub func(ctx context.Context, e env, c *client.Client, name string) error
	if len(args) == 2 {
		sub = scheduleCommands[args[0]]
	}
	if !ls && sub == nil {
		return errors.New(scheduleUsage)
	}
	c, err := connect(e)
	if err != nil {
		return err
	}
	if ls {
		return listSchedules(ctx, e, c)
	}
	return sub(ctx, e, c, args[1])
}

func fireSchedule(ctx context.Context, e env, c *client.Client, name string) error {
	t, err := c.FireSchedule(ctx, name)
	if err != nil {
		return fmt.Errorf("fire %s: %w", name, err)
	}
	_, _ = fmt.Fprintf(e.out, "Task %d is %s.\n", t.ID, t.Status)
	return nil
}

func enable(ctx context.Context, e env, c *client.Client, name string, on bool) error {
	s, err := c.SetScheduleEnabled(ctx, name, on)
	if err != nil {
		return fmt.Errorf("switch %s: %w", name, err)
	}
	_, _ = fmt.Fprintf(e.out, "%s is %s.\n", s.Name, onOff(s.Enabled))
	return nil
}

func onOff(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

func stamp(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("Mon 02 Jan 15:04")
}

func listSchedules(ctx context.Context, e env, c *client.Client) error {
	ss, err := c.Schedules(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	w := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tREPO\tCRON\tTZ\tSTATE\tNEXT\tLAST\tTASK\tSKIPPED")
	for _, s := range ss {
		task := "-"
		if s.LastTask != 0 {
			task = fmt.Sprint(s.LastTask)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Repo, s.Cron, s.TZ, onOff(s.Enabled),
			stamp(s.Next), stamp(s.LastDue), task, stamp(s.LastSkipped))
	}
	return w.Flush() //nolint:wrapcheck // a write to stdout
}

func readFile(e env, name string) ([]byte, error) {
	if name == "-" {
		b, err := io.ReadAll(e.in)
		return b, err //nolint:wrapcheck // the caller names the file
	}
	return os.ReadFile(name) //nolint:wrapcheck,gosec // the caller names the file; reading it is the point
}

func putSchedule(ctx context.Context, e env, c *client.Client, file string) error {
	b, err := readFile(e, file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	var s client.Schedule
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("parse %s: %w", file, err)
	}
	got, err := c.PutSchedule(ctx, s)
	if err != nil {
		return fmt.Errorf("put %s: %w", s.Name, err)
	}
	_, _ = fmt.Fprintf(e.out, "%s is %s; next %s.\n", got.Name, onOff(got.Enabled), stamp(got.Next))
	return nil
}

// workflow is one workflow in the prototype's export.
type workflow struct {
	Name       string   `json:"name"`
	Cron       string   `json:"cron"`
	Timezone   string   `json:"timezone"`
	Repos      []string `json:"repos"`
	Also       []string `json:"also"`
	Prompt     string   `json:"prompt"`
	TimeoutMin int      `json:"timeout_min"`
}

// fromWorkflows maps the export to schedules, one per repo, all disabled. A workflow with several
// repos names each schedule after its repo too.
func fromWorkflows(b []byte) ([]client.Schedule, error) {
	var export struct {
		Workflows []workflow `json:"workflows"`
	}
	if err := json.Unmarshal(b, &export); err != nil {
		return nil, fmt.Errorf("parse the export: %w", err)
	}
	var out []client.Schedule
	for _, w := range export.Workflows {
		for _, repo := range w.Repos {
			name := w.Name
			if len(w.Repos) > 1 {
				name += "-" + path.Base(repo)
			}
			out = append(out, client.Schedule{
				Name: name, Cron: w.Cron, TZ: w.Timezone, Repo: repo, Also: w.Also,
				Prompt: w.Prompt, Title: w.Name, TimeoutMin: w.TimeoutMin,
			})
		}
	}
	return out, nil
}

func importWorkflows(ctx context.Context, e env, c *client.Client, file string) error {
	b, err := readFile(e, file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	ss, err := fromWorkflows(b)
	if err != nil {
		return err
	}
	for _, s := range ss {
		if _, err := c.PutSchedule(ctx, s); err != nil {
			return fmt.Errorf("import %s: %w", s.Name, err)
		}
		_, _ = fmt.Fprintf(e.out, "Imported %s (%s, %s %s), disabled.\n", s.Name, s.Repo, s.Cron, s.TZ)
	}
	return nil
}
