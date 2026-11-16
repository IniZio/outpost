package machine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

// boxEnv is the box's own environment. It is the same for every task, so it is safe in a saved
// template, which keeps container env. hc's runtime dir is on the box's tmpfs, so no stale socket
// outlives a stop.
var boxEnv = map[string]string{
	"HC_SESSION":      "box",
	"XDG_RUNTIME_DIR": "/run/box",
	"BASH_ENV":        "/opt/outpost/lib/oom-reset.sh",
}

// convergeRun is one converge job: the request, the box, and the hc connection it opens once the
// box is up.
type convergeRun struct {
	m   *Machine
	j   *job
	req ops.ConvergeReq
	key ports.BoxKey
	hc  *hc.Conn
	rev bool
}

type step struct {
	phase string
	run   func(c *convergeRun, ctx context.Context) error
}

// steps is the order. Each checks before it acts, so a rerun after a cut, a crash or a preemption
// redoes only what is missing.
var steps = []step{
	{"Preparing the box", (*convergeRun).up},
	{"", (*convergeRun).connect},
	{"", (*convergeRun).seed},
	{"", (*convergeRun).seedHome},
	{"", (*convergeRun).trust},
	{"", (*convergeRun).profile},
	{"", (*convergeRun).clone},
	{"Starting the agent", (*convergeRun).agent},
}

func (m *Machine) runConverge(ctx context.Context, j *job, req ops.ConvergeReq) (any, error) {
	c := &convergeRun{m: m, j: j, req: req, key: ports.BoxKey{Task: req.Box.Task, Gen: req.Box.Gen}}
	defer c.close()
	for _, s := range steps {
		if s.phase != "" {
			j.add(s.phase, "")
		}
		if err := s.run(c, ctx); err != nil {
			return nil, err
		}
	}
	return ops.Converged{Revived: c.rev}, nil
}

func (c *convergeRun) close() {
	if c.hc != nil {
		_ = c.hc.Close()
	}
}

func (c *convergeRun) up(ctx context.Context) error {
	spec := ports.BoxSpec{Key: c.key, Image: c.req.Image, Repo: c.req.Repos[0], Fresh: c.req.Fresh, Env: boxEnv}
	if _, err := c.m.cfg.Sandbox.Up(ctx, spec, c.j.add); err != nil {
		return fmt.Errorf("machine: preparing the box: %w", err)
	}
	return nil
}

func (c *convergeRun) connect(ctx context.Context) error {
	conn, err := c.m.dial(ctx, c.key)
	c.hc = conn
	return err
}

// run executes a step in the box through the provider, with the oom reset in front, and fails on a
// non-zero exit. It does not go through hc, whose exec refuses timeouts over ten minutes: a clone,
// a seed or a save of a big repo takes longer, and only the job's budget bounds it. Cancelling ctx
// ends the step.
func (c *convergeRun) run(ctx context.Context, env map[string]string, argv ...string) error {
	r, err := c.m.cfg.Sandbox.Exec(ctx, c.key, ports.ExecReq{Argv: append([]string{oomReset}, argv...), Env: env})
	if err != nil {
		return fmt.Errorf("machine: %s: %w", argv[0], err)
	}
	if r.Code != 0 {
		return fmt.Errorf("machine: %s failed: %s", strings.Join(argv, " "), tail(string(r.Output), 600))
	}
	return nil
}

func (c *convergeRun) exists(ctx context.Context, p string) (bool, error) {
	return exists(ctx, c.hc, p)
}

func exists(ctx context.Context, conn *hc.Conn, p string) (bool, error) {
	r, err := conn.Exec(ctx, hc.ExecReq{Argv: []string{"test", "-e", p}, Timeout: 30})
	if err != nil {
		return false, fmt.Errorf("machine: test %s: %w", p, err)
	}
	return r.OK(), nil
}

// seed restores a ready template's workspace and inner images, which a template carries on its
// own disk because volumes are not part of it. box-seed keeps its own markers on the volumes.
func (c *convergeRun) seed(ctx context.Context) error {
	ok, err := c.exists(ctx, seedDir)
	if err != nil || !ok {
		return err
	}
	c.j.add("Restoring the ready image", "")
	return c.run(ctx, nil, boxSeed)
}

func (c *convergeRun) seedHome(ctx context.Context) error {
	done, err := c.exists(ctx, homeMark)
	if err != nil || done {
		return err
	}
	if len(c.req.Home) > 0 {
		b, err := homeTar(c.req.Home)
		if err != nil {
			return err
		}
		if err := c.m.cfg.Sandbox.CopyIn(ctx, c.key, "/root", bytes.NewReader(b)); err != nil {
			return fmt.Errorf("machine: home files: %w", err)
		}
	}
	return c.run(ctx, nil, "touch", homeMark)
}

func homeTar(files []ops.HomeFile) ([]byte, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, f := range files {
		if err := writeHome(w, dirs, f); err != nil {
			return nil, fmt.Errorf("machine: home tar: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("machine: home tar: %w", err)
	}
	return buf.Bytes(), nil
}

func writeHome(w *tar.Writer, dirs map[string]bool, f ops.HomeFile) error {
	for d := path.Dir(path.Clean(f.Path)); d != "." && !dirs[d]; d = path.Dir(d) {
		dirs[d] = true
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o700}); err != nil {
			return err //nolint:wrapcheck // homeTar wraps
		}
	}
	mode := int64(f.Mode)
	if mode == 0 {
		mode = 0o600
	}
	if err := w.WriteHeader(&tar.Header{Name: path.Clean(f.Path), Mode: mode, Size: int64(len(f.Data))}); err != nil {
		return err //nolint:wrapcheck // homeTar wraps
	}
	_, err := w.Write(f.Data)
	return err //nolint:wrapcheck // homeTar wraps
}

// trust installs the credential proxy's CA before any clone, because clones go through the proxy.
// It runs on every converge: a replaced box is a new container over the old volumes.
func (c *convergeRun) trust(ctx context.Context) error {
	if len(c.req.CA) == 0 {
		return nil
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{Name: caName, Mode: 0o644, Size: int64(len(c.req.CA))}); err != nil {
		return fmt.Errorf("machine: ca tar: %w", err)
	}
	if _, err := w.Write(c.req.CA); err != nil {
		return fmt.Errorf("machine: ca tar: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("machine: ca tar: %w", err)
	}
	if err := c.m.cfg.Sandbox.CopyIn(ctx, c.key, caDir, &buf); err != nil {
		return fmt.Errorf("machine: proxy ca: %w", err)
	}
	return c.run(ctx, nil, boxTrust, path.Join(caDir, caName))
}

// profile runs the plan's setup commands once per home volume. It comes after trust, because the
// commands may reach GitHub through the proxy.
func (c *convergeRun) profile(ctx context.Context) error {
	if len(c.req.Setup) == 0 {
		return nil
	}
	done, err := c.exists(ctx, profMark)
	if err != nil || done {
		return err
	}
	c.j.add("Applying the agent profile", "")
	for _, argv := range c.req.Setup {
		if err := c.run(ctx, c.req.Env, argv...); err != nil {
			return err
		}
	}
	return c.run(ctx, nil, "touch", profMark)
}

// clone clones each repo not yet in the workspace into <dir>.partial and then moves it in place,
// so a half clone is never taken for a done one.
func (c *convergeRun) clone(ctx context.Context) error {
	for _, repo := range c.req.Repos {
		if err := c.cloneOne(ctx, repo); err != nil {
			return err
		}
	}
	return nil
}

func (c *convergeRun) cloneOne(ctx context.Context, repo domain.RepoName) error {
	dir := domain.RepoDir(repo)
	if ok, err := c.exists(ctx, dir); err != nil || ok {
		return err
	}
	c.j.add("Cloning "+string(repo), "")
	partial := dir + ".partial"
	if err := c.run(ctx, nil, "rm", "-rf", partial); err != nil {
		return err
	}
	if err := c.run(ctx, c.req.Env, c.m.cfg.Clone(repo, partial)...); err != nil {
		return err
	}
	return c.run(ctx, nil, "mv", partial, dir)
}

// agent ends the terminals of older runs, refuses when a newer run is live, and starts this run's
// terminal unless it is live already. hc refuses a second live terminal of one name, so two
// racing starts of one run end with one agent.
func (c *convergeRun) agent(ctx context.Context) error {
	if err := c.j.awaitFollower(ctx, c.m.cfg.FollowWait); err != nil {
		return err
	}
	ts, err := c.hc.List(ctx, taskLabels(c.req.Box.Task))
	if err != nil {
		return fmt.Errorf("machine: list terminals: %w", err)
	}
	live, err := c.endOlder(ctx, ts)
	if err != nil || live {
		return err
	}
	_, err = c.hc.Run(ctx, c.launch())
	switch {
	case hc.IsKind(err, hc.KindNameTaken):
		return nil
	case err != nil:
		return fmt.Errorf("machine: start the agent: %w", err)
	}
	c.rev = true
	return nil
}

func (c *convergeRun) endOlder(ctx context.Context, ts []hc.Terminal) (bool, error) {
	live := false
	for _, t := range ts {
		run := runOf(t)
		switch {
		case !t.Live():
		case run > c.req.Box.Run:
			return false, superseded("a newer agent run is live")
		case run == c.req.Box.Run:
			live = true
		default:
			if err := c.hc.Kill(ctx, t.Address); err != nil && !hc.IsKind(err, hc.KindNotFound) {
				return false, fmt.Errorf("machine: end run %d: %w", run, err)
			}
		}
	}
	return live, nil
}

// launch is this run's terminal. The plan's env goes here and to clone execs, never into the
// box's own env, so no placeholder ends up in a saved template.
func (c *convergeRun) launch() hc.RunReq {
	r := c.req.Box
	labels := taskLabels(r.Task)
	labels[labelRun] = strconv.FormatUint(uint64(r.Run), 10)
	return hc.RunReq{
		Argv: c.req.Argv, Name: "agent-r" + labels[labelRun], Cwd: domain.RepoDir(c.req.Repos[0]),
		Env: c.req.Env, Labels: labels, NoParent: true, OutliveParent: true, NoNotify: true,
	}
}

func taskLabels(t domain.TaskID) map[string]string {
	return map[string]string{labelTask: strconv.FormatInt(int64(t), 10)}
}

func runOf(t hc.Terminal) domain.AgentRun {
	n, _ := strconv.ParseUint(t.Labels[labelRun], 10, 32)
	return domain.AgentRun(n)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// serveConverge is the box.converge handler: submit or join the job, then stream its phases and
// its end as ConvergeLine values.
func (m *Machine) serveConverge(ctx context.Context, _ link.Meta, req ops.ConvergeReq) (struct{}, link.Bytes, error) {
	key := jobKey{kind: jobConverge, run: req.Box.Run, gen: req.Box.Gen}
	j, err := m.submit(ctx, req.Box.Task, key, req.Budget, func(jctx context.Context, j *job) (any, error) {
		return m.runConverge(jctx, j, req)
	})
	if err != nil {
		return struct{}{}, nil, wire(err)
	}
	return struct{}{}, func(ctx context.Context, s link.Stream) error {
		ctx, cancel := leaving(ctx, s)
		defer cancel()
		enc := json.NewEncoder(s)
		res, err := j.follow(ctx, func(p ops.Phase) error { return enc.Encode(ops.ConvergeLine{Phase: &p}) })
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var le *link.Error
			errors.As(wire(err), &le)
			return enc.Encode(ops.ConvergeLine{Err: le})
		}
		done, _ := res.(ops.Converged)
		return enc.Encode(ops.ConvergeLine{Done: &done})
	}, nil
}
