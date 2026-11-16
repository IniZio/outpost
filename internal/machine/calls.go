package machine

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

// maxRead is what one ReadFile returns: hc's exec keeps 8 MB of stdout, and the reply must fit the
// link's 1 MiB frame as base64.
const maxRead = 700 << 10

// Register serves every op a sandbox machine serves on r.
func (m *Machine) Register(r *link.Registry) {
	link.Handle(r, ops.Ping, func(_ context.Context, _ link.Meta, q ops.PingReq) (ops.PingResp, error) {
		return ops.PingResp(q), nil
	})
	link.Handle(r, ops.Status, func(context.Context, link.Meta, struct{}) (ops.StatusResp, error) {
		return ops.StatusResp{Build: m.cfg.Build, Role: domain.RoleSandbox}, nil
	})
	link.HandleStream(r, ops.BoxConverge, m.serveConverge)
	link.HandleResumable(r, ops.BoxWatch, m.feed.serve)
	link.Handle(r, ops.BoxType, func(ctx context.Context, _ link.Meta, q ops.TypeReq) (struct{}, error) {
		return struct{}{}, m.onRun(ctx, q.Box, func(c *hc.Conn, addr string) error { return c.Send(ctx, addr, q.Text) })
	})
	link.Handle(r, ops.BoxInterrupt, func(ctx context.Context, _ link.Meta, q ops.BoxRef) (struct{}, error) {
		return struct{}{}, m.onRun(ctx, q, func(c *hc.Conn, addr string) error { return c.Interrupt(ctx, addr) })
	})
	link.Handle(r, ops.BoxKill, func(ctx context.Context, _ link.Meta, q ops.BoxRef) (struct{}, error) {
		return struct{}{}, m.onRun(ctx, q, func(c *hc.Conn, addr string) error { return c.Kill(ctx, addr) })
	})
	link.Handle(r, ops.BoxObserve, m.observe)
	link.Handle(r, ops.BoxInspect, m.inspect)
	link.Handle(r, ops.BoxScreen, m.screen)
	link.Handle(r, ops.BoxStop, func(ctx context.Context, _ link.Meta, q ops.BoxRef) (struct{}, error) {
		return struct{}{}, wire(m.exclusive(ctx, q.Task, func(ctx context.Context) error {
			return m.cfg.Sandbox.Stop(ctx, key(q))
		}))
	})
	link.Handle(r, ops.BoxRelease, func(ctx context.Context, _ link.Meta, q ops.BoxRef) (struct{}, error) {
		return struct{}{}, wire(m.exclusive(ctx, q.Task, func(ctx context.Context) error {
			return m.cfg.Sandbox.Release(ctx, q.Task)
		}))
	})
	link.Handle(r, ops.BoxSave, m.save)
	link.Handle(r, ops.BoxRead, m.read)
	link.Handle(r, ops.BoxExists, m.exists)
	link.Handle(r, ops.BoxRemove, func(ctx context.Context, _ link.Meta, q ops.BoxPath) (struct{}, error) {
		_, err := m.inBox(ctx, q.Box, "rm", "-f", "--", q.Path)
		return struct{}{}, wire(err)
	})
}

func key(r ops.BoxRef) ports.BoxKey { return ports.BoxKey{Task: r.Task, Gen: r.Gen} }

// withHC runs fn on a short-lived hc connection to the box. A box that is gone or stopped has no
// hc, which fn's caller reads as "nothing there": the result is false with no error.
func (m *Machine) withHC(ctx context.Context, r ops.BoxRef, fn func(*hc.Conn) error) (bool, error) {
	c, err := m.dial(ctx, key(r))
	if errors.Is(err, ports.ErrNoBox) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close() }()
	return true, fn(c)
}

// runTerminals lists the terminals labelled with exactly r's run.
func runTerminals(ctx context.Context, c *hc.Conn, r ops.BoxRef) ([]hc.Terminal, error) {
	labels := taskLabels(r.Task)
	labels[labelRun] = strconv.FormatUint(uint64(r.Run), 10)
	return c.List(ctx, labels) //nolint:wrapcheck // callers wrap through wire
}

// onRun applies fn to each live terminal of r's run. None is no error.
func (m *Machine) onRun(ctx context.Context, r ops.BoxRef, fn func(c *hc.Conn, addr string) error) error {
	_, err := m.withHC(ctx, r, func(c *hc.Conn) error {
		ts, err := runTerminals(ctx, c, r)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if !t.Live() {
				continue
			}
			if err := gone(fn(c, t.Address)); err != nil {
				return err
			}
		}
		return nil
	})
	return wire(err)
}

// gone drops the errors of a terminal that ended between the list and the call.
func gone(err error) error {
	if hc.IsKind(err, hc.KindNotFound) || hc.IsKind(err, "finished") {
		return nil
	}
	return err
}

func (m *Machine) observe(ctx context.Context, _ link.Meta, r ops.BoxRef) (ops.ObserveResp, error) {
	var out ops.ObserveResp
	_, err := m.withHC(ctx, r, func(c *hc.Conn) error {
		ts, err := runTerminals(ctx, c, r)
		if err != nil || len(ts) == 0 {
			return err
		}
		t := ts[len(ts)-1]
		out = ops.ObserveResp{Present: true, Finished: !t.Live()}
		if !t.Live() {
			out.ExitCode = t.Code()
		}
		return nil
	})
	return out, wire(err)
}

// screen reads the run's newest terminal, live or finished, and keeps the tail that fits a reply.
func (m *Machine) screen(ctx context.Context, _ link.Meta, q ops.ScreenReq) (ops.ScreenResp, error) {
	var out ops.ScreenResp
	_, err := m.withHC(ctx, q.Box, func(c *hc.Conn) error {
		ts, err := runTerminals(ctx, c, q.Box)
		if err != nil || len(ts) == 0 {
			return err
		}
		r, err := c.Read(ctx, ts[len(ts)-1].Address, q.Lines)
		if err != nil {
			return gone(err)
		}
		out.Text = r.Text
		if len(out.Text) > maxRead {
			out.Text = out.Text[len(out.Text)-maxRead:]
		}
		return nil
	})
	return out, wire(err)
}

func (m *Machine) inspect(ctx context.Context, _ link.Meta, r ops.BoxRef) (ops.InspectResp, error) {
	info, err := m.cfg.Sandbox.Inspect(ctx, key(r))
	if errors.Is(err, ports.ErrNoBox) {
		return ops.InspectResp{}, nil
	}
	return ops.InspectResp{Running: info.Running, OOMKilled: info.OOMKilled}, wire(err)
}

// inBox runs argv in the box. It reports false when the box has no hc to ask.
func (m *Machine) inBox(ctx context.Context, r ops.BoxRef, argv ...string) (hc.ExecResult, error) {
	var res hc.ExecResult
	_, err := m.withHC(ctx, r, func(c *hc.Conn) error {
		var err error
		res, err = c.Exec(ctx, hc.ExecReq{Argv: argv, Timeout: 60})
		return err //nolint:wrapcheck // callers wrap through wire
	})
	return res, err
}

func (m *Machine) read(ctx context.Context, _ link.Meta, q ops.BoxPath) (ops.FileResp, error) {
	r, err := m.inBox(ctx, q.Box, "cat", "--", q.Path)
	switch {
	case err != nil:
		return ops.FileResp{}, wire(err)
	case !r.OK():
		return ops.FileResp{}, nil
	case r.Truncated || len(r.Stdout) > maxRead:
		return ops.FileResp{}, link.Errorf(link.CodeRefused, "%s is over %d bytes", q.Path, maxRead)
	}
	return ops.FileResp{Data: r.Stdout, Exists: true}, nil
}

func (m *Machine) exists(ctx context.Context, _ link.Meta, q ops.BoxPath) (ops.FileResp, error) {
	r, err := m.inBox(ctx, q.Box, "test", "-e", q.Path)
	return ops.FileResp{Exists: err == nil && r.OK()}, wire(err)
}

// save submits or joins the save job and waits for its handle. The job outlives a cut call, so
// the caller's retry joins it.
func (m *Machine) save(ctx context.Context, _ link.Meta, q ops.SaveReq) (ops.SaveResp, error) {
	k := jobKey{kind: jobSave, run: q.Box.Run, gen: q.Box.Gen}
	j, err := m.submit(ctx, q.Box.Task, k, q.Budget, func(jctx context.Context, _ *job) (any, error) {
		return m.runSave(jctx, key(q.Box))
	})
	if err != nil {
		return ops.SaveResp{}, wire(err)
	}
	res, err := j.follow(ctx, func(ops.Phase) error { return nil })
	if err != nil {
		return ops.SaveResp{}, wire(err)
	}
	handle, _ := res.(string)
	return ops.SaveResp{Handle: handle}, nil
}

// runSave copies the workspace and the inner images onto the box's own disk, because volumes are
// not part of a commit, commits, and removes the copy whatever happened.
func (m *Machine) runSave(ctx context.Context, k ports.BoxKey) (string, error) {
	c, err := m.dial(ctx, k)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	cr := &convergeRun{m: m, hc: c}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		_ = cr.run(cctx, nil, boxSave, "clean")
	}()
	if err := cr.run(ctx, nil, boxSave, "prepare"); err != nil {
		return "", err
	}
	return m.cfg.Sandbox.Commit(ctx, k) //nolint:wrapcheck // callers wrap through wire
}
