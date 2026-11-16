package machinetest

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/hc"
	"github.com/IniZio/outpost/internal/hc/hctest"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Sandbox = (*Sandbox)(nil)

// Up makes the box of spec.Key.Gen run, as the real provider does.
func (s *Sandbox) Up(_ context.Context, spec ports.BoxSpec, _ ports.Progress) (ports.BoxInfo, error) {
	s.mu.Lock()
	s.record(fmt.Sprintf("up t%d g%d fresh=%v", spec.Key.Task, spec.Key.Gen, spec.Fresh))
	if err := s.prepare(spec); err != nil {
		s.mu.Unlock()
		return ports.BoxInfo{}, err
	}
	b, started := s.start(spec)
	info := b.Info
	s.mu.Unlock()
	if started {
		s.emit(ports.BoxChange{Info: info, Kind: domain.BoxStarted})
	}
	return info, nil
}

// prepare refuses for a newer generation, makes the volumes and removes older generations.
func (s *Sandbox) prepare(spec ports.BoxSpec) error {
	for k := range s.boxes {
		if k.Task == spec.Key.Task && k.Gen > spec.Key.Gen {
			return fmt.Errorf("machinetest: %w", ports.ErrSuperseded)
		}
	}
	if s.volumes[spec.Key.Task] == nil {
		if !spec.Fresh {
			return fmt.Errorf("machinetest: %w", ports.ErrNoVolumes)
		}
		s.volumes[spec.Key.Task] = map[string]string{}
	}
	s.dropOlder(spec.Key)
	return nil
}

func (s *Sandbox) dropOlder(key ports.BoxKey) {
	for k, b := range s.boxes {
		if k.Task == key.Task && k.Gen < key.Gen {
			b.Core.Stop()
			delete(s.boxes, k)
		}
	}
}

// start creates the box unless present and starts it unless running, with a fresh hc core, as
// box-init clears hc's state on every container start.
func (s *Sandbox) start(spec ports.BoxSpec) (*Box, bool) {
	b := s.boxes[spec.Key]
	if b == nil {
		b = &Box{Info: ports.BoxInfo{Key: spec.Key, Repo: spec.Repo}, Image: spec.Image, Env: maps.Clone(spec.Env), disk: map[string]string{}}
		if s.seeded[spec.Image] {
			b.disk["/var/lib/outpost-seed/workspace"] = ""
		}
		s.boxes[spec.Key] = b
	}
	if b.Info.Running {
		return b, false
	}
	b.Info.Running, b.Info.OOMKilled = true, false
	b.Core = hctest.New()
	b.Core.SetExec(func(ctx context.Context, r hc.ExecReq) hc.ExecResult { return s.exec(ctx, b, r) })
	return b, true
}

// Boxes lists every box.
func (s *Sandbox) Boxes(context.Context) ([]ports.BoxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ports.BoxInfo, 0, len(s.boxes))
	for _, b := range s.boxes {
		out = append(out, b.Info)
	}
	return out, nil
}

// Inspect reports the box of k.
func (s *Sandbox) Inspect(_ context.Context, k ports.BoxKey) (ports.BoxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.boxes[k]; b != nil {
		return b.Info, nil
	}
	return ports.BoxInfo{}, errNoBox
}

// Stop stops the box of k unless a newer generation replaced it.
func (s *Sandbox) Stop(_ context.Context, k ports.BoxKey) error {
	s.mu.Lock()
	s.record(fmt.Sprintf("stop t%d g%d", k.Task, k.Gen))
	b := s.boxes[k]
	if b == nil || !b.Info.Running {
		s.mu.Unlock()
		return nil
	}
	b.Info.Running = false
	b.Core.Stop()
	info := b.Info
	s.mu.Unlock()
	s.emit(ports.BoxChange{Info: info, Kind: domain.BoxDied})
	return nil
}

// Release removes every generation of the task's box and its volumes.
func (s *Sandbox) Release(_ context.Context, task domain.TaskID) error {
	s.mu.Lock()
	s.record(fmt.Sprintf("release t%d", task))
	var gone []ports.BoxInfo
	for k, b := range s.boxes {
		if k.Task == task {
			if b.Core != nil {
				b.Core.Stop()
			}
			gone = append(gone, ports.BoxInfo{Key: k})
			delete(s.boxes, k)
		}
	}
	delete(s.volumes, task)
	s.mu.Unlock()
	for _, info := range gone {
		s.emit(ports.BoxChange{Info: info, Kind: domain.BoxDied})
	}
	return nil
}

// Dial connects to the running box's hc core. Only `hc connect` is served.
func (s *Sandbox) Dial(_ context.Context, k ports.BoxKey, argv []string) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[k]
	if b == nil || !b.Info.Running {
		return nil, errNoBox
	}
	if len(argv) != 2 || !strings.HasSuffix(argv[0], "/hc") || argv[1] != "connect" {
		return nil, fmt.Errorf("machinetest: only hc connect is served, not %v", argv)
	}
	return b.Core.Connect(), nil
}

// CopyIn unpacks a tar into the box.
func (s *Sandbox) CopyIn(_ context.Context, k ports.BoxKey, dir string, tar io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(fmt.Sprintf("copyin t%d %s", k.Task, dir))
	b := s.boxes[k]
	if b == nil {
		return errNoBox
	}
	return untar(tar, dir, s.fs(b))
}

// Commit returns a handle named for the box.
func (s *Sandbox) Commit(_ context.Context, k ports.BoxKey) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(fmt.Sprintf("commit t%d g%d", k.Task, k.Gen))
	if s.boxes[k] == nil {
		return "", errNoBox
	}
	return fmt.Sprintf("ready-t%d-g%d", k.Task, k.Gen), nil
}

// Events delivers box changes until ctx ends.
func (s *Sandbox) Events(ctx context.Context, sink func(ports.BoxChange)) error {
	s.mu.Lock()
	s.nsink++
	id := s.nsink
	s.sinks[id] = sink
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	delete(s.sinks, id)
	s.mu.Unlock()
	return nil
}

// exec answers a command the machine runs in box b.
func (s *Sandbox) exec(ctx context.Context, b *Box, r hc.ExecReq) hc.ExecResult {
	argv := r.Argv
	if len(argv) > 0 && strings.HasSuffix(argv[0], "/oom0") {
		argv = argv[1:]
	}
	line := strings.Join(argv, " ")
	s.mu.Lock()
	s.record("exec " + line)
	hold, failed := s.match(line)
	s.mu.Unlock()
	if hold != nil {
		s.entered <- line
		if waitHold(ctx, hold) != nil {
			return execResult(137, "", "killed")
		}
	}
	if failed != "" {
		return execResult(1, "", failed)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return command(s.fs(b), argv)
}

func (s *Sandbox) match(line string) (chan struct{}, string) {
	var hold chan struct{}
	failed := ""
	for p, ch := range s.holds {
		if strings.HasPrefix(line, p) {
			hold = ch
		}
	}
	for p, e := range s.fails {
		if strings.HasPrefix(line, p) {
			failed = e
		}
	}
	return hold, failed
}

func command(v fsView, argv []string) hc.ExecResult {
	if len(argv) == 0 {
		return execResult(127, "", "no command")
	}
	last := argv[len(argv)-1]
	switch argv[0] {
	case "test":
		return exitIf(v.exists(last))
	case "cat":
		data, ok := v.get(last)
		if !ok {
			return execResult(1, "", "no such file")
		}
		return execResult(0, data, "")
	case "rm":
		v.removeAll(last)
	case "touch":
		v.set(last, "")
	case "mv":
		v.move(argv[1], argv[2])
	case "gh":
		v.set(argv[4]+"/.git/HEAD", string(argv[3]))
	}
	return execResult(0, "", "")
}

func exitIf(ok bool) hc.ExecResult {
	if ok {
		return execResult(0, "", "")
	}
	return execResult(1, "", "")
}
