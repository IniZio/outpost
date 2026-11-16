package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// Meta says who a handler is serving. On the server it is the authenticated machine, taken from
// the handshake and never from the request. On the agent Machine is the agent's own id. Peer opens
// ops back on whoever opened this one, routed to the newest session, so a handler such as a
// port-forward listener can open the accepted-connection stream without holding the Agent.
type Meta struct {
	Machine domain.MachineID
	Gen     domain.LinkGen
	Peer    Caller
}

// Registry maps op names to handlers for one side. It is filled at startup, checked against the
// catalogue, and then read-only.
type Registry struct {
	cat  *Catalogue
	side Side
	caps CapSet
	mu   sync.RWMutex
	h    map[string]handler
}

type handler struct {
	spec spec
	run  func(ctx context.Context, m Meta, s Stream, body json.RawMessage)
}

// NewRegistry returns an empty registry for side. caps is what this end enables: the agent passes
// what its machine was paired for, the server passes every capability.
func NewRegistry(c *Catalogue, side Side, caps CapSet) *Registry {
	return &Registry{cat: c, side: side, caps: caps, h: map[string]handler{}}
}

// Check fails if the catalogue holds an op this side must serve and nothing handles it. Ops that
// need a capability this registry does not enable are exempt.
func (r *Registry) Check() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var missing []string
	for _, sp := range r.cat.served(r.side) {
		if _, ok := r.h[sp.name]; !ok && r.caps.Has(sp.needs) {
			missing = append(missing, sp.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("link: no handler for %s", strings.Join(missing, ", "))
	}
	return nil
}

func (r *Registry) put(s spec, run func(context.Context, Meta, Stream, json.RawMessage)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.side != r.side {
		panic(fmt.Sprintf("link: op %q is not served by this side", s.name))
	}
	if _, dup := r.h[s.name]; dup {
		panic(fmt.Sprintf("link: op %q handled twice", s.name))
	}
	r.h[s.name] = handler{spec: s, run: run}
}

func (r *Registry) lookup(name string) (handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.h[name]
	return h, ok
}

// Handle serves a unary op. A request that fails to decode or to Validate never reaches fn. If the
// caller disconnects before fn returns, ctx is cancelled.
func Handle[Req, Resp any](r *Registry, op Op[Req, Resp], fn func(context.Context, Meta, Req) (Resp, error)) {
	r.put(op.s, func(ctx context.Context, m Meta, s Stream, body json.RawMessage) {
		ctx, stop := watchPeer(ctx, s)
		defer stop()
		resp, err := runTyped(ctx, m, body, fn)
		_ = writeReply(s, resp, err)
	})
}

// HandleStream serves a finite stream op. fn returns the opening reply and the function to run on
// the bytes. On error only the error reply is sent and the stream is closed.
func HandleStream[Req, Resp any](r *Registry, op StreamOp[Req, Resp], fn func(context.Context, Meta, Req) (Resp, Bytes, error)) {
	r.put(op.s, streamRunner(fn))
}

// HandleResumable serves a resumable stream op, like HandleStream. ctx is cancelled when a drain
// cuts the stream, so the handler's bytes function ends and the holder reopens the op.
func HandleResumable[Req, Resp any](r *Registry, op ResumableOp[Req, Resp], fn func(context.Context, Meta, Req) (Resp, Bytes, error)) {
	r.put(op.s, streamRunner(fn))
}

func streamRunner[Req, Resp any](fn func(context.Context, Meta, Req) (Resp, Bytes, error)) func(context.Context, Meta, Stream, json.RawMessage) {
	return func(ctx context.Context, m Meta, s Stream, body json.RawMessage) {
		var run Bytes
		resp, err := runTyped(ctx, m, body, func(ctx context.Context, m Meta, req Req) (Resp, error) {
			var resp Resp
			var err error
			resp, run, err = fn(ctx, m, req)
			return resp, err
		})
		if werr := writeReply(s, resp, err); werr != nil || err != nil || run == nil {
			return
		}
		_ = run(ctx, s)
	}
}

// runTyped is the boundary: decode strictly, Validate, call, and turn a Go error into a wire code.
func runTyped[Req, Resp any](ctx context.Context, m Meta, body json.RawMessage, fn func(context.Context, Meta, Req) (Resp, error)) (Resp, error) {
	var req Req
	var zero Resp
	if err := decodeStrict(body, &req); err != nil {
		return zero, Errorf(CodeBadRequest, "%v", err)
	}
	if v, ok := any(&req).(Validator); ok {
		if err := v.Validate(); err != nil {
			return zero, Errorf(CodeBadRequest, "%v", err)
		}
	}
	resp, err := fn(ctx, m, req)
	return resp, wireError(err)
}

func decodeStrict(body json.RawMessage, v any) error {
	if len(body) == 0 {
		body = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

// wireError maps any handler error to an *Error. Only an *Error the handler built keeps its code.
func wireError(err error) error {
	var le *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &le):
		return le
	case errors.Is(err, context.Canceled):
		return Errorf(CodeCancelled, "cancelled")
	}
	return Errorf(CodeInternal, "%v", err)
}

func writeReply[Resp any](w io.Writer, resp Resp, err error) error {
	var le *Error
	if errors.As(err, &le) {
		return writeFrame(w, replyFrame{Err: le})
	}
	body, merr := json.Marshal(resp)
	if merr != nil {
		return writeFrame(w, replyFrame{Err: &Error{Code: CodeInternal, Msg: "encode reply"}})
	}
	return writeFrame(w, replyFrame{Body: body})
}

// watchPeer cancels ctx when the opener closes its end before the reply was written. A unary
// opener sends exactly one frame and then only reads, so any read result on its stream means it
// went away. This is how an op's context follows its caller's across the link.
func watchPeer(ctx context.Context, s Stream) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = io.Copy(io.Discard, s)
		cancel()
	}()
	return ctx, cancel
}

// requestTimeout bounds how long a peer may take to send frame 1 once it opened a stream.
const requestTimeout = 10 * time.Second

// dispatch serves one accepted stream: read frame 1, find the handler in the first registry that
// has it (the built-in link.* registry first), enforce the capability, tell the in-flight set the
// op's kind, run it. It is the single entry for both sides and the only code that reads a request.
// The frame-1 deadline is a context timer that closes the stream, so no wall-clock read is needed.
func dispatch(ctx context.Context, m Meta, st *stream, regs ...*Registry) {
	var rq requestFrame
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	stop := context.AfterFunc(rctx, func() { _ = st.Close() })
	err := readFrame(st, &rq, true)
	timedOut := !stop() // stop first: cancel would itself fire the close
	cancel()
	if timedOut || err != nil {
		return
	}
	for _, r := range regs {
		if h, ok := r.lookup(rq.Op); ok {
			runHandler(ctx, m, st, r, h, rq)
			return
		}
	}
	_ = writeReply(st, struct{}{}, Errorf(CodeUnknownOp, "no such op %q", rq.Op))
}

func runHandler(ctx context.Context, m Meta, st *stream, r *Registry, h handler, rq requestFrame) {
	if !r.caps.Has(h.spec.needs) {
		_ = writeReply(st, struct{}{}, Errorf(CodeForbidden, "%s is not enabled on this machine", rq.Op))
		return
	}
	if !st.setKind(h.spec.kind) {
		_ = writeReply(st, struct{}{}, Errorf(CodeDraining, "session is retiring"))
		return
	}
	h.run(ctx, m, st, rq.Body)
}
