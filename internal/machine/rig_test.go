package machine_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/link/linktest"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/machine/machinetest"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

type rig struct {
	t    *testing.T
	sb   *machinetest.Sandbox
	link *link.Link
	m    *machine.Machine
}

func newRig(t *testing.T, tune func(*machine.Config)) *rig {
	t.Helper()
	sb := machinetest.New()
	cfg := machine.Config{Sandbox: sb, FollowWait: 3 * time.Second, Redial: 5 * time.Millisecond, Build: "test"}
	if tune != nil {
		tune(&cfg)
	}
	m, err := machine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	agent := link.NewRegistry(ops.Catalogue(), link.ServedByAgent, 0)
	m.Register(agent)
	server := link.NewRegistry(ops.Catalogue(), link.ServedByServer, 0)
	link.HandleResumable(server, ops.Report, func(context.Context, link.Meta, struct{}) (struct{}, link.Bytes, error) {
		return struct{}{}, nil, nil
	})
	lr := linktest.New(t, linktest.Options{Server: server, Agent: agent})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return &rig{t: t, sb: sb, link: lr.Hub.Link(lr.Machine), m: m}
}

func (r *rig) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	r.t.Cleanup(cancel)
	return ctx
}

func ref(task domain.TaskID, run domain.AgentRun, gen domain.BoxGen) ops.BoxRef {
	return ops.BoxRef{Task: task, Run: run, Gen: gen}
}

func plan(b ops.BoxRef, fresh bool) ops.ConvergeReq {
	return ops.ConvergeReq{
		Box: b, Image: "ob-base:1", Fresh: fresh, Repos: []domain.RepoName{"o/main", "o/also"},
		Env: map[string]string{"GH_TOKEN": "placeholder"}, Argv: []string{"agent", "--go"},
		Home: []ops.HomeFile{{Path: ".config/agent/settings.json", Data: []byte(`{"a":1}`)}}, Budget: time.Minute,
	}
}

// converge runs box.converge to its end and returns its phases and result.
func (r *rig) converge(req ops.ConvergeReq) ([]string, ops.Converged, error) {
	_, st, err := link.Open(r.ctx(), r.link, ops.BoxConverge, req)
	if err != nil {
		return nil, ops.Converged{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = st.Close() }()
	return readConverge(st)
}

func readConverge(st link.Stream) ([]string, ops.Converged, error) {
	var phases []string
	sc := bufio.NewScanner(st)
	for sc.Scan() {
		var l ops.ConvergeLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return phases, ops.Converged{}, fmt.Errorf("line: %w", err)
		}
		switch {
		case l.Phase != nil:
			phases = append(phases, l.Phase.Phase)
		case l.Done != nil:
			return phases, *l.Done, nil
		case l.Err != nil:
			return phases, ops.Converged{}, l.Err
		}
	}
	return phases, ops.Converged{}, errors.New("stream cut")
}

func (r *rig) must(req ops.ConvergeReq) ops.Converged {
	r.t.Helper()
	_, got, err := r.converge(req)
	if err != nil {
		r.t.Fatalf("converge %+v: %v", req.Box, err)
	}
	return got
}

func (r *rig) box(b ops.BoxRef) *machinetest.Box {
	return r.sb.Box(ports.BoxKey{Task: b.Task, Gen: b.Gen})
}

func (r *rig) waitEntered(want string) {
	r.t.Helper()
	select {
	case got := <-r.sb.Entered():
		if got != want {
			r.t.Fatalf("held %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		r.t.Fatalf("nothing reached %q", want)
	}
}

func isRefused(err error) bool { return errors.Is(err, &link.Error{Code: link.CodeRefused}) }
