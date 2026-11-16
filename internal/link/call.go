package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// Caller is something an op can be opened on: the server's Link to one machine, or the agent's
// view of the server. Only this package can implement it.
type Caller interface {
	// session returns the live session to route a new stream onto, or an error.
	session(ctx context.Context) (*session, error)
	// pacer is how Hold waits between attempts on this caller.
	pacer() pacer
}

// pacer is the waiting seam of Hold: the owning side's backoff, sleep and jitter, and the clock
// that tells how long a stream lived. The zero value is production behaviour.
type pacer struct {
	backoff Backoff
	sleep   Sleeper
	jitter  func() float64
	clock   domain.Clock
}

func (p pacer) now() time.Time {
	if p.clock == nil {
		return domain.SystemClock{}.Now()
	}
	return p.clock.Now()
}

// norm fills in production defaults for the zero parts.
func (p pacer) norm() pacer {
	if p.backoff.Max == 0 {
		p.backoff = DefaultBackoff()
	}
	if p.sleep == nil {
		p.sleep = sleep
	}
	if p.jitter == nil {
		p.jitter = rand.Float64
	}
	return p
}

func (p pacer) wait(ctx context.Context, attempt int) error {
	return p.sleep(ctx, p.backoff.Delay(attempt, time.Duration(math.MaxInt64), p.jitter()))
}

// Call runs a unary op on the peer and returns its typed reply. It returns:
//
//	ErrOffline   no session; nothing was sent
//	ErrDraining  the server is shutting down and takes no new ops
//	*Error       the peer's handler (or dispatcher) refused or failed, with a Code
//	ErrLinkLost  the session died after the request went out; the op may have run
//	ctx.Err()    the caller gave up; the peer's handler context is cancelled
func Call[Req, Resp any](ctx context.Context, c Caller, op Op[Req, Resp], req Req) (Resp, error) {
	var zero Resp
	st, err := openOp(ctx, c, op.s, req)
	if err != nil {
		return zero, err
	}
	defer func() { _ = st.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	return readReply[Resp](ctx, st)
}

// Open runs a finite stream op: it returns the opening reply and then the byte stream. The caller
// owns the stream and must Close it. ctx ending before Open returns closes the stream. After that,
// Close is only a FIN: see Stream for what the peer does with it.
func Open[Req, Resp any](ctx context.Context, c Caller, op StreamOp[Req, Resp], req Req) (Resp, Stream, error) {
	return openStream[Req, Resp](ctx, c, op.s, req)
}

func openStream[Req, Resp any](ctx context.Context, c Caller, s spec, req Req) (Resp, Stream, error) {
	var zero Resp
	st, err := openOp(ctx, c, s, req)
	if err != nil {
		return zero, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	resp, err := readReply[Resp](ctx, st)
	if !stop() || err != nil {
		_ = st.Close()
		if err == nil {
			err = fmt.Errorf("link: open %w", ctx.Err())
		}
		return zero, nil, err
	}
	return resp, st, nil
}

// Hold keeps a resumable op open on c until ctx ends. use runs on each opening with the reply and
// the stream, and returns when the stream ends. Hold then opens the op again, on the current link
// or the next one.
//
// yamux has no stream reset, and a stream that dies with its session reads as a plain EOF, so "the
// peer finished" and "a drain cut it" look the same. Hold therefore treats every end of the stream
// alike. It stops only when ctx ends, when the server it holds on is draining (the next server
// holds again from its own state), or when the peer refuses the op with a reason ("no such box").
// Between openings it backs off with the owning side's schedule, growing while streams die young.
func Hold[Req, Resp any](ctx context.Context, c Caller, op ResumableOp[Req, Resp], req Req, use func(context.Context, Resp, Stream)) error {
	p := c.pacer().norm()
	fails := 0
	for ctx.Err() == nil {
		start := p.now()
		resp, st, err := openStream[Req, Resp](ctx, c, op.s, req)
		if err != nil && !retryable(err) {
			return err
		}
		if err == nil {
			holdUse(ctx, resp, st, use)
		}
		fails = p.backoff.Next(fails, p.now().Sub(start))
		if err := p.wait(ctx, fails); err != nil {
			return err
		}
	}
	return fmt.Errorf("link: hold: %w", ctx.Err())
}

// holdUse runs use on one opening, and closes the stream when ctx ends so use cannot outlive Hold.
func holdUse[Resp any](ctx context.Context, resp Resp, st Stream, use func(context.Context, Resp, Stream)) {
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	use(ctx, resp, st)
	_ = st.Close()
}

func retryable(err error) bool {
	switch {
	case errors.Is(err, ErrOffline), errors.Is(err, ErrLinkLost):
		return true
	case errors.Is(err, &Error{Code: CodeDraining}), errors.Is(err, &Error{Code: CodeBusy}):
		return true
	}
	return false
}

// openOp is the one place a stream is opened for a request: route to a session, capability check,
// admission to the session's in-flight set, yamux open, frame 1.
func openOp(ctx context.Context, c Caller, s spec, req any) (*stream, error) {
	sess, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	if !sess.peerCaps.Has(s.needs) {
		return nil, Errorf(CodeForbidden, "machine lacks the capability %q needs", s.name)
	}
	st, err := sess.open(s.kind)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err == nil {
		err = writeFrame(st, requestFrame{Op: s.name, Body: body})
	}
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("%w: %w", ErrLinkLost, err)
	}
	return st, nil
}

func readReply[Resp any](ctx context.Context, r io.Reader) (Resp, error) {
	var zero, out Resp
	var rep replyFrame
	if err := readFrame(r, &rep, false); err != nil {
		if ctx.Err() != nil {
			return zero, fmt.Errorf("link: %w", ctx.Err())
		}
		return zero, fmt.Errorf("%w: %w", ErrLinkLost, err)
	}
	if rep.Err != nil {
		if ctx.Err() != nil && rep.Err.Code == CodeCancelled {
			return zero, fmt.Errorf("link: %w", ctx.Err()) // our own cancel echoed back
		}
		return zero, rep.Err
	}
	if len(rep.Body) > 0 {
		if err := json.Unmarshal(rep.Body, &out); err != nil {
			return zero, errors.Join(ErrLinkLost, err)
		}
	}
	return out, nil
}
