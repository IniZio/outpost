//go:build docker

package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

const itDockerfile = `ARG BASE
FROM ${BASE} AS box
RUN --mount=type=bind,from=src,source=lock.txt,target=/tmp/lock.txt cp /tmp/lock.txt /lock
`

const itBake = `variable "SRC" { default = "./src" }
group "default" { targets = ["box"] }
group "inner" { targets = ["inner"] }
target "box" {
  dockerfile = "setup.Dockerfile"
  contexts   = { src = SRC }
  target     = "box"
}
target "inner" {
  dockerfile-inline = "FROM busybox:1.36\nRUN echo inner > /inner"
  tags              = ["it-inner:test"]
  output            = ["type=docker,dest=out/inner.tar"]
}
`

func itRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// itSource is a git repo standing in for GitHub, with one lock file.
func itSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lock.txt"), []byte("pinned 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	itRun(t, "git", "-C", dir, "init", "-q", "-b", "main")
	itRun(t, "git", "-C", dir, "-c", "user.name=it", "-c", "user.email=it@example.com", "add", ".")
	itRun(t, "git", "-C", dir, "-c", "user.name=it", "-c", "user.email=it@example.com", "commit", "-qm", "one")
	return dir
}

func TestDockerSetupBuildMakesTheImageAndInnerTarsAndTidies(t *testing.T) {
	id := os.Getenv("OUTPOST_IT_ID")
	if id == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	prefix, src := "obit-"+id+"b", itSource(t)
	d, err := New(Config{
		Prefix: prefix, Label: "dev.outpost.box.it" + id + "b", Network: prefix, Tools: "unused", Data: t.TempDir(),
		GitURL: func(domain.RepoName) string { return src }, GitAuth: []string{}, CacheMax: "2gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, _ := exec.Command("docker", "image", "ls", "-q", "--filter", "reference="+prefix+"-setup/*").Output()
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.Command("docker", append([]string{"image", "rm", "-f"}, ids...)...).Run()
		}
		_ = exec.Command("docker", "buildx", "rm", prefix+"-setup").Run()
		_ = exec.Command("docker", "network", "rm", prefix).Run()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	spec := ports.SetupSpec{
		Repo: "acme/app", Rev: 1, Base: "busybox:1.36",
		Files: map[string]string{domain.SetupBake: itBake, "setup.Dockerfile": itDockerfile, domain.AfterCloneFile: "echo after\n"},
	}
	var phases []string
	got, err := d.BuildSetup(ctx, spec, func(p, n string) { phases = append(phases, p+n) })
	if err != nil {
		t.Fatalf("build: %v\nphases %v", err, phases)
	}
	if got.Image != prefix+"-setup/acme--app:r1" || !strings.HasPrefix(got.Digest, "sha256:") || got.Inner == "" || len(got.Commit) != 40 {
		t.Fatalf("built %+v", got)
	}
	inImage := itRun(t, "docker", "run", "--rm", got.Image, "sh", "-c", "cat /lock "+domain.AfterClonePath)
	if !strings.Contains(inImage, "pinned 1") || !strings.Contains(inImage, "echo after") {
		t.Fatalf("the image has the lock file's step and the after-clone script: %q", inImage)
	}
	if _, err := os.Stat(filepath.Join(d.innerPath("acme/app", got.Inner), "inner.tar")); err != nil {
		t.Fatalf("inner tar: %v", err)
	}
	checkTidy(ctx, t, d, spec)
}

// checkTidy builds revisions 2 and 3, each keeping the one before, and checks that revision 1's
// image goes while the shared inner set stays.
func checkTidy(ctx context.Context, t *testing.T, d *Docker, spec ports.SetupSpec) {
	t.Helper()
	for rev := 2; rev <= 3; rev++ {
		spec.Rev, spec.Keep = rev, []int{rev - 1}
		got, err := d.BuildSetup(ctx, spec, nil)
		if err != nil {
			t.Fatalf("build r%d: %v", rev, err)
		}
		if _, err := os.Stat(d.innerPath("acme/app", got.Inner)); err != nil {
			t.Fatalf("the shared inner set stays: %v", err)
		}
	}
	tags := itRun(t, "docker", "image", "ls", "--filter", "reference="+d.cfg.Prefix+"-setup/acme--app", "--format", "{{.Tag}}")
	if strings.Contains(tags, "r1") || !strings.Contains(tags, "r2") || !strings.Contains(tags, "r3") {
		t.Fatalf("r1 goes, r2 and r3 stay: %q", tags)
	}
	if _, err := os.Stat(d.refPath("acme/app", 1)); !os.IsNotExist(err) {
		t.Fatalf("r1's ref goes: %v", err)
	}
}
