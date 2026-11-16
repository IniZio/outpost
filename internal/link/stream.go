package link

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// Stream is one byte stream on the link. It is a net.Conn, so it can be handed to any library that
// wants one (hc's client, an http.Transport, a socks relay).
//
// What Close means to the peer: nothing more than EOF. yamux 0.1 has no reset, so CloseWrite and
// Close both send one FIN and the peer cannot tell them apart. The rule is therefore per op, and
// the handler decides what its read EOF means. Duplex ops (hc attach, forwards, rsync) treat it as
// half-close and keep writing. Lease ops (forward.listen) end on EOF and release what they hold.
// An op that must be killable (an exec) returns an id and gets an explicit kill op: abort is an
// operation, not a property of the stream. The one abort every handler gets for free is session
// death, which cancels its ctx.
type Stream interface {
	net.Conn
	CloseWrite() error
}

// Bytes is what a stream op's handler returns beside its reply. The dispatcher writes the reply
// first and then runs Bytes with the stream, so a handler cannot write raw bytes before the reply.
// The stream is closed when Bytes returns. A nil Bytes means the op was only the reply.
type Bytes func(ctx context.Context, s Stream) error

// Splice copies both ways between a and b until both directions finish or ctx ends, half-closing
// each side as the other's reads hit EOF. It is the one piece of byte plumbing hc attach, box
// egress, port-forward accepts and rsync all use. It closes both ends before it returns.
func Splice(ctx context.Context, a, b io.ReadWriteCloser) error {
	stop := context.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	pump := func(i int, dst io.WriteCloser, src io.Reader) {
		defer wg.Done()
		_, errs[i] = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
		_ = dst.Close() // no half-close available: end both directions rather than hang
	}
	wg.Add(2)
	go pump(0, b, a)
	go pump(1, a, b)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
	if ctx.Err() != nil {
		return fmt.Errorf("link: splice: %w", ctx.Err())
	}
	for _, err := range errs {
		if err != nil {
			return fmt.Errorf("link: splice: %w", err)
		}
	}
	return nil
}
