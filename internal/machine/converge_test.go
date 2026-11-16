package machine_test

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

func TestFreshConvergeBuildsTheBoxAndStartsTheAgent(t *testing.T) {
	r := newRig(t, nil)
	b := ref(1, 1, 1)
	phases, got, err := r.converge(plan(b, true))
	if err != nil || !got.Revived {
		t.Fatalf("%v %+v", err, got)
	}
	want := []string{"Preparing the box", "Cloning o/main", "Cloning o/also", "Starting the agent"}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases %v, want %v", phases, want)
	}
	k := ports.BoxKey{Task: 1, Gen: 1}
	for _, f := range []string{"/workspace/main/.git/HEAD", "/workspace/also/.git/HEAD", "/root/.config/agent/settings.json", "/root/.outpost-seeded"} {
		if _, ok := r.sb.File(k, f); !ok {
			t.Errorf("%s is missing", f)
		}
	}
	if _, ok := r.sb.File(k, "/workspace/main.partial/.git/HEAD"); ok {
		t.Error("a finished clone leaves no partial")
	}
	checkAgent(t, r.box(b).Core.Terminals(), b)
	if env := r.box(b).Env; env["GH_TOKEN"] != "" || env["BASH_ENV"] == "" {
		t.Fatalf("placeholders stay out of the box env: %v", env)
	}
}

func checkAgent(t *testing.T, ts []hc.Terminal, b ops.BoxRef) {
	t.Helper()
	if len(ts) != 1 || !ts[0].Live() || *ts[0].Name != "agent-r1" ||
		ts[0].Labels["outpost.task"] != "1" || ts[0].Labels["outpost.run"] != "1" {
		t.Fatalf("one agent terminal labelled with the run: %+v", ts)
	}
}

func TestConvergeAgainOnlyReportsTheLiveAgent(t *testing.T) {
	r := newRig(t, nil)
	b := ref(1, 1, 1)
	r.must(plan(b, true))
	if got := r.must(plan(b, true)); got.Revived {
		t.Fatal("a live agent is not revived")
	}
	if n := len(r.box(b).Core.Terminals()); n != 1 || r.sb.Count("exec gh repo clone") != 2 || r.sb.Count("copyin") != 1 {
		t.Fatalf("terminals %d, calls %v", n, r.sb.Calls())
	}
}

func TestResumeOnAStoppedBoxStartsTheNewRun(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	if _, err := link.Call(r.ctx(), r.link, ops.BoxStop, ref(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if got := r.must(plan(ref(1, 2, 1), false)); !got.Revived {
		t.Fatal("the new run starts")
	}
	ts := r.box(ref(1, 2, 1)).Core.Terminals()
	if len(ts) != 1 || ts[0].Labels["outpost.run"] != "2" {
		t.Fatalf("after a restart only the new run's terminal exists: %+v", ts)
	}
}

func TestANewerRunEndsTheOlderAndAnOlderOneIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	r.must(plan(ref(1, 2, 1), false))
	ts := r.box(ref(1, 2, 1)).Core.Terminals()
	if len(ts) != 2 || ts[0].Live() || !ts[1].Live() {
		t.Fatalf("run 1 ended, run 2 live: %+v", ts)
	}
	if _, _, err := r.converge(plan(ref(1, 1, 1), false)); !isRefused(err) || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("an older run is superseded: %v", err)
	}
}

func TestAReplacedBoxKeepsTheWorkAndAnOlderGenIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	r.sb.Crash(ports.BoxKey{Task: 1, Gen: 1}, true)
	r.must(plan(ref(1, 2, 2), false))
	if r.sb.Box(ports.BoxKey{Task: 1, Gen: 1}) != nil || r.sb.Count("exec gh repo clone") != 2 {
		t.Fatalf("gen 1 replaced without recloning: %v", r.sb.Calls())
	}
	if _, _, err := r.converge(plan(ref(1, 3, 1), false)); !isRefused(err) {
		t.Fatalf("gen 1 is gone for good: %v", err)
	}
	if _, _, err := r.converge(plan(ref(9, 1, 1), false)); !isRefused(err) || !strings.Contains(err.Error(), "volumes") {
		t.Fatalf("a resume with no volumes is refused: %v", err)
	}
}

func TestRepeatCallsJoinOneJob(t *testing.T) {
	r := newRig(t, nil)
	r.sb.Hold("gh repo clone o/main")
	var wg sync.WaitGroup
	var first ops.Converged
	var err1 error
	wg.Go(func() { _, first, err1 = r.converge(plan(ref(1, 1, 1), true)) })
	r.waitEntered("gh repo clone o/main /workspace/main.partial -- --filter=blob:none")
	st, latest := r.join(plan(ref(1, 1, 1), true))
	r.sb.Let("gh repo clone o/main")
	_, second, err2 := readConverge(st)
	wg.Wait()
	if err1 != nil || err2 != nil || !first.Revived || !second.Revived || latest != "Cloning o/main" {
		t.Fatalf("both callers see the one job end: %v %v %+v %+v %q", err1, err2, first, second, latest)
	}
	if r.sb.Count("up ") != 1 || len(r.box(ref(1, 1, 1)).Core.Terminals()) != 1 {
		t.Fatalf("one job, one agent: %v", r.sb.Calls())
	}
}

// join opens box.converge and reads its first line, which a joining caller gets at once.
func (r *rig) join(req ops.ConvergeReq) (link.Stream, string) {
	r.t.Helper()
	_, st, err := link.Open(r.ctx(), r.link, ops.BoxConverge, req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = st.Close() })
	var l ops.ConvergeLine
	if err := json.Unmarshal(readLine(st), &l); err != nil || l.Phase == nil {
		r.t.Fatalf("a joining caller first gets the latest phase: %+v %v", l, err)
	}
	return st, l.Phase.Phase
}

// readLine reads one line a byte at a time, so nothing after it is consumed.
func readLine(r io.Reader) []byte {
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := r.Read(b); err != nil || b[0] == '\n' {
			return line
		}
		line = append(line, b[0])
	}
}

func TestACutCallerRejoinsTheRunningJob(t *testing.T) {
	r := newRig(t, nil)
	r.sb.Hold("gh repo clone o/also")
	_, st, err := link.Open(r.ctx(), r.link, ops.BoxConverge, plan(ref(1, 1, 1), true))
	if err != nil {
		t.Fatal(err)
	}
	r.waitEntered("gh repo clone o/also /workspace/also.partial -- --filter=blob:none")
	_ = st.Close()
	again, latest := r.join(plan(ref(1, 1, 1), true))
	r.sb.Let("gh repo clone o/also")
	_, got, err := readConverge(again)
	if err != nil || !got.Revived || latest != "Cloning o/also" {
		t.Fatalf("the rejoin starts at the latest phase: %q %v %+v", latest, err, got)
	}
	if r.sb.Count("up ") != 1 || r.sb.Count("exec gh repo clone o/main") != 1 {
		t.Fatalf("nothing restarts: %v", r.sb.Calls())
	}
}

func TestTheProxyCAIsTrustedBeforeAnyClone(t *testing.T) {
	r := newRig(t, nil)
	req := plan(ref(1, 1, 1), true)
	req.CA = []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n")
	if _, _, err := r.converge(req); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.sb.File(ports.BoxKey{Task: 1, Gen: 1}, "/run/outpost/proxy-ca.crt"); !ok || got != string(req.CA) {
		t.Fatalf("the CA is copied in: %q %v", got, ok)
	}
	calls := r.sb.Calls()
	trust := slices.Index(calls, "exec /opt/outpost/bin/box-trust /run/outpost/proxy-ca.crt")
	clone := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "exec gh repo clone") })
	if trust < 0 || clone < trust {
		t.Fatalf("box-trust runs before the first clone: %v", calls)
	}
}

func TestNoCANoTrustAndAFailedTrustFailsTheConverge(t *testing.T) {
	r := newRig(t, nil)
	if _, _, err := r.converge(plan(ref(1, 1, 1), true)); err != nil || r.sb.Count("exec /opt/outpost/bin/box-trust") != 0 {
		t.Fatalf("no CA, no trust step: %v %v", err, r.sb.Calls())
	}
	r.sb.Fail("/opt/outpost/bin/box-trust", "no bundle")
	req := plan(ref(2, 1, 1), true)
	req.CA = []byte("ca")
	if _, _, err := r.converge(req); err == nil || !strings.Contains(err.Error(), "no bundle") {
		t.Fatalf("a failed trust fails the converge: %v", err)
	}
	if r.sb.Count("exec gh repo clone o/main /workspace/main.partial") != 1 {
		t.Fatalf("nothing is cloned without trust: %v", r.sb.Calls())
	}
}

func TestTheProfileRunsOnceAfterTrustAndBeforeClones(t *testing.T) {
	r := newRig(t, nil)
	req := plan(ref(1, 1, 1), true)
	req.CA = []byte("ca")
	req.Setup = [][]string{{"claude", "plugin", "marketplace", "add", "acme/plugins"}, {"claude", "plugin", "install", "kit@acme"}}
	phases, _, err := r.converge(req)
	if err != nil || !slices.Contains(phases, "Applying the agent profile") {
		t.Fatalf("%v %v", err, phases)
	}
	calls := r.sb.Calls()
	trust := slices.Index(calls, "exec /opt/outpost/bin/box-trust /run/outpost/proxy-ca.crt")
	add := slices.Index(calls, "exec claude plugin marketplace add acme/plugins")
	install := slices.Index(calls, "exec claude plugin install kit@acme")
	clone := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "exec gh repo clone") })
	if trust < 0 || add < trust || install < add || clone < install {
		t.Fatalf("trust, then the profile in order, then clones: %v", calls)
	}
	r.must(plan(ref(1, 2, 1), false))
	req.Box = ref(1, 3, 1)
	req.Fresh = false
	r.must(req)
	if r.sb.Count("exec claude plugin install") != 1 {
		t.Fatalf("the profile runs once per home: %v", r.sb.Calls())
	}
	r.sb.Fail("claude plugin install", "no such plugin")
	req.Box = ref(2, 1, 1)
	req.Fresh = true
	if _, _, err := r.converge(req); err == nil || !strings.Contains(err.Error(), "no such plugin") {
		t.Fatalf("a failed profile fails the converge: %v", err)
	}
}
