package ports

import (
	"context"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// SessionKind says what a session may do. An API token acts for its login. A web session, from a
// cookie, only watches. A link is spent once to start a web session.
type SessionKind uint8

const (
	SessionAPI SessionKind = iota + 1
	SessionLink
	SessionWeb
)

// Session is one stored credential. Hash is the SHA-256 of the raw token, which only its holder has.
type Session struct {
	Hash    []byte
	Login   domain.Login
	Kind    SessionKind
	Name    string
	Expires time.Time
}

// Sessions stores sessions. internal/auth owns the tokens and their rules.
type Sessions interface {
	PutSession(ctx context.Context, s Session) error
	// Session returns the unexpired session for hash. A missing, expired or spent one is
	// ErrNotFound.
	Session(ctx context.Context, hash []byte, now time.Time) (Session, error)
	// Spend marks an unexpired, unspent link as spent and returns it, once. Otherwise ErrNotFound.
	Spend(ctx context.Context, hash []byte, now time.Time) (Session, error)
	DeleteSession(ctx context.Context, hash []byte) error
}
