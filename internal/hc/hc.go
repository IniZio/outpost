// Package hc is herdr-core's JSON-RPC 2.0 protocol (generation 1, one message per line, as
// `hc connect` speaks it on stdio) over any byte stream. It knows no box and no Outpost type. The
// machine reaches hc in a box by handing it the stream of `hc connect` run there.
//
// Checked against hc 0.1.16 (testdata holds the transcripts): connect starts a core when none runs,
// a finished terminal's name is reusable while a live one is name_taken, send refuses a blocked
// program without force, and a connection that subscribed carries only notifications.
package hc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Generation is the protocol generation this client speaks.
const Generation = 1

// Terminal is the part of hc's Terminal fact Outpost reads.
type Terminal struct {
	Address  string            `json:"address"`
	Name     *string           `json:"name"`
	Labels   map[string]string `json:"labels"`
	State    string            `json:"state"` // live or finished
	Status   *Status           `json:"status"`
	ExitCode *int              `json:"exit_code"`
	Signal   *int              `json:"signal"`
	Reason   *string           `json:"reason"`
}

// Status is one OSC 7501 record. State is idle, working, done, blocked or error.
type Status struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

// Live reports whether the terminal's program still runs.
func (t Terminal) Live() bool { return t.State == "live" }

// Code is the exit code Outpost reports: the program's own, or 128+signal for a signal death.
func (t Terminal) Code() int {
	switch {
	case t.ExitCode != nil:
		return *t.ExitCode
	case t.Signal != nil:
		return 128 + *t.Signal
	}
	return -1
}

// RunReq is terminal.run's params, the ones Outpost uses.
type RunReq struct {
	Argv          []string          `json:"argv"`
	Name          string            `json:"name,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	NoParent      bool              `json:"no_parent,omitempty"`
	OutliveParent bool              `json:"outlive_parent,omitempty"`
	NoNotify      bool              `json:"no_notify,omitempty"`
}

// ExecReq is machine.exec's params. What the program started ends with it, at its exit, at its
// timeout, or when this connection goes away.
type ExecReq struct {
	Argv    []string          `json:"argv"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout float64           `json:"timeout,omitempty"` // seconds
}

// ExecResult is machine.exec's result. Stdout and Stderr are base64 in JSON, as hc sends them.
type ExecResult struct {
	Code      *int   `json:"code"`
	Signal    *int   `json:"signal"`
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
}

// OK reports whether the program exited 0.
func (r ExecResult) OK() bool { return r.Code != nil && *r.Code == 0 }

// Error is a failure hc reported, with its stable kind.
type Error struct {
	Kind    string
	Message string
}

func (e *Error) Error() string { return "hc: " + e.Kind + ": " + e.Message }

// The error kinds Outpost branches on.
const (
	KindNameTaken = "name_taken"
	KindNotFound  = "not_found"
)

// IsKind reports whether err is an hc error of kind.
func IsKind(err error, kind string) bool {
	var he *Error
	return errors.As(err, &he) && he.Kind == kind
}

// ErrClosed is returned by calls on a connection whose stream ended.
var ErrClosed = errors.New("hc: connection closed")

type message struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Message string `json:"message"`
	Data    struct {
		Kind string `json:"kind"`
	} `json:"data"`
}

func (w *wireError) err() error { return &Error{Kind: w.Data.Kind, Message: w.Message} }

// Conn is one request and response connection. Calls may run concurrently: replies are matched by
// id. Notifications on it are ignored.
type Conn struct {
	rwc     io.ReadWriteCloser
	wmu     sync.Mutex
	next    atomic.Uint64
	mu      sync.Mutex
	waiting map[uint64]chan message
	dead    chan struct{}
	err     error
}

// Dial wraps a stream. The connection lives until the stream ends or Close.
func Dial(rwc io.ReadWriteCloser) *Conn {
	c := &Conn{rwc: rwc, waiting: map[uint64]chan message{}, dead: make(chan struct{})}
	go c.read()
	return c
}

func (c *Conn) read() {
	sc := newScanner(c.rwc)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch := c.waiting[m.ID]
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	c.mu.Lock()
	c.err = ErrClosed
	if err := sc.Err(); err != nil {
		c.err = fmt.Errorf("%w: %w", ErrClosed, err)
	}
	c.mu.Unlock()
	close(c.dead)
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20) // an exec result carries up to 9 MB of base64
	return sc
}

// Close ends the connection and the `hc connect` process behind it.
func (c *Conn) Close() error {
	if err := c.rwc.Close(); err != nil {
		return fmt.Errorf("hc: close: %w", err)
	}
	return nil
}

// Call sends one request and decodes its result into out, which may be nil.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	id := c.next.Add(1)
	ch := make(chan message, 1)
	c.mu.Lock()
	c.waiting[id] = ch
	c.mu.Unlock()
	defer c.forget(id)
	if err := c.send(id, method, params); err != nil {
		return err
	}
	select {
	case m := <-ch:
		return decode(method, m, out)
	case <-c.dead:
		c.mu.Lock()
		defer c.mu.Unlock()
		return fmt.Errorf("hc: %s: %w", method, c.err)
	case <-ctx.Done():
		return fmt.Errorf("hc: %s: %w", method, ctx.Err())
	}
}

func (c *Conn) send(id uint64, method string, params any) error {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("hc: %s: %w", method, err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.rwc.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("hc: %s: %w", method, err)
	}
	return nil
}

func (c *Conn) forget(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.waiting, id)
}

func decode(method string, m message, out any) error {
	if m.Error != nil {
		return m.Error.err()
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(m.Result, out); err != nil {
		return fmt.Errorf("hc: %s: %w", method, err)
	}
	return nil
}

// Hello checks that the core speaks this client's generation.
func (c *Conn) Hello(ctx context.Context) error {
	var r struct {
		Generation int    `json:"generation"`
		Version    string `json:"version"`
	}
	if err := c.Call(ctx, "hello", struct{}{}, &r); err != nil {
		return err
	}
	if r.Generation != Generation {
		return fmt.Errorf("hc: core %s speaks generation %d, this client %d", r.Version, r.Generation, Generation)
	}
	return nil
}

// Run starts a terminal.
func (c *Conn) Run(ctx context.Context, r RunReq) (Terminal, error) {
	var t Terminal
	return t, c.Call(ctx, "terminal.run", r, &t)
}

// List returns the terminals carrying every label in labels, finished ones included.
func (c *Conn) List(ctx context.Context, labels map[string]string) ([]Terminal, error) {
	var out struct {
		Terminals []Terminal `json:"terminals"`
	}
	err := c.Call(ctx, "terminal.list", map[string]any{"all": true, "labels": labels}, &out)
	return out.Terminals, err
}

// Get returns one terminal by address or name.
func (c *Conn) Get(ctx context.Context, terminal string) (Terminal, error) {
	var t Terminal
	return t, c.Call(ctx, "terminal.get", map[string]string{"terminal": terminal}, &t)
}

// Kill ends a terminal's program.
func (c *Conn) Kill(ctx context.Context, terminal string) error {
	return c.Call(ctx, "terminal.kill", map[string]string{"terminal": terminal}, nil)
}

// Send pastes text and presses enter, even when the program is blocked: whether to type is the
// caller's decision.
func (c *Conn) Send(ctx context.Context, terminal, text string) error {
	return c.Call(ctx, "terminal.send", map[string]any{"terminal": terminal, "text": text, "force": true}, nil)
}

// Interrupt presses the stop key.
func (c *Conn) Interrupt(ctx context.Context, terminal string) error {
	return c.Call(ctx, "terminal.interrupt", map[string]any{"terminal": terminal, "timeout": 1}, nil)
}

// ReadResult is terminal.read's answer: the screen and the history asked for, as text. Complete
// is false when part of it may be missing, as for a full-screen program read while it works.
type ReadResult struct {
	Text     string `json:"text"`
	Complete bool   `json:"complete"`
}

// Read returns a terminal's screen plus lines of history above it, live or finished.
func (c *Conn) Read(ctx context.Context, terminal string, lines int) (ReadResult, error) {
	var out ReadResult
	return out, c.Call(ctx, "terminal.read", map[string]any{"terminal": terminal, "lines": lines}, &out)
}

// Exec runs a program to its end and returns what it wrote.
func (c *Conn) Exec(ctx context.Context, r ExecReq) (ExecResult, error) {
	var out ExecResult
	return out, c.Call(ctx, "machine.exec", r, &out)
}
