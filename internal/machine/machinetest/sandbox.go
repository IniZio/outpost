// Package machinetest is a fake ports.Sandbox for tests of the machine and of the chain above it.
// Each running box has its own fake hc core and a small file system that answers the commands the
// machine runs: test, cat, rm, touch, mv, gh repo clone, box-seed and box-save. Volume paths keep
// their files across a stop, a start and a new generation; the rest of the disk is per box.
package machinetest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/hc/hctest"
	"github.com/IniZio/outpost/internal/ports"
)

// Box is one fake box generation.
type Box struct {
	Info  ports.BoxInfo
	Image string
	Env   map[string]string
	Core  *hctest.Core
	disk  map[string]string
}

// Sandbox is the fake provider.
type Sandbox struct {
	mu      sync.Mutex
	boxes   map[ports.BoxKey]*Box
	volumes map[domain.TaskID]map[string]string
	seeded  map[string]bool
	calls   []string
	sinks   map[int]func(ports.BoxChange)
	nsink   int
	holds   map[string]chan struct{}
	fails   map[string]string
	entered chan string
}

// New returns a sandbox with no boxes.
func New() *Sandbox {
	return &Sandbox{
		boxes: map[ports.BoxKey]*Box{}, volumes: map[domain.TaskID]map[string]string{}, seeded: map[string]bool{},
		sinks: map[int]func(ports.BoxChange){}, holds: map[string]chan struct{}{}, fails: map[string]string{},
		entered: make(chan string, 64),
	}
}

var volumePaths = []string{"/workspace", "/root", "/run/outpost", "/var/lib/docker"}

func onVolume(p string) bool {
	for _, v := range volumePaths {
		if p == v || strings.HasPrefix(p, v+"/") {
			return true
		}
	}
	return false
}

// Seeded makes boxes of image carry a template's seed directory, as a saved template does.
func (s *Sandbox) Seeded(image string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seeded[image] = true
}

// Hold makes every exec whose command line starts with prefix block until Release(prefix) or
// until the exec's connection goes away. Entered reports each one that blocks.
func (s *Sandbox) Hold(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds[prefix] = make(chan struct{})
}

// Let ends a Hold.
func (s *Sandbox) Let(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := s.holds[prefix]; ch != nil {
		close(ch)
		delete(s.holds, prefix)
	}
}

// Entered delivers the command line of each exec that blocks at a Hold.
func (s *Sandbox) Entered() <-chan string { return s.entered }

// Fail makes every exec whose command line starts with prefix exit 1 with stderr.
func (s *Sandbox) Fail(prefix, stderr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[prefix] = stderr
}

// Calls returns every provider call and exec so far, in order.
func (s *Sandbox) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Count returns how many calls start with prefix.
func (s *Sandbox) Count(prefix string) int {
	n := 0
	for _, c := range s.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Box returns the box of k, or nil.
func (s *Sandbox) Box(k ports.BoxKey) *Box {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boxes[k]
}

// File returns a file of the box of k, and whether it exists.
func (s *Sandbox) File(k ports.BoxKey, p string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[k]
	if b == nil {
		return "", false
	}
	v, ok := s.fs(b).get(p)
	return v, ok
}

// Write puts a file into the box of k.
func (s *Sandbox) Write(k ports.BoxKey, p, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.boxes[k]; b != nil {
		s.fs(b).set(p, data)
	}
}

// Crash stops the box of k on its own, as an OOM kill or a host restart does.
func (s *Sandbox) Crash(k ports.BoxKey, oom bool) {
	s.mu.Lock()
	b := s.boxes[k]
	if b == nil || !b.Info.Running {
		s.mu.Unlock()
		return
	}
	b.Info.Running, b.Info.OOMKilled = false, oom
	b.Core.Stop()
	info := b.Info
	s.mu.Unlock()
	kind := domain.BoxDied
	if oom {
		kind = domain.BoxOOM
	}
	s.emit(ports.BoxChange{Info: info, Kind: kind})
}

func (s *Sandbox) record(c string) {
	s.calls = append(s.calls, c)
}

func (s *Sandbox) emit(c ports.BoxChange) {
	s.mu.Lock()
	sinks := slices.Collect(maps.Values(s.sinks))
	s.mu.Unlock()
	for _, sink := range sinks {
		sink(c)
	}
}

// fs is the file system a path of b resolves in: the task's volumes or the box's own disk.
func (s *Sandbox) fs(b *Box) fsView { return fsView{vol: s.volumes[b.Info.Key.Task], disk: b.disk} }

type fsView struct{ vol, disk map[string]string }

func (v fsView) pick(p string) map[string]string {
	if onVolume(p) {
		return v.vol
	}
	return v.disk
}

func (v fsView) get(p string) (string, bool) {
	val, ok := v.pick(p)[p]
	return val, ok
}

func (v fsView) set(p, data string) { v.pick(p)[p] = data }

func (v fsView) removeAll(p string) {
	for _, m := range []map[string]string{v.vol, v.disk} {
		for k := range m {
			if k == p || strings.HasPrefix(k, p+"/") {
				delete(m, k)
			}
		}
	}
}

func (v fsView) exists(p string) bool {
	for _, m := range []map[string]string{v.vol, v.disk} {
		for k := range m {
			if k == p || strings.HasPrefix(k, p+"/") {
				return true
			}
		}
	}
	return false
}

func (v fsView) move(from, to string) {
	for _, m := range []map[string]string{v.vol, v.disk} {
		for k, val := range m {
			if k == from || strings.HasPrefix(k, from+"/") {
				delete(m, k)
				v.set(to+strings.TrimPrefix(k, from), val)
			}
		}
	}
}

var errNoBox = fmt.Errorf("machinetest: %w", ports.ErrNoBox)

func untar(r io.Reader, dir string, v fsView) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("machinetest: untar: %w", err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("machinetest: untar: %w", err)
		}
		v.set(dir+"/"+h.Name, string(b))
	}
}

func execResult(code int, stdout, stderr string) hc.ExecResult {
	r := hctest.Exit(code, stdout)
	r.Stderr = []byte(stderr)
	return r
}

// ctxDone is a helper for blocking on a hold or the end of a test.
func waitHold(ctx context.Context, ch chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("machinetest: %w", ctx.Err())
	}
}
