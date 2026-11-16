package domain

import "errors"

// MachineKey is a machine's ed25519 public key. It is a fixed-size array, so it compares with ==
// and cannot be a truncated slice.
type MachineKey [32]byte

// LinkGen numbers a machine's links. The store bumps it each time a machine links and marks the
// machine offline only when the closing link's generation is still the stored one. During a deploy
// two server processes briefly hold a link to the same machine, and only a durable generation can
// order their closes.
type LinkGen uint64

// ErrUnknownMachine is what a machine lookup returns for an id with no row, or a revoked one.
var ErrUnknownMachine = errors.New("unknown machine")
