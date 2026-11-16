package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// Name names one of an owner's secrets: "github" for the gh token, "claude" for the agent's
// OAuth token, and later one per credential host rule.
type Name string

var nameRule = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ErrBadName is a name outside [a-z][a-z0-9_]{0,63}.
var ErrBadName = errors.New("secrets: a name is a lower-case letter then up to 63 of a-z, 0-9 and _")

// ParseName checks a name from outside.
func ParseName(s string) (Name, error) {
	if !nameRule.MatchString(s) {
		return "", ErrBadName
	}
	return Name(s), nil
}

// Info is what may be shown about a secret: never its value.
type Info struct {
	Name    Name
	Updated time.Time
}

// Vault keeps each owner's secrets encrypted with AES-256-GCM. The owner and name are the
// additional data, so a row copied to another owner or name fails to open.
type Vault struct {
	rows  ports.SecretRows
	aead  cipher.AEAD
	clock domain.Clock
}

// NewVault opens the vault over rows with k's vault subkey.
func NewVault(k Key, rows ports.SecretRows, clock domain.Clock) (*Vault, error) {
	block, err := aes.NewCipher(k.Derive("vault"))
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Vault{rows: rows, aead: aead, clock: clock}, nil
}

func aad(owner domain.Login, name Name) []byte {
	return []byte("outpost secret\x00" + string(owner) + "\x00" + string(name))
}

// Put stores value as the owner's secret name, replacing any earlier one.
func (v *Vault) Put(ctx context.Context, owner domain.Login, name Name, value string) error {
	if value == "" {
		return fmt.Errorf("%w: secret %s is empty", domain.ErrInvalid, name)
	}
	sealed := v.aead.Seal(nil, nil, []byte(value), aad(owner, name))
	x := ports.SealedSecret{Owner: owner, Name: string(name), Sealed: sealed, Updated: v.clock.Now()}
	if err := v.rows.PutSecret(ctx, x); err != nil {
		return fmt.Errorf("secrets: put %s: %w", name, err)
	}
	return nil
}

// Get returns the owner's secret name. A missing one is ports.ErrNotFound.
func (v *Vault) Get(ctx context.Context, owner domain.Login, name Name) (string, error) {
	x, err := v.rows.Secret(ctx, owner, string(name))
	if err != nil {
		return "", fmt.Errorf("secrets: %s of %s: %w", name, owner, err)
	}
	plain, err := v.aead.Open(nil, nil, x.Sealed, aad(owner, name))
	if err != nil {
		return "", fmt.Errorf("secrets: %s of %s does not open with this key: %w", name, owner, err)
	}
	return string(plain), nil
}

// List names the owner's secrets.
func (v *Vault) List(ctx context.Context, owner domain.Login) ([]Info, error) {
	rows, err := v.rows.SecretList(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("secrets: list: %w", err)
	}
	out := make([]Info, len(rows))
	for i, r := range rows {
		out[i] = Info{Name: Name(r.Name), Updated: r.Updated}
	}
	return out, nil
}

// Delete removes the owner's secret name. A missing one is no error.
func (v *Vault) Delete(ctx context.Context, owner domain.Login, name Name) error {
	if err := v.rows.DeleteSecret(ctx, owner, string(name)); err != nil {
		return fmt.Errorf("secrets: delete %s: %w", name, err)
	}
	return nil
}
