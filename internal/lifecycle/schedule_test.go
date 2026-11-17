package lifecycle

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// The plan's credentials are asked for the plan's own target, and carry the proxy CA.
func TestCredentialsAreBoundToThePlan(t *testing.T) {
	g := newRig(t, newStore())
	g.created(domain.KindScheduled)
	g.pass()
	p := calls(g.boxes, "Converge")[0].Plan
	if string(p.CA) != "fake ca" || !slices.Equal(g.secrets.Targets(), []ports.Target{p.Target}) {
		t.Fatalf("credentials for %v, CA %q, plan target %v", g.secrets.Targets(), p.CA, p.Target)
	}
}

// A schedule fires, the task is placed, starts, works, marks itself done and is cleaned up a day
// later.
func TestScheduledRunToDone(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindScheduled)
	g.pass()
	g.wantStatus(id, domain.StatusWorking)

	p := calls(g.boxes, "Converge")[0].Plan
	if !p.Fresh || p.Target.Run != 1 || p.Target.Gen != 1 || p.Image != "base:1" || p.Target.Machine != machine {
		t.Fatalf("fresh start plan %+v", p)
	}
	if p.Env["GH_TOKEN"] == "" || p.Env["AGENT_KEY"] == "" || !slices.Equal(p.Repos, []domain.RepoName{"acme/app"}) {
		t.Fatalf("the plan needs the credential placeholders and the repo: %+v", p)
	}
	if prompt := p.Launch.Argv[len(p.Launch.Argv)-1]; !strings.Contains(prompt, "upgrade deps") || !strings.Contains(prompt, domain.DoneMarkerPath) {
		t.Fatalf("the agent was told %q", prompt)
	}

	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})
	g.clock.Advance(5 * time.Minute)
	g.boxes.Touch(id, domain.DoneMarkerPath)
	g.agent.SetSummary("Bumped 4 deps, opened a PR")
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCIdle})
	g.settle()

	tk := g.task(id)
	if tk.Status() != domain.StatusDone || tk.Life.(domain.Ended).Summary != "Bumped 4 deps, opened a PR" {
		t.Fatalf("ended as %v with %+v", tk.Status(), tk.Life)
	}
	if len(calls(g.boxes, "Stop")) != 1 {
		t.Fatal("box not stopped at the end")
	}
	g.advance(24 * time.Hour)
	if len(calls(g.boxes, "Release")) != 1 {
		t.Fatal("box not released after KeepFinished")
	}
}

// The box dies under a working agent. The task stays working, the box comes back as a new
// generation after the replace delay, and the agent is told.
func TestBoxLoss(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindScheduled)
	g.pass()
	g.feed(id, domain.AgentReport{Run: 1, Status: domain.HCWorking})

	g.feed(id, domain.BoxEvent{Gen: 1, Kind: domain.BoxDied})
	g.feed(id, domain.AgentExited{Run: 1, Code: 137})
	g.settle()
	g.wantStatus(id, domain.StatusWorking)

	g.advance(domain.DefaultPolicy().ReplaceDelay)
	cv := calls(g.boxes, "Converge")
	if p := cv[len(cv)-1].Plan; p.Fresh || p.Target.Gen != 2 || p.Target.Run != 2 || len(cv) != 2 {
		t.Fatalf("replacement plan %+v", p)
	}
	ty := calls(g.boxes, "Type")
	if len(ty) != 1 || ty[0].Target.Run != 2 || ty[0].Text != domain.BoxBackNote("stopped unexpectedly") {
		t.Fatalf("box-back note %+v", ty)
	}
}

// A failed start is not left holding a slot for ever: the domain gives up on it.
func TestAStartThatNeverReturnsIsGivenUpOn(t *testing.T) {
	g := newRig(t, newStore())
	id := g.created(domain.KindInteractive)
	g.boxes.Hold(id)
	if err := g.r.schedule(g.ctx); err != nil {
		t.Fatal(err)
	}
	<-g.boxes.Entered()

	g.clock.Advance(domain.DefaultPolicy().StartBudget)
	if n, err := g.r.RunDue(g.ctx); err != nil || n != 1 {
		t.Fatalf("RunDue = %d, %v", n, err)
	}
	g.settle()
	g.wantStatus(id, domain.StatusFailed)
	if got := methods(g.boxes); !slices.Equal(got, []string{"Converge", "ConvergeReturned", "Release"}) {
		t.Fatalf("calls %v: the start is cancelled and then released", got)
	}
}

// A queued task goes to the machine that holds its repo's template, and the rest start from the
// base image elsewhere.
func TestPlacementPrefersTheMachineHoldingTheTemplate(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	store.AddMachine(domain.Machine{ID: "m2", Owner: owner, Role: domain.RoleSandbox, Slots: 1}, true)
	store.SetRepo(domain.Repo{Name: "acme/app", ReadyImage: "ready-9", ReadyOn: "m2"})
	first, second := g.created(domain.KindInteractive), g.created(domain.KindInteractive)
	g.pass()

	got := map[domain.TaskID]string{}
	for _, c := range calls(g.boxes, "Converge") {
		got[c.Target.Task] = string(c.Target.Machine) + " " + c.Plan.Image
	}
	if got[first] != "m2 ready-9" || got[second] != "m1 base:1" {
		t.Fatalf("placements %v", got)
	}
}

// A repo's setup build wins over its template: placement and the plan follow it.
func TestPlacementAndPlanFollowTheSetupBuild(t *testing.T) {
	store := newStore()
	g := newRig(t, store)
	store.AddMachine(domain.Machine{ID: "m2", Owner: owner, Role: domain.RoleSandbox, Slots: 1}, true)
	build := domain.SetupBuild{Rev: 3, Image: "obd-setup/acme--app:r3", Inner: "0123456789abcdef", Machine: "m2"}
	store.SetRepo(domain.Repo{Name: "acme/app", ReadyImage: "ready-9", ReadyOn: machine, Setup: build})
	id := g.created(domain.KindInteractive)
	g.pass()
	c := calls(g.boxes, "Converge")
	if len(c) != 1 || c[0].Target.Task != id || c[0].Target.Machine != "m2" || c[0].Plan.Image != build.Image || c[0].Plan.Inner != build.Inner {
		t.Fatalf("converges %+v", c)
	}
}

func TestPlacementRespectsCapacityMachinesAndOwners(t *testing.T) {
	store := newStore()
	capacity := domain.Capacity{Max: 1}
	g := newRigWith(t, store, func(c *Config) { c.Capacity = func() domain.Capacity { return capacity } })
	store.SetOnline(machine, false)
	one := g.created(domain.KindInteractive)
	two := g.created(domain.KindInteractive)
	g.pass()
	g.wantStatus(one, domain.StatusQueued)

	store.SetOnline(machine, true)
	capacity.Hold = true
	g.pass()
	g.wantStatus(one, domain.StatusQueued)

	capacity.Hold = false
	g.pass()
	g.wantStatus(one, domain.StatusWorking)
	g.wantStatus(two, domain.StatusQueued)

	if _, err := g.r.Do(g.ctx, one, domain.UserStop{By: owner}); err != nil {
		t.Fatal(err)
	}
	g.settle()
	g.pass()
	g.wantStatus(two, domain.StatusWorking)
}
