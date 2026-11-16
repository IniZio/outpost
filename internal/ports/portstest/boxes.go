package portstest

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Call is one recorded Boxes call: the method, its target, and its text or plan.
type Call struct {
	Method string
	Target ports.Target
	Text   string
	Plan   ports.Plan
}

// Boxes models each task's box and agent terminal as a small record keyed by task, and records
// every call. Converge for a task can be held at a gate and any method can be stalled, so a test
// can act while a call is in flight. Signals are pushed to the sinks of running Watch calls.
type Boxes struct {
	mu       sync.Mutex
	calls    []Call
	entered  chan Call
	gates    map[domain.TaskID]chan error
	stalled  map[string]bool
	failing  map[string]error
	files    map[domain.TaskID]map[string]bool
	obs      map[domain.TaskID]domain.AgentObservation
	inspect  map[domain.TaskID]ports.BoxState
	boxes    map[domain.TaskID]*box
	sinks    map[domain.MachineID]map[int]ports.Sink
	nextSink int
	watching map[domain.MachineID]chan struct{}
}

type box struct {
	gen      domain.BoxGen
	running  bool
	released bool
	alive    map[domain.AgentRun]bool
}

// NewBoxes returns boxes where every call succeeds at once.
func NewBoxes() *Boxes {
	return &Boxes{
		entered:  make(chan Call, 64),
		gates:    map[domain.TaskID]chan error{},
		stalled:  map[string]bool{},
		failing:  map[string]error{},
		files:    map[domain.TaskID]map[string]bool{},
		obs:      map[domain.TaskID]domain.AgentObservation{},
		inspect:  map[domain.TaskID]ports.BoxState{},
		boxes:    map[domain.TaskID]*box{},
		sinks:    map[domain.MachineID]map[int]ports.Sink{},
		watching: map[domain.MachineID]chan struct{}{},
	}
}

// Hold makes Converge for task block, after it has reported its phases, until Let or until its
// context ends.
func (b *Boxes) Hold(task domain.TaskID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gates[task] = make(chan error, 1)
}

// Let opens a task's gate, failing the held Converge with err when err is not nil.
func (b *Boxes) Let(task domain.TaskID, err error) {
	b.mu.Lock()
	gate := b.gates[task]
	delete(b.gates, task)
	b.mu.Unlock()
	if gate != nil {
		gate <- err
	}
}

// Stall makes every later call of method block until its context ends.
func (b *Boxes) Stall(method string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stalled[method] = true
}

// Fail makes every later call of method return err, until it is called with nil.
func (b *Boxes) Fail(method string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failing[method] = err
}

// Entered delivers each call that blocks at a Hold or Stall gate, as it blocks there.
func (b *Boxes) Entered() <-chan Call { return b.entered }

// Calls returns every call so far, in order.
func (b *Boxes) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// Touch creates a file in a task's box, such as the done marker.
func (b *Boxes) Touch(task domain.TaskID, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files[task] == nil {
		b.files[task] = map[string]bool{}
	}
	b.files[task][path] = true
}

// SetObservation sets what Observe reports for a task.
func (b *Boxes) SetObservation(task domain.TaskID, o domain.AgentObservation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.obs[task] = o
}

// SetInspect overrides what Inspect reports for a task's box.
func (b *Boxes) SetInspect(task domain.TaskID, s ports.BoxState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inspect[task] = s
}

// Signal delivers s about task to every Watch on machine m, and returns once each sink has. It
// waits for a Watch on m to exist, so a test need not race the goroutine that starts it.
func (b *Boxes) Signal(m domain.MachineID, task domain.TaskID, s domain.Signal) {
	<-b.ready(m)
	b.mu.Lock()
	sinks := slices.Collect(maps.Values(b.sinks[m]))
	b.mu.Unlock()
	for _, sink := range sinks {
		sink(task, s)
	}
}

func (b *Boxes) ready(m domain.MachineID) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.watching[m]
	if !ok {
		ch = make(chan struct{})
		b.watching[m] = ch
	}
	return ch
}

// begin records a call and then honours Stall and Fail for its method.
func (b *Boxes) begin(ctx context.Context, c Call) error {
	b.mu.Lock()
	b.calls = append(b.calls, c)
	stalled, err := b.stalled[c.Method], b.failing[c.Method]
	b.mu.Unlock()
	if stalled {
		b.enter(c)
		<-ctx.Done()
		return fmt.Errorf("portstest: %s: %w", c.Method, ctx.Err())
	}
	return err
}

func (b *Boxes) note(c Call) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, c)
}

func (b *Boxes) enter(c Call) {
	select {
	case b.entered <- c:
	default:
	}
}

// ReadFile returns the path's name when the file exists.
func (b *Boxes) ReadFile(ctx context.Context, t ports.Target, path string) ([]byte, error) {
	ok, err := b.Exists(ctx, t, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("portstest: %s: %w", path, fs.ErrNotExist)
	}
	return []byte(path), nil
}

// Exists reports whether Touch created path and Remove has not removed it.
func (b *Boxes) Exists(ctx context.Context, t ports.Target, path string) (bool, error) {
	if err := b.begin(ctx, Call{Method: "Exists", Target: t, Text: path}); err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.files[t.Task][path], nil
}

// Remove removes a file.
func (b *Boxes) Remove(ctx context.Context, t ports.Target, path string) error {
	if err := b.begin(ctx, Call{Method: "Remove", Target: t, Text: path}); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.files[t.Task], path)
	return nil
}

// Converge records the plan, reports two phases, and waits at the task's gate if held. It reports
// a revival unless a terminal under the plan's run was already alive. When it returns it records a
// ConvergeReturned call, so a test can see that something waited for it.
func (b *Boxes) Converge(ctx context.Context, p ports.Plan, report ports.Progress) (ports.Converged, error) {
	defer b.note(Call{Method: "ConvergeReturned", Target: p.Target})
	if err := b.begin(ctx, Call{Method: "Converge", Target: p.Target, Plan: p}); err != nil {
		return ports.Converged{}, err
	}
	if report != nil {
		report("Cloning "+string(p.Repos[0]), "Cloning "+string(p.Repos[0]))
		report("Starting the agent", "")
	}
	if err := b.pass(ctx, p.Target); err != nil {
		return ports.Converged{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bx := b.boxOf(p.Target.Task)
	bx.gen, bx.running, bx.released = p.Target.Gen, true, false
	revived := !bx.alive[p.Target.Run]
	bx.alive = map[domain.AgentRun]bool{p.Target.Run: true}
	return ports.Converged{Revived: revived}, nil
}

func (b *Boxes) pass(ctx context.Context, t ports.Target) error {
	b.mu.Lock()
	gate := b.gates[t.Task]
	b.mu.Unlock()
	if gate == nil {
		return nil
	}
	b.enter(Call{Method: "Converge", Target: t})
	select {
	case err := <-gate:
		return err
	case <-ctx.Done():
		return fmt.Errorf("portstest: converge: %w", ctx.Err())
	}
}

func (b *Boxes) boxOf(task domain.TaskID) *box {
	if b.boxes[task] == nil {
		b.boxes[task] = &box{alive: map[domain.AgentRun]bool{}}
	}
	return b.boxes[task]
}

// Type records the text.
func (b *Boxes) Type(ctx context.Context, t ports.Target, text string) error {
	return b.begin(ctx, Call{Method: "Type", Target: t, Text: text})
}

// Interrupt records the call.
func (b *Boxes) Interrupt(ctx context.Context, t ports.Target) error {
	return b.begin(ctx, Call{Method: "Interrupt", Target: t})
}

// Kill ends the terminal of the target's run.
func (b *Boxes) Kill(ctx context.Context, t ports.Target) error {
	if err := b.begin(ctx, Call{Method: "Kill", Target: t}); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.boxOf(t.Task).alive, t.Run)
	return nil
}

// Observe returns what SetObservation set.
func (b *Boxes) Observe(ctx context.Context, t ports.Target) (domain.AgentObservation, error) {
	if err := b.begin(ctx, Call{Method: "Observe", Target: t}); err != nil {
		return domain.AgentObservation{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.obs[t.Task], nil
}

// Screen shows what was typed to the target's run, as a terminal echoes it, while its box exists.
func (b *Boxes) Screen(ctx context.Context, t ports.Target, _ int) (string, error) {
	if err := b.begin(ctx, Call{Method: "Screen", Target: t}); err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bx := b.boxes[t.Task]; bx == nil || bx.released || bx.gen == 0 {
		return "", nil
	}
	var lines []string
	for _, c := range b.calls {
		if c.Method == "Type" && c.Target.Task == t.Task && c.Target.Run == t.Run {
			lines = append(lines, c.Text)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Inspect reports a running box unless the box was stopped or released, or SetInspect said
// otherwise.
func (b *Boxes) Inspect(ctx context.Context, t ports.Target) (ports.BoxState, error) {
	if err := b.begin(ctx, Call{Method: "Inspect", Target: t}); err != nil {
		return ports.BoxState{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.inspect[t.Task]; ok {
		return s, nil
	}
	bx := b.boxOf(t.Task)
	return ports.BoxState{Running: bx.running && bx.gen == t.Gen}, nil
}

// Stop stops the box; its agent dies with it.
func (b *Boxes) Stop(ctx context.Context, t ports.Target) error {
	if err := b.begin(ctx, Call{Method: "Stop", Target: t}); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bx := b.boxOf(t.Task)
	bx.running, bx.alive = false, map[domain.AgentRun]bool{}
	return nil
}

// Release removes the box for good.
func (b *Boxes) Release(ctx context.Context, t ports.Target) error {
	if err := b.begin(ctx, Call{Method: "Release", Target: t}); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bx := b.boxOf(t.Task)
	bx.running, bx.released, bx.alive = false, true, map[domain.AgentRun]bool{}
	return nil
}

// SaveTemplate returns a handle named for the task, on the target's machine.
func (b *Boxes) SaveTemplate(ctx context.Context, t ports.Target) (ports.Template, error) {
	if err := b.begin(ctx, Call{Method: "SaveTemplate", Target: t}); err != nil {
		return ports.Template{}, err
	}
	return ports.Template{Handle: fmt.Sprintf("ready-%d", t.Task), Machine: t.Machine}, nil
}

// Watch registers sink for m and blocks until ctx ends.
func (b *Boxes) Watch(ctx context.Context, m domain.MachineID, sink ports.Sink) error {
	ready := b.ready(m)
	b.mu.Lock()
	if b.sinks[m] == nil {
		b.sinks[m] = map[int]ports.Sink{}
	}
	b.nextSink++
	id := b.nextSink
	b.sinks[m][id] = sink
	select {
	case <-ready:
	default:
		close(ready)
	}
	b.mu.Unlock()
	<-ctx.Done()
	b.mu.Lock()
	delete(b.sinks[m], id)
	b.mu.Unlock()
	return nil
}
