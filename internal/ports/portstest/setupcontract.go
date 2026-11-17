package portstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// contractSetups checks setup revisions: numbering per repo, one build at a time unless resumed,
// and a ready build becoming the repo's current one, while a failed one leaves it be.
func contractSetups(t *testing.T, r StoreRig) {
	ctx := context.Background()
	files := map[string]string{domain.SetupBake: `target "box" {}`}
	r.SetRepo(domain.Repo{Name: "acme/app", Notes: "keep me", ReadyImage: "old:1", ReadyOn: "m1"})
	one := putSetup(t, r.Setups, "acme/app", files)
	two := putSetup(t, r.Setups, "acme/app", files)
	other := putSetup(t, r.Setups, "acme/lib", files)
	if one.Rev != 1 || two.Rev != 2 || other.Rev != 1 || two.Status != domain.SetupNew {
		t.Fatalf("revisions are numbered per repo: %d %d %d %s", one.Rev, two.Rev, other.Rev, two.Status)
	}
	if _, err := r.Setups.Setup(ctx, "acme/app", 3); !errors.Is(err, ports.ErrNoSetup) {
		t.Fatalf("an unknown revision: %v", err)
	}
	startTwice(t, r.Setups)
	ready := domain.SetupRev{
		Repo: "acme/app", Rev: 1, Status: domain.SetupReady, Machine: "m1", Image: "obd-setup/acme--app:r1",
		Digest: "sha256:aa", Inner: "k1", Commit: "c1", Log: "ok", BuiltAt: contractT0.Add(time.Minute),
	}
	if err := r.Setups.EndBuild(ctx, ready); err != nil {
		t.Fatal(err)
	}
	failed := domain.SetupRev{Repo: "acme/app", Rev: 2, Status: domain.SetupFailed, Machine: "m1", Log: "step 3 failed", BuiltAt: contractT0}
	if err := r.Setups.EndBuild(ctx, failed); err != nil {
		t.Fatal(err)
	}
	repo, err := r.Store.Repo(ctx, "acme/app")
	if err != nil || repo.Setup != ready.Build() || repo.Notes != "keep me" || repo.ReadyImage != "old:1" {
		t.Fatalf("the ready build is current and the rest stays: %+v %v", repo, err)
	}
	checkSetupList(t, r.Setups, ready)
}

func putSetup(t *testing.T, s ports.Setups, repo domain.RepoName, files map[string]string) domain.SetupRev {
	t.Helper()
	got, err := s.PutSetup(context.Background(), domain.SetupRev{Repo: repo, Files: files, CreatedBy: "ana", CreatedAt: contractT0})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func startTwice(t *testing.T, s ports.Setups) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.StartBuild(ctx, "acme/app", 2, "m1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBuild(ctx, "acme/app", 2, "m1", false); !errors.Is(err, ports.ErrBuilding) {
		t.Fatalf("a second build of a building revision: %v", err)
	}
	if got, err := s.StartBuild(ctx, "acme/app", 2, "m1", true); err != nil || got.Status != domain.SetupBuilding {
		t.Fatalf("a resume: %+v %v", got, err)
	}
	b, err := s.Building(ctx)
	if err != nil || len(b) != 1 || b[0].Rev != 2 || b[0].Machine != "m1" {
		t.Fatalf("building: %+v %v", b, err)
	}
}

func checkSetupList(t *testing.T, s ports.Setups, ready domain.SetupRev) {
	t.Helper()
	revs, err := s.Setups(context.Background(), "acme/app")
	if err != nil || len(revs) != 2 || revs[0].Rev != 2 || revs[1].Rev != 1 {
		t.Fatalf("newest first: %+v %v", revs, err)
	}
	got := revs[1]
	if got.Status != domain.SetupReady || got.Digest != ready.Digest || got.Commit != "c1" || got.Log != "ok" ||
		got.Files[domain.SetupBake] == "" || got.CreatedBy != "ana" || !got.BuiltAt.Equal(ready.BuiltAt) || !got.CreatedAt.Equal(contractT0) {
		t.Fatalf("revision 1 as stored: %+v", got)
	}
	if revs[0].Status != domain.SetupFailed || revs[0].Log != "step 3 failed" {
		t.Fatalf("revision 2 as stored: %+v", revs[0])
	}
	if b, _ := s.Building(context.Background()); len(b) != 0 {
		t.Fatalf("nothing builds: %+v", b)
	}
}
