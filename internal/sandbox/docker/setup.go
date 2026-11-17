package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// The machine's setup dirs under Config.Data: a bare mirror per repo, the inner image tars of each
// build by key, and the work dirs of builds in flight.
//
//	mirrors/<owner>--<repo>.git
//	inner/<owner>--<repo>/<key>/*.tar
//	inner/<owner>--<repo>/refs/r<rev>   holds the key of revision <rev>
//	builds/<owner>--<repo>-r<rev>/
func (d *Docker) mirrorPath(r domain.RepoName) string {
	return filepath.Join(d.cfg.Data, "mirrors", domain.RepoSlug(r)+".git")
}

func (d *Docker) innerRoot(r domain.RepoName) string {
	return filepath.Join(d.cfg.Data, "inner", domain.RepoSlug(r))
}

func (d *Docker) innerPath(r domain.RepoName, key string) string {
	return filepath.Join(d.innerRoot(r), key)
}

func (d *Docker) refsPath(r domain.RepoName) string { return filepath.Join(d.innerRoot(r), "refs") }

// setupMounts are the read-only binds of a box: each repo's mirror the machine has, and the setup
// build's inner images. A missing inner images dir is ErrNoImage, as a missing template is.
func (d *Docker) setupMounts(s ports.BoxSpec) ([]string, error) {
	if d.cfg.Data == "" {
		if s.Inner != "" {
			return nil, fmt.Errorf("docker: inner images %s without a data dir: %w", s.Inner, ports.ErrNoImage)
		}
		return nil, nil
	}
	var out []string
	bind := func(src, dst string) {
		out = append(out, "--mount", "type=bind,src="+src+",dst="+dst+",readonly")
	}
	for _, r := range s.Repos {
		if p := d.mirrorPath(r); isDir(p) {
			bind(p, domain.MirrorDir(r))
		}
	}
	if s.Inner == "" {
		return out, nil
	}
	p := d.innerPath(s.Repo, s.Inner)
	if !isDir(p) {
		return nil, fmt.Errorf("docker: inner images %s: %w", p, ports.ErrNoImage)
	}
	bind(p, domain.InnerImagesDir)
	return append(out, "--label", d.cfg.Label+".inner="+s.Inner), nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func missingOK(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
