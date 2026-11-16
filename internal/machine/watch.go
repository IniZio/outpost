package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

// level is what the feed last knew of one task: its newest box and the agent terminal of the
// highest run in it.
type level struct {
	box  *ports.BoxInfo
	term *ops.WatchLine
}

// feed merges the provider's events and one hc subscription per running box into levels, and fans
// edges out to every open box.watch stream. hc events are only wake-ups: on each one the feed asks
// hc for the terminals again, so a new event shape cannot lose a level.
type feed struct {
	m       *Machine
	mu      sync.Mutex
	levels  map[domain.TaskID]*level
	subs    map[*sub]bool
	follows map[ports.BoxKey]context.CancelFunc
	base    context.Context
}

type sub struct {
	ch   chan ops.WatchLine
	gone chan struct{}
}

func newFeed(m *Machine) *feed {
	return &feed{
		m: m, levels: map[domain.TaskID]*level{}, subs: map[*sub]bool{},
		follows: map[ports.BoxKey]context.CancelFunc{}, base: context.Background(),
	}
}

func (f *feed) run(ctx context.Context) error {
	f.mu.Lock()
	f.base = ctx
	f.mu.Unlock()
	boxes, err := f.m.cfg.Sandbox.Boxes(ctx)
	if err != nil {
		return fmt.Errorf("machine: list boxes: %w", err)
	}
	for _, b := range boxes {
		f.change(ports.BoxChange{Info: b}, false)
	}
	err = f.m.cfg.Sandbox.Events(ctx, func(c ports.BoxChange) { f.change(c, true) })
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("machine: box events: %w", err)
}

// change records a box's facts, publishes the edge, and starts or stops following its hc.
func (f *feed) change(c ports.BoxChange, edge bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := c.Info.Key
	lv := f.level(k.Task)
	if lv.box == nil || k.Gen >= lv.box.Key.Gen {
		info := c.Info
		lv.box = &info
		if !info.Running {
			lv.term = nil
		}
	}
	if edge {
		f.publish(ops.WatchLine{Task: k.Task, Kind: ops.WatchBox, Gen: k.Gen, Box: c.Kind, Up: c.Info.Running, OOM: c.Info.OOMKilled})
	}
	stop, following := f.follows[k]
	switch {
	case c.Info.Running && !following:
		ctx, cancel := context.WithCancel(f.base)
		f.follows[k] = cancel
		go f.follow(ctx, k)
	case !c.Info.Running && following:
		stop()
		delete(f.follows, k)
	}
}

func (f *feed) level(t domain.TaskID) *level {
	lv := f.levels[t]
	if lv == nil {
		lv = &level{}
		f.levels[t] = lv
	}
	return lv
}

func (f *feed) follow(ctx context.Context, k ports.BoxKey) {
	for ctx.Err() == nil {
		f.followOnce(ctx, k)
		t := time.NewTimer(f.m.cfg.Redial)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
}

func (f *feed) followOnce(ctx context.Context, k ports.BoxKey) {
	cmd, err := f.m.dial(ctx, k)
	if err != nil {
		return
	}
	defer func() { _ = cmd.Close() }()
	rwc, err := f.m.cfg.Sandbox.Dial(ctx, k, []string{hcBin, "connect"})
	if err != nil {
		return
	}
	refresh := func() {
		if ts, err := cmd.List(ctx, taskLabels(k.Task)); err == nil {
			f.terms(k, ts)
		}
	}
	_ = hc.Subscribe(ctx, rwc, taskLabels(k.Task), func([]hc.Terminal) { refresh() }, func(hc.Event) { refresh() })
}

// terms sets the task's terminal level from the terminal of the highest run, and publishes it
// when it changed.
func (f *feed) terms(k ports.BoxKey, ts []hc.Terminal) {
	var top *hc.Terminal
	for i := range ts {
		if top == nil || runOf(ts[i]) >= runOf(*top) {
			top = &ts[i]
		}
	}
	if top == nil {
		return
	}
	l, ok := line(k.Task, *top)
	f.mu.Lock()
	defer f.mu.Unlock()
	lv := f.level(k.Task)
	if !ok || (lv.box != nil && lv.box.Key.Gen > k.Gen) || (lv.term != nil && *lv.term == l) {
		return
	}
	lv.term = &l
	f.publish(l)
}

var statuses = map[string]domain.HCStatus{
	"working": domain.HCWorking, "idle": domain.HCIdle, "done": domain.HCDone,
	"blocked": domain.HCBlocked, "error": domain.HCError,
}

// line is a terminal's level. A live terminal that never said a status gives none: hc's null
// status means "not said", not "working".
func line(task domain.TaskID, t hc.Terminal) (ops.WatchLine, bool) {
	run := runOf(t)
	if !t.Live() {
		return ops.WatchLine{Task: task, Kind: ops.WatchExit, Run: run, Code: t.Code()}, true
	}
	if t.Status == nil {
		return ops.WatchLine{}, false
	}
	st, ok := statuses[t.Status.State]
	phase := t.Status.Msg
	if phase == "" {
		phase = t.Status.Kind
	}
	return ops.WatchLine{Task: task, Kind: ops.WatchReport, Run: run, Status: st, Phase: phase}, ok
}

func (f *feed) publish(l ops.WatchLine) {
	for s := range f.subs {
		select {
		case s.ch <- l:
		default:
			delete(f.subs, s)
			close(s.gone)
		}
	}
}

// subscribe queues the levels and registers for edges under one lock, so no edge falls between.
func (f *feed) subscribe() (*sub, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &sub{ch: make(chan ops.WatchLine, 256+2*len(f.levels)), gone: make(chan struct{})}
	for t, lv := range f.levels {
		if lv.box != nil && !lv.box.Running {
			s.ch <- ops.WatchLine{Task: t, Kind: ops.WatchBox, Gen: lv.box.Key.Gen, Box: domain.BoxDied, OOM: lv.box.OOMKilled}
		}
		if lv.term != nil {
			s.ch <- *lv.term
		}
	}
	f.subs[s] = true
	return s, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, s)
	}
}

// serve is the box.watch handler. A stream that falls behind is dropped, and the holder's reopen
// starts again from levels.
func (f *feed) serve(context.Context, link.Meta, struct{}) (struct{}, link.Bytes, error) {
	return struct{}{}, func(ctx context.Context, st link.Stream) error {
		ctx, cancel := leaving(ctx, st)
		defer cancel()
		s, done := f.subscribe()
		defer done()
		enc := json.NewEncoder(st)
		for {
			select {
			case l := <-s.ch:
				if err := enc.Encode(l); err != nil {
					return nil //nolint:nilerr // the holder went away; it reopens
				}
			case <-s.gone:
				return nil
			case <-ctx.Done():
				return nil
			}
		}
	}, nil
}
