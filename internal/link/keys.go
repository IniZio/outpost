package link

import (
	"context"

	"github.com/IniZio/outpost/internal/domain"
)

// KeyStore is what the server needs to know about a machine at hello time. It is read on every
// handshake, so revoking a machine (deleting its row) takes effect on its next connect, and
// Hub.Kick ends a live link at once. It returns domain.ErrUnknownMachine for an unknown or revoked
// id. It names only domain types, so a store adapter implements it without importing link.
type KeyStore interface {
	Lookup(ctx context.Context, id domain.MachineID) (domain.Machine, domain.MachineKey, error)
}

// Presence records links durably, so the order of two links is decided by the store and not by a
// process. Linked bumps the machine's generation and marks it online. Unlinked marks it offline only
// if gen is still the stored generation (UPDATE ... WHERE link_gen = gen). The compare lives in the
// store because during a deploy two server processes hold links to the same machine, and an
// in-process counter cannot order them.
type Presence interface {
	Linked(ctx context.Context, id domain.MachineID, build string) (domain.LinkGen, error)
	Unlinked(ctx context.Context, id domain.MachineID, gen domain.LinkGen) error
}

// capsOf derives the capabilities a machine is granted. Only a tool machine whose owner opted in at
// pairing gets exec, so the grant has no flag of its own to drift from the machine row.
func capsOf(m domain.Machine) CapSet {
	if m.Role == domain.RoleTool && m.Exec {
		return CapSet(0).With(CapExec)
	}
	return 0
}

// allCaps is what the server enables on its own registry and assumes of its peer's server side.
func allCaps() CapSet { return CapSet(0).With(CapExec) }
