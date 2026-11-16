package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
)

// fakeAPI answers the routes the CLI calls, with the API's wire types.
func fakeAPI(t *testing.T) *httptest.Server {
	var polls atomic.Int32
	task := client.Task{ID: 7, Repo: "o/r", Prompt: "bump\nmore", Status: "working", CreatedAt: time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)}
	routes := map[string]func(w http.ResponseWriter, r *http.Request){
		"POST /api/sessions": func(w http.ResponseWriter, r *http.Request) {
			var in client.SignIn
			_ = json.NewDecoder(r.Body).Decode(&in)
			_ = json.NewEncoder(w).Encode(client.Session{Token: "otp_" + in.GitHubToken, Login: "ana"})
		},
		"GET /api/me": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(client.Me{Login: "ana", Act: true})
		},
		"POST /api/tasks": func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(task) },
		"GET /api/tasks":  func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode([]client.Task{task}) },
		"POST /api/tasks/7/stop": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(client.Task{ID: 7, Status: "stopped"})
		},
		"POST /api/tasks/7/message": func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(task) },
		"GET /api/tasks/7": func(w http.ResponseWriter, _ *http.Request) {
			t := task
			switch n := polls.Add(1); {
			case n > 4:
				t.Status = "stopped"
			case n > 1:
				t.Status = "waiting"
			}
			_ = json.NewEncoder(w).Encode(t)
		},
		"GET /api/tasks/7/events": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, "id: 1\nevent: entry\ndata: {\"seq\":1,\"kind\":\"queued\",\"text\":\"Queued\"}\n\n")
		},
		"GET /api/tasks/7/screen": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("agent screen")) },
		"POST /api/machines/confirm": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(client.Machine{ID: "m1", Name: "lab"})
		},
		"GET /api/machines": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]client.Machine{{ID: "m1", Name: "lab", Role: "sandbox", Slots: 2, Online: true}})
		},
		"POST /api/sessions/web-link": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(client.WebLink{URL: "http://x/login?t=otl_1"})
		},
	}
	mux := http.NewServeMux()
	for route, h := range routes {
		mux.HandleFunc(route, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type cli struct {
	t   *testing.T
	env map[string]string
}

func (c cli) run(stdin string, args ...string) (string, error) {
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, args, env{get: func(k string) string { return c.env[k] }, in: strings.NewReader(stdin), out: &out})
	return out.String(), err
}

func (c cli) must(stdin string, args ...string) string {
	c.t.Helper()
	out, err := c.run(stdin, args...)
	if err != nil {
		c.t.Fatalf("%v: %v", args, err)
	}
	return out
}

func TestSignInThenWorkWithTasks(t *testing.T) {
	pollEvery = 10 * time.Millisecond
	srv := fakeAPI(t)
	c := cli{t: t, env: map[string]string{"OUTPOST_CLI_CONFIG": filepath.Join(t.TempDir(), "cli.json")}}
	if out := c.must("gho_x\n", "login", srv.URL+"/"); !strings.Contains(out, "as ana") {
		t.Fatal(out)
	}
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "-d", "-also", "o/a,o/b", "o/r", "bump", "deps"}, "Task 7 is working"},
		{[]string{"ls"}, "bump"},
		{[]string{"msg", "7", "also", "go"}, "Sent"},
		{[]string{"stop", "7"}, "is stopped"},
		{[]string{"machine", "confirm", "bcdf-ghjk"}, "Paired lab"},
		{[]string{"machines"}, "sandbox"},
		{[]string{"weblink"}, "otl_1"},
		{[]string{"watch", "7"}, "Queued"},
		{[]string{"watch", "-screen", "7"}, "agent screen"},
	} {
		if out := c.must("", step.args...); !strings.Contains(out, step.want) {
			t.Fatalf("%v: %q", step.args, out)
		}
	}
	if out := c.must("", "login", srv.URL, "otp_given"); !strings.Contains(out, "as ana") {
		t.Fatal(out)
	}
}

func TestUsageAndMissingLogin(t *testing.T) {
	c := cli{t: t, env: map[string]string{"OUTPOST_CLI_CONFIG": filepath.Join(t.TempDir(), "none.json")}}
	for _, args := range [][]string{nil, {"fly"}, {"ls"}, {"run", "o/r"}, {"msg", "x", "hi"}, {"stop"}, {"machine", "list"}, {"login"}, {"login", "http://127.0.0.1:1"}, {"watch", "-x"}} {
		if _, err := c.run("", args...); err == nil {
			t.Errorf("%v should fail", args)
		}
	}
	c.env["OUTPOST_SERVER"], c.env["OUTPOST_TOKEN"] = "http://127.0.0.1:1", "otp_x"
	if _, err := c.run("", "ls"); err == nil {
		t.Error("an unreachable server")
	}
}
