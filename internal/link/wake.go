package link

import "sync"

// attachSignal tells waiters that a session attached. next returns a channel that closes at the next
// fire, so a waiter takes it before looking at the route and cannot miss an attach in between. The
// zero value is ready.
type attachSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (a *attachSignal) next() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ch == nil {
		a.ch = make(chan struct{})
	}
	return a.ch
}

func (a *attachSignal) fire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ch != nil {
		close(a.ch)
		a.ch = nil
	}
}
