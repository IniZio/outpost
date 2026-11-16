package link

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/IniZio/outpost/internal/domain"
)

// HubConfig wires the server side. Reg serves the ops agents open (reports, accepts, egress).
type HubConfig struct {
	Host     string // HostOf the public URL agents dial; bound into every proof
	Keys     KeyStore
	Presence Presence
	Reg      *Registry
	Clock    domain.Clock
	Timing   Timing
	Backoff  Backoff // paces Hold on this hub's links
	Sleep    Sleeper
	Jitter   func() float64
	MinProto uint16
	Rand     io.Reader
	Log      *slog.Logger
}

// Hub accepts agent links and hands out per-machine Links. There is one per server process, and
// one process is active at a time: a deploy overlaps the old and the new for the drain budget.
type Hub struct {
	cfg      HubConfig
	draining atomic.Bool
	pending  chan struct{} // capacity bounds handshakes in progress
	mu       sync.Mutex
	links    map[domain.MachineID]*Link
}

// NewHub returns a hub that accepts nothing until its Handler is mounted. Zero parts of cfg take
// production defaults.
func NewHub(cfg HubConfig) (*Hub, error) {
	if cfg.Host == "" || cfg.Keys == nil || cfg.Presence == nil || cfg.Reg == nil {
		return nil, errors.New("link: hub needs a host, a key store, presence and a registry")
	}
	if cfg.Clock == nil {
		cfg.Clock = domain.SystemClock{}
	}
	if cfg.Timing == (Timing{}) {
		cfg.Timing = DefaultTiming()
	}
	if cfg.MinProto == 0 {
		cfg.MinProto = 1
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Hub{cfg: cfg, pending: make(chan struct{}, 64), links: map[domain.MachineID]*Link{}}, nil
}

// Handler is the WebSocket endpoint, mounted at /machines/link by cmd/outpost. The api package
// never sees this package: it gets an http.Handler.
func (h *Hub) Handler() http.Handler { return http.HandlerFunc(h.serveHTTP) }

func (h *Hub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.pending <- struct{}{}:
	default:
		http.Error(w, "too many handshakes", http.StatusServiceUnavailable)
		return
	}
	var once sync.Once
	release := func() { once.Do(func() { <-h.pending }) }
	defer release()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	c.SetReadLimit(maxHandshake)
	acc, err := h.serverHandshake(r.Context(), c)
	release()
	if err != nil {
		h.reject(c, err)
		return
	}
	h.run(r.Context(), c, acc)
}

// reject closes a socket whose handshake failed. A refusal the agent was told about closes
// normally. Anything else is the server's own trouble and closes with 1011.
func (h *Hub) reject(c *websocket.Conn, err error) {
	code := websocket.StatusInternalError
	if errors.Is(err, errRejected) || errors.Is(err, errRelinkLater) {
		code = websocket.StatusNormalClosure
	} else {
		h.cfg.Log.Warn("link handshake failed", "err", err)
	}
	_ = c.Close(code, "")
}

// run serves one welcomed link until it ends.
func (h *Hub) run(ctx context.Context, c *websocket.Conn, acc accepted) {
	l := h.Link(acc.machine.ID)
	s, err := newSession(websocket.NetConn(ctx, c, websocket.MessageBinary), false, sessionOpts{
		Info:     Info{Machine: acc.machine.ID, Gen: acc.gen, Build: acc.build, Proto: acc.proto, Since: h.cfg.Clock.Now()},
		PeerCaps: capsOf(acc.machine), Peer: l, Reg: h.cfg.Reg, Ctl: newCtlRegistry(), Timing: h.cfg.Timing,
	})
	if err != nil {
		h.unlinked(acc.machine.ID, acc.gen)
		_ = c.Close(websocket.StatusInternalError, "")
		return
	}
	if old := l.attach(s); old != nil {
		go h.retire(old)
	}
	if !h.stillKnown(ctx, acc) {
		s.kill()
	}
	if h.draining.Load() {
		go h.handOff(context.WithoutCancel(ctx), s)
	}
	s.serve(ctx)
	l.detach(s)
}

// stillKnown re-reads the machine after its link is attached. A revoke that landed between the
// handshake's lookup and the attach has already run Kick, which saw no link to close, so without
// this check the revoked machine would stay linked. A failed lookup is not a revoke.
func (h *Hub) stillKnown(ctx context.Context, acc accepted) bool {
	_, key, err := h.cfg.Keys.Lookup(ctx, acc.machine.ID)
	if errors.Is(err, domain.ErrUnknownMachine) {
		return false
	}
	return err != nil || key == acc.key
}

func (h *Hub) retire(s *session) {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.Timing.Drain)
	defer cancel()
	s.retire(ctx)
}

// unlinked tells the store a link of generation gen ended. The store applies it only if gen is
// still the machine's current generation.
func (h *Hub) unlinked(id domain.MachineID, gen domain.LinkGen) {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.Timing.Handshake)
	defer cancel()
	if err := h.cfg.Presence.Unlinked(ctx, id, gen); err != nil {
		h.cfg.Log.Warn("link offline not recorded", "machine", id, "err", err)
	}
}

// Link returns the handle for a machine. It is never nil and is valid before the machine ever
// connects, so callers keep it instead of looking it up per call.
func (h *Hub) Link(id domain.MachineID) *Link {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.links[id]
	if !ok {
		l = &Link{id: id, hub: h, changed: make(chan struct{})}
		h.links[id] = l
	}
	return l
}

// Kick ends a machine's live link now, for a revoke. The agent's next hello is refused because the
// key store no longer knows it. Calls for the machine fail at once, without waiting for a link to
// return. Calling it for an offline machine does nothing.
func (h *Hub) Kick(id domain.MachineID) {
	l := h.Link(id)
	l.mu.Lock()
	l.gone = true
	s := l.cur.Load()
	l.mu.Unlock()
	if s != nil {
		s.kill()
	}
}

// Drain is the server half of a deploy. Call it on SIGTERM with a ctx bounded by the drain budget.
// New hellos are answered with relink and new ops are refused. Every live link is asked to relink,
// which the agent acks, then dials a successor, moves its traffic there and closes this link once
// its finite streams are done. A stream still running when ctx ends is cut and never replayed. It
// returns an error if any link had to be cut.
func (h *Hub) Drain(ctx context.Context) error {
	h.draining.Store(true)
	var wg sync.WaitGroup
	var cut atomic.Int32
	for _, s := range h.sessions() {
		wg.Go(func() {
			if !h.handOff(ctx, s) {
				cut.Add(1)
			}
		})
	}
	wg.Wait()
	if n := cut.Load(); n > 0 {
		return fmt.Errorf("link: drain ended with %d links cut", n)
	}
	return nil
}

func (h *Hub) sessions() []*session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*session
	for _, l := range h.links {
		if s := l.cur.Load(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// handOff asks one agent to relink, cuts the session's resumable streams and waits for the agent to
// let go. It reports whether the agent did before ctx ended. It does not seal the session or send
// GoAway: the draining flag already keeps this server from opening ops, and the agent's own ops keep
// being served on the old link until its successor is up, when the agent retires it. An agent that
// does not ack (an old one) gets a plain retire instead, so it sees the close and reconnects.
func (h *Hub) handOff(ctx context.Context, s *session) bool {
	if _, err := Call(ctx, s, ctlRelink, struct{}{}); err != nil {
		h.cfg.Log.Warn("link relink not acked", "machine", s.info.Machine, "err", err)
		s.retire(ctx)
		return ctx.Err() == nil
	}
	s.flight.cutResumable()
	select {
	case <-s.Done():
		return true
	case <-ctx.Done():
		_ = s.mux.Close()
		return false
	}
}

// Link is the server's handle on one machine. It survives reconnects: the session it routes to
// changes, the Link does not. It implements Caller.
type Link struct {
	id  domain.MachineID
	hub *Hub
	cur atomic.Pointer[session] // read lock-free by every Call

	mu      sync.Mutex // guards the two fields below, and orders attach and detach
	changed chan struct{}
	gone    bool // Kick ended the link: do not wait for it to return
}

// Machine is the id this link is for.
func (l *Link) Machine() domain.MachineID { return l.id }

// Info returns the live link's details, or false if the machine is offline.
func (l *Link) Info() (Info, bool) {
	s := l.cur.Load()
	if s == nil || s.mux.IsClosed() {
		return Info{}, false
	}
	return s.info, true
}

func (l *Link) pacer() pacer {
	c := l.hub.cfg
	return pacer{backoff: c.Backoff, sleep: c.Sleep, jitter: c.Jitter, clock: c.Clock}
}

// session routes a new op. A machine with no session here is waited for when the store says its link
// is elsewhere or went down less than Timing.LinkWait ago: it is about to relink, here or to another
// process. Any other offline machine fails at once. The store spans processes, and a deploy overlaps
// them, so a hub's own age says nothing about whether a link is moving.
func (l *Link) session(ctx context.Context) (*session, error) {
	start := l.hub.cfg.Clock.Now()
	for {
		if l.hub.draining.Load() {
			return nil, ErrDraining
		}
		s, changed, gone := l.look()
		if s != nil {
			return s, nil
		}
		if gone {
			return nil, ErrOffline
		}
		if err := l.await(ctx, changed, start); err != nil {
			return nil, err
		}
	}
}

// look returns the live session, whether Kick ended the link, and the channel that closes when a
// link attaches. The channel is taken before the store is read, so an attach in between is not lost.
func (l *Link) look() (*session, <-chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.cur.Load(); s != nil && !s.mux.IsClosed() {
		return s, nil, false
	}
	return nil, l.changed, l.gone
}

// moving is how much longer a Call that began at start may wait for the machine's link. A failed
// read is treated as offline, so the caller fails with ErrOffline, which Hold retries.
func (l *Link) moving(ctx context.Context, start time.Time) (time.Duration, error) {
	linked, since, err := l.hub.cfg.Presence.Seen(ctx, l.id)
	if err != nil {
		return 0, fmt.Errorf("%w: presence: %w", ErrOffline, err)
	}
	if linked {
		since = start
	}
	return since.Add(l.hub.cfg.Timing.LinkWait).Sub(l.hub.cfg.Clock.Now()), nil
}

// await waits for a link to attach, for as long as the machine's link may still be moving. It
// returns nil when one attached, and ErrOffline when none is expected or none came in time.
func (l *Link) await(ctx context.Context, changed <-chan struct{}, start time.Time) error {
	wait, err := l.moving(ctx, start)
	if err != nil {
		return err
	}
	if wait <= 0 {
		return ErrOffline
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-changed:
		return nil
	case <-t.C:
		return ErrOffline
	case <-ctx.Done():
		return fmt.Errorf("link: waiting for %s: %w", l.id, ctx.Err())
	}
}

// attach makes s the route and returns the session that must be retired: the one it replaced, or s
// itself if a link of a later generation is already the route. Two handshakes for one machine can
// reach Presence.Linked in one order and attach in the other, and the store's generation is the
// authority.
func (l *Link) attach(s *session) (retire *session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur := l.cur.Load(); cur != nil && cur.info.Gen > s.info.Gen {
		return s
	}
	retire = l.cur.Swap(s)
	l.gone = false
	close(l.changed)
	l.changed = make(chan struct{})
	return retire
}

// detach clears s if it is still the route and records the machine offline for s's generation. A late close of a replaced session finds a different route and does
// nothing here, and a stale generation does nothing in the store.
func (l *Link) detach(s *session) {
	l.mu.Lock()
	cleared := l.cur.CompareAndSwap(s, nil)
	l.mu.Unlock()
	if cleared {
		l.hub.unlinked(l.id, s.info.Gen)
	}
}
