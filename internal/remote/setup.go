package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Builds = (*Boxes)(nil)

// BuildSetup submits or joins the machine's build of the revision and streams its phases into
// report. After a cut it calls again with the same request, which joins the build still running.
func (b *Boxes) BuildSetup(ctx context.Context, m domain.MachineID, s ports.SetupSpec, budget time.Duration, report ports.Progress) (ports.SetupBuilt, error) {
	req := ops.SetupBuildReq{
		Repo: s.Repo, Rev: s.Rev, Commit: s.Commit, Files: s.Files, Base: s.Base, Profile: s.Profile, Keep: s.Keep,
		Budget: budget,
	}
	for {
		got, err := b.build(ctx, m, req, report)
		if !rejoinable(err) || ctx.Err() != nil {
			return got, wrap("setup build", err)
		}
		if err := b.pause(ctx); err != nil {
			return ports.SetupBuilt{}, wrap("setup build", err)
		}
	}
}

func (b *Boxes) build(ctx context.Context, m domain.MachineID, req ops.SetupBuildReq, report ports.Progress) (ports.SetupBuilt, error) {
	_, st, err := link.Open(ctx, b.cfg.Links.Link(m), ops.SetupBuild, req)
	if err != nil {
		return ports.SetupBuilt{}, err //nolint:wrapcheck // BuildSetup wraps
	}
	defer func() { _ = st.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	sc := bufio.NewScanner(st)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l ops.SetupLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return ports.SetupBuilt{}, fmt.Errorf("remote: setup line: %w", err)
		}
		switch {
		case l.Phase != nil && report != nil:
			report(l.Phase.Phase, l.Phase.Note)
		case l.Done != nil:
			return ports.SetupBuilt(*l.Done), nil
		case l.Err != nil:
			return ports.SetupBuilt{}, l.Err
		}
	}
	if ctx.Err() != nil {
		return ports.SetupBuilt{}, fmt.Errorf("remote: %w", ctx.Err())
	}
	return ports.SetupBuilt{}, errCut
}
