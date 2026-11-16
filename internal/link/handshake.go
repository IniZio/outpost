package link

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/IniZio/outpost/internal/domain"
)

// ProtoVersion is the link protocol this build speaks. It is one integer and not semver: the
// protocol either falls in the server's accepted range or the agent must be updated. Additive
// changes still bump it, because requests are decoded strictly.
const ProtoVersion uint16 = 1

// proofLabel is the domain separator in the signed bytes, so a signature made for this protocol can
// never be a signature for anything else the key signs.
const proofLabel = "outpost-link/1"

// maxHandshake bounds one handshake frame. The limit is set before the socket is wrapped for
// yamux, and the wrap lifts it, so after the handshake a peer is trusted by authentication only.
const maxHandshake = 64 << 10

// HostOf is the host both ends sign into a proof: lower-cased, with the port kept only when it is
// not the scheme's default. The server's HubConfig.Host and an agent's URL both go through it, from
// their own configuration and never from anything the peer sent.
func HostOf(u *url.URL) string {
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" || (port == "443" && (u.Scheme == "https" || u.Scheme == "wss")) || (port == "80" && (u.Scheme == "http" || u.Scheme == "ws")) {
		return host
	}
	return net.JoinHostPort(host, port)
}

// proof is what an agent signs: the label, the server's public host, the per-connection nonce the
// server chose, the machine id and the protocol. Binding the host means a rogue server that relays
// a real server's challenge gets a proof for the wrong host and learns nothing it can replay. There
// is no timestamp: the nonce is fresh per connection, so a stale clock cannot matter and a captured
// proof is useless on the next connection.
type proof struct {
	Host    string
	Nonce   []byte
	Machine domain.MachineID
	Proto   uint16
}

// message is the signed bytes. Each field is length-prefixed, so no choice of fields can shift a
// boundary.
func (p proof) message() []byte {
	var b []byte
	for _, part := range []string{proofLabel, p.Host, string(p.Nonce), string(p.Machine), strconv.Itoa(int(p.Proto))} {
		b = strconv.AppendInt(b, int64(len(part)), 10)
		b = append(b, ':')
		b = append(b, part...)
		b = append(b, '\n')
	}
	return b
}

func (p proof) sign(key ed25519.PrivateKey) []byte { return ed25519.Sign(key, p.message()) }

// dummyKey stands in for an unknown machine's key, so verifying a proof for an id the store does
// not know costs about what a real verify costs. It narrows the timing difference, it does not
// prove constant time.
var dummyKey = func() ed25519.PublicKey {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub
}()

func (p proof) verify(key domain.MachineKey, known bool, sig []byte) bool {
	pub := ed25519.PublicKey(key[:])
	if !known {
		pub = dummyKey
	}
	ok := ed25519.Verify(pub, p.message(), sig)
	return known && ok
}

// refusedReason is the one reason a server gives. "Unknown machine" and "bad proof" are the same
// answer, so the reply is not an oracle for which ids exist.
const refusedReason = "unauthorized"

// The handshake frames are WebSocket text frames before the yamux wrap. They are two sealed sums,
// one per direction, so gochecksumtype makes every switch over them exhaustive.

//sumtype:decl
type agentFrame interface{ isAgentFrame() }

type helloFrame struct {
	Machine domain.MachineID
	Proto   uint16
	Build   string
}

type proofFrame struct{ Sig []byte }

func (helloFrame) isAgentFrame() {}
func (proofFrame) isAgentFrame() {}

//sumtype:decl
type serverFrame interface{ isServerFrame() }

type challengeFrame struct{ Nonce []byte }

type welcomeFrame struct {
	Proto uint16
	Gen   domain.LinkGen
}

// updateRequiredFrame's shape never changes, because the oldest agent in the field must still
// parse it. It carries both versions so the agent can tell which side is stale.
type updateRequiredFrame struct{ ServerProto, MinProto uint16 }

type refusedFrame struct{ Reason string }

// relinkFrame answers a hello while the server drains: come back in a moment, a replacement is up.
type relinkFrame struct{}

func (challengeFrame) isServerFrame()      {}
func (welcomeFrame) isServerFrame()        {}
func (updateRequiredFrame) isServerFrame() {}
func (refusedFrame) isServerFrame()        {}
func (relinkFrame) isServerFrame()         {}

// wire is the JSON of every handshake frame: a tag and the union of the fields. Decoding is
// lenient, so an older peer ignores fields a newer one adds. encode and decode are the only code
// that sees it.
type wire struct {
	T           string           `json:"t"`
	Machine     domain.MachineID `json:"machine,omitempty"`
	Proto       uint16           `json:"proto,omitempty"`
	Build       string           `json:"build,omitempty"`
	Nonce       []byte           `json:"nonce,omitempty"`
	Sig         []byte           `json:"sig,omitempty"`
	Gen         domain.LinkGen   `json:"gen,omitempty"`
	ServerProto uint16           `json:"server_proto,omitempty"`
	MinProto    uint16           `json:"min_proto,omitempty"`
	Reason      string           `json:"reason,omitempty"`
}

func encodeAgent(f agentFrame) wire {
	switch f := f.(type) {
	case helloFrame:
		return wire{T: "hello", Machine: f.Machine, Proto: f.Proto, Build: f.Build}
	case proofFrame:
		return wire{T: "proof", Sig: f.Sig}
	}
	panic("unreachable")
}

func decodeAgent(w wire) (agentFrame, bool) {
	switch w.T {
	case "hello":
		return helloFrame{Machine: w.Machine, Proto: w.Proto, Build: w.Build}, true
	case "proof":
		return proofFrame{Sig: w.Sig}, true
	}
	return nil, false
}

func encodeServer(f serverFrame) wire {
	switch f := f.(type) {
	case challengeFrame:
		return wire{T: "challenge", Nonce: f.Nonce}
	case welcomeFrame:
		return wire{T: "welcome", Proto: f.Proto, Gen: f.Gen}
	case updateRequiredFrame:
		return wire{T: "update_required", ServerProto: f.ServerProto, MinProto: f.MinProto}
	case refusedFrame:
		return wire{T: "refused", Reason: f.Reason}
	case relinkFrame:
		return wire{T: "relink"}
	}
	panic("unreachable")
}

// decodeServer turns a tag into a frame. An unknown tag is not a frame: a newer server spoke, and
// the agent treats it like update-required.
func decodeServer(w wire) (serverFrame, bool) {
	switch w.T {
	case "challenge":
		return challengeFrame{Nonce: w.Nonce}, true
	case "welcome":
		return welcomeFrame{Proto: w.Proto, Gen: w.Gen}, true
	case "update_required":
		return updateRequiredFrame{ServerProto: w.ServerProto, MinProto: w.MinProto}, true
	case "refused":
		return refusedFrame{Reason: w.Reason}, true
	case "relink":
		return relinkFrame{}, true
	}
	return nil, false
}

var (
	errBadFrame = errors.New("link: unreadable handshake frame")
	errProtocol = errors.New("link: unexpected handshake frame")
	// errRelinkLater is the agent's view of a draining server's answer to its hello.
	errRelinkLater = errors.New("link: server is draining, retry shortly")
	errRejected    = errors.New("link: handshake rejected")
)

// RefusedError is why a handshake ended in StateRefused.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "link: server refused this machine: " + e.Reason }

// UpdateRequiredError carries both versions so the operator sees which side is stale.
type UpdateRequiredError struct{ ServerProto, MinProto uint16 }

func (e *UpdateRequiredError) Error() string {
	return fmt.Sprintf("link: protocol mismatch (server speaks %d, accepts from %d, this agent speaks %d)", e.ServerProto, e.MinProto, ProtoVersion)
}

// hsio reads and writes handshake frames, each bounded by one timeout.
type hsio struct {
	c       *websocket.Conn
	timeout time.Duration
}

func (h hsio) read(ctx context.Context) (wire, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	typ, data, err := h.c.Read(ctx)
	if err != nil {
		return wire{}, fmt.Errorf("link: read handshake: %w", err)
	}
	var w wire
	if typ != websocket.MessageText || json.Unmarshal(data, &w) != nil {
		return wire{}, errBadFrame
	}
	return w, nil
}

func (h hsio) write(ctx context.Context, w wire) error {
	data, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("link: encode handshake: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	if err := h.c.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("link: write handshake: %w", err)
	}
	return nil
}

func (h hsio) readAgent(ctx context.Context) (agentFrame, error) {
	w, err := h.read(ctx)
	if err != nil {
		return nil, err
	}
	f, ok := decodeAgent(w)
	if !ok {
		return nil, errBadFrame
	}
	return f, nil
}

func (h hsio) sendServer(ctx context.Context, f serverFrame) error {
	return h.write(ctx, encodeServer(f))
}

func (h hsio) readServer(ctx context.Context) (serverFrame, error) {
	w, err := h.read(ctx)
	if err != nil {
		return nil, err
	}
	f, ok := decodeServer(w)
	if !ok {
		return nil, &UpdateRequiredError{}
	}
	return f, nil
}

// accepted is what a successful server handshake yields.
type accepted struct {
	machine domain.Machine
	build   string
	proto   uint16
	gen     domain.LinkGen
}

// serverHandshake runs the server's half on an accepted WebSocket. Every read and write is bounded
// by Timing.Handshake. Only two conditions send a refused frame: an unknown machine and a bad proof,
// and they send the same one. A failure of the server's own (the key store, Presence) closes the
// socket with status 1011 and no frame, so a database blip never makes an agent give up.
func (h *Hub) serverHandshake(ctx context.Context, c *websocket.Conn) (accepted, error) {
	io := hsio{c: c, timeout: h.cfg.Timing.Handshake}
	hello, err := h.readHello(ctx, io)
	if err != nil {
		return accepted{}, err
	}
	if h.draining.Load() {
		_ = io.sendServer(ctx, relinkFrame{})
		return accepted{}, errRelinkLater
	}
	m, key, err := h.cfg.Keys.Lookup(ctx, hello.Machine)
	known := err == nil
	if err != nil && !errors.Is(err, domain.ErrUnknownMachine) {
		return accepted{}, fmt.Errorf("link: key lookup: %w", err)
	}
	if err := h.authenticate(ctx, io, hello, key, known); err != nil {
		return accepted{}, err
	}
	gen, err := h.cfg.Presence.Linked(ctx, m.ID, hello.Build)
	if err != nil {
		return accepted{}, fmt.Errorf("link: record link: %w", err)
	}
	if err := io.sendServer(ctx, welcomeFrame{Proto: hello.Proto, Gen: gen}); err != nil {
		h.unlinked(m.ID, gen)
		return accepted{}, err
	}
	return accepted{machine: m, build: hello.Build, proto: hello.Proto, gen: gen}, nil
}

// readHello reads the agent's first frame. Any hello this server cannot read, or whose protocol is
// outside [MinProto, ProtoVersion], is answered with update-required: a future agent's hello looks
// like "update me" to an old server, and the reverse.
func (h *Hub) readHello(ctx context.Context, io hsio) (helloFrame, error) {
	f, err := io.readAgent(ctx)
	hello, isHello := f.(helloFrame)
	switch {
	case err != nil && !errors.Is(err, errBadFrame):
		return helloFrame{}, err
	case err != nil || !isHello || hello.Proto < h.cfg.MinProto || hello.Proto > ProtoVersion:
		_ = io.sendServer(ctx, updateRequiredFrame{ServerProto: ProtoVersion, MinProto: h.cfg.MinProto})
		return helloFrame{}, errRejected
	}
	return hello, nil
}

// authenticate sends a fresh nonce and checks the proof over it, the server's own host and the
// machine id.
func (h *Hub) authenticate(ctx context.Context, io hsio, hello helloFrame, key domain.MachineKey, known bool) error {
	nonce := make([]byte, 32)
	if _, err := h.cfg.Rand.Read(nonce); err != nil {
		return fmt.Errorf("link: nonce: %w", err)
	}
	if err := io.sendServer(ctx, challengeFrame{Nonce: nonce}); err != nil {
		return err
	}
	f, err := io.readAgent(ctx)
	pf, isProof := f.(proofFrame)
	if err != nil && !errors.Is(err, errBadFrame) {
		return err
	}
	if err != nil || !isProof || !(proof{h.cfg.Host, nonce, hello.Machine, hello.Proto}).verify(key, known, pf.Sig) {
		_ = io.sendServer(ctx, refusedFrame{Reason: refusedReason})
		return errRejected
	}
	return nil
}

// agentHandshake runs the agent's half. host is what the agent signs: HostOf its own URL.
func agentHandshake(ctx context.Context, c *websocket.Conn, cfg *AgentConfig, host string) (welcomeFrame, error) {
	io := hsio{c: c, timeout: cfg.Timing.Handshake}
	if err := io.write(ctx, encodeAgent(helloFrame{Machine: cfg.ID, Proto: cfg.Proto, Build: cfg.Build})); err != nil {
		return welcomeFrame{}, err
	}
	f, err := io.readServer(ctx)
	if err != nil {
		return welcomeFrame{}, err
	}
	ch, ok := f.(challengeFrame)
	if !ok {
		return welcomeFrame{}, unexpected(f)
	}
	sig := proof{host, ch.Nonce, cfg.ID, cfg.Proto}.sign(cfg.Key)
	if err := io.write(ctx, encodeAgent(proofFrame{Sig: sig})); err != nil {
		return welcomeFrame{}, err
	}
	f, err = io.readServer(ctx)
	if err != nil {
		return welcomeFrame{}, err
	}
	if w, ok := f.(welcomeFrame); ok {
		return w, nil
	}
	return welcomeFrame{}, unexpected(f)
}

// unexpected maps a server frame the agent did not ask for to what it means.
func unexpected(f serverFrame) error {
	switch f := f.(type) {
	case updateRequiredFrame:
		return &UpdateRequiredError{ServerProto: f.ServerProto, MinProto: f.MinProto}
	case refusedFrame:
		return &RefusedError{Reason: f.Reason}
	case relinkFrame:
		return errRelinkLater
	case challengeFrame, welcomeFrame:
		return errProtocol
	}
	panic("unreachable")
}
