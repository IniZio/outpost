package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

type container struct {
	Name   string
	Config struct {
		Labels map[string]string
	}
	State struct {
		Status    string
		Running   bool
		OOMKilled bool
	}
}

func (d *Docker) name(k ports.BoxKey) string {
	return fmt.Sprintf("%s-t%d-g%d", d.cfg.Prefix, k.Task, k.Gen)
}

func (d *Docker) volume(task domain.TaskID, kind string) string {
	return fmt.Sprintf("%s-t%d-%s", d.cfg.Prefix, task, kind)
}

var volumeKinds = []string{"work", "home", "run", "docker"}

func (d *Docker) docker(ctx context.Context, args ...string) ([]byte, error) {
	return d.cfg.Run.Output(ctx, nil, args...) //nolint:wrapcheck // CmdError already names the command
}

func (d *Docker) info(c container) ports.BoxInfo {
	task, _ := strconv.ParseInt(c.Config.Labels[d.cfg.Label+".task"], 10, 64)
	gen, _ := strconv.ParseUint(c.Config.Labels[d.cfg.Label+".gen"], 10, 32)
	return ports.BoxInfo{
		Key:     ports.BoxKey{Task: domain.TaskID(task), Gen: domain.BoxGen(gen)},
		Repo:    domain.RepoName(c.Config.Labels[d.cfg.Label+".repo"]),
		Running: c.State.Running, OOMKilled: c.State.OOMKilled,
	}
}

// list returns the containers carrying label, which is "<Label>.task" for all of ours or
// "<Label>.task=<n>" for one task's.
func (d *Docker) list(ctx context.Context, label string) ([]container, error) {
	ids, err := d.docker(ctx, "ps", "-aq", "--no-trunc", "--filter", "label="+label)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(ids))
	if len(fields) == 0 {
		return nil, nil
	}
	out, err := d.docker(ctx, append([]string{"container", "inspect"}, fields...)...)
	if err != nil && !notFound(err) {
		return nil, err
	}
	var cs []container
	if err := json.Unmarshal(bytes.TrimSpace(out), &cs); err != nil && len(bytes.TrimSpace(out)) > 0 {
		return nil, fmt.Errorf("docker: inspect: %w", err)
	}
	return cs, nil
}

func (d *Docker) taskBoxes(ctx context.Context, task domain.TaskID) ([]ports.BoxInfo, error) {
	cs, err := d.list(ctx, fmt.Sprintf("%s.task=%d", d.cfg.Label, task))
	if err != nil {
		return nil, err
	}
	out := make([]ports.BoxInfo, len(cs))
	for i, c := range cs {
		out[i] = d.info(c)
	}
	return out, nil
}

// Boxes lists every box of this provider on the machine.
func (d *Docker) Boxes(ctx context.Context) ([]ports.BoxInfo, error) {
	cs, err := d.list(ctx, d.cfg.Label+".task")
	if err != nil {
		return nil, err
	}
	out := make([]ports.BoxInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, d.info(c))
	}
	return out, nil
}

func (d *Docker) inspect(ctx context.Context, name string) (container, error) {
	out, err := d.docker(ctx, "container", "inspect", name)
	if notFound(err) {
		return container{}, ports.ErrNoBox
	}
	if err != nil {
		return container{}, err
	}
	var cs []container
	if err := json.Unmarshal(out, &cs); err != nil || len(cs) != 1 {
		return container{}, fmt.Errorf("docker: inspect %s: %d results, %w", name, len(cs), err)
	}
	return cs[0], nil
}

var settled = map[string]bool{"running": true, "exited": true, "created": true, "dead": true}

// Inspect waits until the box has settled and reports it.
func (d *Docker) Inspect(ctx context.Context, k ports.BoxKey) (ports.BoxInfo, error) {
	for {
		c, err := d.inspect(ctx, d.name(k))
		if err != nil {
			return ports.BoxInfo{}, err
		}
		if settled[c.State.Status] {
			return d.info(c), nil
		}
		if err := d.sleep(ctx); err != nil {
			return ports.BoxInfo{}, err
		}
	}
}

func (d *Docker) sleep(ctx context.Context) error {
	t := time.NewTimer(d.cfg.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("docker: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// Stop stops the box unless a newer generation replaced it.
func (d *Docker) Stop(ctx context.Context, k ports.BoxKey) error {
	boxes, err := d.taskBoxes(ctx, k.Task)
	if err != nil {
		return err
	}
	for _, b := range boxes {
		if b.Key.Gen > k.Gen {
			return nil
		}
	}
	if _, err := d.docker(ctx, "stop", "--time", "10", d.name(k)); err != nil && !notFound(err) {
		return err
	}
	return nil
}

// Release removes every generation of the task's box and its volumes.
func (d *Docker) Release(ctx context.Context, task domain.TaskID) error {
	boxes, err := d.taskBoxes(ctx, task)
	if err != nil {
		return err
	}
	for _, b := range boxes {
		if _, err := d.docker(ctx, "rm", "-f", "--volumes", d.name(b.Key)); err != nil && !notFound(err) {
			return err
		}
	}
	for _, kind := range volumeKinds {
		if _, err := d.docker(ctx, "volume", "rm", "-f", d.volume(task, kind)); err != nil && !notFound(err) {
			return err
		}
	}
	return nil
}

func (d *Docker) running(ctx context.Context, k ports.BoxKey) error {
	c, err := d.inspect(ctx, d.name(k))
	if err != nil {
		return err
	}
	if !c.State.Running {
		return fmt.Errorf("docker: %s is %s: %w", d.name(k), c.State.Status, ports.ErrNoBox)
	}
	return nil
}

// Dial runs argv in the running box with its stdin and stdout as one stream.
func (d *Docker) Dial(ctx context.Context, k ports.BoxKey, argv []string) (io.ReadWriteCloser, error) {
	if err := d.running(ctx, k); err != nil {
		return nil, err
	}
	return d.cfg.Run.Start(ctx, append([]string{"exec", "-i", d.name(k)}, argv...)...) //nolint:wrapcheck // CmdError names it
}

// StepBin runs a step in the box and ends it, with everything it started, when its stdin closes.
// A killed docker exec client leaves its command running in the container, but it does close the
// command's stdin.
const StepBin = "/opt/outpost/bin/box-step"

// Exec runs r through box-step. The env values reach docker through its own environment and only
// their names through argv, so they never show in the host's process list.
func (d *Docker) Exec(ctx context.Context, k ports.BoxKey, r ports.ExecReq) (ports.ExecResult, error) {
	if err := d.running(ctx, k); err != nil {
		return ports.ExecResult{}, err
	}
	args := []string{"exec", "-i"}
	env := make([]string, 0, len(r.Env))
	for _, name := range slices.Sorted(maps.Keys(r.Env)) {
		args = append(args, "-e", name)
		env = append(env, name+"="+r.Env[name])
	}
	args = append(append(args, d.name(k), StepBin), r.Argv...)
	code, out, err := d.cfg.Run.Step(ctx, env, args...)
	return ports.ExecResult{Code: code, Output: out}, err //nolint:wrapcheck // Step wraps
}

// CopyIn unpacks a tar stream into dir inside the box.
func (d *Docker) CopyIn(ctx context.Context, k ports.BoxKey, dir string, tar io.Reader) error {
	if _, err := d.cfg.Run.Output(ctx, tar, "cp", "-", d.name(k)+":"+dir); err != nil {
		return err //nolint:wrapcheck // CmdError names it
	}
	return nil
}

// Commit saves the box as <Prefix>-ready/<owner>--<repo>:t<task>-g<gen> without pausing it.
// Mounts, volumes and tmpfs are not part of a commit, so neither the tools nor the task's work
// enter a template.
func (d *Docker) Commit(ctx context.Context, k ports.BoxKey) (string, error) {
	c, err := d.inspect(ctx, d.name(k))
	if err != nil {
		return "", err
	}
	repo := strings.ToLower(strings.ReplaceAll(string(d.info(c).Repo), "/", "--"))
	handle := fmt.Sprintf("%s-ready/%s:t%d-g%d", d.cfg.Prefix, repo, k.Task, k.Gen)
	if _, err := d.docker(ctx, "commit", "--no-pause", "--change", `ENTRYPOINT ["/opt/outpost/bin/box-init"]`, d.name(k), handle); err != nil {
		return "", err
	}
	return handle, nil
}
