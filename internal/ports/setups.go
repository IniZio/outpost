package ports

import (
	"context"
	"errors"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

var (
	// ErrNoSetup is an unknown setup revision.
	ErrNoSetup = errors.New("store: no such setup revision")
	// ErrBuilding is a build asked of a revision that is building already.
	ErrBuilding = errors.New("store: that setup revision is building already")
)

// Setups stores repos' setup revisions. The repo row points at its current ready revision, which
// Store.Repo returns as Repo.Setup.
type Setups interface {
	// PutSetup stores r.Files as the repo's next revision, status new, and returns it.
	PutSetup(ctx context.Context, r domain.SetupRev) (domain.SetupRev, error)

	// Setups lists the repo's revisions, newest first.
	Setups(ctx context.Context, repo domain.RepoName) ([]domain.SetupRev, error)

	// Setup returns one revision, or ErrNoSetup.
	Setup(ctx context.Context, repo domain.RepoName, rev int) (domain.SetupRev, error)

	// StartBuild marks the revision building on machine m. A revision building already is
	// ErrBuilding, unless resume is set, which a server restart uses to pick its builds up again.
	StartBuild(ctx context.Context, repo domain.RepoName, rev int, m domain.MachineID, resume bool) (domain.SetupRev, error)

	// Building lists every revision marked building, for a restart to resume.
	Building(ctx context.Context) ([]domain.SetupRev, error)

	// EndBuild records how the build ended: status, image, digest, inner key, commit, log and
	// BuiltAt. A ready revision becomes the repo's current one in the same write.
	EndBuild(ctx context.Context, r domain.SetupRev) error
}

// SetupSpec is one build of a setup revision on a machine. Base is the box image the revision
// builds on, Profile the agent profile commands baked into the image's home, and Keep the other
// revisions whose builds the machine keeps when this one is ready.
type SetupSpec struct {
	Repo    domain.RepoName
	Rev     int
	Commit  string // "" builds the default branch's head
	Files   map[string]string
	Base    string
	Profile [][]string
	Keep    []int
}

// SetupBuilt is a finished build: the image and its digest, the inner images' key ("" for none),
// the commit built and the tail of the build log.
type SetupBuilt struct {
	Image  string
	Digest string
	Inner  string
	Commit string
	Log    string
}

// SetupBuilder builds setup revisions on a machine: it refreshes the repo's mirror, runs the
// build, stores the inner image tars and keeps the machine's disk bounded. A failed build's error
// carries the log's tail. internal/sandbox/docker implements it with BuildKit.
type SetupBuilder interface {
	BuildSetup(ctx context.Context, s SetupSpec, report Progress) (SetupBuilt, error)
}

// Builds runs a setup build on machine m from the server. internal/remote implements it over the
// link, rejoining the machine's build after a cut.
type Builds interface {
	BuildSetup(ctx context.Context, m domain.MachineID, s SetupSpec, budget time.Duration, report Progress) (SetupBuilt, error)
}
