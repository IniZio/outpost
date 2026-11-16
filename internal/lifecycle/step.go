package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// casTries is how often step reloads and applies again after losing the compare-and-swap.
const casTries = 3

// cell is one task's serial point and its lane. It exists while anything holds it: a step in
// progress, effects queued, or a job running. The last holder to let go drops it from
// Runner.cells, so an idle task costs nothing and the next event makes a fresh cell.
type cell struct {
	id    domain.TaskID
	apply sync.Mutex
	refs  int

	mu      sync.Mutex
	queue   []item
	running bool
	start   *job
}

// item is one effect waiting on the lane, with the task as it was committed, so the effect acts on
// the run, generation and machine of the Apply that emitted it. A command has no effect and carries
// its outbox row instead.
type item struct {
	effect domain.Effect
	task   domain.Task
	row    ports.OutboxRow
}

func (r *Runner) acquire(id domain.TaskID) *cell {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cells[id]
	if c == nil {
		c = &cell{id: id}
		r.cells[id] = c
	}
	c.refs++
	return c
}

func (r *Runner) hold(c *cell) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.refs++
}

func (r *Runner) release(c *cell) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.refs--; c.refs > 0 {
		return
	}
	delete(r.cells, c.id)
	if len(r.cells) == 0 {
		for _, ch := range r.idle {
			close(ch)
		}
		r.idle = nil
	}
}

// step is the one place an event meets the domain. It returns the task after the event, or the
// task unchanged when Apply dropped the event as stale. A refusal is the domain's error wrapped, so
// callers test it with errors.Is(err, domain.ErrForbidden) and the like.
func (r *Runner) step(ctx context.Context, id domain.TaskID, ev domain.Event) (domain.Task, error) {
	return r.stepWith(ctx, id, ev, nil)
}

// stepWith is step for an answer whose commit also writes tpl. The row is written only when Apply
// accepts the answer, so a save the finish window already gave up on changes no repo.
func (r *Runner) stepWith(ctx context.Context, id domain.TaskID, ev domain.Event, tpl *ports.TemplateRow) (domain.Task, error) {
	if !r.lead.Load() {
		return domain.Task{}, ErrNotLeading
	}
	c := r.acquire(id)
	defer r.release(c)
	c.apply.Lock()
	defer c.apply.Unlock()
	var err error
	for range casTries {
		var t domain.Task
		var done bool
		if t, done, err = r.attempt(ctx, c, ev, tpl); done {
			return t, err
		}
	}
	return domain.Task{}, fmt.Errorf("lifecycle: task %d: %w", id, err)
}

// attempt loads, applies and commits once. done is false only for a lost compare-and-swap.
func (r *Runner) attempt(ctx context.Context, c *cell, ev domain.Event, tpl *ports.TemplateRow) (t domain.Task, done bool, err error) {
	t, err = r.cfg.Store.Load(ctx, c.id)
	if err != nil {
		return t, true, fmt.Errorf("lifecycle: load task %d: %w", c.id, err)
	}
	res, err := domain.Apply(t, ev, r.cfg.Clock.Now(), r.cfg.Policy)
	switch {
	case err != nil:
		return t, true, fmt.Errorf("lifecycle: task %d: %w", c.id, err)
	case res.Dropped != domain.Applied:
		return t, true, nil
	}
	err = r.commit(ctx, c, t.Version, res, tpl)
	return res.Task, !errors.Is(err, ports.ErrConflict), err
}

// commit stores one Result and queues its effects on the lane. The caller holds c.apply, so the
// lane order is the commit order.
func (r *Runner) commit(ctx context.Context, c *cell, prev uint64, res domain.Result, tpl *ports.TemplateRow) error {
	items, rows := split(res)
	ids, err := r.cfg.Store.Commit(ctx, ports.Commit{
		Prev: prev, Task: res.Task.Record(), Entries: res.Timeline, Outbox: rows, Template: tpl,
	})
	if err != nil {
		return fmt.Errorf("lifecycle: commit task %d: %w", c.id, err)
	}
	next := 0
	for i := range items {
		if items[i].effect == nil {
			items[i].row.ID = ids[next]
			next++
		}
	}
	c.enqueue(r, items)
	r.publish(c.id)
	return nil
}

// split turns a Result's effects into lane items in order, and the outbox rows for the commands
// among them. An ask has no row, because the task records it, and AbortStart and RunScheduler are
// in-process signals with nothing to replay.
func split(res domain.Result) ([]item, []ports.OutboxRow) {
	var items []item
	var rows []ports.OutboxRow
	for _, e := range res.Effects {
		if domain.Meta(e).Delivery != domain.DeliverReplay {
			items = append(items, item{effect: e, task: res.Task})
			continue
		}
		row := toRow(e, target(res.Task))
		rows = append(rows, row)
		items = append(items, item{row: row})
	}
	return items, rows
}

// toRow is the one switch from a replayable effect to its outbox row.
func toRow(e domain.Effect, tg ports.Target) ports.OutboxRow {
	switch e := e.(type) {
	case domain.SendToAgent:
		return ports.OutboxRow{Kind: ports.RowType, Target: tg, Text: e.Text}
	case domain.InterruptAgent:
		return ports.OutboxRow{Kind: ports.RowInterrupt, Target: tg}
	case domain.KillAgent:
		return ports.OutboxRow{Kind: ports.RowKill, Target: tg}
	case domain.ClearDoneMarker:
		return ports.OutboxRow{Kind: ports.RowClearDone, Target: tg}
	case domain.StopBox:
		return ports.OutboxRow{Kind: ports.RowStopBox, Target: tg}
	case domain.Release:
		return ports.OutboxRow{Kind: ports.RowRelease, Target: tg}
	case domain.StartTask, domain.AbortStart, domain.EnsureRunning, domain.ProbeDoneMarker,
		domain.ReadSummary, domain.SaveTemplate, domain.InspectBox, domain.RunScheduler:
	}
	return ports.OutboxRow{}
}

// target names the task's current box and terminal.
func target(t domain.Task) ports.Target {
	return ports.Target{Task: t.ID, Machine: t.Machine, Run: t.AgentRun, Gen: t.BoxGen}
}

// watch registers for a nudge after each commit to the task. The channel holds one, so a watcher
// that is busy misses nothing: it reads the task's timeline again when it comes back.
func (r *Runner) watch(id domain.TaskID) (nudge <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.watchers[id] == nil {
		r.watchers[id] = map[chan struct{}]struct{}{}
	}
	r.watchers[id][ch] = struct{}{}
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.watchers[id], ch)
		if len(r.watchers[id]) == 0 {
			delete(r.watchers, id)
		}
	}
}

func (r *Runner) publish(id domain.TaskID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.watchers[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
