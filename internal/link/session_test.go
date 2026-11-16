package link

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A test catalogue stands in for internal/ops: the link package never imports the real one.
var (
	tcat   = NewCatalogue()
	tEcho  = NewOp[struct{ S string }, struct{ S string }](tcat, "t.echo", ServedByAgent)
	tHang  = NewOp[struct{}, struct{}](tcat, "t.hang", ServedByAgent)
	tExec  = NewOp[struct{}, struct{}](tcat, "t.exec", ServedByAgent, CapExec)
	tBad   = NewOp[badReq, struct{}](tcat, "t.bad", ServedByAgent)
	tPipe  = NewStreamOp[struct{ N int }, struct{ N int }](tcat, "t.pipe", ServedByAgent)
	tWatch = NewResumableOp[struct{ ID string }, struct{ Opens int }](tcat, "t.watch", ServedByAgent)
	tFail  = NewOp[struct{ Kind string }, struct{}](tcat, "t.fail", ServedByAgent)
	tRep   = NewOp[struct{ MemFree int }, struct{}](tcat, "t.report", ServedByServer)
)

type badReq struct{}

func (badReq) Validate() error { return errors.New("nope") }

type testSide struct {
	reg   *Registry
	hung  chan struct{}
	opens atomic.Int32
	cut   atomic.Int32
}

// pair builds a server session and an agent session over net.Pipe with the given registries.
func pair(t *testing.T, agentReg, serverReg *Registry, tm Timing, serverCaps CapSet) (srv, agt *session) {
	t.Helper()
	a, b := net.Pipe()
	var err error
	agt, err = newSession(a, true, sessionOpts{Info: Info{Machine: "m1"}, Reg: agentReg, Ctl: newCtlRegistry(), Timing: tm})
	if err != nil {
		t.Fatal(err)
	}
	srv, err = newSession(b, false, sessionOpts{Info: Info{Machine: "m1", Gen: 1}, PeerCaps: serverCaps, Reg: serverReg, Ctl: newCtlRegistry(), Timing: tm})
	if err != nil {
		t.Fatal(err)
	}
	go agt.serve(context.Background())
	go srv.serve(context.Background())
	t.Cleanup(func() { _ = agt.mux.Close(); _ = srv.mux.Close() })
	return srv, agt
}

type fixed struct {
	mu sync.Mutex
	s  *session
	p  pacer
}

func (f *fixed) session(context.Context) (*session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

func (f *fixed) pacer() pacer { return f.p }

func (f *fixed) set(s *session) { f.mu.Lock(); f.s = s; f.mu.Unlock() }

func fix(s *session) *fixed { return &fixed{s: s} }

func newAgentSide(t *testing.T, caps CapSet) *testSide {
	t.Helper()
	ts := &testSide{reg: NewRegistry(tcat, ServedByAgent, caps), hung: make(chan struct{})}
	r := ts.reg
	Handle(r, tEcho, func(_ context.Context, _ Meta, q struct{ S string }) (struct{ S string }, error) { return q, nil })
	Handle(r, tHang, func(ctx context.Context, _ Meta, _ struct{}) (struct{}, error) {
		<-ctx.Done()
		close(ts.hung)
		return struct{}{}, ctx.Err()
	})
	Handle(r, tFail, func(ctx context.Context, _ Meta, q struct{ Kind string }) (struct{}, error) {
		switch q.Kind {
		case "refused":
			return struct{}{}, Errorf(CodeRefused, "not yours")
		case "wrapped":
			return struct{}{}, fmt.Errorf("while doing it: %w", Errorf(CodeBusy, "slots full"))
		case "cancelled":
			return struct{}{}, context.Canceled
		}
		return struct{}{}, errors.New("disk on fire")
	})
	Handle(r, tExec, func(context.Context, Meta, struct{}) (struct{}, error) { return struct{}{}, nil })
	Handle(r, tBad, func(context.Context, Meta, badReq) (struct{}, error) { t.Error("handler ran"); return struct{}{}, nil })
	HandleStream(r, tPipe, func(_ context.Context, _ Meta, q struct{ N int }) (struct{ N int }, Bytes, error) {
		return struct{ N int }{q.N + 1}, echo, nil
	})
	HandleResumable(r, tWatch, func(_ context.Context, _ Meta, _ struct{ ID string }) (struct{ Opens int }, Bytes, error) {
		n := ts.opens.Add(1)
		return struct{ Opens int }{int(n)}, func(ctx context.Context, s Stream) error {
			<-ctx.Done()
			ts.cut.Add(1)
			return nil
		}, nil
	})
	return ts
}

func echo(_ context.Context, s Stream) error {
	if _, err := io.Copy(s, s); err != nil {
		return fmt.Errorf("echo: %w", err)
	}
	return nil
}

func serverReg() *Registry { return NewRegistry(tcat, ServedByServer, 0) }

func TestCallRoundTripsATypedRequest(t *testing.T) {
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	got, err := Call(context.Background(), fix(srv), tEcho, struct{ S string }{"hi"})
	if err != nil || got.S != "hi" {
		t.Fatalf("echo: %v %v", got, err)
	}
}

func TestValidatorRunsBeforeTheHandler(t *testing.T) {
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	if _, err := Call(context.Background(), fix(srv), tBad, badReq{}); !errors.Is(err, &Error{Code: CodeBadRequest}) {
		t.Fatalf("validate must reject before the handler: %v", err)
	}
}

func TestStreamOpCarriesBytesAfterItsReply(t *testing.T) {
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	resp, s, err := Open(context.Background(), fix(srv), tPipe, struct{ N int }{41})
	if err != nil || resp.N != 42 {
		t.Fatalf("open: %v %v", resp, err)
	}
	_, _ = s.Write([]byte("bytes after reply"))
	_ = s.CloseWrite()
	if got, _ := io.ReadAll(s); !bytes.Equal(got, []byte("bytes after reply")) {
		t.Fatalf("echo %q", got)
	}
	_ = s.Close()
}

func TestHandlerContextFollowsTheCaller(t *testing.T) {
	side := newAgentSide(t, 0)
	srv, _ := pair(t, side.reg, serverReg(), DefaultTiming(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := Call(ctx, fix(srv), tHang, struct{}{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("hang: %v", err)
	}
	select {
	case <-side.hung:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler's ctx must follow the caller's across the link")
	}
}

func TestCapabilityEnforcedOnBothEnds(t *testing.T) {
	// The caller thinks exec is granted, the agent's own registry has it off: the agent refuses.
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), CapSet(0).With(CapExec))
	if _, err := Call(context.Background(), fix(srv), tExec, struct{}{}); !errors.Is(err, &Error{Code: CodeForbidden}) {
		t.Fatalf("agent end: %v", err)
	}
	// The caller knows the machine lacks it: nothing is sent.
	srv.peerCaps = 0
	if _, err := Call(context.Background(), fix(srv), tExec, struct{}{}); !errors.Is(err, &Error{Code: CodeForbidden}) {
		t.Fatalf("caller end: %v", err)
	}
}

func TestMissingHandlersAreAStartupFailure(t *testing.T) {
	empty := NewRegistry(tcat, ServedByAgent, 0)
	if err := empty.Check(); err == nil {
		t.Fatal("Check must name the ops nobody serves")
	}
	if err := newAgentSide(t, CapSet(0).With(CapExec)).reg.Check(); err != nil {
		t.Fatalf("complete registry: %v", err)
	}
	if err := newAgentSide(t, 0).reg.Check(); err != nil {
		t.Fatalf("an op needing a capability the registry lacks is exempt: %v", err)
	}
	srv, _ := pair(t, empty, serverReg(), DefaultTiming(), 0)
	if _, err := Call(context.Background(), fix(srv), tEcho, struct{ S string }{}); !errors.Is(err, &Error{Code: CodeUnknownOp}) {
		t.Fatalf("unknown op: %v", err)
	}
}

func TestRegistryRefusesDeclarationMistakes(t *testing.T) {
	r := NewRegistry(tcat, ServedByAgent, 0)
	h := func(context.Context, Meta, struct{ S string }) (struct{ S string }, error) {
		return struct{ S string }{}, nil
	}
	Handle(r, tEcho, h)
	mustPanic(t, "handled twice", func() { Handle(r, tEcho, h) })
	mustPanic(t, "wrong side", func() {
		Handle(r, tRep, func(context.Context, Meta, struct{ MemFree int }) (struct{}, error) { return struct{}{}, nil })
	})
	mustPanic(t, "duplicate name", func() { NewOp[int, int](tcat, "t.echo", ServedByAgent) })
	mustPanic(t, "bad name", func() { NewOp[int, int](tcat, "Echo", ServedByAgent) })
	mustPanic(t, "reserved name", func() { NewOp[int, int](tcat, "link.mine", ServedByAgent) })
	mustPanic(t, "two capabilities", func() { NewOp[int, int](tcat, "t.two", ServedByAgent, CapExec, CapExec) })
}

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s must panic", what)
		}
	}()
	f()
}

func TestDecodeIsStrictForRequests(t *testing.T) {
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	st, err := openOp(context.Background(), fix(srv), tEcho.s, map[string]string{"S": "x", "extra": "y"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := readReply[struct{ S string }](context.Background(), st); !errors.Is(err, &Error{Code: CodeBadRequest}) {
		t.Fatalf("an unknown request field must be refused: %v", err)
	}
}

func TestRetireLetsFiniteStreamsFinish(t *testing.T) {
	srv, agt := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	_, s, err := Open(context.Background(), fix(srv), tPipe, struct{ N int }{1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { srv.retire(context.Background()); close(done) }()
	waitSealed(t, srv)
	if _, err := Call(context.Background(), fix(srv), tEcho, struct{ S string }{}); !errors.Is(err, &Error{Code: CodeDraining}) {
		t.Fatalf("new ops on a retiring session: %v", err)
	}
	if _, err := Call(context.Background(), fix(agt), tRep, struct{ MemFree int }{}); err == nil {
		t.Fatal("the peer's GoAway must stop the agent opening streams here")
	}
	select {
	case <-done:
		t.Fatal("retire returned with a stream still open")
	case <-time.After(30 * time.Millisecond):
	}
	_, _ = s.Write([]byte("last bytes"))
	_ = s.CloseWrite()
	if b, _ := io.ReadAll(s); string(b) != "last bytes" {
		t.Fatalf("in-flight stream lost its data: %q", b)
	}
	_ = s.Close()
	<-done
}

func waitSealed(t *testing.T, s *session) {
	t.Helper()
	waitFor(t, func() bool {
		s.flight.mu.Lock()
		defer s.flight.mu.Unlock()
		return s.flight.sealed
	})
}

func TestRetireCutsAStreamStillRunningAtTheBudget(t *testing.T) {
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	_, s, err := Open(context.Background(), fix(srv), tPipe, struct{ N int }{1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	srv.retire(ctx)
	if _, err := io.ReadAll(s); err == nil {
		t.Log("a cut stream reads as EOF: the caller learns from the session ending")
	}
	select {
	case <-srv.Done():
	case <-time.After(time.Second):
		t.Fatal("the session must be closed after the budget")
	}
}

type recordSleep struct {
	mu sync.Mutex
	ds []time.Duration
}

func (r *recordSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.ds = append(r.ds, d)
	r.mu.Unlock()
	time.Sleep(time.Millisecond)
	if ctx.Err() != nil {
		return fmt.Errorf("stopped: %w", context.Cause(ctx))
	}
	return nil
}

func TestHoldReopensAResumableOpOnTheNextSession(t *testing.T) {
	side := newAgentSide(t, 0)
	srv1, _ := pair(t, side.reg, serverReg(), DefaultTiming(), 0)
	caller := fix(srv1)
	rec := &recordSleep{}
	caller.p = pacer{sleep: rec.sleep, jitter: func() float64 { return 0 }}
	var uses atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan error, 1)
	go func() {
		held <- Hold(ctx, caller, tWatch, struct{ ID string }{"box-1"}, func(ctx context.Context, r struct{ Opens int }, s Stream) {
			uses.Add(1)
			_, _ = io.Copy(io.Discard, s)
		})
	}()
	waitFor(t, func() bool { return side.opens.Load() == 1 })

	srv1.retire(context.Background())
	waitFor(t, func() bool { return side.cut.Load() == 1 })
	srv2, _ := pair(t, side.reg, serverReg(), DefaultTiming(), 0)
	caller.set(srv2)
	waitFor(t, func() bool { return side.opens.Load() == 2 && uses.Load() == 2 })

	cancel()
	if err := <-held; !errors.Is(err, context.Canceled) {
		t.Fatalf("Hold ends with its context: %v", err)
	}
	if len(rec.ds) == 0 {
		t.Fatal("Hold must pace its reopenings")
	}
}

func TestHoldStopsOnARefusalAndOnADrainingServer(t *testing.T) {
	srv, _ := pair(t, NewRegistry(tcat, ServedByAgent, 0), serverReg(), DefaultTiming(), 0)
	rec := &recordSleep{}
	c := fix(srv)
	c.p = pacer{sleep: rec.sleep}
	err := Hold(context.Background(), c, tWatch, struct{ ID string }{"x"}, func(context.Context, struct{ Opens int }, Stream) {})
	if !errors.Is(err, &Error{Code: CodeUnknownOp}) {
		t.Fatalf("a refusal ends Hold: %v", err)
	}
	if retryable(ErrDraining) || !retryable(ErrOffline) || !retryable(fmt.Errorf("x: %w", ErrLinkLost)) {
		t.Fatal("a draining server is not retried here; a missing link is")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 1500; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// blackhole forwards until cut, then swallows writes and never delivers: a dead network path.
type blackhole struct {
	net.Conn
	cut *atomic.Bool
}

func (b blackhole) Write(p []byte) (int, error) {
	if b.cut.Load() {
		return len(p), nil
	}
	n, err := b.Conn.Write(p)
	if err != nil {
		return n, fmt.Errorf("pipe: %w", err)
	}
	return n, nil
}

// TestKeepaliveDetectsDeadPath shrinks Timing so detection takes tens of milliseconds of real
// time. testing/synctest was tried first and abandoned: yamux keeps a package-level timer pool,
// and a timer created inside a bubble that is reused outside one (the next test, or -count=2) is a
// fatal runtime error. Liveness is therefore tested with small real durations.
func TestKeepaliveDetectsDeadPath(t *testing.T) {
	var cut atomic.Bool
	a, b := net.Pipe()
	tm := DefaultTiming()
	tm.KeepAlive = 20 * time.Millisecond
	agt, _ := newSession(blackhole{a, &cut}, true, sessionOpts{Reg: NewRegistry(tcat, ServedByAgent, 0), Ctl: newCtlRegistry(), Timing: tm})
	srv, _ := newSession(blackhole{b, &cut}, false, sessionOpts{Reg: serverReg(), Ctl: newCtlRegistry(), Timing: tm})
	go agt.serve(context.Background())
	go srv.serve(context.Background())
	time.Sleep(5 * tm.KeepAlive)
	if agt.mux.IsClosed() || srv.mux.IsClosed() {
		t.Fatal("a quiet but healthy link must stay up")
	}
	cut.Store(true)
	for _, s := range []*session{agt, srv} {
		select {
		case <-s.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("a dead path was not noticed within the keepalive window")
		}
	}
}

func TestInflightLimitAnswersBusy(t *testing.T) {
	tm := DefaultTiming()
	tm.MaxInflight = 1
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), tm, 0)
	_, s, err := Open(context.Background(), fix(srv), tPipe, struct{ N int }{1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := Call(context.Background(), fix(srv), tEcho, struct{ S string }{}); !errors.Is(err, &Error{Code: CodeBusy}) {
		t.Fatalf("over the limit: %v", err)
	}
}

func TestHandlerErrorsKeepOnlyTheCodeTheyChose(t *testing.T) {
	srv, _ := pair(t, newAgentSide(t, 0).reg, serverReg(), DefaultTiming(), 0)
	cases := map[string]Code{
		"refused": CodeRefused, "wrapped": CodeBusy, "cancelled": CodeCancelled, "plain": CodeInternal,
	}
	for kind, want := range cases {
		_, err := Call(context.Background(), fix(srv), tFail, struct{ Kind string }{kind})
		var le *Error
		if !errors.As(err, &le) || le.Code != want || !errors.Is(err, &Error{Code: want}) {
			t.Errorf("%s: got %v, want code %s", kind, err, want)
		}
	}
	_, err := Call(context.Background(), fix(srv), tFail, struct{ Kind string }{"refused"})
	if err.Error() != "refused: not yours" {
		t.Fatalf("error text: %q", err)
	}
}
