package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
)

// fakeServer answers pairing as the API does: pending once, then enrolled.
func fakeServer(t *testing.T) *httptest.Server {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/machines/pair", func(w http.ResponseWriter, r *http.Request) {
		var p client.PairRequest
		if json.NewDecoder(r.Body).Decode(&p) != nil || len(p.Key) != 32 || p.Name != "lab" || p.Slots != 3 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(client.PairTicket{UserCode: "BCDF-GHJK", DeviceCode: make([]byte, 32), IntervalSec: 0, Expires: time.Now().Add(time.Minute)}) //nolint:forbidigo // a fake's expiry
	})
	mux.HandleFunc("POST /api/machines/pair/collect", func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) == 1 {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(client.Collected{State: "pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(client.Collected{State: "enrolled", MachineID: "mid-1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPairWritesAnIdentityThatRunReads(t *testing.T) {
	srv := fakeServer(t)
	dir := t.TempDir()
	env := func(k string) string { return map[string]string{"OUTPOST_AGENT_DIR": dir}[k] }
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, []string{"pair", srv.URL + "/", "lab", "3"}, env, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "outpost-cli machine confirm BCDF-GHJK") || !strings.Contains(out.String(), "mid-1") {
		t.Fatal(out.String())
	}
	id, key, err := load(dir)
	if err != nil || id.MachineID != "mid-1" || id.Link != "ws"+strings.TrimPrefix(srv.URL, "http")+"/machines/link" || len(key) != 64 {
		t.Fatalf("%+v %v", id, err)
	}
}

func TestRunRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	env := func(k string) string {
		return map[string]string{"OUTPOST_AGENT_DIR": dir, "OUTPOST_LOG": "debug"}[k]
	}
	ctx := context.Background()
	for _, args := range [][]string{nil, {"pair"}, {"pair", "http://x", "n", "many"}, {"run"}, {"fly"}} {
		if err := run(ctx, args, env, &bytes.Buffer{}); err == nil {
			t.Errorf("%v should fail", args)
		}
	}
	if err := run(ctx, []string{"run"}, func(k string) string { return map[string]string{"OUTPOST_LOG": "loud"}[k] }, nil); err == nil {
		t.Error("a bad log level")
	}
	if err := run(ctx, []string{"pair", "http://127.0.0.1:1", "n"}, env, &bytes.Buffer{}); err == nil {
		t.Error("an unreachable server")
	}
}
