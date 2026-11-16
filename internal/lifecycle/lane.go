package lifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Causes a job is cancelled with. Neither wants an answer: after an abort the domain has moved on,
// and after a drain the next process's Recovered asks again. Any other end of a job, a budget
// running out included, is answered.
var (
	errAborted  = errors.New("lifecycle: start aborted")
	errDraining = errors.New("lifecycle: server draining")
)

// job is an ask in flight.
type job struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// enqueue appends items to the lane and starts its goroutine if none runs. The caller holds the
// cell's apply lock, or is replaying a row at boot.
func (c *cell) enqueue(r *Runner, items []item) {
	if len(items) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(c.queue, items...)
	if c.running {
		return
	}
	c.running = true
	r.hold(c)
	go c.drain(r)
}

// pop takes the next item, and marks the lane idle when there is none, under the same lock that
// enqueue uses, so an item is never queued behind a lane that has already decided to stop.
func (c *cell) pop() (item, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		c.running = false
		return item{}, false
	}
	it := c.queue[0]
	c.queue = c.queue[1:]
	return it, true
}

// discard drops what is still queued when the process drains. Commands stay in the outbox for the
// next process, and asks are on the task for its Recovered.
func (c *cell) discard() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue, c.running = nil, false
}

// drain runs the lane until it is empty.
func (c *cell) drain(r *Runner) {
	defer r.release(c)
	for {
		it, ok := c.pop()
		if !ok {
			return
		}
		if r.jobCtx.Err() != nil {
			c.discard()
			return
		}
		r.run(c, it)
	}
}

// run carries out one item. A command runs to completion. An ask starts as a job and the lane moves
// on, and only a fresh start is remembered so AbortStart can cancel it.
func (r *Runner) run(c *cell, it item) {
	meta := domain.Meta(it.effect)
	switch {
	case it.effect == nil:
		r.runCommand(it.row)
	case meta.Delivery == domain.DeliverAsked:
		j := r.spawn(c, it)
		if meta.Abortable {
			c.mu.Lock()
			c.start = j
			c.mu.Unlock()
		}
	default:
		r.signal(c, it.effect)
	}
}

func (r *Runner) signal(c *cell, e domain.Effect) {
	if _, ok := e.(domain.AbortStart); ok {
		r.abort(c)
		return
	}
	r.Poke()
}

// spawn starts an ask as a job on the runner's job context. The job holds the cell until it ends.
func (r *Runner) spawn(c *cell, it item) *job {
	ctx, cancel := context.WithCancelCause(r.jobCtx)
	j := &job{cancel: cancel, done: make(chan struct{})}
	r.hold(c)
	go func() {
		defer r.release(c)
		defer close(j.done)
		defer cancel(nil)
		r.answer(ctx, it)
	}()
	return j
}

// answer carries out an ask within its budget and delivers its one answer, unless the job was
// cancelled by an abort or a drain.
func (r *Runner) answer(ctx context.Context, it item) {
	ctx, stop := context.WithTimeout(ctx, r.cfg.Budgets.forAsk(it.effect))
	defer stop()
	ans := r.ask(ctx, it)
	if cause := context.Cause(ctx); errors.Is(cause, errAborted) || errors.Is(cause, errDraining) || ans.ev == nil {
		return
	}
	err := r.retry(r.jobCtx, answerTries, "answer", func() error {
		_, err := r.stepWith(r.base, it.task.ID, ans.ev, ans.template)
		return err
	})
	if err != nil {
		r.cfg.Log.ErrorContext(ctx, "answer lost; the next start asks again", "task", it.task.ID, "err", err)
	}
}

// answerTries bounds how often an answer's own commit is retried.
const answerTries = 6

// abort ends the task's fresh start, if one runs, and returns once its goroutine has. The adapter's
// Converge returns promptly on its context and leaves the half-built box for the Release behind.
func (r *Runner) abort(c *cell) {
	c.mu.Lock()
	j := c.start
	c.start = nil
	c.mu.Unlock()
	if j != nil {
		j.cancel(errAborted)
		<-j.done
	}
}

// forAsk is the budget of an ask.
func (b Budgets) forAsk(e domain.Effect) time.Duration {
	switch e.(type) {
	case domain.StartTask:
		return b.Start
	case domain.EnsureRunning:
		return b.Ensure
	case domain.ProbeDoneMarker, domain.InspectBox:
		return b.Probe
	case domain.ReadSummary:
		return b.Summary
	case domain.SaveTemplate:
		return b.Template
	case domain.AbortStart, domain.SendToAgent, domain.InterruptAgent, domain.KillAgent,
		domain.ClearDoneMarker, domain.StopBox, domain.Release, domain.RunScheduler:
	}
	return b.Command
}

// runCommand carries out one outbox row and marks it done. A failure is logged and the row is still
// marked done, because commands are best effort: the box may already be gone. A drain cancels the
// command and leaves its row pending, and the next process runs it again.
func (r *Runner) runCommand(row ports.OutboxRow) {
	budget := r.cfg.Budgets.Command
	if row.Kind == ports.RowRelease {
		budget = r.cfg.Budgets.Release
	}
	ctx, cancel := context.WithTimeout(r.jobCtx, budget)
	defer cancel()
	err := r.exec(ctx, row)
	if r.jobCtx.Err() != nil {
		return
	}
	if err != nil {
		r.cfg.Log.WarnContext(ctx, "effect failed", "task", row.Target.Task, "kind", row.Kind, "err", err)
	}
	r.markDone(row.ID)
}

func (r *Runner) exec(ctx context.Context, row ports.OutboxRow) error {
	b, tg := r.cfg.Boxes, row.Target
	var err error
	switch row.Kind {
	case ports.RowType:
		err = b.Type(ctx, tg, row.Text)
	case ports.RowInterrupt:
		err = b.Interrupt(ctx, tg)
	case ports.RowKill:
		err = b.Kill(ctx, tg)
	case ports.RowClearDone:
		err = b.Remove(ctx, tg, domain.DoneMarkerPath)
	case ports.RowStopBox:
		err = b.Stop(ctx, tg)
	case ports.RowRelease:
		err = b.Release(ctx, tg)
	}
	return wrapPort("command", err)
}

// markDone retries until the store takes it or the process drains, so the lane never moves past a
// row the store still lists as pending. Boot replays the rows in order on that guarantee.
func (r *Runner) markDone(id ports.OutboxID) {
	err := r.retry(r.jobCtx, 0, "mark done", func() error {
		return r.cfg.Store.Done(r.base, id)
	})
	if err != nil {
		r.cfg.Log.WarnContext(r.base, "outbox row left pending", "row", id, "err", err)
	}
}
