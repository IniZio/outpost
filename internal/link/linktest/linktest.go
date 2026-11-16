// Package linktest joins a real Hub and a real Agent in one process, for tests of link itself and of
// the packages above it. Nothing is faked below the WebSocket: the handshake, yamux, keepalive,
// drain and relink all run for real over loopback TCP, with shrunk Timing.
package linktest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
)

// FastTiming is Timing shrunk so a test waits milliseconds, with a keepalive loose enough to
// survive a loaded machine under -race.
func FastTiming() link.Timing {
	return link.Timing{
		Dial: 5 * time.Second, Handshake: 2 * time.Second, KeepAlive: 300 * time.Millisecond,
		StreamOpen: 2 * time.Second, StreamClose: 2 * time.Second, Drain: 3 * time.Second,
		LinkWait: 5 * time.Second, MaxInflight: 64,
	}
}

// FastBackoff is a reconnect schedule of milliseconds.
func FastBackoff() link.Backoff {
	return link.Backoff{
		Base: 5 * time.Millisecond, Max: 100 * time.Millisecond, StableAfter: 200 * time.Millisecond,
		RelinkMax: 20 * time.Millisecond, RelinkWindow: 10 * time.Second,
	}
}

// Options adjusts a Rig. Server and Agent are required: the registries the two ends serve from.
type Options struct {
	Server, Agent *link.Registry
	Machine       domain.Machine // its ID is assigned; the zero value is a sandbox machine
	Timing        link.Timing
	Backoff       link.Backoff
	MinProto      uint16
}

// Rig is one store, a hub behind a loopback server, and an agent dialling it.
type Rig struct {
	Store   *Store
	Machine domain.MachineID
	Key     []byte
	Hub     *link.Hub
	Agent   *link.Agent
	URL     string

	t      testing.TB
	o      Options
	active atomic.Pointer[link.Hub]
	conns  *tracker
	done   chan struct{}
	stop   context.CancelFunc
}

// New starts a hub and an agent, waits for the first link and stops both at test cleanup.
func New(t testing.TB, o Options) *Rig {
	t.Helper()
	if o.Timing == (link.Timing{}) {
		o.Timing = FastTiming()
	}
	if o.Backoff.Max == 0 {
		o.Backoff = FastBackoff()
	}
	if o.Machine.Role == 0 {
		o.Machine.Role, o.Machine.Slots = domain.RoleSandbox, 1
	}
	r := &Rig{Store: NewStore(), t: t, o: o, conns: &tracker{}}
	r.Machine, r.Key = r.Store.Pair(o.Machine)
	r.startServer()
	r.Hub = r.NextHub()
	r.startAgent()
	r.WaitLinked(r.Hub)
	return r
}

func (r *Rig) startServer() {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.active.Load().Handler().ServeHTTP(w, q)
	}))
	srv.Listener = &trackedListener{Listener: srv.Listener, t: r.conns}
	srv.Start()
	r.t.Cleanup(srv.Close)
	r.URL = "ws" + srv.URL[len("http"):] + "/machines/link"
}

// NextHub starts another hub on the same store and sends new dials to it, as a second server
// process in a deploy overlap. Drain the old hub to make the agent follow.
func (r *Rig) NextHub() *link.Hub {
	r.t.Helper()
	u, err := url.Parse(r.URL)
	if err != nil {
		r.t.Fatal(err)
	}
	hub, err := link.NewHub(link.HubConfig{
		Host: link.HostOf(u), Keys: r.Store, Presence: r.Store, Reg: r.o.Server,
		Timing: r.o.Timing, Backoff: r.o.Backoff, MinProto: r.o.MinProto,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.active.Store(hub)
	return hub
}

// Deploy is a rolling deploy: it starts the next hub, then drains old in the background within the
// Timing.Drain budget. The returned channel carries Drain's result.
func (r *Rig) Deploy(old *link.Hub) (next *link.Hub, drained <-chan error) {
	next = r.NextHub()
	res := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), r.o.Timing.Drain)
		defer cancel()
		res <- old.Drain(ctx)
	}()
	return next, res
}

func (r *Rig) startAgent() {
	a, err := link.NewAgent(link.AgentConfig{
		URL: r.URL, ID: r.Machine, Key: r.Key, Build: "linktest", Reg: r.o.Agent,
		Timing: r.o.Timing, Backoff: r.o.Backoff,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.Agent = a
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	r.done = make(chan struct{})
	go func(done chan struct{}) { defer close(done); _ = a.Run(ctx) }(r.done)
	r.t.Cleanup(r.Stop)
}

// Stop shuts the agent down as a SIGTERM does and returns when Run has. It is safe to call twice.
func (r *Rig) Stop() {
	r.stop()
	<-r.done
}

// Restart starts a new agent for the machine after Stop, as the service coming back does. It does
// not wait for the link: use WaitLinked.
func (r *Rig) Restart() { r.startAgent() }

// WaitLinked blocks until hub has a live link to the rig's machine, or fails the test.
func (r *Rig) WaitLinked(hub *link.Hub) {
	r.t.Helper()
	if !Eventually(func() bool { _, ok := hub.Link(r.Machine).Info(); return ok }) {
		r.t.Fatal("the agent never linked to the hub")
	}
}

// Eventually polls cond for a few seconds and reports whether it held.
func Eventually(cond func() bool) bool {
	for range 2000 {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// Revoke deletes the machine's row and kicks its live link on every hub, as removing a machine does.
func (r *Rig) Revoke(hubs ...*link.Hub) {
	r.Store.Remove(r.Machine)
	for _, h := range hubs {
		h.Kick(r.Machine)
	}
}

// Blackhole makes the network drop every byte in both directions without closing anything, as a
// dead NAT or a pulled cable does. Both ends notice only through keepalive.
func (r *Rig) Blackhole() { r.conns.blackhole.Store(true) }

type tracker struct{ blackhole atomic.Bool }

type trackedListener struct {
	net.Listener
	t *tracker
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("linktest: accept: %w", err)
	}
	return &faultConn{Conn: c, t: l.t}, nil
}

type faultConn struct {
	net.Conn
	t *tracker
}

func (c *faultConn) Write(p []byte) (int, error) {
	if c.t.blackhole.Load() {
		return len(p), nil
	}
	return same(c.Conn.Write(p))
}

// Read discards what arrives while the network is blackholed.
func (c *faultConn) Read(p []byte) (int, error) {
	for {
		n, err := same(c.Conn.Read(p))
		if err != nil || !c.t.blackhole.Load() {
			return n, err
		}
	}
}

// same returns its results unchanged. A conn's errors must keep their identity (io.EOF is compared
// with ==), so a transparent wrapper must not wrap them.
func same(n int, err error) (int, error) { return n, err }
