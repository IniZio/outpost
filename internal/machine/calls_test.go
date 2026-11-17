package machine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

func TestStopWaitsForAConvergeItPreempts(t *testing.T) {
	r := newRig(t, nil)
	r.sb.Hold("gh repo clone o/main")
	errs := make(chan error, 1)
	go func() { _, _, err := r.converge(plan(ref(1, 1, 1), true)); errs <- err }()
	r.waitEntered("gh repo clone o/main /workspace/main.partial -- --filter=blob:none")
	if _, err := link.Call(r.ctx(), r.link, ops.BoxStop, ref(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err == nil {
		t.Fatal("the preempted converge fails")
	}
	calls := r.sb.Calls()
	if calls[len(calls)-1] != "stop t1 g1" || r.sb.Count("exec mv") != 0 {
		t.Fatalf("stop runs after the job ended, and the clone never lands: %v", calls)
	}
	if b := r.box(ref(1, 1, 1)); b.Info.Running || len(b.Core.Terminals()) != 0 {
		t.Fatal("no agent starts in a stopped box")
	}
}

func TestAnOrphanedStartNeverLaunchesTheAgent(t *testing.T) {
	r := newRig(t, func(c *machine.Config) { c.FollowWait = 50 * time.Millisecond })
	r.sb.Hold("gh repo clone o/also")
	_, st, err := link.Open(r.ctx(), r.link, ops.BoxConverge, plan(ref(1, 1, 1), true))
	if err != nil {
		t.Fatal(err)
	}
	r.waitEntered("gh repo clone o/also /workspace/also.partial -- --filter=blob:none")
	_ = st.Close()
	time.Sleep(20 * time.Millisecond)
	r.sb.Let("gh repo clone o/also")
	deadline := time.After(5 * time.Second)
	for r.sb.Count("exec mv /workspace/also.partial") == 0 {
		select {
		case <-deadline:
			t.Fatal("the clone never finished")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(r.box(ref(1, 1, 1)).Core.Terminals()); n != 0 {
		t.Fatalf("a start nobody waits for launches no agent, got %d terminals", n)
	}
	if got := r.must(plan(ref(1, 1, 1), true)); !got.Revived || r.sb.Count("exec gh repo clone") != 2 {
		t.Fatalf("a later ask finishes the start without recloning: %+v %v", got, r.sb.Calls())
	}
}

func TestTerminalCallsAimAtTheirRunOnly(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	r.must(plan(ref(1, 2, 1), false))
	core := r.box(ref(1, 2, 1)).Core
	ctx := r.ctx()
	ok := func(_ struct{}, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ok(link.Call(ctx, r.link, ops.BoxType, ops.TypeReq{Box: ref(1, 2, 1), Text: "hi"}))
	ok(link.Call(ctx, r.link, ops.BoxType, ops.TypeReq{Box: ref(1, 1, 1), Text: "old"}))
	ok(link.Call(ctx, r.link, ops.BoxInterrupt, ref(1, 2, 1)))
	ok(link.Call(ctx, r.link, ops.BoxKill, ref(1, 1, 1)))
	ok(link.Call(ctx, r.link, ops.BoxKill, ref(7, 1, 1)))
	if got := core.Sent("agent-r2"); len(got) != 1 || got[0] != "hi" || core.Interrupts("agent-r2") != 1 {
		t.Fatalf("sent %v", got)
	}
	if ts := core.Terminals(); !ts[1].Live() {
		t.Fatal("Kill under run 1 never kills run 2")
	}
	core.SetStatus("agent-r2", "blocked", "")
	if _, err := link.Call(ctx, r.link, ops.BoxType, ops.TypeReq{Box: ref(1, 2, 1), Text: "yes"}); err != nil || len(core.Sent("agent-r2")) != 2 {
		t.Fatalf("the adapter types to a blocked agent: %v", err)
	}
}

func TestObserveReportsTheRunsTerminal(t *testing.T) {
	r := newRig(t, nil)
	ctx := r.ctx()
	if o, err := link.Call(ctx, r.link, ops.BoxObserve, ref(1, 1, 1)); err != nil || o.Present {
		t.Fatalf("no box: %+v %v", o, err)
	}
	r.must(plan(ref(1, 1, 1), true))
	if o, _ := link.Call(ctx, r.link, ops.BoxObserve, ref(1, 1, 1)); !o.Present || o.Finished {
		t.Fatalf("live: %+v", o)
	}
	r.box(ref(1, 1, 1)).Core.Exit("agent-r1", 3)
	if o, _ := link.Call(ctx, r.link, ops.BoxObserve, ref(1, 1, 1)); !o.Present || !o.Finished || o.ExitCode != 3 {
		t.Fatalf("finished: %+v", o)
	}
	if o, _ := link.Call(ctx, r.link, ops.BoxObserve, ref(1, 2, 1)); o.Present {
		t.Fatalf("another run has no terminal: %+v", o)
	}
}

func TestInspectAndFilesFollowTheBox(t *testing.T) {
	r := newRig(t, nil)
	ctx := r.ctx()
	b := ref(1, 1, 1)
	if s, err := link.Call(ctx, r.link, ops.BoxInspect, b); err != nil || s.Running {
		t.Fatalf("no box is not running: %+v %v", s, err)
	}
	r.must(plan(b, true))
	r.sb.Crash(ports.BoxKey{Task: 1, Gen: 1}, true)
	if s, _ := link.Call(ctx, r.link, ops.BoxInspect, b); s.Running || !s.OOMKilled {
		t.Fatalf("an OOM kill shows: %+v", s)
	}
	if f, err := link.Call(ctx, r.link, ops.BoxExists, ops.BoxPath{Box: b, Path: domain.DoneMarkerPath}); err != nil || f.Exists {
		t.Fatalf("a stopped box has no files to ask about: %+v %v", f, err)
	}
	r.must(plan(ref(1, 2, 1), false))
	b = ref(1, 2, 1)
	r.sb.Write(ports.BoxKey{Task: 1, Gen: 1}, domain.DoneMarkerPath, "done")
	checkFiles(t, r, b)
}

func checkFiles(t *testing.T, r *rig, b ops.BoxRef) {
	ctx := r.ctx()
	p := ops.BoxPath{Box: b, Path: domain.DoneMarkerPath}
	if f, err := link.Call(ctx, r.link, ops.BoxRead, p); err != nil || !f.Exists || string(f.Data) != "done" {
		t.Fatalf("read: %+v %v", f, err)
	}
	tail := ops.BoxPath{Box: b, Path: domain.DoneMarkerPath, Tail: true}
	if f, err := link.Call(ctx, r.link, ops.BoxRead, tail); err != nil || !f.Exists || string(f.Data) != "done" {
		t.Fatalf("tail read: %+v %v", f, err)
	}
	if _, err := link.Call(ctx, r.link, ops.BoxRemove, p); err != nil {
		t.Fatal(err)
	}
	if f, _ := link.Call(ctx, r.link, ops.BoxExists, p); f.Exists {
		t.Fatal("removed")
	}
	if f, err := link.Call(ctx, r.link, ops.BoxRead, p); err != nil || f.Exists {
		t.Fatalf("reading a missing file says so: %+v %v", f, err)
	}
}

func TestSaveCommitsOnceAndCleansUp(t *testing.T) {
	r := newRig(t, nil)
	b := ref(1, 1, 1)
	r.must(plan(b, true))
	got, err := link.Call(r.ctx(), r.link, ops.BoxSave, ops.SaveReq{Box: b, Budget: time.Minute})
	if err != nil || got.Handle != "ready-t1-g1" {
		t.Fatalf("%+v %v", got, err)
	}
	calls := strings.Join(r.sb.Calls(), "\n")
	if !strings.Contains(calls, "exec /opt/outpost/bin/box-save prepare\ncommit t1 g1\nexec /opt/outpost/bin/box-save clean") {
		t.Fatalf("prepare, commit, clean: %v", calls)
	}
	r.sb.Fail("/opt/outpost/bin/box-save prepare", "disk full")
	if _, err := link.Call(r.ctx(), r.link, ops.BoxSave, ops.SaveReq{Box: b, Budget: time.Minute}); err == nil ||
		!strings.Contains(err.Error(), "disk full") || r.sb.Count("exec /opt/outpost/bin/box-save clean") != 2 {
		t.Fatalf("a failed save still cleans up: %v", err)
	}
}

func TestPingAndStatus(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if p, err := link.Call(ctx, r.link, ops.Ping, ops.PingReq{Token: "x"}); err != nil || p.Token != "x" {
		t.Fatal(err)
	}
	if s, err := link.Call(ctx, r.link, ops.Status, struct{}{}); err != nil || s.Build != "test" || s.Role != domain.RoleSandbox {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := machine.New(machine.Config{}); err == nil {
		t.Fatal("a machine needs a sandbox")
	}
}
