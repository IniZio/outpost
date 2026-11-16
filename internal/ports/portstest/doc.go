// Package portstest holds in-memory fakes of the ports, for the lifecycle's tests and for any
// adapter test that needs the other side. They are deterministic: nothing in them reads a clock or
// sleeps, and a test waits on the channels and hooks they offer.
package portstest

import "github.com/IniZio/outpost/internal/ports"

var (
	_ ports.Store   = (*Store)(nil)
	_ ports.Boxes   = (*Boxes)(nil)
	_ ports.Agent   = (*Agent)(nil)
	_ ports.Secrets = (*Secrets)(nil)
)
