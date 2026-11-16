package link

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/IniZio/outpost/internal/domain"
)

// states records what an agent reports and what it waits for, and stops it after enough waits.
type states struct {
	mu     sync.Mutex
	states []State
	errs   []error
	waits  []time.Duration
	stop   context.CancelFunc
	limit  int
}

func (s *states) onState(st State, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, st)
	s.errs = append(s.errs, err)
}

func (s *states) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	if len(s.waits) >= s.limit {
		s.stop()
	}
	s.mu.Unlock()
	if ctx.Err() != nil {
		return fmt.Errorf("stopped: %w", context.Cause(ctx))
	}
	return nil
}

func (s *states) last() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[len(s.states)-1], s.errs[len(s.errs)-1]
}

// runAgent runs an agent against url until it has waited limit times, with jitter fixed.
func runAgent(t *testing.T, th testHub, url string, jitter float64, limit int) *states {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &states{stop: cancel, limit: limit}
	a, err := NewAgent(AgentConfig{
		URL: url, ID: th.id, Key: th.key, Reg: newAgentSide(t, 0).reg, Timing: shortTiming(), Backoff: shortBackoff(),
		Sleep: rec.sleep, Jitter: func() float64 { return jitter }, OnState: rec.onState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run ends only with its context: %v", err)
	}
	return rec
}

func TestAgentBackoffGrowsToTheCapWithJitter(t *testing.T) {
	th := newTestHub(t, nil)
	dead := th.wsURL()
	th.srv.Close()
	for _, jitter := range []float64{0, 1} {
		rec := runAgent(t, th, dead, jitter, 8)
		want := []time.Duration{5, 10, 20, 40, 80, 80, 80, 80}
		for i, d := range rec.waits {
			full := want[i] * time.Millisecond
			low, high := full/2, full
			if d < low || d > high || (jitter == 0 && d != low) || (jitter == 1 && d < high-100*time.Microsecond) {
				t.Errorf("jitter %v wait %d: %v, want %v to %v", jitter, i, d, low, high)
			}
		}
	}
}

func TestAgentKeepsRetryingWhenRefusedOrTooOld(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(testHub)
		hub   func(*HubConfig)
		state State
		check func(error) bool
	}{
		{
			"the server does not know the machine", func(th testHub) { th.store.known = false }, nil, StateRefused,
			func(err error) bool { var r *RefusedError; return errors.As(err, &r) && r.Reason == "unauthorized" },
		},
		{
			"the server needs a newer agent", func(testHub) {}, func(c *HubConfig) { c.MinProto = 2 }, StateUpdate,
			func(err error) bool {
				var u *UpdateRequiredError
				return errors.As(err, &u) && u.MinProto == 2 && u.ServerProto == ProtoVersion
			},
		},
	}
	for _, c := range cases {
		th := newTestHub(t, c.hub)
		c.edit(th)
		rec := runAgent(t, th, th.wsURL(), 0, 4)
		st, err := rec.last()
		if st != c.state || !c.check(err) {
			t.Errorf("%s: state %v err %v", c.name, st, err)
		}
		if got := rec.waits[len(rec.waits)-1]; got < shortBackoff().Max/2 {
			t.Errorf("%s: a refusal waits at the slow end, got %v", c.name, got)
		}
	}
}

func TestAgentWaitsAShortBeatWhenTheServerDrains(t *testing.T) {
	th := newTestHub(t, nil)
	_ = th.hub.Drain(context.Background())
	rec := runAgent(t, th, th.wsURL(), 0.5, 5)
	b := shortBackoff()
	for i, d := range rec.waits {
		if d < b.Base/2 || d > b.RelinkMax {
			t.Errorf("wait %d: %v is outside the short relink beat", i, d)
		}
		if d != rec.waits[0] {
			t.Errorf("wait %d: %v grew; a draining server must not grow the attempt count", i, d)
		}
	}
}

func TestAgentRefusesBadConfiguration(t *testing.T) {
	good := AgentConfig{URL: "wss://outpost.example/machines/link", Reg: newAgentSide(t, 0).reg}
	if _, err := NewAgent(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*AgentConfig){
		"no host":           func(c *AgentConfig) { c.URL = "/machines/link" },
		"not a URL":         func(c *AgentConfig) { c.URL = "://" },
		"no registry":       func(c *AgentConfig) { c.Reg = nil },
		"a missing handler": func(c *AgentConfig) { c.Reg = NewRegistry(tcat, ServedByAgent, 0) },
	}
	for name, edit := range bad {
		c := good
		edit(&c)
		if _, err := NewAgent(c); err == nil {
			t.Errorf("%s must fail at startup", name)
		}
	}
	if _, err := NewHub(HubConfig{}); err == nil {
		t.Error("a hub with nothing configured must fail at startup")
	}
}

// TestRogueServerCannotRelayAChallenge puts a server in the middle that relays every frame
// between the agent and the real hub. The agent signs the host it dialled, which is the rogue's, so
// the real hub refuses a proof that would otherwise be valid for its challenge.
func TestRogueServerCannotRelayAChallenge(t *testing.T) {
	th := newTestHub(t, nil)
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relay(t, w, r, th.wsURL())
	}))
	defer rogue.Close()

	rec := runAgent(t, th, "ws"+strings.TrimPrefix(rogue.URL, "http"), 0, 3)
	st, err := rec.last()
	var refused *RefusedError
	if st != StateRefused || !errors.As(err, &refused) {
		t.Fatalf("the real hub must refuse a relayed challenge: state %v err %v", st, err)
	}
	if th.store.linkedCount() != 0 {
		t.Fatal("the real hub must never record a link through a rogue relay")
	}
}

func relay(t *testing.T, w http.ResponseWriter, r *http.Request, real string) {
	agentSide, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = agentSide.CloseNow() }()
	ctx := r.Context()
	serverSide, _, err := websocket.Dial(ctx, real, nil)
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = serverSide.CloseNow() }()
	for range 3 {
		pipe(ctx, agentSide, serverSide)
		pipe(ctx, serverSide, agentSide)
	}
}

func pipe(ctx context.Context, from, to *websocket.Conn) {
	typ, data, err := from.Read(ctx)
	if err == nil {
		_ = to.Write(ctx, typ, data)
	}
}

func TestAgentFollowsAWelcomeIntoALink(t *testing.T) {
	th := newTestHub(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	rec := &states{stop: cancel, limit: 1 << 30}
	a, err := NewAgent(AgentConfig{
		URL: th.wsURL(), ID: th.id, Key: th.key, Build: "b1", Reg: newAgentSide(t, 0).reg, Timing: shortTiming(),
		Backoff: shortBackoff(), OnState: rec.onState, Clock: domain.SystemClock{},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	waitFor(t, func() bool { _, ok := th.hub.Link(th.id).Info(); return ok })
	info, _ := th.hub.Link(th.id).Info()
	if info.Machine != th.id || info.Gen != 1 || info.Build != "b1" || info.Proto != ProtoVersion {
		t.Fatalf("info: %+v", info)
	}
	waitFor(t, func() bool { return a.State() == StateUp })
	cancel()
	<-done
	waitFor(t, func() bool { _, ok := th.hub.Link(th.id).Info(); return !ok })
	waitFor(t, func() bool {
		th.store.mu.Lock()
		defer th.store.mu.Unlock()
		return len(th.store.unlinkedGen) == 1 && th.store.unlinkedGen[0] == 1
	})
}
