package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls one Outpost server as one login.
type Client struct {
	Base  string // http://localhost:7400
	Token string
	HTTP  *http.Client
	// Retry is how long to wait before retrying a 503 from a server that is not leading. Zero
	// takes two seconds.
	Retry time.Duration
}

// New returns a client for base with token.
func New(base, token string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: http.DefaultClient}
}

const retries = 5

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("client: encode: %w", err)
		}
	}
	for try := 0; ; try++ {
		err := c.once(ctx, method, path, body, out)
		if !standby(err) || try == retries {
			return err
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) error {
	res, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	return decode(res, out)
}

// standby reports a 503 from a server that is not leading right now, which a retry gets past.
func standby(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusServiceUnavailable
}

func (c *Client) wait(ctx context.Context) error {
	d := c.Retry
	if d == 0 {
		d = 2 * time.Second
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("client: %w", ctx.Err())
	}
}

func (c *Client) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	res, err := h.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	return res, nil
}

func decode(res *http.Response, out any) error {
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		e := &Error{Status: res.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if json.Unmarshal(b, e) != nil || e.Code == "" {
			e.Code, e.Message = "http", strings.TrimSpace(string(b))
		}
		return e
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *string:
		b, err := io.ReadAll(res.Body)
		*o = string(b)
		return wrapRead(err)
	default:
		return wrapRead(json.NewDecoder(res.Body).Decode(out))
	}
}

func wrapRead(err error) error {
	if err != nil {
		return fmt.Errorf("client: read reply: %w", err)
	}
	return nil
}

func taskPath(id int64, rest string) string { return "/api/tasks/" + strconv.FormatInt(id, 10) + rest }

// CreateTask starts a task.
func (c *Client) CreateTask(ctx context.Context, n NewTask) (Task, error) {
	var t Task
	return t, c.do(ctx, http.MethodPost, "/api/tasks", n, &t)
}

// Tasks lists the caller's tasks, newest first. An empty status lists all.
func (c *Client) Tasks(ctx context.Context, status string, limit int) ([]Task, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var ts []Task
	return ts, c.do(ctx, http.MethodGet, "/api/tasks?"+q.Encode(), nil, &ts)
}

// Task returns one task.
func (c *Client) Task(ctx context.Context, id int64) (Task, error) {
	var t Task
	return t, c.do(ctx, http.MethodGet, taskPath(id, ""), nil, &t)
}

// Message types text to the task's agent. A message to an ended task resumes it.
func (c *Client) Message(ctx context.Context, id int64, text string) (Task, error) {
	var t Task
	return t, c.do(ctx, http.MethodPost, taskPath(id, "/message"), Text{Text: text}, &t)
}

// Stop ends the task.
func (c *Client) Stop(ctx context.Context, id int64) (Task, error) {
	return c.command(ctx, id, "/stop")
}

// Resume wakes an ended task.
func (c *Client) Resume(ctx context.Context, id int64) (Task, error) {
	return c.command(ctx, id, "/resume")
}

// SaveTemplate saves the task's box as its repo's ready template.
func (c *Client) SaveTemplate(ctx context.Context, id int64) (Task, error) {
	return c.command(ctx, id, "/save-template")
}

func (c *Client) command(ctx context.Context, id int64, verb string) (Task, error) {
	var t Task
	return t, c.do(ctx, http.MethodPost, taskPath(id, verb), nil, &t)
}

// Screen returns the agent terminal's screen with lines of history above it.
func (c *Client) Screen(ctx context.Context, id int64, lines int) (string, error) {
	var s string
	return s, c.do(ctx, http.MethodGet, taskPath(id, "/screen?lines="+strconv.Itoa(lines)), nil, &s)
}

// Follow streams the task's timeline after entry `after` to fn until ctx ends, the server closes
// the stream, or fn returns an error. It returns the Seq of the last entry it handed over, so a
// caller resumes from there.
func (c *Client) Follow(ctx context.Context, id int64, after int, fn func(Entry) error) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+taskPath(id, "/events"), nil)
	if err != nil {
		return after, fmt.Errorf("client: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.Itoa(after))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	res, err := h.Do(req)
	if err != nil {
		return after, fmt.Errorf("client: follow: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return after, decode(res, nil)
	}
	defer func() { _ = res.Body.Close() }()
	return readEvents(res.Body, after, fn)
}

// readEvents reads SSE "entry" events. Comments, other events and ids are skipped.
func readEvents(r io.Reader, after int, fn func(Entry) error) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(data), &e) != nil || e.Seq <= after {
			continue
		}
		if err := fn(e); err != nil {
			return after, err
		}
		after = e.Seq
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return after, fmt.Errorf("client: follow: %w", err)
	}
	return after, nil
}

// SignIn trades a GitHub token for an API token. It needs no token of its own.
func (c *Client) SignIn(ctx context.Context, githubToken string) (Session, error) {
	var s Session
	return s, c.do(ctx, http.MethodPost, "/api/sessions", SignIn{GitHubToken: githubToken}, &s)
}

// WebLink returns a one-time link that signs a browser in to watch.
func (c *Client) WebLink(ctx context.Context) (WebLink, error) {
	var l WebLink
	return l, c.do(ctx, http.MethodPost, "/api/sessions/web-link", nil, &l)
}

// Me says who the token belongs to.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var m Me
	return m, c.do(ctx, http.MethodGet, "/api/me", nil, &m)
}

// Machines lists the caller's machines.
func (c *Client) Machines(ctx context.Context) ([]Machine, error) {
	var ms []Machine
	return ms, c.do(ctx, http.MethodGet, "/api/machines", nil, &ms)
}

// DeleteMachine revokes a machine and ends its link.
func (c *Client) DeleteMachine(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/machines/"+url.PathEscape(id), nil, nil)
}

// Pair starts pairing a machine. It needs no token.
func (c *Client) Pair(ctx context.Context, p PairRequest) (PairTicket, error) {
	var t PairTicket
	return t, c.do(ctx, http.MethodPost, "/api/machines/pair", p, &t)
}

// Collect asks whether the pairing was confirmed. It needs no token.
func (c *Client) Collect(ctx context.Context, device []byte) (Collected, error) {
	var got Collected
	return got, c.do(ctx, http.MethodPost, "/api/machines/pair/collect", Collect{DeviceCode: device}, &got)
}

// Confirm pairs the machine showing code to the caller.
func (c *Client) Confirm(ctx context.Context, code string) (Machine, error) {
	var m Machine
	return m, c.do(ctx, http.MethodPost, "/api/machines/confirm", Confirm{UserCode: code}, &m)
}

// Repos lists repos.
func (c *Client) Repos(ctx context.Context) ([]Repo, error) {
	var rs []Repo
	return rs, c.do(ctx, http.MethodGet, "/api/repos", nil, &rs)
}

// SetRepo sets a repo's ready image, its machine and notes.
func (c *Client) SetRepo(ctx context.Context, r Repo) (Repo, error) {
	var out Repo
	return out, c.do(ctx, http.MethodPut, "/api/repos/"+r.Name, r, &out)
}

// SetSecret stores one of the caller's secrets. The value is never read back.
func (c *Client) SetSecret(ctx context.Context, name, value string) error {
	return c.do(ctx, http.MethodPut, "/api/secrets/"+url.PathEscape(name), Text{Text: value}, nil)
}

// Status reads the server's state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodGet, "/api/status", nil, &s)
}
