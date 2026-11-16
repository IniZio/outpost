package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/IniZio/outpost/api/client"
)

const export = `{"workflows": [
  {"id": 1, "name": "app-audit", "cron": "0 7 * * 1,3,5", "timezone": "Asia/Hong_Kong", "repos": ["acme/app"],
   "also": [], "prompt": "audit", "timeout_min": 120, "enabled": true, "runs": []},
  {"id": 2, "name": "docs-audit", "cron": "0 9 * * 1-5", "timezone": "UTC", "repos": ["acme/docs", "acme/site"],
   "also": ["acme/lib"], "prompt": "check", "timeout_min": 180, "enabled": false}
]}`

type scheduleAPI struct {
	mu   sync.Mutex
	puts []client.Schedule
}

func (a *scheduleAPI) serve(t *testing.T) cli {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/schedules/{name}", func(w http.ResponseWriter, r *http.Request) {
		var s client.Schedule
		_ = json.NewDecoder(r.Body).Decode(&s)
		a.mu.Lock()
		a.puts = append(a.puts, s)
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("GET /api/schedules", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(a.puts) })
	mux.HandleFunc("POST /api/schedules/app-audit/fire", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Task{ID: 9, Status: "queued"})
	})
	mux.HandleFunc("POST /api/schedules/app-audit/enable", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Schedule{Name: "app-audit", Enabled: true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return cli{t: t, env: map[string]string{"OUTPOST_SERVER": srv.URL, "OUTPOST_TOKEN": "otp_x", "OUTPOST_CLI_CONFIG": filepath.Join(t.TempDir(), "none.json")}}
}

func TestImportAddsEveryWorkflowRepoDisabled(t *testing.T) {
	a := &scheduleAPI{}
	c := a.serve(t)
	file := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(file, []byte(export), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := c.must("", "schedule", "import", file); strings.Count(out, "disabled") != 3 {
		t.Fatal(out)
	}
	names := map[string]client.Schedule{}
	for _, s := range a.puts {
		names[s.Name] = s
		if s.Enabled {
			t.Errorf("%s was imported enabled", s.Name)
		}
	}
	app, site := names["app-audit"], names["docs-audit-site"]
	if len(a.puts) != 3 || app.TZ != "Asia/Hong_Kong" || app.TimeoutMin != 120 || app.Title != "app-audit" ||
		site.Repo != "acme/site" || len(site.Also) != 1 || names["docs-audit-docs"].Repo != "acme/docs" {
		t.Fatalf("schedules: %+v", a.puts)
	}
}

func TestScheduleCommands(t *testing.T) {
	c := (&scheduleAPI{}).serve(t)
	for args, want := range map[string]string{
		"schedule ls":               "SKIPPED",
		"schedule fire app-audit":   "Task 9",
		"schedule enable app-audit": "app-audit is enabled",
		"schedule put -":            "app-audit is disabled",
	} {
		if out := c.must(`{"name":"app-audit","repo":"acme/app","prompt":"x","cron":"0 6 * * *","tz":"UTC"}`, strings.Fields(args)...); !strings.Contains(out, want) {
			t.Errorf("%s: %q", args, out)
		}
	}
	for _, args := range [][]string{{"schedule"}, {"schedule", "fire"}, {"schedule", "nope", "x"}, {"schedule", "import", "/no/such/file"}} {
		if _, err := c.run("", args...); err == nil {
			t.Errorf("%v should fail", args)
		}
	}
}
