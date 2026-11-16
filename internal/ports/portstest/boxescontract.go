package portstest

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// BoxesRig is a Boxes under test with the hooks a check needs to act inside a box. The fake and
// the real chain (remote over the link to a machine) each build one, and BoxesContract runs the
// same checks on both, so lifecycle's tests against the fake hold for the real thing.
type BoxesRig struct {
	Boxes   ports.Boxes
	Machine domain.MachineID
	// Report makes the agent terminal of t.Run report a status, as its program would.
	Report func(t ports.Target, s domain.HCStatus)
	// Touch creates a file in the task's box.
	Touch func(t ports.Target, path string)
	// Image is the box image plans name, or "" for a placeholder the fake accepts.
	Image string
}

// BoxesRigFor wraps the in-memory Boxes.
func BoxesRigFor(b *Boxes) BoxesRig {
	return BoxesRig{
		Boxes: b, Machine: "m1",
		Report: func(t ports.Target, s domain.HCStatus) {
			b.Signal(t.Machine, t.Task, domain.AgentReport{Run: t.Run, Status: s})
		},
		Touch: func(t ports.Target, path string) { b.Touch(t.Task, path) },
	}
}

// BoxesContract checks the behaviour ports.Boxes promises.
func BoxesContract(t *testing.T, newRig func(t *testing.T) BoxesRig) {
	t.Helper()
	cases := map[string]func(*testing.T, BoxesRig){
		"ConvergeRevivesOnce":  boxesConvergeOnce,
		"StopThenResume":       boxesStopResume,
		"KillAimsAtItsRun":     boxesKillAims,
		"ReleaseIsIdempotent":  boxesRelease,
		"Files":                boxesFiles,
		"WatchDeliversReports": boxesWatch,
		"MissingBoxIsNoError":  boxesMissing,
		"ScreenShowsTyping":    boxesScreen,
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t, newRig(t))
		})
	}
}

func boxesCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func target(r BoxesRig, task domain.TaskID, run domain.AgentRun, gen domain.BoxGen) ports.Target {
	return ports.Target{Task: task, Machine: r.Machine, Run: run, Gen: gen}
}

func boxPlan(r BoxesRig, tg ports.Target, fresh bool) ports.Plan {
	image := r.Image
	if image == "" {
		image = "ob-base:1"
	}
	return ports.Plan{
		Target: tg, Image: image, Fresh: fresh, Repos: []domain.RepoName{"o/main"},
		Env: map[string]string{"GH_TOKEN": "placeholder"}, Launch: ports.Launch{Argv: []string{"agent"}},
	}
}

func converge(t *testing.T, r BoxesRig, tg ports.Target, fresh bool) ports.Converged {
	t.Helper()
	got, err := r.Boxes.Converge(boxesCtx(t), boxPlan(r, tg, fresh), nil)
	if err != nil {
		t.Fatalf("converge %+v: %v", tg, err)
	}
	return got
}

func running(t *testing.T, r BoxesRig, tg ports.Target) bool {
	t.Helper()
	s, err := r.Boxes.Inspect(boxesCtx(t), tg)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return s.Running
}

func boxesConvergeOnce(t *testing.T, r BoxesRig) {
	tg := target(r, 1, 1, 1)
	var phases int
	got, err := r.Boxes.Converge(boxesCtx(t), boxPlan(r, tg, true), func(string, string) { phases++ })
	if err != nil || !got.Revived || phases == 0 {
		t.Fatalf("a fresh start starts the agent and reports phases: %+v %v %d", got, err, phases)
	}
	if !running(t, r, tg) {
		t.Fatal("the box runs")
	}
	if converge(t, r, tg, true).Revived {
		t.Fatal("a second converge finds the agent alive")
	}
}

func boxesStopResume(t *testing.T, r BoxesRig) {
	tg := target(r, 2, 1, 1)
	converge(t, r, tg, true)
	for range 2 {
		if err := r.Boxes.Stop(boxesCtx(t), tg); err != nil {
			t.Fatalf("stop is idempotent: %v", err)
		}
	}
	if running(t, r, tg) {
		t.Fatal("a stopped box is not running")
	}
	next := target(r, 2, 2, 1)
	if !converge(t, r, next, false).Revived || !running(t, r, next) {
		t.Fatal("a resume starts the new run in the same box")
	}
}

func boxesKillAims(t *testing.T, r BoxesRig) {
	converge(t, r, target(r, 3, 1, 1), true)
	newer := target(r, 3, 2, 1)
	converge(t, r, newer, false)
	if err := r.Boxes.Kill(boxesCtx(t), target(r, 3, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if converge(t, r, newer, false).Revived {
		t.Fatal("Kill under run 1 never kills run 2")
	}
	if err := r.Boxes.Kill(boxesCtx(t), newer); err != nil {
		t.Fatal(err)
	}
	if !converge(t, r, newer, false).Revived {
		t.Fatal("after its own Kill, run 2 is started again")
	}
}

func boxesRelease(t *testing.T, r BoxesRig) {
	if err := r.Boxes.Release(boxesCtx(t), target(r, 4, 0, 0)); err != nil {
		t.Fatalf("releasing a task that never had a box: %v", err)
	}
	tg := target(r, 4, 1, 1)
	converge(t, r, tg, true)
	for range 2 {
		if err := r.Boxes.Release(boxesCtx(t), tg); err != nil {
			t.Fatal(err)
		}
	}
	if running(t, r, tg) {
		t.Fatal("a released box is gone")
	}
}

func boxesFiles(t *testing.T, r BoxesRig) {
	tg := target(r, 5, 1, 1)
	converge(t, r, tg, true)
	ctx := boxesCtx(t)
	if ok, err := r.Boxes.Exists(ctx, tg, domain.DoneMarkerPath); err != nil || ok {
		t.Fatalf("no marker yet: %v %v", ok, err)
	}
	r.Touch(tg, domain.DoneMarkerPath)
	if ok, err := r.Boxes.Exists(ctx, tg, domain.DoneMarkerPath); err != nil || !ok {
		t.Fatalf("the marker shows: %v %v", ok, err)
	}
	if _, err := r.Boxes.ReadFile(ctx, tg, domain.DoneMarkerPath); err != nil {
		t.Fatal(err)
	}
	if err := r.Boxes.Remove(ctx, tg, domain.DoneMarkerPath); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.Boxes.Exists(ctx, tg, domain.DoneMarkerPath); ok {
		t.Fatal("removed")
	}
	if _, err := r.Boxes.ReadFile(ctx, tg, domain.DoneMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file is fs.ErrNotExist: %v", err)
	}
}

func boxesWatch(t *testing.T, r BoxesRig) {
	tg := target(r, 6, 1, 1)
	converge(t, r, tg, true)
	ctx, cancel := context.WithCancel(boxesCtx(t))
	got := make(chan domain.Signal, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Boxes.Watch(ctx, r.Machine, func(task domain.TaskID, s domain.Signal) {
			if task == tg.Task {
				got <- s
			}
		})
	}()
	defer func() { cancel(); <-done }()
	r.Report(tg, domain.HCWorking)
	want := domain.AgentReport{Run: 1, Status: domain.HCWorking}
	for {
		select {
		case s := <-got:
			if s == want {
				return
			}
		case <-ctx.Done():
			t.Fatalf("Watch never delivered %+v", want)
		}
	}
}

func boxesMissing(t *testing.T, r BoxesRig) {
	tg := target(r, 7, 1, 1)
	ctx := boxesCtx(t)
	if err := r.Boxes.Type(ctx, tg, "hello"); err != nil {
		t.Fatalf("typing to no box: %v", err)
	}
	if err := r.Boxes.Interrupt(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if o, err := r.Boxes.Observe(ctx, tg); err != nil || o.Present {
		t.Fatalf("no box is no terminal: %+v %v", o, err)
	}
	if s, err := r.Boxes.Inspect(ctx, tg); err != nil || s.Running {
		t.Fatalf("no box is not running: %+v %v", s, err)
	}
	if s, err := r.Boxes.Screen(ctx, tg, 10); err != nil || s != "" {
		t.Fatalf("no box has an empty screen: %q %v", s, err)
	}
}

func boxesScreen(t *testing.T, r BoxesRig) {
	tg := target(r, 8, 1, 1)
	converge(t, r, tg, true)
	ctx := boxesCtx(t)
	if err := r.Boxes.Type(ctx, tg, "look-at-me"); err != nil {
		t.Fatal(err)
	}
	for {
		s, err := r.Boxes.Screen(ctx, tg, 50)
		if err != nil {
			t.Fatalf("screen: %v", err)
		}
		if strings.Contains(s, "look-at-me") {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the screen never showed what was typed: %q", s)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
