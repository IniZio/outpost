package link

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/IniZio/outpost/internal/domain"
)

// Timing holds every duration the package uses, so tests shrink them and nothing is a literal in
// logic. Liveness is yamux's keepalive only: a dead peer is noticed within KeepAlive plus the pong
// wait (also KeepAlive), 30 s by default. WebSocket pings are not used.
type Timing struct {
	Dial        time.Duration // the agent's WebSocket dial and handshake, together
	Handshake   time.Duration // each handshake frame, read or write
	KeepAlive   time.Duration // yamux ping interval and pong wait
	StreamOpen  time.Duration // peer must ack a new stream within this
	StreamClose time.Duration // a half-closed stream the peer never finishes is reclaimed after this
	Drain       time.Duration // how long a retiring session may finish in-flight streams
	LinkWait    time.Duration // a Call waits this long for a machine whose link is moving
	MaxInflight int           // concurrent ops per session, both directions together
}

// DefaultTiming is what production uses.
func DefaultTiming() Timing {
	return Timing{
		Dial:        15 * time.Second,
		Handshake:   10 * time.Second,
		KeepAlive:   15 * time.Second,
		StreamOpen:  15 * time.Second,
		StreamClose: 30 * time.Second,
		Drain:       30 * time.Second,
		LinkWait:    15 * time.Second,
		MaxInflight: 128,
	}
}

func (t Timing) yamux() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = t.KeepAlive
	c.ConnectionWriteTimeout = t.KeepAlive
	c.StreamOpenTimeout = t.StreamOpen
	c.StreamCloseTimeout = t.StreamClose
	c.LogOutput = io.Discard
	return c
}

// Info describes one live link. It is a value, safe to keep.
type Info struct {
	Machine domain.MachineID
	Gen     domain.LinkGen
	Build   string
	Proto   uint16
	Since   time.Time
}

// session is one live connection after a successful handshake: the yamux mux, the registries that
// serve the peer's streams and the in-flight set that drain waits on. The server creates one per
// accepted link and the agent one per dial. A session only moves live, then retiring, then closed.
type session struct {
	info     Info
	peerCaps CapSet // what the peer may be asked to do (server side: from the key store)
	peer     Caller // routes ops back to the peer's newest session, for Meta.Peer
	mux      *yamux.Session
	reg      *Registry // serves streams the peer opens
	ctl      *Registry // built-in link.* ops, served on both sides
	flight   *flight
}

// sessionOpts is everything newSession needs beyond the conn.
type sessionOpts struct {
	Info     Info
	PeerCaps CapSet
	Peer     Caller
	Reg      *Registry // serves the peer's ops
	Ctl      *Registry // the agent's relink handler; empty on the server
	Timing   Timing
}

// newSession wraps a post-handshake conn. client is the agent's end: yamux gives the dialler odd
// stream ids and the acceptor even ones, which is the only difference between the two sides.
func newSession(conn net.Conn, client bool, o sessionOpts) (*session, error) {
	start := yamux.Server
	if client {
		start = yamux.Client
	}
	mux, err := start(conn, o.Timing.yamux())
	if err != nil {
		return nil, fmt.Errorf("link: start session: %w", err)
	}
	return &session{
		info: o.Info, peerCaps: o.PeerCaps, peer: o.Peer, mux: mux, reg: o.Reg, ctl: o.Ctl,
		flight: newFlight(o.Timing.MaxInflight),
	}, nil
}

// session makes a *session a Caller pinned to itself. Control ops use this so they reach the
// session they are about, not the newest one a Link routes to, and are not refused by the hub's
// draining flag.
func (s *session) session(context.Context) (*session, error) { return s, nil }

func (s *session) pacer() pacer { return pacer{} }

// Done is closed when the mux dies: keepalive timeout, conn error, or Close.
func (s *session) Done() <-chan struct{} { return s.mux.CloseChan() }

// serve accepts the peer's streams until the mux ends. Each stream is admitted to the in-flight
// set first, so a flood is answered with CodeBusy instead of goroutines, and a retiring session
// answers CodeDraining. ctx given to handlers is cancelled when serve returns, which is when the
// mux dies, and per stream when a drain cuts it.
func (s *session) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := Meta{Machine: s.info.Machine, Gen: s.info.Gen, Peer: s.peer}
	for {
		ys, err := s.mux.AcceptStream()
		if err != nil {
			return
		}
		st := newStream(ys, s.flight)
		sctx, scancel := context.WithCancel(ctx)
		st.cancel = scancel
		if err := s.flight.enter(st); err != nil {
			go reject(st, err)
			continue
		}
		go func() {
			defer func() { _ = st.Close() }()
			dispatch(sctx, m, st, s.ctl, s.reg)
		}()
	}
}

func reject(st *stream, err error) {
	defer func() { _ = st.Close() }()
	_ = writeReply(st, struct{}{}, err)
}

// open starts a stream for an op this end opens. The slot is released when the stream is closed.
func (s *session) open(k kind) (*stream, error) {
	ys, err := s.mux.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLinkLost, err) // ErrRemoteGoAway: the peer is retiring this session
	}
	st := newStream(ys, s.flight)
	st.kind = k
	if err := s.flight.enter(st); err != nil {
		_ = ys.Close()
		return nil, err
	}
	return st, nil
}

// retire stops taking new streams, cuts the resumable ones and lets finite ones finish. It is the
// make-before-break half of a relink, and what a replaced session gets.
//
// The mux is closed only after the finite streams are idle: closing earlier drops their last
// frames. A stream still running when ctx ends is cut, its caller sees ErrLinkLost, and it is never
// replayed.
func (s *session) retire(ctx context.Context) {
	s.flight.seal()
	s.flight.cutResumable()
	_ = s.mux.GoAway()
	select {
	case <-s.flight.idle():
	case <-ctx.Done():
	case <-s.Done():
	}
	_ = s.mux.Close()
}

// kill ends the session without waiting for anything. The mux is shut the moment Close starts, so
// routing stops at once, but closing the WebSocket underneath waits for the peer's close frame, so a
// peer that never answers must not hold up the caller.
func (s *session) kill() { go func() { _ = s.mux.Close() }() }

// stream is a yamux stream that tells the in-flight set when it ends. It is the Stream the public
// API returns. kind is owned by the flight's mutex.
type stream struct {
	*yamux.Stream
	fl     *flight
	kind   kind
	cancel context.CancelFunc // ends the handler's ctx, for a stream this end accepted
	once   sync.Once
}

func newStream(ys *yamux.Stream, fl *flight) *stream { return &stream{Stream: ys, fl: fl} }

// setKind records what an accepted stream turned out to be. It reports false if the session
// already cut its resumable streams, in which case the handler must not run.
func (s *stream) setKind(k kind) bool { return s.fl.setKind(s, k) }

// CloseWrite sends FIN. yamux's Close is a half-close: reads continue until the peer finishes.
func (s *stream) CloseWrite() error {
	if err := s.Stream.Close(); err != nil {
		return fmt.Errorf("link: close write: %w", err)
	}
	return nil
}

// Close ends both directions: FIN, then unblock any local reader, then release the slot. yamux 0.1
// has no reset, so a peer that never finishes is bounded by Timing.StreamClose.
func (s *stream) Close() error {
	err := s.Stream.Close()
	_ = s.SetReadDeadline(time.Unix(1, 0))
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.fl.exit(s)
	})
	if err != nil {
		return fmt.Errorf("link: close stream: %w", err)
	}
	return nil
}

// flight is the session's in-flight set, both directions. Drain's whole bookkeeping is here: seal
// stops admission, cutResumable ends the streams a holder will reopen, and idle fires when no
// finite stream is left.
type flight struct {
	mu      sync.Mutex
	max     int
	sealed  bool
	cut     bool
	live    map[*stream]struct{}
	finite  int
	idleCh  chan struct{} // closed once sealed and no finite stream is left
	idleSet bool
}

func newFlight(max int) *flight {
	return &flight{max: max, live: map[*stream]struct{}{}, idleCh: make(chan struct{})}
}

// enter admits st. A stream whose kind is not known yet (an accepted one) counts as finite until
// setKind says otherwise.
func (f *flight) enter(st *stream) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.sealed:
		return Errorf(CodeDraining, "session is retiring")
	case len(f.live) >= f.max:
		return Errorf(CodeBusy, "%d operations in flight", len(f.live))
	}
	f.live[st] = struct{}{}
	if !st.kind.resumable() {
		f.finite++
	}
	return nil
}

func (f *flight) setKind(st *stream, k kind) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k.resumable() && f.cut {
		return false
	}
	if _, ok := f.live[st]; ok && k.resumable() && !st.kind.resumable() {
		f.finite--
		f.maybeIdle()
	}
	st.kind = k
	return true
}

func (f *flight) exit(st *stream) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[st]; !ok {
		return
	}
	delete(f.live, st)
	if !st.kind.resumable() {
		f.finite--
	}
	f.maybeIdle()
}

// seal stops admission. It is idempotent.
func (f *flight) seal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed = true
	f.maybeIdle()
}

// cutResumable closes every resumable stream now and refuses to start new ones. Their holders
// reopen them on the next link.
func (f *flight) cutResumable() {
	f.mu.Lock()
	f.cut = true
	var cut []*stream
	for st := range f.live {
		if st.kind.resumable() {
			cut = append(cut, st)
		}
	}
	f.mu.Unlock()
	for _, st := range cut {
		_ = st.Close()
	}
}

func (f *flight) maybeIdle() {
	if f.sealed && f.finite == 0 && !f.idleSet {
		f.idleSet = true
		close(f.idleCh)
	}
}

// idle is closed when the session is sealed and no finite stream is running.
func (f *flight) idle() <-chan struct{} { return f.idleCh }

// ctlCat is the private catalogue of built-in ops, served by the agent for the server.
var ctlCat = NewCatalogue()

// newCtlRegistry returns the registry for the built-in ops. The agent fills it, the server leaves
// it empty.
func newCtlRegistry() *Registry { return NewRegistry(ctlCat, ServedByAgent, 0) }
