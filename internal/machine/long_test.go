package machine_test

import (
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/ops"
)

// hc's machine.exec cuts a command at ten minutes. A clone, a seed or a save of a big repo runs
// longer, so those steps must not go through it.
func TestStepsLongerThanHCsExecLimitRunToTheirEnd(t *testing.T) {
	clock := domain.NewManualClock(time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC))
	r := newRig(t, func(c *machine.Config) { c.Clock = clock })
	r.sb.LimitHC(clock, 10*time.Minute)
	b := ref(1, 1, 1)
	req := plan(b, true)
	req.Budget = 2 * time.Hour
	r.sb.Hold("gh repo clone o/main")
	done := make(chan error, 1)
	go func() { _, _, err := r.converge(req); done <- err }()
	r.waitEntered("gh repo clone o/main /workspace/main.partial -- --filter=blob:none")
	clock.Advance(40 * time.Minute)
	r.sb.Let("gh repo clone o/main")
	if err := <-done; err != nil {
		t.Fatalf("a 40-minute clone: %v", err)
	}
	r.sb.Hold("/opt/outpost/bin/box-save prepare")
	saved := make(chan error, 1)
	go func() {
		_, err := link.Call(r.ctx(), r.link, ops.BoxSave, ops.SaveReq{Box: b, Budget: 2 * time.Hour})
		saved <- err
	}()
	r.waitEntered("/opt/outpost/bin/box-save prepare")
	clock.Advance(40 * time.Minute)
	r.sb.Let("/opt/outpost/bin/box-save prepare")
	if err := <-saved; err != nil {
		t.Fatalf("a 40-minute save: %v", err)
	}
}
