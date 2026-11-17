package machine

import (
	"context"
	"path"

	"github.com/IniZio/outpost/internal/domain"
)

// afterCloneMark is on the workspace volume, so the after-clone steps run once per task.
const afterCloneMark = "/workspace/.outpost/after-clone-done"

// loadImages loads the setup build's inner image tars into the box's Docker in the background
// and touches the ready marker when every one is in. A marker on the Docker volume, named for the
// images key, skips the load when the same images are there already, as after a box restart.
const loadImages = `set -u
mark=/var/lib/docker/.outpost-images-$1
if [ -e "$mark" ]; then touch ` + domain.ImagesReadyPath + `; exit 0; fi
setsid sh -c '
  i=0
  until docker info >/dev/null 2>&1; do
    i=$((i + 1)); [ "$i" -lt 300 ] || { echo "dockerd never answered"; exit 1; }; sleep 1
  done
  pids=""
  for f in ` + domain.InnerImagesDir + `/*.tar; do
    [ -e "$f" ] || continue
    docker load -q -i "$f" & pids="$pids $!"
  done
  ok=1
  for p in $pids; do wait "$p" || ok=0; done
  [ "$ok" = 1 ] || { echo "an image failed to load"; exit 1; }
  touch "$1" ` + domain.ImagesReadyPath + `
' sh "$mark" </dev/null >/run/box/images.log 2>&1 &
`

// images starts the background load of the setup build's inner images. It runs on every converge,
// because the ready marker is on the box's tmpfs. It comes before the clone so the load overlaps it.
func (c *convergeRun) images(ctx context.Context) error {
	if c.req.Inner == "" {
		return nil
	}
	c.j.add("Loading the repo's Docker images in the background", "")
	return c.run(ctx, nil, "sh", "-c", loadImages, "sh", c.req.Inner)
}

// afterClone runs the setup revision's after-clone script once per workspace, in the main repo's
// checkout, with the plan's env, so it may reach GitHub through the proxy.
func (c *convergeRun) afterClone(ctx context.Context) error {
	ok, err := c.exists(ctx, domain.AfterClonePath)
	if err != nil || !ok {
		return err
	}
	done, err := c.exists(ctx, afterCloneMark)
	if err != nil || done {
		return err
	}
	c.j.add("Running the repo's after-clone steps", "")
	dir := domain.RepoDir(c.req.Repos[0])
	if err := c.run(ctx, c.req.Env, "bash", "-c", `cd "$1" && exec bash -e "$2"`, "after-clone", dir, domain.AfterClonePath); err != nil {
		return err
	}
	if err := c.run(ctx, nil, "mkdir", "-p", path.Dir(afterCloneMark)); err != nil {
		return err
	}
	return c.run(ctx, nil, "touch", afterCloneMark)
}
