package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

// builds is a fake ports.Builds: each call sends its spec and waits for an answer.
type builds struct {
	specs chan ports.SetupSpec
	ends  chan error
}

func (b *builds) BuildSetup(ctx context.Context, m domain.MachineID, s ports.SetupSpec, _ time.Duration, report ports.Progress) (ports.SetupBuilt, error) {
	report("Building", "")
	b.specs <- s
	select {
	case err := <-b.ends:
		if err != nil {
			return ports.SetupBuilt{}, err
		}
		return ports.SetupBuilt{Image: "img:r" + string(rune('0'+s.Rev)), Digest: "sha256:1", Inner: "k", Commit: "c" + string(m)}, nil
	case <-ctx.Done():
		return ports.SetupBuilt{}, fmt.Errorf("builds: %w", ctx.Err())
	}
}

type setupRig struct {
	t      *testing.T
	ctx    context.Context
	store  *portstest.Store
	builds *builds
	s      *Setups
}

func newSetupRig(t *testing.T) *setupRig {
	t.Helper()
	store := portstest.NewStore()
	store.AddMachine(domain.Machine{ID: machine, Name: "box-1", Owner: owner, Role: domain.RoleSandbox, Slots: 1}, true)
	store.AddMachine(domain.Machine{ID: "mac", Name: "mac", Owner: owner, Role: domain.RoleTool, Slots: 1}, true)
	b := &builds{specs: make(chan ports.SetupSpec, 4), ends: make(chan error, 4)}
	s := NewSetups(SetupConfig{
		Store: store, Setups: store, Builds: b, Clock: domain.NewManualClock(t0), Base: "base:1",
		Profile: [][]string{{"claude", "plugin", "install", "p"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return &setupRig{t: t, ctx: ctx, store: store, builds: b, s: s}
}

var bake = map[string]string{domain.SetupBake: `target "box" {}`}

// built answers the build in flight with err and waits for the revision to leave building.
func (g *setupRig) built(rev int, err error) domain.SetupRev {
	g.t.Helper()
	g.builds.ends <- err
	for range 200 {
		r, _ := g.store.Setup(g.ctx, "acme/app", rev)
		if r.Status != domain.SetupBuilding {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.t.Fatalf("revision %d is still building", rev)
	return domain.SetupRev{}
}

func TestASetupBuildBecomesTheRepoSetupAndKeepsThePreviousOne(t *testing.T) {
	g := newSetupRig(t)
	if _, err := g.s.Put(g.ctx, "acme/app", map[string]string{"x": ""}, owner); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a revision without a bake file: %v", err)
	}
	g.put(2)
	if _, err := g.s.Build(g.ctx, "acme/app", 1, "bob", ""); !errors.Is(err, ErrNoBuilder) {
		t.Fatalf("only the owner's machines build: %v", err)
	}
	if r, err := g.s.Build(g.ctx, "acme/app", 1, owner, "box-1"); err != nil || r.Status != domain.SetupBuilding || r.Machine != machine {
		t.Fatalf("build: %+v %v", r, err)
	}
	if spec := <-g.builds.specs; spec.Base != "base:1" || len(spec.Profile) != 1 || spec.Keep != nil {
		t.Fatalf("spec %+v", spec)
	}
	if r := g.built(1, nil); r.Status != domain.SetupReady || r.Image != "img:r1" || r.Commit != "cm1" {
		t.Fatalf("revision 1: %+v", r)
	}
	g.failSecond()
}

func (g *setupRig) put(n int) {
	g.t.Helper()
	for range n {
		if _, err := g.s.Put(g.ctx, "acme/app", bake, owner); err != nil {
			g.t.Fatal(err)
		}
	}
}

// failSecond builds revision 2, which keeps revision 1 and fails, leaving revision 1 current.
func (g *setupRig) failSecond() {
	t := g.t
	t.Helper()
	if _, err := g.s.Build(g.ctx, "acme/app", 2, owner, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Build(g.ctx, "acme/app", 2, owner, ""); !errors.Is(err, ErrBuilding) {
		t.Fatalf("a second build of a building revision: %v", err)
	}
	if spec := <-g.builds.specs; len(spec.Keep) != 1 || spec.Keep[0] != 1 {
		t.Fatalf("the current revision is kept: %+v", spec.Keep)
	}
	if r := g.built(2, errors.New("step 4 failed")); r.Status != domain.SetupFailed || r.Log == "" {
		t.Fatalf("revision 2: %+v", r)
	}
	if repo, _ := g.store.Repo(g.ctx, "acme/app"); repo.Setup.Rev != 1 {
		t.Fatalf("a failed build leaves the current one: %+v", repo.Setup)
	}
	if revs, err := g.s.List(g.ctx, "acme/app"); err != nil || len(revs) != 2 || revs[0].Rev != 2 {
		t.Fatalf("list %+v %v", revs, err)
	}
}

func TestABuildLeftBuildingIsResumedAtStart(t *testing.T) {
	store := portstest.NewStore()
	if _, err := store.PutSetup(context.Background(), domain.SetupRev{Repo: "acme/app", Files: bake}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartBuild(context.Background(), "acme/app", 1, machine, false); err != nil {
		t.Fatal(err)
	}
	b := &builds{specs: make(chan ports.SetupSpec, 1), ends: make(chan error, 1)}
	s := NewSetups(SetupConfig{Store: store, Setups: store, Builds: b, Base: "base:1"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	if spec := <-b.specs; spec.Rev != 1 {
		t.Fatalf("resumed %+v", spec)
	}
	cancel()
	<-done
	if r, _ := store.Setup(context.Background(), "acme/app", 1); r.Status != domain.SetupBuilding {
		t.Fatalf("a build cut by shutdown stays building for the next start: %+v", r)
	}
}
