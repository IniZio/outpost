package link

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/IniZio/outpost/internal/domain"
)

// State is what the agent reports about its link. Run never returns for a refusal or an old
// version: it stays up, retries at the slow end of the backoff and says why, so fixing the server
// or re-pairing the machine heals it without a restart.
type State uint8

const (
	StateDialing State = iota + 1
	StateUp
	StateRelinking // a replacement is being dialled while the old link still serves
	StateRefused   // the server does not know this machine: re-pair it
	StateUpdate    // the server needs a newer agent, or the agent a newer server
)

// AgentConfig wires the agent. Reg serves the ops the server opens and must Check clean. Zero parts
// take production defaults.
type AgentConfig struct {
	URL     string // wss://host/machines/link
	ID      domain.MachineID
	Key     ed25519.PrivateKey
	Build   string
	Proto   uint16 // ProtoVersion unless a test says otherwise
	Reg     *Registry
	Timing  Timing
	Backoff Backoff
	Sleep   Sleeper
	Jitter  func() float64 // [0,1)
	HTTP    *http.Client   // the dial's transport; tests wrap its conns to inject faults
	Clock   domain.Clock
	OnState func(State, error)
	Log     *slog.Logger
}

// Agent keeps one link to the server alive and serves its ops.
type Agent struct {
	cfg  AgentConfig
	host string
	ctl  *Registry
	cur  atomic.Pointer[session]
	st   atomic.Uint32

	relink      chan struct{} // the server asked for a relink
	attempt     int           // consecutive failed dials, owned by Run
	relinkedAt  time.Time     // when the server last asked for a relink, owned by Run
	hasRelinked bool
}

// NewAgent fails fast if the registry does not serve everything the catalogue says this machine
// must, so a missing handler is a startup error and not a CodeUnknownOp in production.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("link: agent URL %q is not a URL with a host", cfg.URL)
	}
	if cfg.Reg == nil {
		return nil, errors.New("link: agent needs a registry")
	}
	if err := cfg.Reg.Check(); err != nil {
		return nil, err
	}
	a := &Agent{cfg: withAgentDefaults(cfg), host: HostOf(u), ctl: newCtlRegistry(), relink: make(chan struct{}, 1)}
	Handle(a.ctl, ctlRelink, func(context.Context, Meta, struct{}) (struct{}, error) {
		select {
		case a.relink <- struct{}{}:
		default:
		}
		return struct{}{}, nil
	})
	return a, nil
}

func withAgentDefaults(cfg AgentConfig) AgentConfig {
	if cfg.Proto == 0 {
		cfg.Proto = ProtoVersion
	}
	if cfg.Timing == (Timing{}) {
		cfg.Timing = DefaultTiming()
	}
	if cfg.Backoff.Max == 0 {
		cfg.Backoff = DefaultBackoff()
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Jitter == nil {
		cfg.Jitter = rand.Float64
	}
	if cfg.Clock == nil {
		cfg.Clock = domain.SystemClock{}
	}
	if cfg.OnState == nil {
		cfg.OnState = func(State, error) {}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return cfg
}

// Server is the Caller for ops the agent opens on the server. It routes to the newest session, and
// says ErrOffline between links.
func (a *Agent) Server() Caller { return agentPeer{a} }

// State returns the current state.
func (a *Agent) State() State { return State(a.st.Load()) }

func (a *Agent) setState(s State, err error) {
	a.st.Store(uint32(s))
	a.cfg.OnState(s, err)
}

type agentPeer struct{ a *Agent }

func (p agentPeer) session(context.Context) (*session, error) {
	s := p.a.cur.Load()
	if s == nil || s.mux.IsClosed() {
		return nil, ErrOffline
	}
	return s, nil
}

func (p agentPeer) pacer() pacer {
	c := p.a.cfg
	return pacer{backoff: c.Backoff, sleep: c.Sleep, jitter: c.Jitter, clock: c.Clock}
}

// Run dials, serves and redials until ctx ends, and returns then. A deploy is a relink and not an
// error: the agent dials the successor before it lets the old link go, so a stream mid-copy finishes
// on the link it started on.
func (a *Agent) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		sess, err := a.dial(ctx)
		if err != nil {
			a.dialFailed(ctx, err)
			continue
		}
		a.serveLink(ctx, sess)
	}
	return fmt.Errorf("link: agent stopped: %w", ctx.Err())
}

// dial opens the WebSocket, runs the handshake and wraps the socket as a session.
func (a *Agent) dial(ctx context.Context) (*session, error) {
	dctx, cancel := context.WithTimeout(ctx, a.cfg.Timing.Dial)
	defer cancel()
	c, _, err := websocket.Dial(dctx, a.cfg.URL, &websocket.DialOptions{HTTPClient: a.cfg.HTTP, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, fmt.Errorf("link: dial: %w", err)
	}
	c.SetReadLimit(maxHandshake)
	w, err := agentHandshake(dctx, c, &a.cfg, a.host)
	if err != nil {
		_ = c.Close(websocket.StatusNormalClosure, "")
		return nil, err
	}
	s, err := newSession(websocket.NetConn(ctx, c, websocket.MessageBinary), true, sessionOpts{
		Info:     Info{Machine: a.cfg.ID, Gen: w.Gen, Build: a.cfg.Build, Proto: w.Proto, Since: a.cfg.Clock.Now()},
		PeerCaps: allCaps(), Peer: a.Server(), Reg: a.cfg.Reg, Ctl: a.ctl, Timing: a.cfg.Timing,
	})
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "")
		return nil, err
	}
	return s, nil
}

// dialFailed records why a dial failed and waits. A refusal or an old version waits at the slow end
// of the schedule, a draining server's answer waits a short jittered beat, and anything else backs
// off further each time.
func (a *Agent) dialFailed(ctx context.Context, err error) {
	var refused *RefusedError
	var update *UpdateRequiredError
	state, attempt := StateDialing, a.attempt
	switch {
	case errors.As(err, &refused):
		state, attempt = StateRefused, math.MaxInt32
	case errors.As(err, &update):
		state, attempt = StateUpdate, math.MaxInt32
	case errors.Is(err, errRelinkLater):
		a.markRelink()
		attempt = 0
	default:
		a.attempt++
	}
	a.setState(state, err)
	a.cfg.Log.Warn("link dial failed", "err", err)
	_ = a.cfg.Sleep(ctx, a.delay(attempt))
}

func (a *Agent) markRelink() {
	a.relinkedAt, a.hasRelinked = a.cfg.Clock.Now(), true
}

func (a *Agent) delay(attempt int) time.Duration {
	since := time.Duration(math.MaxInt64)
	if a.hasRelinked {
		since = a.cfg.Clock.Now().Sub(a.relinkedAt)
	}
	return a.cfg.Backoff.Delay(attempt, since, a.cfg.Jitter())
}

// serveLink serves sess until it ends, following relinks, then waits before the next dial. A link
// that held for Backoff.StableAfter resets the schedule, one that dropped young grows it.
func (a *Agent) serveLink(ctx context.Context, sess *session) {
	up := a.cfg.Clock.Now()
	a.install(ctx, sess)
	for {
		select {
		case <-ctx.Done():
			a.release(ctx, sess)
			return
		case <-sess.Done():
			a.cur.CompareAndSwap(sess, nil)
			a.attempt = a.cfg.Backoff.Next(a.attempt, a.cfg.Clock.Now().Sub(up))
			_ = a.cfg.Sleep(ctx, a.delay(a.attempt))
			return
		case <-a.relink:
			var moved bool
			if sess, moved = a.handOver(ctx, sess); moved {
				up = a.cfg.Clock.Now()
			}
		}
	}
}

func (a *Agent) install(ctx context.Context, s *session) {
	a.cur.Store(s)
	a.setState(StateUp, nil)
	go s.serve(ctx)
}

// release lets a session go at shutdown, giving its finite streams the drain budget.
func (a *Agent) release(ctx context.Context, s *session) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.Timing.Drain)
	defer cancel()
	a.cur.CompareAndSwap(s, nil)
	s.retire(rctx)
}

// handOver is the agent half of a relink: dial a second session while the first keeps serving,
// move the route to it, and only then retire the first. It returns the session to serve and
// whether it is a new one. If no successor comes, the old session stays and its own end is handled
// by serveLink.
func (a *Agent) handOver(ctx context.Context, old *session) (*session, bool) {
	a.setState(StateRelinking, nil)
	a.markRelink()
	next := a.redial(ctx, old)
	if next == nil {
		return old, false
	}
	a.install(ctx, next)
	select {
	case <-a.relink:
	default:
	}
	go a.release(ctx, old)
	return next, true
}

// redial dials until a session is up, the old one dies or ctx ends.
func (a *Agent) redial(ctx context.Context, old *session) *session {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		select {
		case <-old.Done():
			return nil
		default:
		}
		s, err := a.dial(ctx)
		if err == nil {
			return s
		}
		a.cfg.Log.Warn("link redial failed", "err", err)
		_ = a.cfg.Sleep(ctx, a.delay(attempt))
	}
	return nil
}
