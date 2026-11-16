package link

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Side says which end serves an operation. The opener of a stream is the other side.
type Side uint8

const (
	// ServedByAgent ops are opened by the server and run on the machine.
	ServedByAgent Side = iota + 1
	// ServedByServer ops are opened by the agent and run on the server (reports, port-forward
	// accepts, box egress).
	ServedByServer
)

// Cap is a capability a machine must have been granted for an op to run. Zero means none needed.
// Both ends enforce it: the caller refuses to open the stream, and the serving registry refuses to
// run the handler.
type Cap uint8

const (
	// CapExec runs a command on the machine itself. Only tool machines whose owner opted in at
	// pairing get it.
	CapExec Cap = iota + 1
)

// CapSet is the capabilities granted to one machine.
type CapSet uint8

// Has reports whether c is granted. An op that needs no capability is always allowed.
func (s CapSet) Has(c Cap) bool { return c == 0 || s&(1<<c) != 0 }

// With returns s plus c.
func (s CapSet) With(c Cap) CapSet { return s | 1<<c }

type kind uint8

const (
	kindUnary kind = iota + 1
	kindStream
	kindResumable
)

// resumable reports whether a drain cuts this kind at once and Hold reopens it.
func (k kind) resumable() bool { return k == kindResumable }

type spec struct {
	name  string
	side  Side
	needs Cap
	kind  kind
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$`)

// Op is a finite request that gets one reply. Req and Resp are the wire shapes, serialised as
// JSON. A drain waits for it up to the budget, then cuts it. It is never replayed. The zero Op is
// invalid, only NewOp makes one.
type Op[Req, Resp any] struct{ s spec }

// StreamOp is a finite request that gets one reply and then becomes a byte stream. Resp is the
// opening reply, for example the address a listener bound. The bytes that follow are untyped. A
// drain treats it like an Op: waited for, then cut, never replayed. A cut stream reads as a clean
// EOF, the same as one the peer finished, so a finite op that must tell the two apart carries its
// own end marker in the bytes (a length, a trailer, an exit record).
type StreamOp[Req, Resp any] struct{ s spec }

// ResumableOp is a long-lived, idempotent stream op, such as an hc byte stream, a held listen or a
// machine report. A drain cuts it at once, and Hold opens it again on the next link. Its handler
// adopts state it already holds (the Mac keeps a listener by forward id, hc keeps its session by
// box) instead of creating it afresh, because it cannot tell a reopen from a first open.
type ResumableOp[Req, Resp any] struct{ s spec }

// Catalogue is the list of every operation a binary knows. internal/ops builds the one catalogue
// and hands it to both ends. A Registry checks itself against it, so an op that nobody serves is a
// startup failure, not a runtime surprise.
type Catalogue struct {
	mu  sync.Mutex
	ops map[string]spec
}

// NewCatalogue returns an empty catalogue.
func NewCatalogue() *Catalogue { return &Catalogue{ops: map[string]spec{}} }

// NewOp declares a finite unary op in c. A bad or repeated name is a programming error and panics
// at package initialisation.
func NewOp[Req, Resp any](c *Catalogue, name string, side Side, needs ...Cap) Op[Req, Resp] {
	return Op[Req, Resp]{c.add(name, side, kindUnary, needs)}
}

// NewStreamOp declares a finite stream op in c, under the same rules as NewOp.
func NewStreamOp[Req, Resp any](c *Catalogue, name string, side Side, needs ...Cap) StreamOp[Req, Resp] {
	return StreamOp[Req, Resp]{c.add(name, side, kindStream, needs)}
}

// NewResumableOp declares a resumable stream op in c, under the same rules as NewOp.
func NewResumableOp[Req, Resp any](c *Catalogue, name string, side Side, needs ...Cap) ResumableOp[Req, Resp] {
	return ResumableOp[Req, Resp]{c.add(name, side, kindResumable, needs)}
}

func (c *Catalogue) add(name string, side Side, k kind, needs []Cap) spec {
	if !nameRE.MatchString(name) || len(needs) > 1 || strings.HasPrefix(name, "link.") != (c == ctlCat) {
		panic(fmt.Sprintf("link: bad op declaration %q", name))
	}
	s := spec{name: name, side: side, kind: k}
	if len(needs) == 1 {
		s.needs = needs[0]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.ops[name]; dup {
		panic(fmt.Sprintf("link: op %q declared twice", name))
	}
	c.ops[name] = s
	return s
}

// served returns the ops a side must serve, sorted by name.
func (c *Catalogue) served(side Side) []spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []spec
	for _, s := range c.ops {
		if s.side == side {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b spec) int { return strings.Compare(a.name, b.name) })
	return out
}

// Validator is implemented by request types that carry rules beyond their shape: an env var name,
// a path inside a box. The dispatcher calls it before the handler, so handlers trust their input.
// This is the one place those rules live.
type Validator interface{ Validate() error }
