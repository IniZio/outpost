package portstest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Store keeps records, timelines, outbox rows, repos and machines in maps. Commit restores what it
// stores, so a record a real store would reject fails the test too. Crash simulates a restart.
type Store struct {
	mu        sync.Mutex
	tasks     map[domain.TaskID]domain.Record
	timeline  map[domain.TaskID][]domain.Entry
	outbox    []ports.OutboxRow
	done      map[ports.OutboxID]bool
	repos     map[domain.RepoName]domain.Repo
	machines  map[domain.MachineID]ports.MachineState
	nextID    domain.TaskID
	nextRow   ports.OutboxID
	lease     chan struct{}
	scheds    []ports.ScheduleRow
	slots     map[ports.FireRow]domain.TaskID
	lastSched domain.ScheduleID
	setups    map[domain.RepoName][]domain.SetupRev

	conflicts int
	commitErr faults
	doneErr   faults
}

// faults is a count of calls still to fail and the error they fail with.
type faults struct {
	n   int
	err error
}

func (f *faults) next() error {
	if f.n == 0 {
		return nil
	}
	f.n--
	return f.err
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		tasks:    map[domain.TaskID]domain.Record{},
		timeline: map[domain.TaskID][]domain.Entry{},
		done:     map[ports.OutboxID]bool{},
		repos:    map[domain.RepoName]domain.Repo{},
		machines: map[domain.MachineID]ports.MachineState{},
		lease:    make(chan struct{}, 1),
		slots:    map[ports.FireRow]domain.TaskID{},
	}
}

// AddMachine adds or replaces a machine row, online or not.
func (s *Store) AddMachine(m domain.Machine, online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.machines[m.ID] = ports.MachineState{Machine: m, Online: online}
}

// SetOnline flips a machine's online flag, as link.Presence would.
func (s *Store) SetOnline(id domain.MachineID, online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	m.Online = online
	s.machines[id] = m
}

// SetRepo adds or replaces a repo row.
func (s *Store) SetRepo(r domain.Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[r.Name] = r
}

// ConflictNext makes the next n Commits fail with ports.ErrConflict, as if another writer won.
func (s *Store) ConflictNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conflicts = n
}

// FailCommits makes the next n Commits fail with err.
func (s *Store) FailCommits(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitErr = faults{n, err}
}

// FailDones makes the next n Done calls fail with err.
func (s *Store) FailDones(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doneErr = faults{n, err}
}

// Crash returns a store holding what was committed here and nothing else, as after a restart. The
// lease is free in the new store, and this store keeps working, so a test can leave the old
// process running.
func (s *Store) Crash() *Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := NewStore()
	maps.Copy(n.tasks, s.tasks)
	maps.Copy(n.done, s.done)
	maps.Copy(n.repos, s.repos)
	n.setups = map[domain.RepoName][]domain.SetupRev{}
	for name, revs := range s.setups {
		n.setups[name] = slices.Clone(revs)
	}
	maps.Copy(n.machines, s.machines)
	for id, entries := range s.timeline {
		n.timeline[id] = slices.Clone(entries)
	}
	n.outbox = slices.Clone(s.outbox)
	n.nextID, n.nextRow = s.nextID, s.nextRow
	return n
}

// Lease is held by one caller at a time, until it is released or its context ends.
func (s *Store) Lease(ctx context.Context) (func(), error) {
	select {
	case s.lease <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("portstest: lease: %w", ctx.Err())
	}
	var once sync.Once
	return func() { once.Do(func() { <-s.lease }) }, nil
}

// NewTaskID returns the next id.
func (s *Store) NewTaskID(context.Context) (domain.TaskID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID, nil
}

// Load restores the record.
func (s *Store) Load(_ context.Context, id domain.TaskID) (domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

func (s *Store) load(id domain.TaskID) (domain.Task, error) {
	rec, ok := s.tasks[id]
	if !ok {
		return domain.Task{}, fmt.Errorf("task %d: %w", id, ports.ErrNotFound)
	}
	t, err := domain.Restore(rec)
	if err != nil {
		return domain.Task{}, fmt.Errorf("portstest: %w", err)
	}
	return t, nil
}

// Commit checks Prev and restores the record to validate it, then writes everything.
func (s *Store) Commit(_ context.Context, c ports.Commit) ([]ports.OutboxID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	if f := c.Fire; f != nil {
		if _, ok := s.slots[slot(f.Schedule, f.Due)]; ok {
			return nil, ports.ErrFired
		}
		s.slots[slot(f.Schedule, f.Due)] = c.Task.ID
	}
	s.tasks[c.Task.ID] = c.Task
	s.timeline[c.Task.ID] = append(s.timeline[c.Task.ID], c.Entries...)
	ids := make([]ports.OutboxID, len(c.Outbox))
	for i, row := range c.Outbox {
		s.nextRow++
		row.ID = s.nextRow
		ids[i] = row.ID
		s.outbox = append(s.outbox, row)
	}
	if tpl := c.Template; tpl != nil {
		r := s.repos[tpl.Repo]
		r.Name, r.ReadyImage, r.ReadyOn, r.ReadyAt = tpl.Repo, tpl.Template.Handle, tpl.Template.Machine, tpl.At
		if tpl.Notes != "" {
			r.Notes = tpl.Notes
		}
		s.repos[tpl.Repo] = r
	}
	return ids, nil
}

func (s *Store) admit(c ports.Commit) error {
	if s.conflicts > 0 {
		s.conflicts--
		return ports.ErrConflict
	}
	if err := s.commitErr.next(); err != nil {
		return err
	}
	cur, exists := s.tasks[c.Task.ID]
	if exists != (c.Prev != 0) || exists && cur.Version != c.Prev {
		return ports.ErrConflict
	}
	if c.Task.Version != c.Prev+1 {
		return fmt.Errorf("portstest: record version %d does not follow %d", c.Task.Version, c.Prev)
	}
	if _, err := domain.Restore(c.Task); err != nil {
		return fmt.Errorf("portstest: %w", err)
	}
	return nil
}

// Tasks filters in memory, newest first.
func (s *Store) Tasks(_ context.Context, q ports.TaskQuery) ([]domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Task
	for _, id := range s.ids(true) {
		t, err := s.load(id)
		switch {
		case err != nil:
			return nil, err
		case q.Owner != "" && t.Owner != q.Owner, q.Status != 0 && t.Status() != q.Status:
		case q.Before != 0 && t.ID >= q.Before:
		default:
			out = append(out, t)
		}
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (s *Store) ids(newestFirst bool) []domain.TaskID {
	ids := slices.Sorted(maps.Keys(s.tasks))
	if newestFirst {
		slices.Reverse(ids)
	}
	return ids
}

// Timeline slices the stored entries.
func (s *Store) Timeline(_ context.Context, id domain.TaskID, skip int) ([]domain.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.timeline[id]
	if skip >= len(entries) {
		return nil, nil
	}
	return slices.Clone(entries[max(skip, 0):]), nil
}

// Live lists unfinished tasks in id order.
func (s *Store) Live(context.Context) ([]domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Task
	for _, id := range s.ids(false) {
		t, err := s.load(id)
		if err != nil {
			return nil, err
		}
		if t.Unfinished() {
			out = append(out, t)
		}
	}
	return out, nil
}

// Due lists tasks with a wake at or before now, earliest first.
func (s *Store) Due(_ context.Context, now time.Time) ([]domain.TaskID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []domain.Record
	for _, rec := range s.tasks {
		if !rec.Wake.IsZero() && !rec.Wake.After(now) {
			due = append(due, rec)
		}
	}
	slices.SortFunc(due, func(a, b domain.Record) int {
		if c := a.Wake.Compare(b.Wake); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
	ids := make([]domain.TaskID, len(due))
	for i, rec := range due {
		ids[i] = rec.ID
	}
	return ids, nil
}

// NextWake is the earliest wake.
func (s *Store) NextWake(context.Context) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next time.Time
	for _, rec := range s.tasks {
		if !rec.Wake.IsZero() && (next.IsZero() || rec.Wake.Before(next)) {
			next = rec.Wake
		}
	}
	return next, !next.IsZero(), nil
}

// Pending lists rows not marked done.
func (s *Store) Pending(context.Context) ([]ports.OutboxRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.outbox), func(r ports.OutboxRow) bool { return s.done[r.ID] }), nil
}

// Done marks a row.
func (s *Store) Done(_ context.Context, id ports.OutboxID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.doneErr.next(); err != nil {
		return err
	}
	s.done[id] = true
	return nil
}

// Machines lists machine rows by id.
func (s *Store) Machines(context.Context) ([]ports.MachineState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.MachineState
	for _, id := range slices.Sorted(maps.Keys(s.machines)) {
		out = append(out, s.machines[id])
	}
	return out, nil
}

// Repo returns the repo row, or the zero Repo with only the name set.
func (s *Store) Repo(_ context.Context, name domain.RepoName) (domain.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.repos[name]; ok {
		return r, nil
	}
	return domain.Repo{Name: name}, nil
}
