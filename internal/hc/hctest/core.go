// Package hctest is a fake herdr-core: hc's JSON-RPC over in-memory pipes, with terminals, labels,
// statuses, finished terminals, name_taken, blocked sends and event subscriptions. The transcripts
// in ../testdata, recorded from the real hc, keep it honest.
package hctest

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"

	"github.com/IniZio/outpost/internal/hc"
)

// Core is one fake hc core. The zero value is not usable: call New.
type Core struct {
	mu     sync.Mutex
	terms  []*term
	seq    uint64
	conns  map[*conn]bool
	exec   func(hc.ExecReq) hc.ExecResult
	nconns int
}

type term struct {
	t     hc.Terminal
	sent  []string
	intrs int
}

type conn struct {
	core  *Core
	pipe  net.Conn
	sub   map[string]string // labels it subscribed to; nil until it subscribes
	queue chan []byte
	done  chan struct{}
	once  sync.Once
}

// New returns a core with no terminals, whose machine.exec succeeds with no output.
func New() *Core {
	return &Core{conns: map[*conn]bool{}, exec: func(hc.ExecReq) hc.ExecResult { return Exit(0, "") }}
}

// Exit is an exec result with a code and stdout.
func Exit(code int, stdout string) hc.ExecResult {
	return hc.ExecResult{Code: &code, Stdout: []byte(stdout), Stderr: []byte{}}
}

// SetExec makes machine.exec answer with fn.
func (c *Core) SetExec(fn func(hc.ExecReq) hc.ExecResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exec = fn
}

// Connect is `hc connect`: a new connection to the core.
func (c *Core) Connect() io.ReadWriteCloser {
	ours, theirs := net.Pipe()
	k := &conn{core: c, pipe: ours, queue: make(chan []byte, 256), done: make(chan struct{})}
	c.mu.Lock()
	c.conns[k] = true
	c.nconns++
	c.mu.Unlock()
	go k.serve()
	return theirs
}

// Connections counts the connections ever made.
func (c *Core) Connections() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nconns
}

// Stop closes every connection, as the core dying with its box does.
func (c *Core) Stop() {
	c.mu.Lock()
	conns := slices.Collect(maps.Keys(c.conns))
	c.mu.Unlock()
	for _, k := range conns {
		k.close()
	}
}

// Terminals returns every terminal, finished ones included, oldest first.
func (c *Core) Terminals() []hc.Terminal {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]hc.Terminal, len(c.terms))
	for i, t := range c.terms {
		out[i] = t.t
	}
	return out
}

// Sent returns the texts sent to the terminal named or addressed ref.
func (c *Core) Sent(ref string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.find(ref); t != nil {
		return slices.Clone(t.sent)
	}
	return nil
}

// Interrupts counts the interrupts the terminal got.
func (c *Core) Interrupts(ref string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.find(ref); t != nil {
		return t.intrs
	}
	return 0
}

// SetStatus makes the program of a live terminal report state, as its OSC 7501 record would. An
// empty state clears the status, as hc does when a program exits.
func (c *Core) SetStatus(ref, state, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.find(ref)
	if t == nil {
		return
	}
	t.t.Status = nil
	if state != "" {
		t.t.Status = &hc.Status{State: state, Msg: msg}
	}
	c.emit(t, "status", map[string]any{"status": t.t.Status, "reported": true})
}

// Exit ends a live terminal's program with code.
func (c *Core) Exit(ref string, code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.find(ref); t != nil && t.t.Live() {
		c.finish(t, &code, nil, "exited")
	}
}

// Signal ends a live terminal's program with a signal.
func (c *Core) Signal(ref string, sig int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.find(ref); t != nil && t.t.Live() {
		c.finish(t, nil, &sig, "exited")
	}
}

func (c *Core) finish(t *term, code, sig *int, reason string) {
	t.t.State, t.t.ExitCode, t.t.Signal, t.t.Reason = "finished", code, sig, &reason
	if t.t.Status != nil {
		t.t.Status = nil
		c.emit(t, "status", map[string]any{"status": nil, "reported": true})
	}
	c.emit(t, "finished", map[string]any{"exit_code": code, "signal": sig, "reason": reason})
}

// find returns the newest terminal whose address or name is ref.
func (c *Core) find(ref string) *term {
	for i := len(c.terms) - 1; i >= 0; i-- {
		t := c.terms[i]
		if t.t.Address == ref || (t.t.Name != nil && *t.t.Name == ref) {
			return t
		}
	}
	return nil
}

func (c *Core) emit(t *term, typ string, data any) {
	c.seq++
	ev := map[string]any{"seq": c.seq, "terminal": t.t.Address, "type": typ, "data": data}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "event", "params": ev})
	for k := range c.conns {
		if k.sub != nil && matches(t.t.Labels, k.sub) {
			k.enqueue(b)
		}
	}
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (c *Core) addTerm(r hc.RunReq) (hc.Terminal, *rpcError) {
	if r.Name != "" {
		if t := c.find(r.Name); t != nil && t.t.Live() {
			return hc.Terminal{}, &rpcError{Kind: hc.KindNameTaken, Message: fmt.Sprintf("a live terminal is already named %q", r.Name)}
		}
	}
	t := &term{t: hc.Terminal{
		Address: "t" + strconv.Itoa(len(c.terms)+1), Labels: maps.Clone(r.Labels), State: "live",
	}}
	if t.t.Labels == nil {
		t.t.Labels = map[string]string{}
	}
	if r.Name != "" {
		t.t.Name = &r.Name
	}
	c.terms = append(c.terms, t)
	c.emit(t, "created", t.t)
	return t.t, nil
}
