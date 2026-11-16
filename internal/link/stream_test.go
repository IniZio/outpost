package link

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns two connected TCP conns, which half-close the way a stream does.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	type accepted struct {
		c   net.Conn
		err error
	}
	got := make(chan accepted, 1)
	go func() { c, err := l.Accept(); got <- accepted{c, err} }()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a := <-got
	if a.err != nil {
		t.Fatal(a.err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = a.c.Close() })
	return client.(*net.TCPConn), a.c.(*net.TCPConn)
}

func TestSpliceCopiesBothWaysAndHalfCloses(t *testing.T) {
	leftOuter, leftInner := tcpPair(t)
	rightOuter, rightInner := tcpPair(t)
	done := make(chan error, 1)
	go func() { done <- Splice(context.Background(), leftInner, rightInner) }()

	_, _ = leftOuter.Write([]byte("to the right"))
	_ = leftOuter.CloseWrite()
	got, err := io.ReadAll(rightOuter)
	if err != nil || string(got) != "to the right" {
		t.Fatalf("right side got %q %v", got, err)
	}
	_, _ = rightOuter.Write([]byte("to the left"))
	_ = rightOuter.CloseWrite()
	if got, _ = io.ReadAll(leftOuter); string(got) != "to the left" {
		t.Fatalf("left side got %q", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("splice: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("splice must end when both directions finish")
	}
}

func TestSpliceEndsWithItsContext(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer func() { _ = a1.Close(); _ = b1.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := Splice(ctx, a2, b2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestSpliceWithoutHalfCloseEndsBothDirections(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer func() { _ = b1.Close() }()
	done := make(chan error, 1)
	go func() { done <- Splice(context.Background(), a2, b2) }()
	_ = a1.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a conn that cannot half-close must end both directions rather than hang")
	}
}
