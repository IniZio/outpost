package portstest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Agent launches a fixed agent and reads Summary back as its last message.
type Agent struct {
	mu         sync.Mutex
	summary    string
	launchErr  error
	summaryErr error
}

// SetSummary sets what LastMessage returns.
func (a *Agent) SetSummary(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.summary = s
}

// FailLaunch makes Launch fail with err until it is called with nil.
func (a *Agent) FailLaunch(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.launchErr = err
}

// FailSummary makes LastMessage fail with err until it is called with nil.
func (a *Agent) FailSummary(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.summaryErr = err
}

// Launch returns an argv that names the run and whether it starts or resumes, with the prompt last
// on a fresh start, so a test can tell a start from a resume and see what the agent was told.
func (a *Agent) Launch(_ context.Context, f ports.LaunchFor) (ports.Launch, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.launchErr != nil {
		return ports.Launch{}, a.launchErr
	}
	argv := []string{"agent", "--run", fmt.Sprint(f.Target.Run)}
	if f.Resume {
		argv = append(argv, "--resume")
	} else {
		argv = append(argv, f.Prompt)
	}
	return ports.Launch{
		Argv: argv,
		Env:  map[string]string{"AGENT_KEY": "agent-placeholder"},
		Home: []ports.File{{Path: ".agent/profile", Mode: 0o600, Data: []byte(f.Owner)}},
	}, nil
}

// LastMessage returns the summary.
func (a *Agent) LastMessage(context.Context, ports.BoxFS, ports.Target) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.summary, a.summaryErr
}

// Secrets hands out one placeholder and a CA, or fails. It records the targets it was asked for.
type Secrets struct {
	mu      sync.Mutex
	err     error
	targets []ports.Target
}

// Fail makes Credentials fail with err until it is called with nil.
func (s *Secrets) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Credentials returns GH_TOKEN set to a placeholder and a fake CA.
func (s *Secrets) Credentials(_ context.Context, t ports.Target, _ domain.Login, _ []domain.RepoName) (ports.Creds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return ports.Creds{}, s.err
	}
	s.targets = append(s.targets, t)
	return ports.Creds{Env: map[string]string{"GH_TOKEN": "outpost-placeholder"}, CA: []byte("fake ca")}, nil
}

// Targets lists the targets Credentials handed out for, in order.
func (s *Secrets) Targets() []ports.Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.targets)
}
