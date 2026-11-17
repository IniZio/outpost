package domain_test

import (
	"strings"
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

func TestValidSetupFiles(t *testing.T) {
	ok := map[string]string{domain.SetupBake: "target \"box\" {}", "setup.Dockerfile": "FROM x", domain.AfterCloneFile: "true"}
	if err := domain.ValidSetupFiles(ok); err != nil {
		t.Fatalf("a good revision: %v", err)
	}
	big := map[string]string{domain.SetupBake: strings.Repeat("x", domain.MaxSetupBytes+1)}
	tests := map[string]map[string]string{
		"no bake file":     {"setup.Dockerfile": "FROM x"},
		"a path":           {domain.SetupBake: "", "a/b": ""},
		"a dot name":       {domain.SetupBake: "", "..": ""},
		"a reserved name":  {domain.SetupBake: "", "outpost.hcl": ""},
		"the source dir":   {domain.SetupBake: "", "src": ""},
		"over the size":    big,
		"a hidden escape":  {domain.SetupBake: "", ".env": ""},
		"an empty name":    {domain.SetupBake: "", "": ""},
		"too many files":   manyFiles(domain.MaxSetupFiles + 1),
		"a name with a sp": {domain.SetupBake: "", "a b": ""},
	}
	for name, files := range tests {
		if err := domain.ValidSetupFiles(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func manyFiles(n int) map[string]string {
	m := map[string]string{domain.SetupBake: ""}
	for i := range n {
		m["f"+strings.Repeat("x", i)] = ""
	}
	return m
}

func TestSetupPathsAndSlug(t *testing.T) {
	if got := domain.RepoSlug("acme/app"); got != "acme--app" {
		t.Errorf("slug %q", got)
	}
	if got := domain.MirrorDir("acme/app"); got != "/opt/outpost-mirrors/acme--app.git" {
		t.Errorf("mirror %q", got)
	}
	r := domain.SetupRev{Rev: 3, Image: "i", Inner: "k", Machine: "m"}
	if b := r.Build(); b != (domain.SetupBuild{Rev: 3, Image: "i", Inner: "k", Machine: "m"}) {
		t.Errorf("build %+v", b)
	}
}

func TestLaunchPromptSaysImagesMayStillLoad(t *testing.T) {
	task := newTask(1, domain.KindInteractive, spec())
	task.Machine = "m1"
	repo := domain.Repo{Name: "acme/app", Setup: domain.SetupBuild{Rev: 1, Image: "img", Inner: "k", Machine: "m1"}}
	checkContains(t, domain.LaunchPrompt(task, repo), []string{domain.ImagesReadyPath}, true)
	repo.Setup.Inner = ""
	checkContains(t, domain.LaunchPrompt(task, repo), []string{domain.ImagesReadyPath}, false)
	repo.Setup.Inner, task.Machine = "k", "m2"
	checkContains(t, domain.LaunchPrompt(task, repo), []string{domain.ImagesReadyPath}, false)
}
