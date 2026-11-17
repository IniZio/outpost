package ops_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ops"
)

func goodConverge() ops.ConvergeReq {
	return ops.ConvergeReq{
		Box: ops.BoxRef{Task: 1, Run: 1, Gen: 1}, Image: "ob-base:1", Repos: []domain.RepoName{"o/r"},
		Env: map[string]string{"GH_TOKEN": "p"}, Argv: []string{"agent"}, Budget: time.Minute,
		Home: []ops.HomeFile{{Path: ".config/x", Data: []byte("x")}},
	}
}

func TestConvergeValidateRefusesWhatCouldEscape(t *testing.T) {
	if err := goodConverge().Validate(); err != nil {
		t.Fatalf("a good request is valid: %v", err)
	}
	bad := map[string]func(*ops.ConvergeReq){
		"no task":        func(r *ops.ConvergeReq) { r.Box.Task = 0 },
		"flag image":     func(r *ops.ConvergeReq) { r.Image = "--privileged" },
		"no repo":        func(r *ops.ConvergeReq) { r.Repos = nil },
		"dotdot repo":    func(r *ops.ConvergeReq) { r.Repos = []domain.RepoName{"o/.."} },
		"no argv":        func(r *ops.ConvergeReq) { r.Argv = nil },
		"no budget":      func(r *ops.ConvergeReq) { r.Budget = 0 },
		"env name":       func(r *ops.ConvergeReq) { r.Env = map[string]string{"A=B": "x"} },
		"absolute home":  func(r *ops.ConvergeReq) { r.Home = []ops.HomeFile{{Path: "/etc/passwd"}} },
		"escaping home":  func(r *ops.ConvergeReq) { r.Home = []ops.HomeFile{{Path: "a/../../x"}} },
		"oversized home": func(r *ops.ConvergeReq) { r.Home = []ops.HomeFile{{Path: "a", Data: make([]byte, ops.HomeBudget+1)}} },
		"path as inner":  func(r *ops.ConvergeReq) { r.Inner = "../../etc" },
	}
	for name, mut := range bad {
		r := goodConverge()
		mut(&r)
		if r.Validate() == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestBoxPathMustBeAbsoluteAndClean(t *testing.T) {
	box := ops.BoxRef{Task: 2}
	for _, p := range []string{"rel", "/a/../b", "/a/", ""} {
		if (ops.BoxPath{Box: box, Path: p}).Validate() == nil {
			t.Errorf("%q must be refused", p)
		}
	}
	if err := (ops.BoxPath{Box: box, Path: domain.DoneMarkerPath}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (ops.SaveReq{Box: box}).Validate() == nil || (ops.TypeReq{}).Validate() == nil {
		t.Fatal("a save needs a budget and a type needs a box")
	}
}

func TestWatchLineCarriesEachSignal(t *testing.T) {
	want := []domain.Signal{
		domain.AgentReport{Run: 3, Status: domain.HCBlocked, Phase: "question"},
		domain.AgentExited{Run: 3, Code: 143},
		domain.BoxEvent{Gen: 2, Kind: domain.BoxDied, OOMKilled: true},
	}
	lines := []ops.WatchLine{
		{Task: 1, Kind: ops.WatchReport, Run: 3, Status: domain.HCBlocked, Phase: "question"},
		{Task: 1, Kind: ops.WatchExit, Run: 3, Code: 143},
		{Task: 1, Kind: ops.WatchBox, Gen: 2, Box: domain.BoxDied, OOM: true},
	}
	for i, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		var back ops.WatchLine
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		got, ok := back.Signal()
		if !ok || got != want[i] {
			t.Errorf("line %d: got %#v, want %#v", i, got, want[i])
		}
	}
	if _, ok := (ops.WatchLine{Kind: "later"}).Signal(); ok {
		t.Fatal("an unknown kind is dropped")
	}
}
