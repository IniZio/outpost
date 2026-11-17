package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/auth"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/lifecycle"
)

func (s *Server) taskRoutes() {
	m := s.mux
	m.HandleFunc("POST /api/tasks", s.act(s.createTask))
	m.HandleFunc("GET /api/tasks", s.watch(s.listTasks))
	m.HandleFunc("GET /api/tasks/{id}", s.watch(s.getTask))
	m.HandleFunc("GET /api/tasks/{id}/events", s.watch(s.events))
	m.HandleFunc("GET /api/tasks/{id}/screen", s.watch(s.screen))
	m.HandleFunc("POST /api/tasks/{id}/message", s.act(s.message))
	for verb, cmd := range map[string]func(domain.Login) domain.Command{
		"stop":          func(by domain.Login) domain.Command { return domain.UserStop{By: by} },
		"resume":        func(by domain.Login) domain.Command { return domain.UserResume{By: by} },
		"save-template": func(by domain.Login) domain.Command { return domain.SaveTemplateRequested{By: by} },
	} {
		m.HandleFunc("POST /api/tasks/{id}/"+verb, s.act(func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			return s.do(w, r, p, cmd(p.Login))
		}))
	}
}

// createKinds are the kinds a person may start. Scheduled tasks come only from schedules.
var createKinds = map[string]domain.Kind{"": domain.KindInteractive, "interactive": domain.KindInteractive, "setup": domain.KindSetup}

var kindNames = map[domain.Kind]string{domain.KindInteractive: "interactive", domain.KindScheduled: "scheduled", domain.KindSetup: "setup"}

func view(t domain.Task, a domain.Access) client.Task {
	v := client.Task{
		ID: int64(t.ID), Owner: string(t.Owner), Kind: kindNames[t.Kind], Repo: string(t.Spec.Repo),
		Title: t.Spec.Title, Prompt: t.Spec.Prompt, Status: t.Status().String(), Phase: t.Phase(),
		Machine: string(t.Machine), CreatedAt: t.CreatedAt, Access: "watch",
	}
	for _, r := range t.Spec.Also {
		v.Also = append(v.Also, string(r))
	}
	if a == domain.AccessControl {
		v.Access = "control"
	}
	switch l := t.Life.(type) {
	case domain.Active:
		v.Summary = l.Summary
	case domain.Closing:
		v.Summary = l.Summary
	case domain.Ended:
		v.Summary, v.Error = l.Summary, l.Error
	}
	return v
}

// task loads the task named in the path for p. No access is not found, so ids do not leak.
// Control needs the owner and an API token.
func (s *Server) task(r *http.Request, p auth.Principal, need domain.Access) (domain.Task, domain.Access, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return domain.Task{}, 0, errNotFound
	}
	t, err := s.cfg.Tasks.Task(r.Context(), domain.TaskID(id))
	if err != nil {
		return domain.Task{}, 0, err //nolint:wrapcheck // lifecycle wraps its own
	}
	a := t.AccessFor(p.Login, false)
	switch {
	case a == domain.AccessNone:
		return domain.Task{}, 0, errNotFound
	case need == domain.AccessControl && a != domain.AccessControl:
		return domain.Task{}, 0, domain.ErrForbidden
	}
	return t, a, nil
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var in client.NewTask
	if err := decode(w, r, &in); err != nil {
		return err
	}
	spec := domain.Spec{Title: in.Title, Repo: domain.RepoName(in.Repo), Prompt: in.Prompt, Timeout: time.Duration(in.TimeoutMin) * time.Minute}
	for _, a := range in.Also {
		spec.Also = append(spec.Also, domain.RepoName(a))
	}
	kind, ok := createKinds[in.Kind]
	if !ok {
		return fmt.Errorf("%w: kind is interactive or setup", errBadRequest)
	}
	t, err := s.cfg.Tasks.Create(r.Context(), p.Login, kind, spec)
	if err != nil {
		return err //nolint:wrapcheck // lifecycle wraps its own
	}
	reply(w, http.StatusCreated, view(t, domain.AccessControl))
	return nil
}

var statusByName = func() map[string]domain.Status {
	m := map[string]domain.Status{}
	for st := domain.StatusQueued; st <= domain.StatusStopped; st++ {
		m[st.String()] = st
	}
	return m
}()

// listTasks lists the caller's own tasks. Teammates' tasks join the list when sharing lands.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	q := r.URL.Query()
	tq := lifecycle.TaskQuery{Owner: p.Login, Limit: 50}
	if name := q.Get("status"); name != "" {
		st, ok := statusByName[name]
		if !ok {
			return fmt.Errorf("%w: unknown status %q", errBadRequest, name)
		}
		tq.Status = st
	}
	before, err1 := intParam(q.Get("before"), 0, 1<<62)
	limit, err2 := intParam(q.Get("limit"), 50, 200)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("%w: before and limit are numbers", errBadRequest)
	}
	tq.Before, tq.Limit = domain.TaskID(before), int(limit)
	ts, err := s.cfg.Tasks.List(r.Context(), tq)
	if err != nil {
		return err //nolint:wrapcheck // lifecycle wraps its own
	}
	out := make([]client.Task, 0, len(ts))
	for _, t := range ts {
		out = append(out, view(t, t.AccessFor(p.Login, false)))
	}
	reply(w, http.StatusOK, out)
	return nil
}

// intParam parses an optional number in [0, max], def when empty.
func intParam(v string, def, maxV int64) (int64, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > maxV {
		return 0, errBadRequest
	}
	return n, nil
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	t, a, err := s.task(r, p, domain.AccessWatch)
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, view(t, a))
	return nil
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var in client.Text
	if err := decode(w, r, &in); err != nil {
		return err
	}
	return s.do(w, r, p, domain.UserMessage{By: p.Login, Text: in.Text})
}

func (s *Server) do(w http.ResponseWriter, r *http.Request, p auth.Principal, c domain.Command) error {
	t, _, err := s.task(r, p, domain.AccessControl)
	if err != nil {
		return err
	}
	t, err = s.cfg.Tasks.Do(r.Context(), t.ID, c)
	if err != nil {
		return err //nolint:wrapcheck // lifecycle wraps the domain's refusal
	}
	reply(w, http.StatusOK, view(t, domain.AccessControl))
	return nil
}

// keepAlive is how often an idle event stream sends a comment, so proxies keep it open.
const keepAlive = 15 * time.Second

// events streams the timeline as server-sent events. The event id is the entry's Seq, so a client
// that reconnects with Last-Event-ID, or ?after=, resumes after the last entry it saw.
func (s *Server) events(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	t, _, err := s.task(r, p, domain.AccessWatch)
	if err != nil {
		return err
	}
	after := r.Header.Get("Last-Event-ID")
	if after == "" {
		after = r.URL.Query().Get("after")
	}
	n, err := intParam(after, 0, 1<<31)
	if err != nil {
		return fmt.Errorf("%w: after is an entry number", errBadRequest)
	}
	sw := newSSE(w)
	stop := sw.keepAlive(keepAlive)
	defer stop()
	for row, err := range s.cfg.Tasks.Follow(r.Context(), t.ID, int(n)) {
		if err != nil {
			sw.send("error", "", client.Error{Code: "internal", Message: "the timeline could not be read"})
			return nil
		}
		sw.send("entry", strconv.Itoa(row.Seq), client.Entry{Seq: row.Seq, At: row.At, Kind: string(row.Kind), Text: row.Text})
	}
	return nil
}

// sse writes events. The keep-alive goroutine and the handler share it, so writes take a lock.
type sse struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sse{w: w, rc: http.NewResponseController(w)}
	_ = s.rc.Flush()
	return s
}

func (s *sse) send(event, id string, v any) {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		_, _ = fmt.Fprintf(s.w, "id: %s\n", id)
	}
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	_ = s.rc.Flush()
}

func (s *sse) keepAlive(every time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.mu.Lock()
				_, _ = fmt.Fprint(s.w, ": keep-alive\n\n")
				_ = s.rc.Flush()
				s.mu.Unlock()
			}
		}
	}()
	return func() { close(done) }
}

func (s *Server) screen(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	t, _, err := s.task(r, p, domain.AccessWatch)
	if err != nil {
		return err
	}
	lines, err := intParam(r.URL.Query().Get("lines"), 0, 2000)
	if err != nil {
		return fmt.Errorf("%w: lines is 0 to 2000", errBadRequest)
	}
	text, err := s.screens.get(t.ID, int(lines), func() (string, error) {
		return s.cfg.Tasks.Screen(r.Context(), t.ID, int(lines))
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(text))
	return nil
}

// screenCache holds each screen for a second, so many watchers polling each second cost one read
// of the box.
type screenCache struct {
	clock domain.Clock
	mu    sync.Mutex
	shots map[screenKey]shot
}

type screenKey struct {
	task  domain.TaskID
	lines int
}

type shot struct {
	text string
	at   time.Time
}

const screenTTL = time.Second

func newScreenCache(c domain.Clock) *screenCache {
	return &screenCache{clock: c, shots: map[screenKey]shot{}}
}

func (c *screenCache) get(id domain.TaskID, lines int, read func() (string, error)) (string, error) {
	k, now := screenKey{id, lines}, c.clock.Now()
	c.mu.Lock()
	sh, ok := c.shots[k]
	c.mu.Unlock()
	if ok && now.Sub(sh.at) < screenTTL {
		return sh.text, nil
	}
	text, err := read()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, old := range c.shots {
		if now.Sub(old.at) > 10*screenTTL {
			delete(c.shots, key)
		}
	}
	c.shots[k] = shot{text: text, at: now}
	return text, nil
}
