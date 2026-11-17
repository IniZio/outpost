package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

// storeInner moves a build's inner image tars from out into the machine's store under their
// content key, and records the key for the revision. Builds with the same tars share one set. No
// tars is the empty key.
func (d *Docker) storeInner(repo domain.RepoName, rev int, out string) (string, error) {
	tars, _ := filepath.Glob(filepath.Join(out, "*.tar"))
	if len(tars) == 0 {
		return "", nil
	}
	key, err := tarsKey(tars)
	if err != nil {
		return "", err
	}
	dst := d.innerPath(repo, key)
	if err := os.MkdirAll(d.refsPath(repo), 0o755); err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	if isDir(dst) {
		_ = os.RemoveAll(out)
	} else if err := os.Rename(out, dst); err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	if err := os.WriteFile(d.refPath(repo, rev), []byte(key), 0o644); err != nil { //nolint:gosec // a key, not a secret
		return "", fmt.Errorf("docker: %w", err)
	}
	return key, nil
}

func (d *Docker) refPath(repo domain.RepoName, rev int) string {
	return filepath.Join(d.refsPath(repo), "r"+strconv.Itoa(rev))
}

// tarsKey is the sha256 of each tar's name and content, in name order: 32 hex characters.
func tarsKey(tars []string) (string, error) {
	slices.Sort(tars)
	all := sha256.New()
	for _, p := range tars {
		f, err := os.Open(p) //nolint:gosec // a path this package made
		if err != nil {
			return "", fmt.Errorf("docker: %w", err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", fmt.Errorf("docker: %w", err)
		}
		_, _ = fmt.Fprintf(all, "%s %x\n", filepath.Base(p), h.Sum(nil))
	}
	return hex.EncodeToString(all.Sum(nil))[:32], nil
}

// tidy keeps the machine's disk bounded after a ready build: the revisions in s.Keep and s.Rev keep
// their images and inner sets, and older ones go. An image a box still uses stays, because Docker
// refuses to remove it, and so does its inner set; a set a box mounts stays too. Last, the builder's
// cache is pruned to Config.CacheMax.
func (d *Docker) tidy(ctx context.Context, s ports.SetupSpec) error {
	keep := append(slices.Clone(s.Keep), s.Rev)
	repoImage := fmt.Sprintf("%s-setup/%s", d.cfg.Prefix, domain.RepoSlug(s.Repo))
	out, err := d.docker(ctx, "image", "ls", "--filter", "reference="+repoImage, "--format", "{{.Tag}}")
	if err != nil {
		return err
	}
	tags := map[int]bool{}
	for _, tag := range strings.Fields(string(out)) {
		rev, ok := revOf(tag)
		if !ok {
			continue
		}
		tags[rev] = true
		if !slices.Contains(keep, rev) {
			if _, err := d.docker(ctx, "image", "rm", repoImage+":"+tag); err == nil {
				tags[rev] = false
			}
		}
	}
	if err := d.dropInner(ctx, s.Repo, keep, tags); err != nil {
		return err
	}
	_, err = d.docker(ctx, "buildx", "prune", "--builder", d.cfg.Builder, "--force", "--max-used-space", d.cfg.CacheMax)
	return err
}

func revOf(tag string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(tag, "r"))
	return n, err == nil && strings.HasPrefix(tag, "r") && n > 0
}

// dropInner forgets the refs of revisions whose image is gone and that are not kept, then removes
// every inner set no ref names and no box mounts.
func (d *Docker) dropInner(ctx context.Context, repo domain.RepoName, keep []int, tagged map[int]bool) error {
	named, err := d.liveRefs(repo, keep, tagged)
	if err != nil {
		return err
	}
	sets, err := os.ReadDir(d.innerRoot(repo))
	if err != nil {
		return missingOK(err)
	}
	for _, e := range sets {
		if e.Name() == "refs" || named[e.Name()] || d.mounted(ctx, e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.innerRoot(repo), e.Name())); err != nil {
			return fmt.Errorf("docker: %w", err)
		}
	}
	return nil
}

// liveRefs removes the refs of revisions not kept whose image is gone and returns the keys the
// rest name.
func (d *Docker) liveRefs(repo domain.RepoName, keep []int, tagged map[int]bool) (map[string]bool, error) {
	refs, err := os.ReadDir(d.refsPath(repo))
	if err != nil {
		return nil, missingOK(err)
	}
	named := map[string]bool{}
	for _, e := range refs {
		p := filepath.Join(d.refsPath(repo), e.Name())
		rev, ok := revOf(e.Name())
		key, rerr := os.ReadFile(p) //nolint:gosec // a path this package made
		switch {
		case !ok || rerr != nil:
		case !slices.Contains(keep, rev) && !tagged[rev]:
			_ = os.Remove(p)
		default:
			named[string(key)] = true
		}
	}
	return named, nil
}

// mounted reports whether any box, running or not, mounts the inner set key. A failed ask counts
// as mounted, so a set is never removed on a guess.
func (d *Docker) mounted(ctx context.Context, key string) bool {
	out, err := d.docker(ctx, "ps", "-aq", "--filter", "label="+d.cfg.Label+".inner="+key)
	return err != nil || strings.TrimSpace(string(out)) != ""
}
