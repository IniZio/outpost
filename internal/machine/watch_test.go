package machine_test

import (
	"bufio"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

// watch opens box.watch and delivers its lines until the test ends.
func (r *rig) watch() <-chan ops.WatchLine {
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	lines := make(chan ops.WatchLine, 64)
	go func() {
		_ = link.Hold(ctx, r.link, ops.BoxWatch, struct{}{}, func(_ context.Context, _ struct{}, s link.Stream) {
			sc := bufio.NewScanner(s)
			for sc.Scan() {
				var l ops.WatchLine
				if json.Unmarshal(sc.Bytes(), &l) == nil {
					lines <- l
				}
			}
		})
	}()
	return lines
}

// next waits until every one of want has arrived, in any order, skipping other lines.
func next(t *testing.T, lines <-chan ops.WatchLine, want ...ops.WatchLine) {
	t.Helper()
	missing := map[ops.WatchLine]bool{}
	for _, w := range want {
		missing[w] = true
	}
	deadline := time.After(5 * time.Second)
	for len(missing) > 0 {
		select {
		case l := <-lines:
			delete(missing, l)
		case <-deadline:
			t.Fatalf("never saw %+v", missing)
		}
	}
}

func TestWatchSendsStatusLevelsThenEdges(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	core := r.box(ref(1, 1, 1)).Core
	core.SetStatus("agent-r1", "working", "")
	lines := r.watch()
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchReport, Run: 1, Status: domain.HCWorking})
	core.SetStatus("agent-r1", "blocked", "permission")
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchReport, Run: 1, Status: domain.HCBlocked, Phase: "permission"})
	core.Signal("agent-r1", 15)
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchExit, Run: 1, Code: 143})
	r.sb.Crash(ports.BoxKey{Task: 1, Gen: 1}, true)
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchBox, Gen: 1, Box: domain.BoxOOM, OOM: true})
}

func TestANewWatchGetsLevelsForBoxesAndExitedAgents(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	r.must(plan(ref(2, 1, 1), true))
	r.box(ref(1, 1, 1)).Core.Exit("agent-r1", 0)
	first := r.watch()
	next(t, first, ops.WatchLine{Task: 1, Kind: ops.WatchExit, Run: 1, Code: 0})
	if _, err := link.Call(r.ctx(), r.link, ops.BoxStop, ref(2, 1, 1)); err != nil {
		t.Fatal(err)
	}
	next(t, first, ops.WatchLine{Task: 2, Kind: ops.WatchBox, Gen: 1, Box: domain.BoxDied})
	next(t, r.watch(),
		ops.WatchLine{Task: 2, Kind: ops.WatchBox, Gen: 1, Box: domain.BoxDied},
		ops.WatchLine{Task: 1, Kind: ops.WatchExit, Run: 1, Code: 0})
}

func TestWatchFollowsANewRunAfterARestart(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	lines := r.watch()
	if _, err := link.Call(r.ctx(), r.link, ops.BoxStop, ref(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchBox, Gen: 1, Box: domain.BoxDied})
	r.must(plan(ref(1, 2, 1), false))
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchBox, Gen: 1, Box: domain.BoxStarted, Up: true})
	r.box(ref(1, 2, 1)).Core.SetStatus("agent-r2", "idle", "")
	next(t, lines, ops.WatchLine{Task: 1, Kind: ops.WatchReport, Run: 2, Status: domain.HCIdle})
}
