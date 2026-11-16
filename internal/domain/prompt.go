package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// DoneMarkerPath is the file an unattended agent touches when the whole task is complete.
const DoneMarkerPath = "/run/outpost/done"

// RepoDir is where a repo is checked out in the box.
func RepoDir(r RepoName) string {
	_, name, _ := strings.Cut(string(r), "/")
	return "/workspace/" + name
}

var slashCommand = regexp.MustCompile(`^\s*(/[\w:-]+)[ \t]*(?:\n|$)([\s\S]*)$`)

// LaunchPrompt is the first message a fresh agent gets: what the box is, where the repos are, what
// the run's kind requires of the agent, and the task. A prompt that opens with a slash command
// keeps it first, so the agent still sees a command.
func LaunchPrompt(t Task, r Repo) string {
	command, task := "", t.Spec.Prompt
	if m := slashCommand.FindStringSubmatch(task); m != nil && t.Kind != KindSetup {
		command, task = m[1], strings.TrimSpace(m[2])
	}
	lines := append(boxLines(t.Spec, r), kindLines(t, task)...)
	if command != "" {
		return command + "\n\n" + strings.Join(lines, "\n")
	}
	return strings.Join(lines, "\n")
}

func boxLines(s Spec, r Repo) []string {
	lines := []string{fmt.Sprintf("You are working on %s, checked out at %s.", s.Repo, RepoDir(s.Repo))}
	if len(s.Also) > 0 {
		beside := make([]string, len(s.Also))
		for i, a := range s.Also {
			beside[i] = fmt.Sprintf("%s at %s", a, RepoDir(a))
		}
		lines = append(lines, "These repos are cloned beside it, each pushable: "+strings.Join(beside, ", ")+".")
	}
	lines = append(lines,
		"Your shell commands run on this task's own Linux computer (Ubuntu, you are root): install whatever you need with apt or mise.",
		"Docker and Docker Compose work there.",
		"GitHub access uses the requester's identity through `gh` and git; push branches and open PRs with it.",
		"Put screenshots, logs and other proof you want the requester to see in /run/outpost/artifacts/.",
	)
	if notes := strings.TrimSpace(r.Notes); notes != "" {
		lines = append(lines, "", "Notes from preparing this repo's computer:", notes, "")
	}
	return lines
}

func kindLines(t Task, task string) []string {
	switch {
	case t.Kind == KindSetup:
		return []string{
			"",
			fmt.Sprintf("Your job: prepare this computer so future tasks on %s start ready to work.", t.Spec.Repo),
			"1. Read the README, AGENTS.md or CLAUDE.md, CI workflows, Dockerfiles and compose files to learn the toolchains.",
			"2. Install the toolchains and system packages (prefer mise for language runtimes; apt for system packages), and fetch dependencies so caches are warm.",
			"3. Prove it: build, run the unit tests, and if there is a compose stack bring it up once and check it answers, then bring it down.",
			"4. Write /run/outpost/notes.md: what you installed, how to build, test and start the stack, and any test users or gotchas. Keep it short and factual.",
			"Do not commit, push or open PRs, and do not edit tracked files.",
			"Nobody is watching this run. When everything above is done, run `touch " + DoneMarkerPath + "`, then reply with a one-paragraph summary.",
		}
	case t.Kind.Unattended():
		return []string{
			"",
			"Nobody is watching this run and nobody will answer questions: decide sensibly and note your assumptions. If a person closed a PR or issue you opened, leave it closed: they decided. When the whole task is complete, run `touch " + DoneMarkerPath + "`, then end with a short summary: what you did, links to PRs, and anything left for a human.",
			"", "Task:", task,
		}
	}
	return []string{
		"",
		"When you finish, end with a short summary: what you did, links to PRs, and anything left for a human.",
		"", "Task:", task,
	}
}
