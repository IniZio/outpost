package lifecycle

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// TaskQuery is ports.TaskQuery under lifecycle's name, because internal/api may import lifecycle
// and domain but not ports.
type TaskQuery = ports.TaskQuery

// Row is one timeline entry and its place in the task's timeline, counted from 1.
type Row struct {
	Seq int
	domain.Entry
}

// Create builds a queued task and stores it. A task that a schedule fires comes from Fire.
func (r *Runner) Create(ctx context.Context, owner domain.Login, kind domain.Kind, spec domain.Spec) (domain.Task, error) {
	id, err := r.cfg.Store.NewTaskID(ctx)
	if err != nil {
		return domain.Task{}, fmt.Errorf("lifecycle: new task id: %w", err)
	}
	res, err := domain.New(id, owner, kind, spec, r.cfg.Clock.Now())
	if err != nil {
		return domain.Task{}, fmt.Errorf("lifecycle: %w", err)
	}
	return r.insert(ctx, res)
}

// Fire creates the task one firing of a schedule makes. The schedule package's callback calls it.
func (r *Runner) Fire(ctx context.Context, s domain.Schedule) (domain.Task, error) {
	id, err := r.cfg.Store.NewTaskID(ctx)
	if err != nil {
		return domain.Task{}, fmt.Errorf("lifecycle: new task id: %w", err)
	}
	res, err := s.Fire(id, r.cfg.Clock.Now())
	if err != nil {
		return domain.Task{}, fmt.Errorf("lifecycle: %w", err)
	}
	return r.insert(ctx, res)
}

func (r *Runner) insert(ctx context.Context, res domain.Result) (domain.Task, error) {
	if !r.lead.Load() {
		return domain.Task{}, ErrNotLeading
	}
	c := r.acquire(res.Task.ID)
	defer r.release(c)
	c.apply.Lock()
	defer c.apply.Unlock()
	if err := r.commit(ctx, c, 0, res, nil); err != nil {
		return domain.Task{}, err
	}
	return res.Task, nil
}

// Do applies a person's command to a task and returns the task after it. A refusal is the domain's
// error wrapped, so the API tests it with errors.Is(err, domain.ErrForbidden), ErrNotReady,
// ErrClosing or ErrReleased. A command the task has nothing to do for returns the task unchanged.
// Do returns once the event is committed. Its effects run after, on the task's lane.
func (r *Runner) Do(ctx context.Context, id domain.TaskID, c domain.Command) (domain.Task, error) {
	return r.step(ctx, id, c)
}

// Task returns one task. The API checks Task.AccessFor before showing it.
func (r *Runner) Task(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	t, err := r.cfg.Store.Load(ctx, id)
	if err != nil {
		return domain.Task{}, fmt.Errorf("lifecycle: task %d: %w", id, err)
	}
	return t, nil
}

// List returns tasks for the API's lists, newest first.
func (r *Runner) List(ctx context.Context, q TaskQuery) ([]domain.Task, error) {
	ts, err := r.cfg.Store.Tasks(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: list tasks: %w", err)
	}
	return ts, nil
}

// followPoll is how often Follow reads the timeline again without a nudge. A standby process gets
// no nudges, because the leader commits, so this is what keeps a follower on a standby current.
const followPoll = 2 * time.Second

// Follow yields a task's timeline entries after the first `after`, then each new entry as it is
// committed, until ctx ends or the consumer stops. A failure to read is yielded once and ends the
// sequence. A consumer that resumes passes the Seq of the last Row it saw.
func (r *Runner) Follow(ctx context.Context, id domain.TaskID, after int) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		nudge, stop := r.watch(id)
		defer stop()
		if _, err := r.Task(ctx, id); err != nil {
			yield(Row{}, err)
			return
		}
		for r.stream(ctx, id, &after, yield) {
			select {
			case <-nudge:
			case <-time.After(followPoll):
			case <-ctx.Done():
				return
			}
		}
	}
}

// stream yields every entry after *after, and reports whether the consumer should now wait for
// more. It stops at once when the consumer does or the timeline cannot be read.
func (r *Runner) stream(ctx context.Context, id domain.TaskID, after *int, yield func(Row, error) bool) bool {
	for {
		entries, err := r.cfg.Store.Timeline(ctx, id, *after)
		if err != nil {
			yield(Row{}, fmt.Errorf("lifecycle: timeline of task %d: %w", id, err))
			return false
		}
		for _, e := range entries {
			*after++
			if !yield(Row{Seq: *after, Entry: e}, nil) {
				return false
			}
		}
		if len(entries) == 0 {
			return true
		}
	}
}

// Screen reads the agent terminal of the task's current run, with lines of history above the
// screen. A task that never got a box, or whose box is gone, has an empty screen.
func (r *Runner) Screen(ctx context.Context, id domain.TaskID, lines int) (string, error) {
	t, err := r.Task(ctx, id)
	if err != nil {
		return "", err
	}
	if t.Machine == "" || t.BoxWant() == domain.BoxReleased {
		return "", nil
	}
	s, err := r.cfg.Boxes.Screen(ctx, target(t), lines)
	if err != nil {
		return "", fmt.Errorf("lifecycle: screen of task %d: %w", id, err)
	}
	return s, nil
}

// Leading reports whether this process holds the lease and drives tasks.
func (r *Runner) Leading() bool { return r.lead.Load() }

// Started is closed once Run holds the lease and has recovered every unfinished task.
func (r *Runner) Started() <-chan struct{} { return r.started }

// Poke asks for a scheduler pass soon. cmd/outpost calls it when a machine links or unlinks and
// when box images finish building. Pokes coalesce: many before a pass cause one pass.
func (r *Runner) Poke() {
	select {
	case r.poke <- struct{}{}:
	default:
	}
}
