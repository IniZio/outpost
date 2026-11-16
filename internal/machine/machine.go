// Package machine serves the agent's fixed operation set over the link and drives a sandbox
// provider. The machine keeps no state file: boxes and generations come from the provider, agent
// runs and status from hc terminal labels, and step progress from the task's volumes. The only
// memory is the per-task job table and the watch feed's level cache, both rebuilt on start.
package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ports"
)

// Paths inside every box. The tools image is mounted at /opt/outpost.
const (
	hcBin    = "/opt/outpost/bin/hc"
	oomReset = "/opt/outpost/bin/oom0"
	boxSeed  = "/opt/outpost/bin/box-seed"
	boxSave  = "/opt/outpost/bin/box-save"
	boxTrust = "/opt/outpost/bin/box-trust"
	caDir    = "/run/outpost"
	caName   = "proxy-ca.crt"
	seedDir  = "/var/lib/outpost-seed"
	homeMark = "/root/.outpost-seeded"
)

// Labels on hc terminals. Run lives here and not on the box, because a run moves within one box
// generation on sleep and wake, and container labels never change.
const (
	labelTask = "outpost.task"
	labelRun  = "outpost.run"
)

// Config wires a machine.
type Config struct {
	Sandbox ports.Sandbox
	Clock   domain.Clock
	// Clone is the argv that clones repo into dir inside the box. It runs with the plan's env.
	Clone func(repo domain.RepoName, dir string) []string
	// FollowWait bounds how long a start waits for a caller before it launches the agent.
	FollowWait time.Duration
	// Redial is the pause between attempts to follow a box's hc.
	Redial time.Duration
	// Build is what machine.status reports.
	Build string
}

// GHClone clones with the GitHub CLI, which the proxy's placeholders authenticate.
func GHClone(repo domain.RepoName, dir string) []string {
	return []string{"gh", "repo", "clone", string(repo), dir, "--", "--filter=blob:none"}
}

// Machine serves the box ops.
type Machine struct {
	cfg   Config
	mu    sync.Mutex
	slots map[domain.TaskID]*slot
	feed  *feed
}

// New returns a machine. Run must be running for box.watch to see anything.
func New(cfg Config) (*Machine, error) {
	if cfg.Sandbox == nil {
		return nil, errors.New("machine: a sandbox is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = domain.SystemClock{}
	}
	if cfg.Clone == nil {
		cfg.Clone = GHClone
	}
	if cfg.FollowWait == 0 {
		cfg.FollowWait = 2 * time.Minute
	}
	if cfg.Redial == 0 {
		cfg.Redial = time.Second
	}
	m := &Machine{cfg: cfg, slots: map[domain.TaskID]*slot{}}
	m.feed = newFeed(m)
	return m, nil
}

// Run follows the provider's events and every running box's hc until ctx ends.
func (m *Machine) Run(ctx context.Context) error { return m.feed.run(ctx) }

func (m *Machine) dial(ctx context.Context, k ports.BoxKey) (*hc.Conn, error) {
	rwc, err := m.cfg.Sandbox.Dial(ctx, k, []string{hcBin, "connect"})
	if err != nil {
		return nil, fmt.Errorf("machine: connect to hc: %w", err)
	}
	c := hc.Dial(rwc)
	if err := c.Hello(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("machine: %w", err)
	}
	return c, nil
}

// deadline is the seconds left before ctx's deadline, for hc's exec timeout, at most hc's limit. Zero is
// none.
func (m *Machine) deadline(ctx context.Context) float64 {
	if dl, ok := ctx.Deadline(); ok {
		return min(max(dl.Sub(m.cfg.Clock.Now()).Seconds(), 1), maxExec)
	}
	return 0
}

// maxExec is the longest timeout hc's machine.exec accepts, in seconds.
const maxExec = 600

func superseded(why string) error { return link.Errorf(link.CodeRefused, "superseded: %s", why) }

// wire turns a provider or machine error into the code the server sees.
func wire(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ports.ErrSuperseded):
		return superseded(err.Error())
	case errors.Is(err, ports.ErrNoVolumes), errors.Is(err, ports.ErrNoImage), errors.Is(err, errOrphaned):
		return link.Errorf(link.CodeRefused, "%v", err)
	}
	var le *link.Error
	if errors.As(err, &le) {
		return le
	}
	return link.Errorf(link.CodeInternal, "%v", err)
}

// leaving cancels the returned context when the peer closes its end of s, which is how a
// stream handler learns that its caller went away.
func leaving(ctx context.Context, s io.Reader) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = io.Copy(io.Discard, s)
		cancel()
	}()
	return ctx, cancel
}
