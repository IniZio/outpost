package ports

import (
	"context"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// Enrollments stores pairing attempts. Each method runs the matching domain.Enrollment transition
// under the store's own lock, so a code is confirmed once and collected once even with several
// server processes.
type Enrollments interface {
	// Begin stores a waiting attempt. It returns domain.ErrTooManyPairings when too many wait.
	Begin(ctx context.Context, e domain.Enrollment) error
	// Confirm enrolls the machine for owner: it applies Enrollment.Confirm, writes the machine row
	// (the same owner and name keep their id and get the new key) and applies Enrolled. A failed
	// write applies Failed and returns the error. An unknown code is domain.ErrUnknownPairing.
	Confirm(ctx context.Context, code domain.UserCode, owner domain.Login, now time.Time) (domain.Enrollment, error)
	// Collect applies Enrollment.Collect to the attempt for a device code.
	Collect(ctx context.Context, device [32]byte, now time.Time) (e domain.Enrollment, ok bool, err error)
}
