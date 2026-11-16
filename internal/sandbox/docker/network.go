package docker

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ensureNetwork makes the boxes' bridge exist with the MTU of the narrowest route out of the
// host, capped at 1500. A host whose traffic leaves over a tunnel (an MTU of 1280, say) would
// otherwise see large packets from boxes vanish. A network in use keeps its MTU until it is idle.
func (d *Docker) ensureNetwork(ctx context.Context) (int, error) {
	d.net.Lock()
	defer d.net.Unlock()
	want := d.cfg.MTU
	if want == 0 {
		want = probeMTU(ctx, d.cfg.Probes)
	}
	out, err := d.docker(ctx, "network", "inspect", "--format", `{{index .Options "com.docker.network.driver.mtu"}}`, d.cfg.Network)
	if err == nil {
		cur := parseMTU(string(out))
		if cur == want {
			return cur, nil
		}
		if _, err := d.docker(ctx, "network", "rm", d.cfg.Network); err != nil {
			return cur, nil //nolint:nilerr // boxes still use it: keep its MTU until it is idle
		}
	} else if !notFound(err) {
		return 0, err
	}
	_, err = d.docker(ctx, "network", "create", "--label", d.cfg.Label+".network=1",
		"--opt", "com.docker.network.driver.mtu="+strconv.Itoa(want), d.cfg.Network)
	return want, err
}

func parseMTU(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 1500
	}
	return n
}

var routeDev = regexp.MustCompile(` dev (\S+)`)

func probeMTU(ctx context.Context, hosts []string) int {
	mtu := 1500
	for _, h := range hosts {
		out, err := exec.CommandContext(ctx, "ip", "route", "get", h).Output() //nolint:gosec // hosts come from config
		if err != nil {
			continue
		}
		m := routeDev.FindStringSubmatch(string(out))
		if m == nil {
			continue
		}
		b, err := os.ReadFile("/sys/class/net/" + m[1] + "/mtu")
		if n := parseMTU(string(b)); err == nil && n < mtu {
			mtu = n
		}
	}
	return mtu
}
