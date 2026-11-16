package lifecycle

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// minWake is the shortest the wake loop sleeps when the store reports a wake already past. Such a
// wake was just handed to RunDue, so one that is still past means a Tick the domain dropped, and the
// floor keeps that from spinning.
const minWake = time.Second

// RunDue feeds Tick to every task whose wake has come and returns how many it handled. The wake loop
// calls it, and tests call it after moving the clock. A Tick that fails to apply leaves the task
// due, and the next pass tries it again.
func (r *Runner) RunDue(ctx context.Context) (int, error) {
	ids, err := r.cfg.Store.Due(ctx, r.cfg.Clock.Now())
	if err != nil {
		return 0, fmt.Errorf("lifecycle: due tasks: %w", err)
	}
	n := 0
	for _, id := range ids {
		if _, err := r.step(ctx, id, domain.Tick{}); err != nil {
			r.cfg.Log.WarnContext(ctx, "tick not applied", "task", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// wakeLoop is the one timer. It sleeps until the earliest wake in the store, or PollEvery, and
// wakes early when a commit sets an earlier one.
func (r *Runner) wakeLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := r.RunDue(ctx); err != nil {
			r.cfg.Log.WarnContext(ctx, "wake pass failed", "err", err)
		}
		wait := r.sleepFor(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.cfg.After(wait):
		case <-r.nudge:
		}
	}
}

// sleepFor is how long the wake loop sleeps, and records the instant it will wake for noteWake.
// armed is cleared before the store is read, so a commit that lands in between nudges the loop.
func (r *Runner) sleepFor(ctx context.Context) time.Duration {
	now := r.cfg.Clock.Now()
	wait := r.cfg.PollEvery
	r.armed.Store(0)
	next, ok, err := r.cfg.Store.NextWake(ctx)
	if err == nil && ok {
		until := next.Sub(now)
		if until <= 0 {
			until = minWake
		}
		wait = min(wait, until)
	}
	r.armed.Store(now.Add(wait).UnixNano())
	return wait
}

// noteWake wakes the loop when a task's deadline is earlier than the one the loop sleeps for. A
// deadline that moves later or goes away leaves the loop to wake early, find nothing due and sleep
// again, which cannot go wrong because the domain drops a Tick with nothing due.
func (r *Runner) noteWake(t domain.Task) {
	at, ok := t.NextDeadline()
	if !ok {
		return
	}
	if armed := r.armed.Load(); armed == 0 || at.UnixNano() < armed {
		select {
		case r.nudge <- struct{}{}:
		default:
		}
	}
}

// passLoop runs a placement pass when poked and every PollEvery, so a missed poke costs a delay and
// not a stuck queue.
func (r *Runner) passLoop(ctx context.Context) {
	for ctx.Err() == nil {
		r.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.poke:
		case <-r.cfg.After(r.cfg.PollEvery):
		}
	}
}

func (r *Runner) pass(ctx context.Context) {
	if err := r.schedule(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "placement pass failed", "err", err)
	}
	r.startFeeds(ctx)
	r.retryRecovery(ctx)
}

// schedule runs one placement pass. Passes never overlap, and a race with a stale pick is safe:
// SlotGranted to a task that is no longer queued is dropped.
func (r *Runner) schedule(ctx context.Context) error {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()
	tasks, err := r.cfg.Store.Live(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: live tasks: %w", err)
	}
	machines, err := r.onlineSandboxes(ctx)
	if err != nil {
		return err
	}
	repos, err := r.queuedRepos(ctx, tasks)
	if err != nil {
		return err
	}
	for _, p := range domain.PickStarts(tasks, machines, repos, r.cfg.Capacity()) {
		if _, err := r.step(ctx, p.Task, domain.SlotGranted{Machine: p.Machine}); err != nil {
			r.cfg.Log.WarnContext(ctx, "slot not granted", "task", p.Task, "err", err)
		}
	}
	return nil
}

// onlineSandboxes lists the machines that run boxes and are linked now.
func (r *Runner) onlineSandboxes(ctx context.Context) ([]domain.Machine, error) {
	states, err := r.cfg.Store.Machines(ctx)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: machines: %w", err)
	}
	var out []domain.Machine
	for _, s := range states {
		if s.Online && s.Machine.Role == domain.RoleSandbox {
			out = append(out, s.Machine)
		}
	}
	return out, nil
}

// queuedRepos reads the repo row of each repo a queued task is waiting on, once.
func (r *Runner) queuedRepos(ctx context.Context, tasks []domain.Task) ([]domain.Repo, error) {
	seen := map[domain.RepoName]bool{}
	var repos []domain.Repo
	for _, t := range tasks {
		if _, queued := t.Life.(domain.Queued); !queued || seen[t.Spec.Repo] {
			continue
		}
		seen[t.Spec.Repo] = true
		repo, err := r.cfg.Store.Repo(ctx, t.Spec.Repo)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: repo %s: %w", t.Spec.Repo, err)
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

// recoverAll is the start of a leading process. It runs the outbox rows the last process left
// first, in order, so they keep their place ahead of whatever Recovered emits: each is idempotent on
// its target, and no effect emitted after a pending row ever started, because the lane does not
// move past a row until the store lists it done. A lost Release or StopBox is never re-issued by
// Recovered, which is why they replay. Then every unfinished task is recovered.
func (r *Runner) recoverAll(ctx context.Context) error {
	rows, err := r.cfg.Store.Pending(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: pending outbox: %w", err)
	}
	for _, row := range rows {
		c := r.acquire(row.Target.Task)
		c.enqueue(r, []item{{row: row}})
		r.release(c)
	}
	tasks, err := r.cfg.Store.Live(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: live tasks: %w", err)
	}
	machines, err := r.onlineSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		r.recoverTask(ctx, t, machines)
	}
	r.Poke()
	return nil
}

// recoverTask feeds Recovered to one task. Only an active task whose box should be up is judged
// against what its agent's terminal looks like now, so it waits for its machine to be online.
// Every other task is recovered at once, which lets a half-built start go back to the queue while
// its machine is away.
func (r *Runner) recoverTask(ctx context.Context, t domain.Task, online []domain.Machine) {
	switch {
	case !t.NeedsObservation():
		r.recovered(ctx, t.ID, domain.Recovered{})
	case !slices.ContainsFunc(online, func(m domain.Machine) bool { return m.ID == t.Machine }):
		r.awaitRecovery(t.ID)
	default:
		r.observe(ctx, t)
	}
}

// observe asks the machine about the task's agent and feeds Recovered with the answer. The
// observation may be a moment old when it is applied. The domain judges it, and a feed signal that
// follows corrects it.
func (r *Runner) observe(ctx context.Context, t domain.Task) {
	octx, cancel := context.WithTimeout(ctx, r.cfg.Budgets.Observe)
	defer cancel()
	obs, err := r.cfg.Boxes.Observe(octx, target(t))
	if err != nil {
		r.cfg.Log.WarnContext(ctx, "agent not observed; waiting for its machine", "task", t.ID, "err", err)
		r.awaitRecovery(t.ID)
		return
	}
	r.recovered(ctx, t.ID, domain.Recovered{Agent: obs})
}

func (r *Runner) recovered(ctx context.Context, id domain.TaskID, ev domain.Recovered) {
	if _, err := r.step(ctx, id, ev); err != nil {
		r.cfg.Log.WarnContext(ctx, "task not recovered; trying again later", "task", id, "err", err)
		r.awaitRecovery(id)
	}
}

func (r *Runner) awaitRecovery(id domain.TaskID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waiting[id] = struct{}{}
}

// retryRecovery tries again the tasks whose recovery waited for their machine or failed to apply.
func (r *Runner) retryRecovery(ctx context.Context) {
	r.mu.Lock()
	ids := slices.Sorted(maps.Keys(r.waiting))
	clear(r.waiting)
	r.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	machines, err := r.onlineSandboxes(ctx)
	for _, id := range ids {
		if err != nil {
			r.awaitRecovery(id)
			continue
		}
		r.retryOne(ctx, id, machines)
	}
}

func (r *Runner) retryOne(ctx context.Context, id domain.TaskID, online []domain.Machine) {
	t, err := r.cfg.Store.Load(ctx, id)
	if err != nil {
		r.cfg.Log.WarnContext(ctx, "task not loaded for recovery", "task", id, "err", err)
		r.awaitRecovery(id)
		return
	}
	if t.Unfinished() {
		r.recoverTask(ctx, t, online)
	}
}

// startFeeds starts one Boxes.Watch for each sandbox machine that has none, for the life of ctx.
// Run calls it at start and on every pass, so a newly paired machine gets a feed, and a feed that
// ended because its machine was revoked comes back if the row does.
func (r *Runner) startFeeds(ctx context.Context) {
	states, err := r.cfg.Store.Machines(ctx)
	if err != nil {
		r.cfg.Log.WarnContext(ctx, "machines not listed", "err", err)
		return
	}
	for _, s := range states {
		if s.Machine.Role != domain.RoleSandbox || !r.claimFeed(s.Machine.ID) {
			continue
		}
		r.bg.Go(func() { r.feed(ctx, s.Machine.ID) })
	}
}

func (r *Runner) claimFeed(m domain.MachineID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.feeds[m] {
		return false
	}
	r.feeds[m] = true
	return true
}

func (r *Runner) feed(ctx context.Context, m domain.MachineID) {
	err := r.cfg.Boxes.Watch(ctx, m, func(task domain.TaskID, s domain.Signal) {
		if _, err := r.step(ctx, task, s); err != nil {
			r.cfg.Log.WarnContext(ctx, "signal not applied", "task", task, "err", err)
		}
	})
	r.mu.Lock()
	delete(r.feeds, m)
	r.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		r.cfg.Log.WarnContext(ctx, "feed ended", "machine", m, "err", err)
	}
}
