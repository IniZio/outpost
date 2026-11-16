package link

import (
	"testing"
	"time"
)

func acceptedStream(f *flight) *stream {
	st := &stream{fl: f}
	st.cancel = func() {}
	return st
}

func TestFlightSealWaitsForFiniteStreams(t *testing.T) {
	f := newFlight(2)
	a, b := acceptedStream(f), acceptedStream(f)
	if f.enter(a) != nil || f.enter(b) != nil || f.enter(acceptedStream(f)) == nil {
		t.Fatal("the in-flight limit is the session's")
	}
	f.seal()
	select {
	case <-f.idle():
		t.Fatal("idle with finite streams running")
	default:
	}
	f.exit(a)
	f.exit(a) // a second exit is a no-op
	f.exit(b)
	select {
	case <-f.idle():
	case <-time.After(time.Second):
		t.Fatal("idle once no finite stream is left")
	}
	if f.enter(acceptedStream(f)) == nil {
		t.Fatal("a sealed flight admits nothing")
	}
}

func TestFlightResumableStreamsDoNotHoldADrain(t *testing.T) {
	f := newFlight(4)
	st := acceptedStream(f)
	if f.enter(st) != nil || !f.setKind(st, kindResumable) {
		t.Fatal("admit")
	}
	f.seal()
	select {
	case <-f.idle():
	default:
		t.Fatal("a resumable stream is cut, not waited for")
	}
	f.exit(st)
	f.cutResumable()
	if f.setKind(acceptedStream(f), kindResumable) {
		t.Fatal("after the cut a new resumable stream must not run")
	}
}
