package portstest

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/ports"
)

// Sessions keeps sessions in a map.
type Sessions struct {
	mu    sync.Mutex
	rows  map[string]ports.Session
	spent map[string]bool
}

var _ ports.Sessions = (*Sessions)(nil)

// NewSessions returns an empty store.
func NewSessions() *Sessions {
	return &Sessions{rows: map[string]ports.Session{}, spent: map[string]bool{}}
}

// PutSession stores a session.
func (s *Sessions) PutSession(_ context.Context, x ports.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[string(x.Hash)] = x
	return nil
}

// Session returns the unexpired, unspent session.
func (s *Sessions) Session(_ context.Context, hash []byte, now time.Time) (ports.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.rows[string(hash)]
	if !ok || !now.Before(x.Expires) || s.spent[string(hash)] {
		return ports.Session{}, ports.ErrNotFound
	}
	return x, nil
}

// Spend spends a link once.
func (s *Sessions) Spend(ctx context.Context, hash []byte, now time.Time) (ports.Session, error) {
	x, err := s.Session(ctx, hash, now)
	if err != nil || x.Kind != ports.SessionLink {
		return ports.Session{}, ports.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent[string(hash)] {
		return ports.Session{}, ports.ErrNotFound
	}
	s.spent[string(hash)] = true
	return x, nil
}

// DeleteSession removes a session.
func (s *Sessions) DeleteSession(_ context.Context, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, string(hash))
	return nil
}

// SessionsContract checks what ports.Sessions promises.
func SessionsContract(t *testing.T, s ports.Sessions) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	hash := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
	api := ports.Session{Hash: hash("a"), Login: "ana", Kind: ports.SessionAPI, Name: "cli", Expires: t0.Add(time.Hour)}
	link := ports.Session{Hash: hash("l"), Login: "ana", Kind: ports.SessionLink, Expires: t0.Add(time.Minute)}
	for _, x := range []ports.Session{api, link} {
		if err := s.PutSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.Session(ctx, api.Hash, t0); err != nil || got.Login != "ana" || got.Kind != ports.SessionAPI || got.Name != "cli" {
		t.Fatalf("a stored session reads back: %+v %v", got, err)
	}
	if _, err := s.Session(ctx, api.Hash, t0.Add(time.Hour)); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("an expired session is not found: %v", err)
	}
	if _, err := s.Spend(ctx, api.Hash, t0); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("an API token is not a link: %v", err)
	}
	if got, err := s.Spend(ctx, link.Hash, t0); err != nil || got.Login != "ana" {
		t.Fatalf("a link is spent once: %+v %v", got, err)
	}
	sessionsGone(t, s, link.Hash, t0)
	if err := s.DeleteSession(ctx, api.Hash); err != nil {
		t.Fatal(err)
	}
	sessionsGone(t, s, api.Hash, t0)
}

func sessionsGone(t *testing.T, s ports.Sessions, hash []byte, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Spend(ctx, hash, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("gone, so not spendable: %v", err)
	}
	if _, err := s.Session(ctx, hash, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("gone: %v", err)
	}
}
