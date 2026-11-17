//go:build docker

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/api/client"
	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/sandbox/docker"
)

// dockerWorld is this test's Docker namespace within one `make it` run. It sits beside
// internal/boxchain's, which runs at the same time with the same task ids, and `make it-sweep`
// removes both.
type dockerWorld struct{ id, prefix, label, network, tools, box string }

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:forbidigo // the test drives Docker
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newDockerWorld(t *testing.T) dockerWorld {
	t.Helper()
	id := os.Getenv("OUTPOST_IT_ID")
	if id == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	w := dockerWorld{
		id: id, prefix: "obit-" + id + "-e2e", label: "dev.outpost.box.it" + id + "e2e", network: "obit-" + id + "-e2e",
		tools: "obit-e2e-tools:" + id, box: "obit-e2e:" + id,
	}
	hcBin, err := exec.LookPath("hc")
	if err != nil {
		t.Skip("hc is not installed")
	}
	dir := t.TempDir()
	files, _ := filepath.Glob("../../images/tools/*")
	for _, f := range append(files, hcBin) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dockerCLI(t, "build", "-q", "--target", "tools", "--label", "dev.outpost.it="+id, "-t", w.tools, dir)
	dockerCLI(t, "build", "-q", "--label", "dev.outpost.it="+id, "-t", w.box, "testdata/box")
	return w
}

func (w dockerWorld) sandbox(t *testing.T) *docker.Docker {
	t.Helper()
	sb, err := docker.New(docker.Config{Prefix: w.prefix, Label: w.label, Network: w.network, Tools: w.tools, Poll: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func (w dockerWorld) running(t *testing.T, task int64) bool {
	return dockerCLI(t, "ps", "-q", "--filter", "label="+w.label+".task="+strconv.FormatInt(task, 10)) != ""
}

// TestDockerEndToEnd runs serve, a paired agent, Postgres and real Docker boxes with a fake
// claude. A task starts, the agent's turn ends with its summary in the timeline and its words on
// the screen, a message reaches it, and a stop takes the box down.
func TestDockerEndToEnd(t *testing.T) {
	w := newDockerWorld(t)
	l := startServer(t, map[string]string{"OUTPOST_BASE_IMAGE": w.box, "OUTPOST_CLAUDE_BIN": "fake-claude"})
	ctx := testCtx(t, 3*time.Minute)
	ana := l.client("ana")
	id, key := pairAgent(ctx, t, l, ana)
	startAgent(t, l, id, key, w.sandbox(t), func(c *machine.Config) {
		c.Clone = func(_ domain.RepoName, dir, _ string) []string {
			return []string{"git", "clone", "-q", "/srv/main.git", dir}
		}
	})
	waitFor(ctx, t, "the machine links", func() bool { return machineOnline(ctx, ana, id) })
	task, err := ana.CreateTask(ctx, client.NewTask{Repo: "o/main", Prompt: "say hello"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dumpTask(t, ana, task.ID) })
	waitFor(ctx, t, "the turn ends", func() bool { return status(ctx, ana, task.ID, "waiting") })
	waitFor(ctx, t, "the turn's summary lands", func() bool {
		got, err := ana.Task(ctx, task.ID)
		return err == nil && got.Summary == "fake claude finished"
	})
	if !timelineHas(ctx, ana, task.ID, "The agent is waiting for you") {
		t.Fatal("the timeline says the agent waits")
	}
	waitFor(ctx, t, "the screen shows the agent", func() bool {
		return screenHas(ctx, ana, task.ID, "fake claude was asked") && screenHas(ctx, ana, task.ID, "say hello")
	})
	if _, err := ana.Message(ctx, task.ID, "and goodbye"); err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, "the message reaches the agent", func() bool { return screenHas(ctx, ana, task.ID, "fake claude got: and goodbye") })
	if _, err := ana.Stop(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(ctx, t, "the task stops", func() bool { return status(ctx, ana, task.ID, "stopped") })
	waitFor(ctx, t, "the box is down", func() bool { return !w.running(t, task.ID) })
}

// dumpTask logs a failed run's task, timeline and screen.
func dumpTask(t *testing.T, c *client.Client, id int64) {
	if !t.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task, err := c.Task(ctx, id)
	t.Logf("task: %+v %v", task, err)
	_, _ = c.Follow(ctx, id, 0, func(e client.Entry) error {
		t.Logf("  %s %s", e.Kind, e.Text)
		return nil
	})
	s, err := c.Screen(context.Background(), id, 50)
	t.Logf("screen: %q %v", s, err)
}

func screenHas(ctx context.Context, c *client.Client, id int64, text string) bool {
	s, err := c.Screen(ctx, id, 200)
	return err == nil && strings.Contains(s, text)
}

func timelineHas(ctx context.Context, c *client.Client, id int64, text string) bool {
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	found := false
	_, _ = c.Follow(fctx, id, 0, func(e client.Entry) error {
		if strings.Contains(e.Text, text) {
			found = true
			cancel()
		}
		return nil
	})
	return found
}
