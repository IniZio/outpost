package main

import (
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/machine/machinetest"
	"github.com/IniZio/outpost/internal/ports"
)

func TestAPairedAgentLinksAndRunsATask(t *testing.T) {
	l := startServer(t, nil)
	ctx := testCtx(t, 60*time.Second)
	ana := l.client("ana")
	id, key := pairAgent(ctx, t, l, ana)
	sb := machinetest.New()
	startAgent(t, l, id, key, sb, nil)
	waitFor(ctx, t, "the machine links", func() bool { return machineOnline(ctx, ana, id) })
	task, err := ana.CreateTask(ctx, client.NewTask{Repo: "o/r", Prompt: "look around"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, "the task starts on the machine", func() bool { return status(ctx, ana, task.ID, "working", "waiting") })
	if _, err := ana.Screen(ctx, task.ID, 20); err != nil {
		t.Fatalf("screen: %v", err)
	}
	box := ports.BoxKey{Task: domain.TaskID(task.ID), Gen: 1}
	sb.Write(box, "/root/.outpost/session.json", `{"transcript_path":"/root/t.jsonl"}`)
	sb.Write(box, "/root/t.jsonl", `{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"all done"}]}}`+"\n")
	if _, err := ana.Stop(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, "the task stops", func() bool { return status(ctx, ana, task.ID, "stopped") })
	waitFor(ctx, t, "the box stops", func() bool { return sb.Count("stop ")+sb.Count("release ") > 0 })
	if err := ana.DeleteMachine(ctx, string(id)); err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, "a revoked machine is kicked off", func() bool { ms, err := ana.Machines(ctx); return err == nil && len(ms) == 0 })
}
