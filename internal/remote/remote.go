// Package remote is ports.Boxes over the machine link, on the server. It holds no state and no
// behaviour beyond the wire: convert, call, convert back, and rejoin a machine job when the link
// was cut. The machine agent does the work next to the box.
package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

// Links is the part of link.Hub remote uses.
type Links interface {
	Link(m domain.MachineID) *link.Link
	Revoked(m domain.MachineID) <-chan struct{}
}

// Config wires remote. Zero durations take the defaults.
type Config struct {
	Links Links
	Clock domain.Clock
	// StartBudget is a converge job's budget when the caller's context has no deadline.
	StartBudget time.Duration
	// SaveBudget is a save job's budget when the caller's context has no deadline.
	SaveBudget time.Duration
	// Rejoin is the pause before calling a cut job again.
	Rejoin time.Duration
}

// Boxes implements ports.Boxes.
type Boxes struct{ cfg Config }

var _ ports.Boxes = (*Boxes)(nil)

// New returns Boxes over cfg.Links.
func New(cfg Config) (*Boxes, error) {
	if cfg.Links == nil {
		return nil, errors.New("remote: links are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = domain.SystemClock{}
	}
	if cfg.StartBudget == 0 {
		cfg.StartBudget = 40 * time.Minute
	}
	if cfg.SaveBudget == 0 {
		cfg.SaveBudget = 45 * time.Minute
	}
	if cfg.Rejoin == 0 {
		cfg.Rejoin = time.Second
	}
	return &Boxes{cfg: cfg}, nil
}

func ref(t ports.Target) ops.BoxRef { return ops.BoxRef{Task: t.Task, Run: t.Run, Gen: t.Gen} }

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("remote: %s: %w", what, err)
}

var errCut = errors.New("remote: the stream ended without a result")

// rejoinable is a cut, or a link that is not back yet. The machine's job runs on meanwhile, so
// calling again joins it.
func rejoinable(err error) bool {
	return errors.Is(err, link.ErrLinkLost) || errors.Is(err, link.ErrOffline) || errors.Is(err, errCut) ||
		errors.Is(err, link.ErrDraining) || errors.Is(err, &link.Error{Code: link.CodeDraining})
}

func (b *Boxes) pause(ctx context.Context) error {
	t := time.NewTimer(b.cfg.Rejoin)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("remote: %w", ctx.Err())
	}
}

// budget is what is left of ctx's deadline, or def.
func (b *Boxes) budget(ctx context.Context, def time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return max(dl.Sub(b.cfg.Clock.Now()), time.Second)
	}
	return def
}

// Converge submits or joins the machine's converge job and streams its phases into report. After
// a cut it calls again with the same plan, which joins the job still running on the machine.
func (b *Boxes) Converge(ctx context.Context, p ports.Plan, report ports.Progress) (ports.Converged, error) {
	req := ops.ConvergeReq{
		Box: ref(p.Target), Image: p.Image, Fresh: p.Fresh, Repos: p.Repos, Env: p.Env, CA: p.CA,
		Argv: p.Launch.Argv, Setup: p.Launch.Setup, Budget: b.budget(ctx, b.cfg.StartBudget),
	}
	for _, f := range p.Launch.Home {
		req.Home = append(req.Home, ops.HomeFile{Path: f.Path, Mode: f.Mode, Data: f.Data})
	}
	for {
		got, err := b.converge(ctx, p.Target.Machine, req, report)
		if !rejoinable(err) || ctx.Err() != nil {
			return got, wrap("converge", err)
		}
		if err := b.pause(ctx); err != nil {
			return ports.Converged{}, wrap("converge", err)
		}
	}
}

func (b *Boxes) converge(ctx context.Context, m domain.MachineID, req ops.ConvergeReq, report ports.Progress) (ports.Converged, error) {
	_, st, err := link.Open(ctx, b.cfg.Links.Link(m), ops.BoxConverge, req)
	if err != nil {
		return ports.Converged{}, err //nolint:wrapcheck // Converge wraps
	}
	defer func() { _ = st.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	sc := bufio.NewScanner(st)
	for sc.Scan() {
		var l ops.ConvergeLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return ports.Converged{}, fmt.Errorf("remote: converge line: %w", err)
		}
		switch {
		case l.Phase != nil && report != nil:
			report(l.Phase.Phase, l.Phase.Note)
		case l.Done != nil:
			return ports.Converged{Revived: l.Done.Revived}, nil
		case l.Err != nil:
			return ports.Converged{}, l.Err
		}
	}
	if ctx.Err() != nil {
		return ports.Converged{}, fmt.Errorf("remote: %w", ctx.Err())
	}
	return ports.Converged{}, errCut
}

// SaveTemplate calls again after a cut: the machine's save job is joined, not restarted.
func (b *Boxes) SaveTemplate(ctx context.Context, t ports.Target) (ports.Template, error) {
	req := ops.SaveReq{Box: ref(t), Budget: b.budget(ctx, b.cfg.SaveBudget)}
	for {
		r, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxSave, req)
		if !rejoinable(err) || ctx.Err() != nil {
			return ports.Template{Handle: r.Handle, Machine: t.Machine}, wrap("save template", err)
		}
		if err := b.pause(ctx); err != nil {
			return ports.Template{}, wrap("save template", err)
		}
	}
}

// Type sends text to the agent terminal of t.Run.
func (b *Boxes) Type(ctx context.Context, t ports.Target, text string) error {
	_, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxType, ops.TypeReq{Box: ref(t), Text: text})
	return wrap("type", err)
}

// Interrupt presses the stop key in the agent terminal of t.Run.
func (b *Boxes) Interrupt(ctx context.Context, t ports.Target) error {
	_, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxInterrupt, ref(t))
	return wrap("interrupt", err)
}

// Kill ends the agent terminal of t.Run.
func (b *Boxes) Kill(ctx context.Context, t ports.Target) error {
	_, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxKill, ref(t))
	return wrap("kill", err)
}

// Observe reports the agent terminal of t.Run.
func (b *Boxes) Observe(ctx context.Context, t ports.Target) (domain.AgentObservation, error) {
	r, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxObserve, ref(t))
	return domain.AgentObservation{Present: r.Present, Finished: r.Finished, ExitCode: r.ExitCode}, wrap("observe", err)
}

// Screen reads the agent terminal of t.Run as text.
func (b *Boxes) Screen(ctx context.Context, t ports.Target, lines int) (string, error) {
	r, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxScreen, ops.ScreenReq{Box: ref(t), Lines: lines})
	return r.Text, wrap("screen", err)
}

// Inspect reports the settled box of t.Gen.
func (b *Boxes) Inspect(ctx context.Context, t ports.Target) (ports.BoxState, error) {
	r, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxInspect, ref(t))
	return ports.BoxState{Running: r.Running, OOMKilled: r.OOMKilled}, wrap("inspect", err)
}

// Stop stops the box, keeping its volumes.
func (b *Boxes) Stop(ctx context.Context, t ports.Target) error {
	_, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxStop, ref(t))
	return wrap("stop", err)
}

// Release removes the box and its volumes.
func (b *Boxes) Release(ctx context.Context, t ports.Target) error {
	_, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxRelease, ref(t))
	return wrap("release", err)
}

// ReadFile reads a file in the box. A missing file is fs.ErrNotExist.
func (b *Boxes) ReadFile(ctx context.Context, t ports.Target, path string) ([]byte, error) {
	r, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxRead, ops.BoxPath{Box: ref(t), Path: path})
	if err == nil && !r.Exists {
		err = fs.ErrNotExist
	}
	return r.Data, wrap("read "+path, err)
}

// Exists reports whether a file exists in the box.
func (b *Boxes) Exists(ctx context.Context, t ports.Target, path string) (bool, error) {
	r, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxExists, ops.BoxPath{Box: ref(t), Path: path})
	return r.Exists, wrap("exists "+path, err)
}

// Remove removes a file in the box.
func (b *Boxes) Remove(ctx context.Context, t ports.Target, path string) error {
	_, err := link.Call(ctx, b.cfg.Links.Link(t.Machine), ops.BoxRemove, ops.BoxPath{Box: ref(t), Path: path})
	return wrap("remove "+path, err)
}

// Watch holds box.watch on m and turns each line into a signal for sink. Hold reopens it after a
// relink or a server drain, and every opening starts with levels. It returns early, with nil, only
// when m is revoked.
func (b *Boxes) Watch(ctx context.Context, m domain.MachineID, sink ports.Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	revoked := b.cfg.Links.Revoked(m)
	go func() {
		select {
		case <-revoked:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := link.Hold(ctx, b.cfg.Links.Link(m), ops.BoxWatch, struct{}{}, func(_ context.Context, _ struct{}, st link.Stream) {
		deliver(st, sink)
	})
	if ctx.Err() != nil {
		return nil
	}
	return wrap("watch", err)
}

func deliver(st link.Stream, sink ports.Sink) {
	sc := bufio.NewScanner(st)
	for sc.Scan() {
		var l ops.WatchLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if s, ok := l.Signal(); ok {
			sink(l.Task, s)
		}
	}
}
