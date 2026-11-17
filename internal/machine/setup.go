package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

// builds runs setup builds one at a time, because a build takes the machine's disk and CPU, and
// lets a repeat call for the same revision join the build in flight.
type builds struct {
	turn chan struct{}
	jobs map[string]*job
}

func buildKey(repo domain.RepoName, rev int) string { return string(repo) + "#" + strconv.Itoa(rev) }

// submitBuild joins the revision's build in flight, or starts one under req.Budget that waits for
// the machine's build turn.
func (m *Machine) submitBuild(req ops.SetupBuildReq) *job {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := buildKey(req.Repo, req.Rev)
	if j := m.builds.jobs[k]; j != nil {
		return j
	}
	jctx, cancel := context.WithCancelCause(context.Background())
	jctx, stop := context.WithTimeout(jctx, req.Budget)
	j := &job{cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}), joined: make(chan struct{})}
	m.builds.jobs[k] = j
	go func() {
		defer func() {
			stop()
			m.mu.Lock()
			delete(m.builds.jobs, k)
			m.mu.Unlock()
			close(j.done)
		}()
		j.res, j.err = m.runBuild(jctx, j, req)
	}()
	return j
}

func (m *Machine) runBuild(ctx context.Context, j *job, req ops.SetupBuildReq) (any, error) {
	j.add("Waiting for the machine's build turn", "")
	select {
	case m.builds.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("machine: waiting to build: %w", ctx.Err())
	}
	defer func() { <-m.builds.turn }()
	spec := ports.SetupSpec{
		Repo: req.Repo, Rev: req.Rev, Commit: req.Commit, Files: req.Files, Base: req.Base,
		Profile: req.Profile, Keep: req.Keep,
	}
	got, err := m.cfg.Builder.BuildSetup(ctx, spec, j.add)
	if err != nil {
		return nil, err //nolint:wrapcheck // the builder's message is the reply
	}
	return ops.SetupBuilt(got), nil
}

// serveSetupBuild is the setup.build handler: submit or join the build, then stream its phases
// and its end as SetupLine values.
func (m *Machine) serveSetupBuild(_ context.Context, _ link.Meta, req ops.SetupBuildReq) (struct{}, link.Bytes, error) {
	if m.cfg.Builder == nil {
		return struct{}{}, nil, link.Errorf(link.CodeRefused, "this machine builds no setups")
	}
	j := m.submitBuild(req)
	return struct{}{}, func(ctx context.Context, s link.Stream) error {
		ctx, cancel := leaving(ctx, s)
		defer cancel()
		enc := json.NewEncoder(s)
		res, err := j.follow(ctx, func(p ops.Phase) error { return enc.Encode(ops.SetupLine{Phase: &p}) })
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var le *link.Error
			errors.As(wire(err), &le)
			return enc.Encode(ops.SetupLine{Err: le})
		}
		done, _ := res.(ops.SetupBuilt)
		return enc.Encode(ops.SetupLine{Done: &done})
	}, nil
}
