package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

type event struct {
	Action string `json:"Action"`
	Actor  struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time int64 `json:"time"`
}

var actions = map[string]domain.BoxEventKind{
	"start": domain.BoxStarted, "die": domain.BoxDied, "oom": domain.BoxOOM, "destroy": domain.BoxDied,
}

func (d *Docker) eventArgs(since string, only ...string) []string {
	a := []string{"events", "--since", since, "--format", "{{json .}}", "--filter", "type=container", "--filter", "label=" + d.cfg.Label + ".task"}
	if len(only) == 0 {
		only = []string{"start", "die", "oom", "destroy"}
	}
	for _, e := range only {
		a = append(a, "--filter", "event="+e)
	}
	return a
}

// Events replays the removals of the last day that left no box behind, then follows live events,
// restarting the CLI from the last event's time when it ends.
func (d *Docker) Events(ctx context.Context, sink func(ports.BoxChange)) error {
	now := strconv.FormatInt(d.cfg.Clock.Now().Unix(), 10)
	if err := d.replayRemovals(ctx, now, sink); err != nil {
		return err
	}
	since := now
	for {
		last, err := d.follow(ctx, since, sink)
		if err != nil {
			return err
		}
		if last != "" {
			since = last
		}
		if d.sleep(ctx) != nil {
			return nil
		}
	}
}

func (d *Docker) replayRemovals(ctx context.Context, until string, sink func(ports.BoxChange)) error {
	out, err := d.docker(ctx, append(d.eventArgs("24h", "destroy"), "--until", until)...)
	if err != nil {
		return err
	}
	live, err := d.Boxes(ctx)
	if err != nil {
		return err
	}
	seen := map[ports.BoxKey]bool{}
	for _, b := range live {
		seen[b.Key] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var ev event
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		k := d.keyOf(ev)
		if k.Task > 0 && !seen[k] {
			seen[k] = true
			sink(ports.BoxChange{Info: ports.BoxInfo{Key: k}, Kind: domain.BoxDied})
		}
	}
	return nil
}

func (d *Docker) keyOf(ev event) ports.BoxKey {
	task, _ := strconv.ParseInt(ev.Actor.Attributes[d.cfg.Label+".task"], 10, 64)
	gen, _ := strconv.ParseUint(ev.Actor.Attributes[d.cfg.Label+".gen"], 10, 32)
	return ports.BoxKey{Task: domain.TaskID(task), Gen: domain.BoxGen(gen)}
}

func (d *Docker) follow(ctx context.Context, since string, sink func(ports.BoxChange)) (string, error) {
	rwc, err := d.cfg.Run.Start(ctx, d.eventArgs(since)...)
	if err != nil && ctx.Err() != nil {
		return "", nil
	}
	if err != nil {
		return "", err //nolint:wrapcheck // CmdError names it
	}
	defer func() { _ = rwc.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = rwc.Close() })
	defer stop()
	last := ""
	sc := bufio.NewScanner(rwc)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		last = strconv.FormatInt(ev.Time, 10)
		d.deliver(ctx, ev, sink)
	}
	return last, nil
}

func (d *Docker) deliver(ctx context.Context, ev event, sink func(ports.BoxChange)) {
	kind, ok := actions[ev.Action]
	k := d.keyOf(ev)
	if !ok || k.Task <= 0 {
		return
	}
	info := ports.BoxInfo{Key: k, Repo: domain.RepoName(ev.Actor.Attributes[d.cfg.Label+".repo"])}
	if ev.Action != "destroy" {
		if got, err := d.Inspect(ctx, k); err == nil {
			info = got
		}
	}
	sink(ports.BoxChange{Info: info, Kind: kind})
}
