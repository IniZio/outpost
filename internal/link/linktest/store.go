package linktest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// Store is an in-memory key store and presence record. Its generation compare is the one a SQL
// store does with UPDATE ... WHERE link_gen = gen, so tests see the real ordering rules.
type Store struct {
	mu       sync.Mutex
	next     int
	clock    domain.SystemClock
	machines map[domain.MachineID]*row
}

type row struct {
	m      domain.Machine
	key    domain.MachineKey
	gen    domain.LinkGen
	online bool
	since  time.Time
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{machines: map[domain.MachineID]*row{}} }

// Pair adds a machine and returns its id and private key. A zero m.ID is assigned.
func (s *Store) Pair(m domain.Machine) (domain.MachineID, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		s.next++
		m.ID = domain.MachineID(fmt.Sprintf("m%d", s.next))
	}
	r := &row{m: m}
	copy(r.key[:], pub)
	s.machines[m.ID] = r
	return m.ID, priv
}

// Remove deletes a machine, which is what a revoke does to its row.
func (s *Store) Remove(id domain.MachineID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.machines, id)
}

// Lookup implements link.KeyStore.
func (s *Store) Lookup(_ context.Context, id domain.MachineID) (domain.Machine, domain.MachineKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.machines[id]
	if !ok {
		return domain.Machine{}, domain.MachineKey{}, domain.ErrUnknownMachine
	}
	return r.m, r.key, nil
}

// Linked implements link.Presence: it bumps the generation and marks the machine online.
func (s *Store) Linked(_ context.Context, id domain.MachineID, _ string) (domain.LinkGen, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.machines[id]
	if !ok {
		return 0, domain.ErrUnknownMachine
	}
	r.gen++
	r.online = true
	r.since = s.clock.Now()
	return r.gen, nil
}

// Unlinked implements link.Presence: it marks the machine offline only if gen is still current.
func (s *Store) Unlinked(_ context.Context, id domain.MachineID, gen domain.LinkGen) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.machines[id]; ok && r.gen == gen {
		r.online = false
		r.since = s.clock.Now()
	}
	return nil
}

// Seen implements link.Presence.
func (s *Store) Seen(_ context.Context, id domain.MachineID) (bool, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.machines[id]; ok {
		return r.online, r.since, nil
	}
	return false, time.Time{}, nil
}

// Online reports whether the store thinks the machine has a live link.
func (s *Store) Online(id domain.MachineID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.machines[id]
	return ok && r.online
}

// Gen is the machine's current link generation.
func (s *Store) Gen(id domain.MachineID) domain.LinkGen {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.machines[id]; ok {
		return r.gen
	}
	return 0
}
