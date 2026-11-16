package link

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/IniZio/outpost/internal/domain"
)

type fakeStore struct {
	mu          sync.Mutex
	machine     domain.Machine
	key         domain.MachineKey
	known       bool
	lookupErr   error
	linkedErr   error
	seenErr     error
	linked      int
	unlinkedGen []domain.LinkGen
	// afterFirstLookup runs once, after the first Lookup has answered, to land a revoke inside the
	// handshake.
	afterFirstLookup func()
}

func (f *fakeStore) Lookup(_ context.Context, id domain.MachineID) (domain.Machine, domain.MachineKey, error) {
	f.mu.Lock()
	hook := f.afterFirstLookup
	f.afterFirstLookup = nil
	m, key, err := f.lookup(id)
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return m, key, err
}

func (f *fakeStore) lookup(id domain.MachineID) (domain.Machine, domain.MachineKey, error) {
	switch {
	case f.lookupErr != nil:
		return domain.Machine{}, domain.MachineKey{}, f.lookupErr
	case !f.known || id != f.machine.ID:
		return domain.Machine{}, domain.MachineKey{}, domain.ErrUnknownMachine
	}
	return f.machine, f.key, nil
}

func (f *fakeStore) Linked(context.Context, domain.MachineID, string) (domain.LinkGen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linkedErr != nil {
		return 0, f.linkedErr
	}
	f.linked++
	return domain.LinkGen(f.linked), nil
}

func (f *fakeStore) Unlinked(_ context.Context, _ domain.MachineID, gen domain.LinkGen) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlinkedGen = append(f.unlinkedGen, gen)
	return nil
}

func (f *fakeStore) Seen(context.Context, domain.MachineID) (bool, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return false, time.Time{}, f.seenErr
}

func (f *fakeStore) linkedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.linked
}

type testHub struct {
	hub   *Hub
	store *fakeStore
	srv   *httptest.Server
	key   ed25519.PrivateKey
	id    domain.MachineID
}

func (h testHub) wsURL() string { return "ws" + h.srv.URL[len("http"):] }

func newTestHub(t *testing.T, edit func(*HubConfig)) testHub {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{machine: domain.Machine{ID: "m1", Role: domain.RoleSandbox}, known: true}
	copy(st.key[:], pub)
	th := testHub{store: st, key: priv, id: "m1"}
	th.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { th.hub.Handler().ServeHTTP(w, r) }))
	t.Cleanup(th.srv.Close)
	u, _ := url.Parse(th.srv.URL)
	cfg := HubConfig{Host: HostOf(u), Keys: st, Presence: st, Reg: serverReg(), Timing: shortTiming(), Backoff: shortBackoff()}
	if edit != nil {
		edit(&cfg)
	}
	th.hub, err = NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func shortTiming() Timing {
	tm := DefaultTiming()
	tm.Handshake = time.Second
	tm.KeepAlive = 300 * time.Millisecond
	tm.Drain = 2 * time.Second
	tm.LinkWait = time.Second
	return tm
}

func shortBackoff() Backoff {
	return Backoff{Base: 5 * time.Millisecond, Max: 80 * time.Millisecond, StableAfter: 200 * time.Millisecond, RelinkMax: 20 * time.Millisecond, RelinkWindow: time.Minute}
}

// rawConn is an agent written by hand, so a test can say anything the wire allows.
type rawConn struct {
	t *testing.T
	c *websocket.Conn
}

func dialRaw(t *testing.T, url string) rawConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return rawConn{t: t, c: c}
}

func (r rawConn) sendText(s string) {
	r.t.Helper()
	if err := r.c.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		r.t.Fatal(err)
	}
}

func (r rawConn) send(w wire) {
	r.t.Helper()
	b, _ := json.Marshal(w)
	r.sendText(string(b))
}

// recv returns the next text frame as the server sent it.
func (r rawConn) recv() string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := r.c.Read(ctx)
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	return string(data)
}

func (r rawConn) recvWire() wire {
	r.t.Helper()
	var w wire
	if err := json.Unmarshal([]byte(r.recv()), &w); err != nil {
		r.t.Fatal(err)
	}
	return w
}

// challenge says hello as machine and returns the server's nonce.
func (r rawConn) challenge(machine domain.MachineID) []byte {
	r.t.Helper()
	r.send(wire{T: "hello", Machine: machine, Proto: ProtoVersion, Build: "raw"})
	w := r.recvWire()
	if w.T != "challenge" || len(w.Nonce) != 32 {
		r.t.Fatalf("want a 32 byte challenge, got %+v", w)
	}
	return w.Nonce
}

func (r rawConn) closeStatus() websocket.StatusCode {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := r.c.Read(ctx)
	return websocket.CloseStatus(err)
}

func TestHandshakeAnswersUnknownMachineAndBadProofAlike(t *testing.T) {
	th := newTestHub(t, nil)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	cases := []struct {
		name    string
		machine domain.MachineID
		key     ed25519.PrivateKey
	}{
		{"unknown machine", "ghost", th.key},
		{"proof from another key", th.id, other},
	}
	var answers []string
	for _, c := range cases {
		rc := dialRaw(t, th.wsURL())
		nonce := rc.challenge(c.machine)
		rc.send(wire{T: "proof", Sig: proof{th.hub.cfg.Host, nonce, c.machine, ProtoVersion}.sign(c.key)})
		answers = append(answers, rc.recv())
		if got := rc.closeStatus(); got != websocket.StatusNormalClosure {
			t.Errorf("%s: a refusal closes normally, got %d", c.name, got)
		}
	}
	if answers[0] != answers[1] || answers[0] != `{"t":"refused","reason":"unauthorized"}` {
		t.Fatalf("refusals must be identical so they are no oracle: %q", answers)
	}
	if th.store.linkedCount() != 0 {
		t.Fatal("a refused hello must not be recorded as a link")
	}
}

func TestHandshakeRefusesAProofReplayedOnANewLink(t *testing.T) {
	th := newTestHub(t, nil)
	first := dialRaw(t, th.wsURL())
	n1 := first.challenge(th.id)
	sig := proof{th.hub.cfg.Host, n1, th.id, ProtoVersion}.sign(th.key)
	first.send(wire{T: "proof", Sig: sig})
	if w := first.recvWire(); w.T != "welcome" {
		t.Fatalf("the honest proof must be welcomed: %+v", w)
	}

	second := dialRaw(t, th.wsURL())
	n2 := second.challenge(th.id)
	if string(n1) == string(n2) {
		t.Fatal("every connection gets a fresh nonce")
	}
	second.send(wire{T: "proof", Sig: sig})
	if w := second.recvWire(); w.T != "refused" {
		t.Fatalf("a replayed proof must be refused: %+v", w)
	}
	if th.store.linkedCount() != 1 {
		t.Fatal("only the honest link is recorded")
	}
}

func TestHandshakeAnswersUpdateRequiredForALowOrUnparsableHello(t *testing.T) {
	th := newTestHub(t, nil)
	const frozen = `{"t":"update_required","server_proto":1,"min_proto":1}`
	cases := []struct {
		name string
		send func(rawConn)
	}{
		{"protocol below the floor", func(r rawConn) { r.send(wire{T: "hello", Machine: th.id, Proto: 0}) }},
		{"protocol from the future", func(r rawConn) { r.send(wire{T: "hello", Machine: th.id, Proto: 9}) }},
		{"not JSON", func(r rawConn) { r.sendText("hello?") }},
		{"JSON of another shape", func(r rawConn) { r.sendText(`["hello"]`) }},
		{"another frame first", func(r rawConn) { r.send(wire{T: "proof"}) }},
		{"an unknown tag", func(r rawConn) { r.sendText(`{"t":"hello2","machine":"m1","proto":1}`) }},
		{"a binary frame", func(r rawConn) { _ = r.c.Write(context.Background(), websocket.MessageBinary, []byte{1, 2}) }},
	}
	for _, c := range cases {
		rc := dialRaw(t, th.wsURL())
		c.send(rc)
		if got := rc.recv(); got != frozen {
			t.Errorf("%s: got %s", c.name, got)
		}
	}
}

func TestHandshakeHelloDecodingIsLenient(t *testing.T) {
	th := newTestHub(t, nil)
	rc := dialRaw(t, th.wsURL())
	rc.sendText(`{"t":"hello","machine":"m1","proto":1,"build":"x","from_the_future":{"a":[1,2]}}`)
	if w := rc.recvWire(); w.T != "challenge" {
		t.Fatalf("a newer agent's extra hello fields must be ignored: %+v", w)
	}
}

func TestHandshakeInfrastructureErrorsCloseWithoutAFrame(t *testing.T) {
	cases := map[string]func(*fakeStore){
		"key store down": func(s *fakeStore) { s.lookupErr = errors.New("db down") },
		"presence down":  func(s *fakeStore) { s.linkedErr = errors.New("db down") },
	}
	for name, fail := range cases {
		th := newTestHub(t, nil)
		fail(th.store)
		rc := dialRaw(t, th.wsURL())
		rc.send(wire{T: "hello", Machine: th.id, Proto: ProtoVersion})
		if name == "presence down" {
			nonce := rc.recvWire().Nonce
			rc.send(wire{T: "proof", Sig: proof{th.hub.cfg.Host, nonce, th.id, ProtoVersion}.sign(th.key)})
		}
		if got := rc.closeStatus(); got != websocket.StatusInternalError {
			t.Errorf("%s: want status 1011 and no frame, got %d", name, got)
		}
	}
}

func TestHandshakeSlowClientIsCutLoose(t *testing.T) {
	th := newTestHub(t, func(c *HubConfig) { c.Timing.Handshake = 50 * time.Millisecond })
	rc := dialRaw(t, th.wsURL())
	var clock domain.SystemClock
	start := clock.Now()
	_ = rc.closeStatus()
	if clock.Now().Sub(start) > 2*time.Second {
		t.Fatal("a client that never says hello must be dropped after Timing.Handshake")
	}
}

func TestHubRefusesPastTheHandshakeCap(t *testing.T) {
	th := newTestHub(t, nil)
	for range cap(th.hub.pending) {
		th.hub.pending <- struct{}{}
	}
	resp, err := http.Get(th.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestDrainingHubAnswersRelink(t *testing.T) {
	th := newTestHub(t, nil)
	if err := th.hub.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	rc := dialRaw(t, th.wsURL())
	rc.send(wire{T: "hello", Machine: th.id, Proto: ProtoVersion})
	if got := rc.recv(); got != `{"t":"relink"}` {
		t.Fatalf("got %s", got)
	}
}

func TestHostOfCanonicalisesWhatIsSigned(t *testing.T) {
	cases := map[string]string{
		"https://Outpost.Example/machines/link": "outpost.example",
		"wss://outpost.example:443/x":           "outpost.example",
		"https://outpost.example:8443":          "outpost.example:8443",
		"ws://127.0.0.1:80":                     "127.0.0.1",
		"ws://127.0.0.1:7300":                   "127.0.0.1:7300",
		"wss://[::1]:7300":                      "[::1]:7300",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := HostOf(u); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}

func TestProofBindsHostNonceMachineAndProto(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	var mk domain.MachineKey
	copy(mk[:], pub)
	nonce := make([]byte, 32)
	sig := proof{"outpost.example", nonce, "m1", 1}.sign(key)
	other := make([]byte, 32)
	other[0] = 1
	cases := []struct {
		name  string
		host  string
		nonce []byte
		id    domain.MachineID
		proto uint16
		known bool
		ok    bool
	}{
		{"same", "outpost.example", nonce, "m1", 1, true, true},
		{"rogue server relaying the challenge", "evil.example", nonce, "m1", 1, true, false},
		{"another connection's nonce", "outpost.example", other, "m1", 1, true, false},
		{"another machine", "outpost.example", nonce, "m2", 1, true, false},
		{"downgraded proto", "outpost.example", nonce, "m1", 0, true, false},
		{"unknown machine, right signature", "outpost.example", nonce, "m1", 1, false, false},
	}
	for _, c := range cases {
		claim := proof{c.host, c.nonce, c.id, c.proto}
		if got := claim.verify(mk, c.known, sig); got != c.ok {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestProofFieldsCannotShift(t *testing.T) {
	a := proof{"ab", []byte("c"), "m", 1}.message()
	b := proof{"a", []byte("bc"), "m", 1}.message()
	if string(a) == string(b) {
		t.Fatal("moving a byte between fields must change the signed message")
	}
}

func TestRevokeDuringTheHandshakeStillEndsTheLink(t *testing.T) {
	// Keepalive is an hour, so only the post-attach check can end this link inside the test.
	th := newTestHub(t, func(c *HubConfig) { c.Timing.KeepAlive = time.Hour })
	th.store.afterFirstLookup = func() {
		th.store.mu.Lock()
		th.store.known = false
		th.store.mu.Unlock()
		th.hub.Kick(th.id) // finds no link yet: the machine is still mid-handshake
	}
	rc := dialRaw(t, th.wsURL())
	nonce := rc.challenge(th.id)
	rc.send(wire{T: "proof", Sig: proof{th.hub.cfg.Host, nonce, th.id, ProtoVersion}.sign(th.key)})
	if w := rc.recvWire(); w.T != "welcome" {
		t.Fatalf("the handshake had already passed: %+v", w)
	}
	waitFor(t, func() bool {
		th.store.mu.Lock()
		defer th.store.mu.Unlock()
		return len(th.store.unlinkedGen) == 1
	})
	if _, ok := th.hub.Link(th.id).Info(); ok {
		t.Fatal("a machine revoked mid-handshake must not stay linked")
	}
}

func TestAttachKeepsTheHigherGeneration(t *testing.T) {
	l := newTestHub(t, nil).hub.Link("m1")
	s5, s6 := &session{info: Info{Gen: 5}}, &session{info: Info{Gen: 6}}
	if l.attach(s6) != nil {
		t.Fatal("the first attach replaces nothing")
	}
	if got := l.attach(s5); got != s5 || l.cur.Load() != s6 {
		t.Fatal("a link of an earlier generation that attaches late must be the one retired")
	}
	if got := l.attach(&session{info: Info{Gen: 7}}); got != s6 {
		t.Fatal("a later generation replaces the route")
	}
}

func TestCallForAMachineWhosePresenceCannotBeReadFailsOffline(t *testing.T) {
	th := newTestHub(t, nil)
	th.store.seenErr = errors.New("store down")
	_, err := Call(context.Background(), th.hub.Link("m1"), tEcho, struct{ S string }{})
	if !errors.Is(err, ErrOffline) || !errors.Is(err, th.store.seenErr) || !retryable(err) {
		t.Fatalf("an unreadable store reads as offline, and Hold retries it: %v", err)
	}
}
