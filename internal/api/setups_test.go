package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/api"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/lifecycle"
	"github.com/IniZio/outpost/internal/ports"
)

// instantBuilds finishes every build at once.
type instantBuilds struct{}

func (instantBuilds) BuildSetup(_ context.Context, m domain.MachineID, s ports.SetupSpec, _ time.Duration, _ ports.Progress) (ports.SetupBuilt, error) {
	return ports.SetupBuilt{Image: "obd-setup/" + domain.RepoSlug(s.Repo) + ":r1", Digest: "sha256:1", Commit: string(m)}, nil
}

func withSetups(t *testing.T) option {
	t.Helper()
	return func(c *api.Config) {
		store := c.Machines.(machines).s
		s := lifecycle.NewSetups(lifecycle.SetupConfig{Store: store, Setups: store, Builds: instantBuilds{}, Base: "base:1"})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = s.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		c.Setups = s
	}
}

func TestSetupRevisionsArePutListedAndBuilt(t *testing.T) {
	g := newRig(t, withSetups(t))
	c := ctx(t)
	if _, err := g.owner.PutSetup(c, "acme/app", map[string]string{"x": ""}); code(err) != "invalid" {
		t.Fatalf("no bake file: %v", err)
	}
	put, err := g.owner.PutSetup(c, "acme/app", map[string]string{domain.SetupBake: "x", domain.AfterCloneFile: "y"})
	if err != nil || put.Rev != 1 || put.Status != "new" || len(put.Files) != 2 || put.CreatedBy != "ana" {
		t.Fatalf("put %+v %v", put, err)
	}
	if _, err := g.as("bob").BuildSetup(c, "acme/app", 1, ""); code(err) != "no_machine" {
		t.Fatalf("bob has no machine: %v", err)
	}
	if _, err := g.owner.BuildSetup(c, "acme/app", 7, ""); code(err) != "not_found" {
		t.Fatalf("an unknown revision: %v", err)
	}
	b, err := g.owner.BuildSetup(c, "acme/app", 1, "box1")
	if err != nil || b.Status != "building" || b.Machine != "m1" {
		t.Fatalf("build %+v %v", b, err)
	}
	eventually(t, "the build is ready", func() bool {
		revs, err := g.owner.Setups(c, "acme/app")
		return err == nil && len(revs) == 1 && revs[0].Status == "ready" && revs[0].Image == "obd-setup/acme--app:r1"
	})
	repo, err := g.store.Repo(c, "acme/app")
	if err != nil || repo.Setup.Rev != 1 || repo.Setup.Machine != "m1" {
		t.Fatalf("the repo points at the build: %+v %v", repo.Setup, err)
	}
}

func TestSetupRoutesAnswerNotBuiltWithoutSetups(t *testing.T) {
	g := newRig(t)
	if _, err := g.owner.Setups(ctx(t), "acme/app"); code(err) != "not_built" {
		t.Fatalf("%v", err)
	}
}
