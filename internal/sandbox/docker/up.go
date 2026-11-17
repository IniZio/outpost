package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/IniZio/outpost/internal/ports"
)

// ReadyMarker is the file box-init touches once hc runs. It is on a tmpfs, so it is never stale.
const ReadyMarker = "/run/box/ready"

// Up makes the box of spec.Key.Gen run, checking each part before it acts.
func (d *Docker) Up(ctx context.Context, spec ports.BoxSpec, report ports.Progress) (ports.BoxInfo, error) {
	for _, img := range []string{d.cfg.Tools, spec.Image} {
		if err := d.ensureImage(ctx, img, report); err != nil {
			return ports.BoxInfo{}, err
		}
	}
	mtu, err := d.ensureNetwork(ctx)
	if err != nil {
		return ports.BoxInfo{}, err
	}
	if err := d.ensureVolumes(ctx, spec); err != nil {
		return ports.BoxInfo{}, err
	}
	if err := d.ensureBox(ctx, spec, mtu); err != nil {
		return ports.BoxInfo{}, err
	}
	if err := d.awaitReady(ctx, spec.Key); err != nil {
		return ports.BoxInfo{}, err
	}
	return d.Inspect(ctx, spec.Key)
}

// IsTemplate reports whether image is a template this provider saved or a setup build it made,
// which lives only on the machine that made it and is never pulled.
func (d *Docker) IsTemplate(image string) bool {
	return strings.HasPrefix(image, d.cfg.Prefix+"-ready/") || strings.HasPrefix(image, d.cfg.Prefix+"-setup/")
}

func (d *Docker) ensureImage(ctx context.Context, image string, report ports.Progress) error {
	_, err := d.docker(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	switch {
	case err == nil:
		return nil
	case !notFound(err):
		return err
	case d.IsTemplate(image):
		return fmt.Errorf("docker: %s: %w", image, ports.ErrNoImage)
	}
	if report != nil {
		report("Pulling "+image, "")
	}
	_, err = d.docker(ctx, "pull", "--quiet", image)
	return err
}

func (d *Docker) ensureVolumes(ctx context.Context, spec ports.BoxSpec) error {
	for _, kind := range volumeKinds {
		name := d.volume(spec.Key.Task, kind)
		if !spec.Fresh {
			if _, err := d.docker(ctx, "volume", "inspect", name); notFound(err) {
				return fmt.Errorf("docker: %s: %w", name, ports.ErrNoVolumes)
			} else if err != nil {
				return err
			}
			continue
		}
		label := fmt.Sprintf("%s.task=%d", d.cfg.Label, spec.Key.Task)
		if _, err := d.docker(ctx, "volume", "create", "--label", label, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureBox removes older generations, refuses when a newer one exists, and creates and starts
// this one unless it already runs.
func (d *Docker) ensureBox(ctx context.Context, spec ports.BoxSpec, mtu int) error {
	this, err := d.clearOlder(ctx, spec.Key)
	if err != nil {
		return err
	}
	if this == nil {
		mounts, err := d.setupMounts(spec)
		if err != nil {
			return err
		}
		args := append(append(d.createArgs(spec, mtu), mounts...), spec.Image)
		if _, err := d.docker(ctx, args...); err != nil {
			return err
		}
	} else if this.Running {
		return nil
	}
	_, err = d.docker(ctx, "start", d.name(spec.Key))
	return err
}

// clearOlder removes the task's boxes of older generations and returns this generation's, if any.
func (d *Docker) clearOlder(ctx context.Context, k ports.BoxKey) (*ports.BoxInfo, error) {
	boxes, err := d.taskBoxes(ctx, k.Task)
	if err != nil {
		return nil, err
	}
	var this *ports.BoxInfo
	for _, b := range boxes {
		switch {
		case b.Key.Gen > k.Gen:
			return nil, fmt.Errorf("docker: %s: %w", d.name(b.Key), ports.ErrSuperseded)
		case b.Key.Gen < k.Gen:
			if _, err := d.docker(ctx, "rm", "-f", d.name(b.Key)); err != nil && !notFound(err) {
				return nil, err
			}
		default:
			this = &b
		}
	}
	return this, nil
}

func (d *Docker) createArgs(s ports.BoxSpec, mtu int) []string {
	vol := func(kind, dst string) string {
		return fmt.Sprintf("type=volume,src=%s,dst=%s", d.volume(s.Key.Task, kind), dst)
	}
	a := []string{
		"create", "--name", d.name(s.Key), "--hostname", "box",
		"--label", d.cfg.Label + ".task=" + strconv.FormatInt(int64(s.Key.Task), 10),
		"--label", d.cfg.Label + ".gen=" + strconv.FormatUint(uint64(s.Key.Gen), 10),
		"--label", d.cfg.Label + ".repo=" + string(s.Repo),
		"--network", d.cfg.Network, "--privileged", "--init",
		"--mount", vol("work", "/workspace"), "--mount", vol("home", "/root"),
		"--mount", vol("run", "/run/outpost"), "--mount", vol("docker", "/var/lib/docker"),
		"--mount", "type=image,src=" + d.cfg.Tools + ",dst=/opt/outpost,image-subpath=opt/outpost",
		"--tmpfs", "/run/box",
		"--env", "OUTPOST_MTU=" + strconv.Itoa(mtu),
		"--entrypoint", "/opt/outpost/bin/box-init",
	}
	if d.cfg.Memory != "" {
		a = append(a, "--memory", d.cfg.Memory)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		a = append(a, "--env", k+"="+s.Env[k])
	}
	for _, dev := range d.cfg.Devices {
		a = append(a, "--device", dev)
	}
	return a
}

// awaitReady waits for box-init's ready marker, and fails fast with the box's last log lines if
// the box stops instead.
func (d *Docker) awaitReady(ctx context.Context, k ports.BoxKey) error {
	for {
		if _, err := d.docker(ctx, "exec", d.name(k), "test", "-e", ReadyMarker); err == nil {
			return nil
		}
		c, err := d.inspect(ctx, d.name(k))
		if err != nil {
			return err
		}
		if c.State.Status == "exited" || c.State.Status == "dead" {
			logs, _ := d.cfg.Run.Output(ctx, nil, "logs", "--tail", "30", d.name(k))
			return fmt.Errorf("docker: %s stopped while starting: %s", d.name(k), tail(string(logs), 600))
		}
		if err := d.sleep(ctx); err != nil {
			return err
		}
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
