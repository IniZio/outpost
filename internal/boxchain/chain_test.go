package boxchain_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/link/linktest"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/machine/machinetest"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
	"github.com/IniZio/outpost/internal/remote"
)

type chain struct {
	sb  *machinetest.Sandbox
	rig *linktest.Rig
	rp  portstest.BoxesRig
}

// newChain joins remote, a real link, a machine and a sandbox. With a nil sandbox it uses the fake.
func newChain(t *testing.T, sb ports.Sandbox, tune func(*machine.Config)) chain {
	t.Helper()
	cfg := machine.Config{Sandbox: sb, Redial: 10 * time.Millisecond, FollowWait: 10 * time.Second}
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
	rig := linktest.New(t, linktest.Options{Server: server, Agent: agent})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	boxes, err := remote.New(remote.Config{Links: rig.Hub, Rejoin: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return chain{rig: rig, rp: portstest.BoxesRig{Boxes: boxes, Machine: rig.Machine}}
}

func fakeChain(t *testing.T) chain {
	sb := machinetest.New()
	c := newChain(t, sb, nil)
	c.sb = sb
	c.rp.Report = func(tg ports.Target, s domain.HCStatus) {
		sb.Box(ports.BoxKey{Task: tg.Task, Gen: tg.Gen}).Core.SetStatus(terminal(tg), words[s], "")
	}
	c.rp.Touch = func(tg ports.Target, p string) { sb.Write(ports.BoxKey{Task: tg.Task, Gen: tg.Gen}, p, "") }
	return c
}

var words = map[domain.HCStatus]string{
	domain.HCWorking: "working", domain.HCIdle: "idle", domain.HCDone: "done",
	domain.HCBlocked: "blocked", domain.HCError: "error",
}

func terminal(tg ports.Target) string { return "agent-r" + strconv.FormatUint(uint64(tg.Run), 10) }

func TestTheRealChainMeetsTheBoxesContract(t *testing.T) {
	portstest.BoxesContract(t, func(t *testing.T) portstest.BoxesRig { return fakeChain(t).rp })
}

func TestAConvergeSurvivesTheAgentRestarting(t *testing.T) {
	c := fakeChain(t)
	c.sb.Hold("gh repo clone o/main")
	tg := ports.Target{Task: 1, Machine: c.rig.Machine, Run: 1, Gen: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type result struct {
		got ports.Converged
		err error
	}
	res := make(chan result, 1)
	go func() {
		p := ports.Plan{Target: tg, Image: "ob-base:1", Fresh: true, Repos: []domain.RepoName{"o/main"}, Launch: ports.Launch{Argv: []string{"agent"}}}
		got, err := c.rp.Boxes.Converge(ctx, p, nil)
		res <- result{got, err}
	}()
	select {
	case <-c.sb.Entered():
	case <-ctx.Done():
		t.Fatal("the clone never started")
	}
	c.rig.Stop()
	c.rig.Restart()
	c.rig.WaitLinked(c.rig.Hub)
	c.sb.Let("gh repo clone o/main")
	r := <-res
	if r.err != nil || !r.got.Revived {
		t.Fatalf("the caller rejoins the job across the cut: %+v %v", r.got, r.err)
	}
	if c.sb.Count("up ") != 1 || c.sb.Count("exec gh repo clone") != 1 {
		t.Fatalf("nothing restarted on the machine: %v", c.sb.Calls())
	}
}
