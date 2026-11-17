package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/api"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/lifecycle"
	"github.com/IniZio/outpost/internal/ports"
	"github.com/IniZio/outpost/internal/ports/portstest"
)

func errOf[T any](_ T, err error) error { return err }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestATaskRunsAndItsOwnerSteersIt(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	task, err := g.owner.CreateTask(c, client.NewTask{Repo: "o/r", Prompt: "bump deps", Title: "deps"})
	if err != nil || task.Status != "queued" || task.Access != "control" {
		t.Fatalf("%+v %v", task, err)
	}
	eventually(t, "the task starts", func() bool {
		got, err := g.owner.Task(c, task.ID)
		return err == nil && got.Machine == "m1" && got.Status != "queued" && got.Status != "starting"
	})
	list, err := g.owner.Tasks(c, "", 0)
	if err != nil || len(list) != 1 || list[0].ID != task.ID {
		t.Fatalf("the owner's list: %+v %v", list, err)
	}
	if _, err := g.owner.Message(c, task.ID, "also go"); err != nil {
		t.Fatalf("message: %v", err)
	}
	eventually(t, "the screen shows the message", func() bool {
		s, err := g.owner.Screen(c, task.ID, 10)
		return err == nil && strings.Contains(s, "also go")
	})
	if _, err := g.owner.Stop(c, task.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task stops with the agent's summary", func() bool {
		got, err := g.owner.Task(c, task.ID)
		return err == nil && got.Ended() && got.Summary == "all done"
	})
}

func TestTheTimelineStreamsAndResumes(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	task, err := g.owner.CreateTask(c, client.NewTask{Repo: "o/r", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var seen []client.Entry
	stop := errors.New("enough")
	last, err := g.owner.Follow(c, task.ID, 0, func(e client.Entry) error {
		seen = append(seen, e)
		if len(seen) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || last != 1 || seen[0].Seq != 1 || seen[0].Kind != "queued" {
		t.Fatalf("%d %+v %v", last, seen, err)
	}
	_, err = g.owner.Follow(c, task.ID, 1, func(e client.Entry) error {
		if e.Seq != 2 {
			t.Errorf("resumed at %d", e.Seq)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

func TestOthersSeeNothingAndWatchersCannotAct(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	task, err := g.owner.CreateTask(c, client.NewTask{Repo: "o/r", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	bob := g.as("bob")
	for _, err := range []error{
		errOf(bob.Task(c, task.ID)),
		errOf(bob.Stop(c, task.ID)),
		errOf(bob.Screen(c, task.ID, 0)),
		errOf(g.owner.Task(c, 999)),
	} {
		if code(err) != "not_found" {
			t.Fatalf("not found, so ids do not leak: %v", err)
		}
	}
	if list, err := bob.Tasks(c, "", 0); err != nil || len(list) != 0 {
		t.Fatalf("bob lists only his own: %+v %v", list, err)
	}
	if _, err := client.New(g.srv.URL, "").Tasks(c, "", 0); code(err) != "no_session" {
		t.Fatalf("no token: %v", err)
	}
	if _, err := g.owner.Tasks(c, "nonsense", 0); code(err) != "bad_request" {
		t.Fatalf("bad status: %v", err)
	}
	if _, err := g.owner.CreateTask(c, client.NewTask{Repo: "", Prompt: "p"}); code(err) != "invalid" {
		t.Fatalf("no repo: %v", err)
	}
}

func TestAPersonStartsSetupButNotScheduledTasks(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	if _, err := g.owner.CreateTask(c, client.NewTask{Repo: "o/r", Prompt: "p", Kind: "scheduled"}); code(err) != "bad_request" {
		t.Fatalf("a person starts a scheduled task: %v", err)
	}
	setup, err := g.owner.CreateTask(c, client.NewTask{Repo: "o/r", Prompt: "p", Kind: "setup"})
	if err != nil || setup.Kind != "setup" {
		t.Fatalf("setup task: %+v %v", setup, err)
	}
}

func TestAWebLinkSignsABrowserInToWatchOnly(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	task, err := g.owner.CreateTask(c, client.NewTask{Repo: "o/r", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	link, err := g.owner.WebLink(c)
	if err != nil || !strings.HasPrefix(link.URL, "http://outpost.test/login?t=") {
		t.Fatalf("%+v %v", link, err)
	}
	u, _ := url.Parse(link.URL)
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := browser.Get(g.srv.URL + "/login?" + u.RawQuery + "&next=//evil.example")
	if err != nil || res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || len(res.Cookies()) != 1 {
		t.Fatalf("redeem: %v %+v", err, res)
	}
	_ = res.Body.Close()
	cookie := res.Cookies()[0]
	get := func(method, path string) int { return status(t, method, g.srv.URL+path, cookie) }
	path := "/api/tasks/" + itoa(task.ID)
	if get(http.MethodGet, path) != http.StatusOK || get(http.MethodPost, path+"/stop") != http.StatusForbidden {
		t.Fatal("the cookie watches and cannot act")
	}
	if get(http.MethodDelete, "/api/sessions/current") != http.StatusNoContent || get(http.MethodGet, path) != http.StatusUnauthorized {
		t.Fatal("signing out ends the cookie")
	}
}

func status(t *testing.T, method, url string, cookie *http.Cookie) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx(t), method, url, nil)
	req.AddCookie(cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	return r.StatusCode
}

func TestSignInTradesAGitHubToken(t *testing.T) {
	g := newRig(t)
	c := ctx(t)
	anon := client.New(g.srv.URL, "")
	s, err := anon.SignIn(c, "ana")
	if err != nil || s.Login != "ana" {
		t.Fatalf("%+v %v", s, err)
	}
	me, err := client.New(g.srv.URL, s.Token).Me(c)
	if err != nil || me != (client.Me{Login: "ana", Act: true}) {
		t.Fatalf("%+v %v", me, err)
	}
	if _, err := anon.SignIn(c, "carol"); code(err) != "not_allowed" {
		t.Fatalf("not on the list: %v", err)
	}
	if st, err := anon.Status(c); err != nil || !st.Leading {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestAStandbyRefusesWritesWithRetryAfter(t *testing.T) {
	g := newRig(t, func(cfg *api.Config) { cfg.Tasks = newRunner(t, portstest.NewStore(), portstest.NewBoxes()) })
	g.owner.Retry = 1
	_, err := g.owner.CreateTask(ctx(t), client.NewTask{Repo: "o/r", Prompt: "p"})
	var e *client.Error
	if !errors.As(err, &e) || e.Status != http.StatusServiceUnavailable || e.Code != "not_leading" {
		t.Fatalf("%v", err)
	}
}

func TestEveryRefusalHasAStatus(t *testing.T) {
	for _, err := range []error{
		domain.ErrForbidden, domain.ErrReleased, domain.ErrNotReady, domain.ErrClosing, domain.ErrInvalid,
		domain.ErrEnrollExpired, domain.ErrEnrollUsed, domain.ErrExecNotTool, domain.ErrBadPairing,
		domain.ErrTooManyPairings, domain.ErrUnknownPairing, domain.ErrUnknownMachine, lifecycle.ErrNotLeading,
		lifecycle.ErrNotFound,
	} {
		if s := api.StatusOf(err); s == http.StatusInternalServerError {
			t.Errorf("%v has no status", err)
		}
	}
	if api.StatusOf(context.Canceled) != http.StatusInternalServerError {
		t.Error("anything else is a 500")
	}
}

func TestAScreenOfAnUnreachableBoxIsNotRunning(t *testing.T) {
	g := newRig(t)
	tk, err := g.owner.CreateTask(ctx(t), client.NewTask{Repo: "acme/app", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the task has a box", func() bool {
		got, _ := g.owner.Task(ctx(t), tk.ID)
		return got.Machine != ""
	})
	g.boxes.Fail("Screen", fmt.Errorf("remote: screen: %w", ports.ErrNotRunning))
	if _, err := g.owner.Screen(ctx(t), tk.ID, 0); code(err) != "not_running" {
		t.Fatalf("an offline machine's screen: %v", err)
	}
}
