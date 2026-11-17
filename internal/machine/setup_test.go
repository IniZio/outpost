package machine_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/link"
	"github.com/IniZio/outpost/internal/machine"
	"github.com/IniZio/outpost/internal/ops"
	"github.com/IniZio/outpost/internal/ports"
)

func TestConvergeFromASetupBuildLoadsImagesAndRunsAfterCloneOnce(t *testing.T) {
	r := newRig(t, nil)
	r.sb.ImageFile("obd-setup/o--main:r1", domain.AfterClonePath)
	r.sb.ImageFile("obd-setup/o--main:r1", domain.MirrorDir("o/main"))
	b := ref(1, 1, 1)
	req := plan(b, true)
	req.Image, req.Inner = "obd-setup/o--main:r1", "0123456789abcdef"
	phases, _, err := r.converge(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Preparing the box", "Loading the repo's Docker images in the background", "Cloning o/main", "Cloning o/also",
		"Running the repo's after-clone steps", "Starting the agent",
	}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases %v, want %v", phases, want)
	}
	spec := r.box(b).Spec
	if spec.Inner != req.Inner || !slices.Equal(spec.Repos, req.Repos) {
		t.Fatalf("the box gets the images key and the repos: %+v", spec)
	}
	calls := strings.Join(r.sb.Calls(), "\n")
	for _, s := range []string{
		"gh repo clone o/main /workspace/main.partial -- --reference " + domain.MirrorDir("o/main"),
		"gh repo clone o/also /workspace/also.partial -- --filter=blob:none",
		"sh " + req.Inner, domain.AfterClonePath,
	} {
		if !strings.Contains(calls, s) {
			t.Errorf("no call with %q:\n%s", s, calls)
		}
	}
	r.must(plan(ref(1, 2, 1), false))
	if n := r.sb.Count("exec bash -c"); n != 1 {
		t.Fatalf("after-clone ran %d times, want once per workspace", n)
	}
}

func TestConvergeWithoutASetupBuildSkipsItsSteps(t *testing.T) {
	r := newRig(t, nil)
	r.must(plan(ref(1, 1, 1), true))
	if r.sb.Count("exec sh -c") != 0 || r.sb.Count("exec bash -c") != 0 {
		t.Fatalf("no image load and no after-clone: %v", r.sb.Calls())
	}
}

// builder is a fake ports.SetupBuilder that holds each build until let.
type builder struct {
	mu    sync.Mutex
	specs []ports.SetupSpec
	let   chan struct{}
	err   error
}

func (b *builder) BuildSetup(ctx context.Context, s ports.SetupSpec, report ports.Progress) (ports.SetupBuilt, error) {
	b.mu.Lock()
	b.specs = append(b.specs, s)
	b.mu.Unlock()
	report("Building the box image", "")
	select {
	case <-b.let:
	case <-ctx.Done():
		return ports.SetupBuilt{}, fmt.Errorf("builder: %w", ctx.Err())
	}
	if b.err != nil {
		return ports.SetupBuilt{}, b.err
	}
	return ports.SetupBuilt{Image: fmt.Sprintf("obd-setup/%s:r%d", domain.RepoSlug(s.Repo), s.Rev), Digest: "sha256:1", Commit: "abc"}, nil
}

func buildReq(rev int) ops.SetupBuildReq {
	return ops.SetupBuildReq{
		Repo: "o/main", Rev: rev, Files: map[string]string{domain.SetupBake: "x"}, Base: "ob-base:1",
		Keep: []int{1}, Budget: time.Minute,
	}
}

func (r *rig) build(req ops.SetupBuildReq) ([]string, ops.SetupBuilt, error) {
	_, st, err := link.Open(r.ctx(), r.link, ops.SetupBuild, req)
	if err != nil {
		return nil, ops.SetupBuilt{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = st.Close() }()
	var phases []string
	sc := bufio.NewScanner(st)
	for sc.Scan() {
		var l ops.SetupLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return phases, ops.SetupBuilt{}, fmt.Errorf("line: %w", err)
		}
		switch {
		case l.Phase != nil:
			phases = append(phases, l.Phase.Phase)
		case l.Done != nil:
			return phases, *l.Done, nil
		case l.Err != nil:
			return phases, ops.SetupBuilt{}, l.Err
		}
	}
	return phases, ops.SetupBuilt{}, errors.New("stream cut")
}

func TestASetupBuildIsJoinedByARepeatCall(t *testing.T) {
	b := &builder{let: make(chan struct{})}
	r := newRig(t, func(c *machine.Config) { c.Builder = b })
	type result struct {
		got ops.SetupBuilt
		err error
	}
	out := make(chan result, 2)
	for range 2 {
		go func() {
			_, got, err := r.build(buildReq(2))
			out <- result{got, err}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(b.let)
	for range 2 {
		res := <-out
		if res.err != nil || res.got.Image != "obd-setup/o--main:r2" {
			t.Fatalf("%+v", res)
		}
	}
	if len(b.specs) != 1 || !slices.Equal(b.specs[0].Keep, []int{1}) || b.specs[0].Base != "ob-base:1" {
		t.Fatalf("one build with the request's spec: %+v", b.specs)
	}
}

func TestASetupBuildFailureReachesTheCaller(t *testing.T) {
	b := &builder{let: make(chan struct{}), err: errors.New("step 3/7 failed: pip")}
	close(b.let)
	r := newRig(t, func(c *machine.Config) { c.Builder = b })
	phases, _, err := r.build(buildReq(1))
	if err == nil || !strings.Contains(err.Error(), "step 3/7 failed") {
		t.Fatalf("the builder's message: %v", err)
	}
	if !slices.Contains(phases, "Building the box image") {
		t.Fatalf("phases %v", phases)
	}
}

func TestAMachineWithoutABuilderRefusesSetupBuilds(t *testing.T) {
	r := newRig(t, nil)
	_, _, err := r.build(buildReq(1))
	if !errors.Is(err, &link.Error{Code: link.CodeRefused}) {
		t.Fatalf("refused: %v", err)
	}
}
