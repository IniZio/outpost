package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/secrets"
	"github.com/IniZio/outpost/internal/store/postgres"
)

// noCredentials hands a box no credentials. It stands in until the egress grants land; a box can
// then clone only public repos and Claude has no token.
type noCredentials struct{}

func (noCredentials) Credentials(context.Context, ports.Target, domain.Login, []domain.RepoName) (ports.Creds, error) {
	return ports.Creds{}, nil
}

// presence is the store's link.Presence that pokes the scheduler when a machine links or drops, so
// a queued task starts as soon as its machine is there.
type presence struct {
	*postgres.Store
	poke atomic.Pointer[func()]
}

func (p *presence) Linked(ctx context.Context, id domain.MachineID, build string) (domain.LinkGen, error) {
	gen, err := p.Store.Linked(ctx, id, build)
	p.nudge()
	return gen, err //nolint:wrapcheck // the store wraps its own
}

func (p *presence) Unlinked(ctx context.Context, id domain.MachineID, gen domain.LinkGen) error {
	err := p.Store.Unlinked(ctx, id, gen)
	p.nudge()
	return err //nolint:wrapcheck // the store wraps its own
}

func (p *presence) nudge() {
	if f := p.poke.Load(); f != nil {
		(*f)()
	}
}

// machines revokes a machine in the store and ends its live link at once.
type machines struct {
	*postgres.Store
	hub *link.Hub
}

func (m machines) DeleteMachine(ctx context.Context, id domain.MachineID) (bool, error) {
	ok, err := m.Store.DeleteMachine(ctx, id)
	if ok {
		m.hub.Kick(id)
	}
	return ok, err //nolint:wrapcheck // the store wraps its own
}

// pairing kicks the old link of a machine paired again under the same name, which keeps its id and
// gets a new key.
type pairing struct {
	*postgres.Store
	hub *link.Hub
}

func (p pairing) Confirm(ctx context.Context, code domain.UserCode, owner domain.Login, now time.Time) (domain.Enrollment, error) {
	e, err := p.Store.Confirm(ctx, code, owner, now)
	if err == nil {
		p.hub.Kick(e.Machine)
	}
	return e, err //nolint:wrapcheck // the store wraps its own
}

// vault is the secrets vault as the API's write-only Secrets.
type vault struct{ v *secrets.Vault }

func (v vault) SetSecret(ctx context.Context, owner domain.Login, name, value string) error {
	n, err := secrets.ParseName(name)
	if err != nil {
		return fmt.Errorf("%w: %w", domain.ErrInvalid, err)
	}
	return v.v.Put(ctx, owner, n, value) //nolint:wrapcheck // the vault wraps its own
}
