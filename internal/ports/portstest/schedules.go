package portstest

import (
	"context"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Schedules = (*Store)(nil)

// slot keys a schedule's slot by the instant, whatever the location of due.
func slot(id domain.ScheduleID, due time.Time) ports.FireRow {
	return ports.FireRow{Schedule: id, Due: due.UTC()}
}

// PutSchedule inserts or replaces the schedule of that owner and name.
func (s *Store) PutSchedule(_ context.Context, row ports.ScheduleRow) (domain.ScheduleID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row.LastDue, row.LastTask, row.LastSkipped = time.Time{}, 0, time.Time{}
	for i, x := range s.scheds {
		if x.Owner == row.Owner && x.Name == row.Name {
			row.ID = x.ID
			s.scheds[i] = row
			return row.ID, nil
		}
	}
	row.ID = domain.ScheduleID(len(s.scheds) + 1)
	s.scheds = append(s.scheds, row)
	return row.ID, nil
}

// Schedules lists every schedule with its slot bookkeeping.
func (s *Store) Schedules(context.Context) ([]ports.ScheduleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ports.ScheduleRow, 0, len(s.scheds))
	for _, row := range s.scheds {
		out = append(out, s.withSlots(row))
	}
	return out, nil
}

func (s *Store) withSlots(row ports.ScheduleRow) ports.ScheduleRow {
	for k, task := range s.slots {
		if k.Schedule != row.ID {
			continue
		}
		row.LastDue = laterOf(row.LastDue, k.Due)
		if task == 0 {
			row.LastSkipped = laterOf(row.LastSkipped, k.Due)
		}
	}
	row.LastTask = s.slots[slot(row.ID, row.LastDue)]
	return row
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Skip records a slot that was not fired.
func (s *Store) Skip(_ context.Context, id domain.ScheduleID, due, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.slots[slot(id, due)]; ok {
		return ports.ErrFired
	}
	s.slots[slot(id, due)] = 0
	return nil
}
