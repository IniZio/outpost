package ports

import (
	"context"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// SealedSecret is one stored secret, already encrypted. internal/secrets seals and opens it, so the
// store only ever holds ciphertext.
type SealedSecret struct {
	Owner   domain.Login
	Name    string
	Sealed  []byte
	Updated time.Time
}

// SecretRows stores sealed secrets, one per owner and name.
type SecretRows interface {
	// PutSecret writes the row, replacing one of the same owner and name.
	PutSecret(ctx context.Context, s SealedSecret) error
	// Secret returns one row, or ErrNotFound.
	Secret(ctx context.Context, owner domain.Login, name string) (SealedSecret, error)
	// SecretList returns the owner's rows by name.
	SecretList(ctx context.Context, owner domain.Login) ([]SealedSecret, error)
	// DeleteSecret removes a row. A missing one is no error.
	DeleteSecret(ctx context.Context, owner domain.Login, name string) error
}
