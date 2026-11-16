package machine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ops"
)

type jobKind uint8

const (
	jobConverge jobKind = iota + 1
	jobSave
)

type jobKey struct {
	kind jobKind
	run  domain.AgentRun
	gen  domain.BoxGen
}

// newer reports whether k aims at a later agent run or box generation than o.
func (k jobKey) newer(o jobKey) bool {
	return k.gen > o.gen || (k.gen == o.gen && k.run > o.run)
}

// job is one shape operation in flight. It runs on its own context under its budget, so it
// outlives a caller cut off by a relink. A repeat call with the same key joins it.
type job struct {
	key    jobKey
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu        sync.Mutex
	log       []ops.Phase
	wake      chan struct{}
	followers int
	joined    chan struct{}
	res       any
	err       error
}

func (j *job) add(phase, note string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ops.Phase{Phase: phase, Note: note})
	close(j.wake)
	j.wake = make(chan struct{})
}

// follow emits the job's phases, from the latest one on, until it ends, and returns its result.
// A rejoin after a relink therefore repeats at most one phase.
func (j *job) follow(ctx context.Context, emit func(ops.Phase) error) (any, error) {
	j.mu.Lock()
	j.followers++
	seen := max(len(j.log)-1, 0)
	close(j.joined)
	j.joined = make(chan struct{})
	j.mu.Unlock()
	defer j.leave()
	for {
		j.mu.Lock()
		lines, wake := j.log[seen:], j.wake
		j.mu.Unlock()
		for _, p := range lines {
			if err := emit(p); err != nil {
				return nil, err
			}
		}
		seen += len(lines)
		select {
		case <-wake:
		case <-j.done:
			return j.drain(seen, emit)
		case <-ctx.Done():
			return nil, fmt.Errorf("machine: follow: %w", ctx.Err())
		}
	}
}

func (j *job) drain(seen int, emit func(ops.Phase) error) (any, error) {
	j.mu.Lock()
	lines := j.log[seen:]
	j.mu.Unlock()
	for _, p := range lines {
		if err := emit(p); err != nil {
			return nil, err
		}
	}
	return j.res, j.err
}

func (j *job) leave() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.followers--
}

// awaitFollower holds the job until a caller follows it, for up to wait. A start whose callers
// all died must not launch an agent nobody watches: the domain releases that start and asks again.
func (j *job) awaitFollower(ctx context.Context, wait time.Duration) error {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		j.mu.Lock()
		n, joined := j.followers, j.joined
		j.mu.Unlock()
		if n > 0 {
			return nil
		}
		select {
		case <-joined:
		case <-t.C:
			return errOrphaned
		case <-ctx.Done():
			return fmt.Errorf("machine: %w", ctx.Err())
		}
	}
}

var (
	errOrphaned  = errors.New("machine: no caller waited for this start")
	errPreempted = errors.New("machine: replaced by a later call for the same box")
)

// slot serializes the shape calls of one task. lock is a one-place semaphore, so waiting for it
// honours a context.
type slot struct {
	lock chan struct{}
	mu   sync.Mutex
	cur  *job
}

func (s *slot) running() *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *slot) clear(j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == j {
		s.cur = nil
	}
}

// take cancels the running job, waits for it to return the slot, and holds the slot.
func (s *slot) take(ctx context.Context) error {
	if j := s.running(); j != nil {
		j.cancel(errPreempted)
	}
	select {
	case s.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("machine: waiting for the box: %w", ctx.Err())
	}
}

func (m *Machine) slot(t domain.TaskID) *slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[t]
	if s == nil {
		s = &slot{lock: make(chan struct{}, 1)}
		m.slots[t] = s
	}
	return s
}

// submit joins the task's running job when the key matches. It refuses a key older than the
// running job's, and otherwise preempts that job and starts fn as a new one under budget.
func (m *Machine) submit(ctx context.Context, t domain.TaskID, k jobKey, budget time.Duration, fn func(context.Context, *job) (any, error)) (*job, error) {
	s := m.slot(t)
	if j := s.running(); j != nil {
		switch {
		case j.key == k:
			return j, nil
		case j.key.newer(k):
			return nil, superseded("a newer call for this box is running")
		}
	}
	if err := s.take(ctx); err != nil {
		return nil, err
	}
	jctx, cancel := context.WithCancelCause(context.Background())
	jctx, stop := context.WithTimeout(jctx, budget)
	j := &job{key: k, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}), joined: make(chan struct{})}
	s.mu.Lock()
	s.cur = j
	s.mu.Unlock()
	go func() {
		defer func() { stop(); s.clear(j); <-s.lock; close(j.done) }()
		j.res, j.err = fn(jctx, j)
	}()
	return j, nil
}

// exclusive runs fn holding the task's slot, after preempting any job. fn runs on the caller's
// context: a Stop the caller gives up on is asked again.
func (m *Machine) exclusive(ctx context.Context, t domain.TaskID, fn func(context.Context) error) error {
	s := m.slot(t)
	if err := s.take(ctx); err != nil {
		return err
	}
	defer func() { <-s.lock }()
	return fn(ctx)
}
