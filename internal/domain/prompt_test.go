package domain_test

import (
	"strings"
	"testing"

	"github.com/IniZio/outpost/internal/domain"
)

func TestLaunchPrompt(t *testing.T) {
	also := spec()
	also.Also = []domain.RepoName{"acme/docs"}
	tests := []struct {
		name    string
		kind    domain.Kind
		spec    domain.Spec
		repo    domain.Repo
		want    []string
		notWant []string
	}{
		{
			name: "an interactive task names its repos and the task",
			kind: domain.KindInteractive, spec: also,
			want:    []string{"checked out at /workspace/app", "acme/docs at /workspace/docs", "upgrade the dependencies"},
			notWant: []string{domain.DoneMarkerPath},
		},
		{
			name: "an unattended run is told to mark itself done",
			kind: domain.KindScheduled, spec: domain.Spec{Repo: "acme/app", Prompt: "bump deps", Schedule: 1},
			want: []string{"Nobody is watching this run", "touch " + domain.DoneMarkerPath, "bump deps"},
		},
		{
			name: "a setup task prepares the computer and ignores its prompt",
			kind: domain.KindSetup, spec: domain.Spec{Repo: "acme/app", Prompt: "/ignored"},
			want:    []string{"prepare this computer", "touch " + domain.DoneMarkerPath, "Do not commit"},
			notWant: []string{"Task:"},
		},
		{
			name: "the repo's notes reach the agent",
			kind: domain.KindInteractive, spec: spec(), repo: domain.Repo{Notes: "  yarn 4 needs corepack\n"},
			want: []string{"Notes from preparing this repo's computer:", "yarn 4 needs corepack"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.LaunchPrompt(newTask(1, tc.kind, tc.spec), tc.repo)
			checkContains(t, got, tc.want, true)
			checkContains(t, got, tc.notWant, false)
		})
	}
}

func checkContains(t *testing.T, got string, parts []string, want bool) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(got, p) != want {
			t.Errorf("prompt contains %q = %v, want %v:\n%s", p, !want, want, got)
		}
	}
}

func TestLaunchPromptKeepsASlashCommandFirst(t *testing.T) {
	task := newTask(1, domain.KindInteractive, domain.Spec{Repo: "acme/app", Prompt: "/review\nthe last commit"})
	got := domain.LaunchPrompt(task, domain.Repo{})
	if !strings.HasPrefix(got, "/review\n\nYou are working on acme/app") || !strings.HasSuffix(got, "Task:\nthe last commit") {
		t.Errorf("prompt = %q", got)
	}
}

func TestRepoDirIsTheRepoNameUnderWorkspace(t *testing.T) {
	if got := domain.RepoDir("acme/app"); got != "/workspace/app" {
		t.Errorf("RepoDir = %q", got)
	}
}
