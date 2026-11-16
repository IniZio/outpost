package portstest

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

type secretKey struct {
	owner domain.Login
	name  string
}

// SecretRows keeps sealed secrets in a map.
type SecretRows struct {
	mu   sync.Mutex
	rows map[secretKey]ports.SealedSecret
}

var _ ports.SecretRows = (*SecretRows)(nil)

// NewSecretRows returns an empty store.
func NewSecretRows() *SecretRows {
	return &SecretRows{rows: map[secretKey]ports.SealedSecret{}}
}

// PutSecret writes a row.
func (s *SecretRows) PutSecret(_ context.Context, x ports.SealedSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	x.Sealed = slices.Clone(x.Sealed)
	s.rows[secretKey{x.Owner, x.Name}] = x
	return nil
}

// Secret returns one row.
func (s *SecretRows) Secret(_ context.Context, owner domain.Login, name string) (ports.SealedSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.rows[secretKey{owner, name}]
	if !ok {
		return ports.SealedSecret{}, ports.ErrNotFound
	}
	return x, nil
}

// SecretList returns the owner's rows by name.
func (s *SecretRows) SecretList(_ context.Context, owner domain.Login) ([]ports.SealedSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.SealedSecret
	for _, k := range slices.SortedFunc(maps.Keys(s.rows), func(a, b secretKey) int { return cmp.Compare(a.name, b.name) }) {
		if k.owner == owner {
			out = append(out, s.rows[k])
		}
	}
	return out, nil
}

// DeleteSecret removes a row.
func (s *SecretRows) DeleteSecret(_ context.Context, owner domain.Login, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, secretKey{owner, name})
	return nil
}

// SecretRowsContract checks what ports.SecretRows promises.
func SecretRowsContract(t *testing.T, s ports.SecretRows) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	rows := []ports.SealedSecret{
		{Owner: "ana", Name: "github", Sealed: []byte("g1"), Updated: t0},
		{Owner: "ana", Name: "claude", Sealed: []byte("c1"), Updated: t0},
		{Owner: "bo", Name: "github", Sealed: []byte("b1"), Updated: t0},
		{Owner: "ana", Name: "github", Sealed: []byte("g2"), Updated: t0.Add(time.Hour)},
	}
	for _, x := range rows {
		if err := s.PutSecret(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.Secret(ctx, "ana", "github"); err != nil || string(got.Sealed) != "g2" || !got.Updated.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a put replaces the row of the same owner and name: %+v %v", got, err)
	}
	if list, err := s.SecretList(ctx, "ana"); err != nil || len(list) != 2 || list[0].Name != "claude" || list[1].Name != "github" {
		t.Fatalf("the owner's rows by name: %+v %v", list, err)
	}
	secretRowsDelete(t, s)
}

func secretRowsDelete(t *testing.T, s ports.SecretRows) {
	t.Helper()
	ctx := context.Background()
	if err := s.DeleteSecret(ctx, "ana", "github"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "ana", "github"); err != nil {
		t.Fatalf("deleting a missing row is no error: %v", err)
	}
	if _, err := s.Secret(ctx, "ana", "github"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("a deleted row is gone: %v", err)
	}
	if got, err := s.Secret(ctx, "bo", "github"); err != nil || string(got.Sealed) != "b1" {
		t.Fatalf("another owner's row is untouched: %+v %v", got, err)
	}
}
