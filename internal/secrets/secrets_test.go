package secrets

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

func newKey(t *testing.T) Key {
	t.Helper()
	k, err := ParseKey(NewKey())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseKey(t *testing.T) {
	raw := strings.Repeat("A", 43) // 32 zero bytes, unpadded
	for _, s := range []string{raw, raw + "=", " " + raw + "=\n", strings.Repeat("_", 43)} {
		if _, err := ParseKey(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "short", strings.Repeat("A", 44), "!!" + raw[2:]} {
		if _, err := ParseKey(s); !errors.Is(err, ErrBadKey) {
			t.Errorf("%q is refused, got %v", s, err)
		}
	}
	k := newKey(t)
	if bytes.Equal(k.Derive("vault"), k.Derive("egress")) || len(k.Derive("vault")) != KeySize {
		t.Fatal("each purpose has its own 32-byte subkey")
	}
	if !bytes.Equal(k.Derive("vault"), k.Derive("vault")) {
		t.Fatal("a subkey is stable")
	}
}

func TestParseName(t *testing.T) {
	for _, s := range []string{"github", "claude", "a", "linear_2"} {
		if _, err := ParseName(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "GitHub", "2fa", "a-b", "a/b", strings.Repeat("a", 65)} {
		if _, err := ParseName(s); !errors.Is(err, ErrBadName) {
			t.Errorf("%q is refused, got %v", s, err)
		}
	}
}

func TestVaultRoundTripsAndHoldsOnlyCiphertext(t *testing.T) {
	ctx := context.Background()
	rows := portstest.NewSecretRows()
	t0 := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	v, err := NewVault(newKey(t), rows, domain.NewManualClock(t0))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Put(ctx, "ana", "github", "gho_secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := v.Get(ctx, "ana", "github"); err != nil || got != "gho_secret" {
		t.Fatalf("round trip: %q %v", got, err)
	}
	row, _ := rows.Secret(ctx, "ana", "github")
	if bytes.Contains(row.Sealed, []byte("gho_secret")) || !row.Updated.Equal(t0) {
		t.Fatalf("the row holds ciphertext and the put time: %+v", row)
	}
	if list, err := v.List(ctx, "ana"); err != nil || len(list) != 1 || list[0].Name != "github" || !list[0].Updated.Equal(t0) {
		t.Fatalf("list: %+v %v", list, err)
	}
}

func TestVaultRefusesEmptyAndForgetsDeleted(t *testing.T) {
	ctx := context.Background()
	v, _ := NewVault(newKey(t), portstest.NewSecretRows(), domain.NewManualClock(time.Time{}))
	if err := v.Put(ctx, "ana", "claude", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("an empty value is refused: %v", err)
	}
	if err := v.Put(ctx, "ana", "github", "gho_secret"); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete(ctx, "ana", "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get(ctx, "ana", "github"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("a deleted secret is not found: %v", err)
	}
}

func TestARowMovedToAnotherOwnerOrKeyDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	rows := portstest.NewSecretRows()
	clock := domain.NewManualClock(time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC))
	k := newKey(t)
	v, _ := NewVault(k, rows, clock)
	if err := v.Put(ctx, "ana", "github", "gho_secret"); err != nil {
		t.Fatal(err)
	}
	row, _ := rows.Secret(ctx, "ana", "github")
	for _, moved := range []ports.SealedSecret{
		{Owner: "bo", Name: "github", Sealed: row.Sealed},
		{Owner: "ana", Name: "claude", Sealed: row.Sealed},
	} {
		_ = rows.PutSecret(ctx, moved)
		if _, err := v.Get(ctx, moved.Owner, Name(moved.Name)); err == nil {
			t.Fatalf("a row copied to %s/%s opens", moved.Owner, moved.Name)
		}
	}
	other, _ := NewVault(newKey(t), rows, clock)
	if _, err := other.Get(ctx, "ana", "github"); err == nil {
		t.Fatal("a row opens under another key")
	}
}
