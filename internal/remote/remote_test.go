package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/link/linktest"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/remote"
)

// agent serves the box ops with scripted answers.
type agent struct {
	converges atomic.Int32
	builds    atomic.Int32
	budget    atomic.Int64
	watchers  atomic.Int32
}

func (a *agent) registry() *link.Registry {
	r := link.NewRegistry(ops.Catalogue(), link.ServedByAgent, 0)
	link.Handle(r, ops.Ping, func(_ context.Context, _ link.Meta, q ops.PingReq) (ops.PingResp, error) { return ops.PingResp(q), nil })
	link.Handle(r, ops.Status, func(context.Context, link.Meta, struct{}) (ops.StatusResp, error) { return ops.StatusResp{}, nil })
	link.HandleStream(r, ops.BoxConverge, a.converge)
	link.HandleStream(r, ops.SetupBuild, a.build)
	link.HandleResumable(r, ops.BoxWatch, a.watch)
	unit := func(context.Context, link.Meta, ops.BoxRef) (struct{}, error) { return struct{}{}, nil }
	for _, op := range []link.Op[ops.BoxRef, struct{}]{ops.BoxInterrupt, ops.BoxKill, ops.BoxStop, ops.BoxRelease} {
		link.Handle(r, op, unit)
	}
	link.Handle(r, ops.BoxType, func(context.Context, link.Meta, ops.TypeReq) (struct{}, error) { return struct{}{}, nil })
	link.Handle(r, ops.BoxObserve, func(context.Context, link.Meta, ops.BoxRef) (ops.ObserveResp, error) {
		return ops.ObserveResp{Present: true, Finished: true, ExitCode: 2}, nil
	})
	link.Handle(r, ops.BoxInspect, func(context.Context, link.Meta, ops.BoxRef) (ops.InspectResp, error) {
		return ops.InspectResp{OOMKilled: true}, nil
	})
	link.Handle(r, ops.BoxScreen, func(_ context.Context, _ link.Meta, q ops.ScreenReq) (ops.ScreenResp, error) {
		return ops.ScreenResp{Text: fmt.Sprintf("lines=%d", q.Lines)}, nil
	})
	link.Handle(r, ops.BoxSave, func(_ context.Context, _ link.Meta, q ops.SaveReq) (ops.SaveResp, error) {
		return ops.SaveResp{Handle: "tpl"}, nil
	})
	link.Handle(r, ops.BoxRead, func(_ context.Context, _ link.Meta, q ops.BoxPath) (ops.FileResp, error) {
		return ops.FileResp{Data: []byte("x"), Exists: q.Path == "/here"}, nil
	})
	link.Handle(r, ops.BoxExists, func(_ context.Context, _ link.Meta, q ops.BoxPath) (ops.FileResp, error) {
		return ops.FileResp{Exists: q.Path == "/here"}, nil
	})
	link.Handle(r, ops.BoxRemove, func(context.Context, link.Meta, ops.BoxPath) (struct{}, error) { return struct{}{}, nil })
	return r
}

// converge cuts its first stream after one phase, and finishes the second.
func (a *agent) converge(_ context.Context, _ link.Meta, q ops.ConvergeReq) (struct{}, link.Bytes, error) {
	n := a.converges.Add(1)
	a.budget.Store(int64(q.Budget))
	if q.Box.Run == 9 {
		return struct{}{}, nil, link.Errorf(link.CodeRefused, "superseded")
	}
	return struct{}{}, func(_ context.Context, s link.Stream) error {
		enc := json.NewEncoder(s)
		_ = enc.Encode(ops.ConvergeLine{Phase: &ops.Phase{Phase: "Cloning"}})
		if n == 1 {
			return nil
		}
		return enc.Encode(ops.ConvergeLine{Done: &ops.Converged{Revived: true}})
	}, nil
}

// build cuts its first stream after a phase, fails revision 9, and finishes the rest.
func (a *agent) build(_ context.Context, _ link.Meta, q ops.SetupBuildReq) (struct{}, link.Bytes, error) {
	n := a.builds.Add(1)
	return struct{}{}, func(_ context.Context, s link.Stream) error {
		enc := json.NewEncoder(s)
		_ = enc.Encode(ops.SetupLine{Phase: &ops.Phase{Phase: "Building"}})
		switch {
		case n == 1:
			return nil
		case q.Rev == 9:
			return enc.Encode(ops.SetupLine{Err: &link.Error{Code: link.CodeInternal, Msg: "step 2 failed"}})
		}
		return enc.Encode(ops.SetupLine{Done: &ops.SetupBuilt{Image: "img:r1", Digest: "sha256:1", Inner: "k", Commit: "c"}})
	}, nil
}

func (a *agent) watch(context.Context, link.Meta, struct{}) (struct{}, link.Bytes, error) {
	a.watchers.Add(1)
	return struct{}{}, func(ctx context.Context, s link.Stream) error {
		enc := json.NewEncoder(s)
		_ = enc.Encode(ops.WatchLine{Task: 3, Kind: ops.WatchReport, Run: 2, Status: domain.HCIdle})
		_ = enc.Encode(ops.WatchLine{Task: 3, Kind: "later"})
		<-ctx.Done()
		return nil
	}, nil
}

func setup(t *testing.T) (*agent, *linktest.Rig, *remote.Boxes) {
	t.Helper()
	a := &agent{}
	server := link.NewRegistry(ops.Catalogue(), link.ServedByServer, 0)
	link.HandleResumable(server, ops.Report, func(context.Context, link.Meta, struct{}) (struct{}, link.Bytes, error) {
		return struct{}{}, nil, nil
	})
	rig := linktest.New(t, linktest.Options{Server: server, Agent: a.registry()})
	b, err := remote.New(remote.Config{Links: rig.Hub, Rejoin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return a, rig, b
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestConvergeRejoinsACutJob(t *testing.T) {
	a, rig, b := setup(t)
	var phases []string
	p := ports.Plan{
		Target: ports.Target{Task: 1, Machine: rig.Machine, Run: 1, Gen: 1}, Image: "img", Repos: []domain.RepoName{"o/r"},
		Launch: ports.Launch{Argv: []string{"agent"}, Home: []ports.File{{Path: "a", Data: []byte("b")}}},
	}
	got, err := b.Converge(ctx(t), p, func(phase, _ string) { phases = append(phases, phase) })
	if err != nil || !got.Revived || a.converges.Load() != 2 || len(phases) != 2 {
		t.Fatalf("%+v %v after %d calls, phases %v", got, err, a.converges.Load(), phases)
	}
	if d := time.Duration(a.budget.Load()); d > 10*time.Second || d < 9*time.Second {
		t.Fatalf("the job's budget is what is left of the caller's: %v", d)
	}
	p.Target.Run = 9
	if _, err := b.Converge(ctx(t), p, nil); !errors.Is(err, &link.Error{Code: link.CodeRefused}) {
		t.Fatalf("a refusal is final: %v", err)
	}
}

func TestBuildSetupRejoinsACutBuild(t *testing.T) {
	a, rig, b := setup(t)
	var phases []string
	s := ports.SetupSpec{Repo: "o/r", Rev: 1, Files: map[string]string{domain.SetupBake: "x"}, Base: "base:1"}
	got, err := b.BuildSetup(ctx(t), rig.Machine, s, time.Hour, func(phase, _ string) { phases = append(phases, phase) })
	if err != nil || got != (ports.SetupBuilt{Image: "img:r1", Digest: "sha256:1", Inner: "k", Commit: "c"}) || len(phases) != 2 {
		t.Fatalf("%+v %v, phases %v", got, err, phases)
	}
	s.Rev = 9
	if _, err := b.BuildSetup(ctx(t), rig.Machine, s, time.Hour, nil); err == nil || !strings.Contains(err.Error(), "step 2 failed") {
		t.Fatalf("a failed build is final: %v", err)
	}
	if n := a.builds.Load(); n != 3 {
		t.Fatalf("calls %d", n)
	}
}

func TestUnaryCallsConvert(t *testing.T) {
	_, rig, b := setup(t)
	tg := ports.Target{Task: 1, Machine: rig.Machine, Run: 1, Gen: 1}
	c := ctx(t)
	for _, err := range []error{b.Type(c, tg, "x"), b.Interrupt(c, tg), b.Kill(c, tg), b.Stop(c, tg), b.Release(c, tg), b.Remove(c, tg, "/here")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	checkReads(t, b, tg)
}

func checkReads(t *testing.T, b *remote.Boxes, tg ports.Target) {
	c := ctx(t)
	if o, err := b.Observe(c, tg); err != nil || o != (domain.AgentObservation{Present: true, Finished: true, ExitCode: 2}) {
		t.Fatalf("%+v %v", o, err)
	}
	if s, err := b.Inspect(c, tg); err != nil || !s.OOMKilled {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := b.Screen(c, tg, 40); err != nil || s != "lines=40" {
		t.Fatalf("%q %v", s, err)
	}
	if tpl, err := b.SaveTemplate(c, tg); err != nil || tpl != (ports.Template{Handle: "tpl", Machine: tg.Machine}) {
		t.Fatalf("%+v %v", tpl, err)
	}
	checkFiles(t, b, tg)
}

func checkFiles(t *testing.T, b *remote.Boxes, tg ports.Target) {
	c := ctx(t)
	if ok, err := b.Exists(c, tg, "/here"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := b.ReadFile(c, tg, "/gone"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file is fs.ErrNotExist: %v", err)
	}
	if d, err := b.ReadFile(c, tg, "/here"); err != nil || string(d) != "x" {
		t.Fatal(d, err)
	}
}

func TestWatchDeliversSignalsAndEndsOnRevoke(t *testing.T) {
	_, rig, b := setup(t)
	got := make(chan domain.Signal, 4)
	done := make(chan error, 1)
	go func() {
		done <- b.Watch(context.Background(), rig.Machine, func(task domain.TaskID, s domain.Signal) {
			if task == 3 {
				got <- s
			}
		})
	}()
	select {
	case s := <-got:
		if s != (domain.AgentReport{Run: 2, Status: domain.HCIdle}) {
			t.Fatalf("%#v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no signal")
	}
	rig.Revoke(rig.Hub)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoke ends Watch")
	}
	if _, err := remote.New(remote.Config{}); err == nil {
		t.Fatal("links are required")
	}
}

func TestAScreenOfAnOfflineMachineIsNotRunning(t *testing.T) {
	_, _, b := setup(t)
	if _, err := b.Screen(ctx(t), ports.Target{Task: 1, Machine: "gone", Run: 1, Gen: 1}, 0); !errors.Is(err, ports.ErrNotRunning) {
		t.Fatalf("an unlinked machine: %v", err)
	}
}
