package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SetupStatus is where a setup revision's build stands.
type SetupStatus string

// The statuses of a setup revision. A new revision has never been built.
const (
	SetupNew      SetupStatus = "new"
	SetupBuilding SetupStatus = "building"
	SetupReady    SetupStatus = "ready"
	SetupFailed   SetupStatus = "failed"
)

// The files a setup revision is made of. SetupBake is the one required file: a BuildKit bake file
// with a target "box", and optionally a group "prepare" built first and a group "inner" whose
// targets write the repo's own Docker images as out/<name>.tar. AfterCloneFile, when present, runs
// in each new box after the clone.
const (
	SetupBake      = "docker-bake.hcl"
	AfterCloneFile = "after-clone.sh"
)

// Paths inside a box started from a setup revision.
const (
	// InnerImagesDir holds the revision's inner image tars, read-only.
	InnerImagesDir = "/opt/outpost-images"
	// ImagesReadyPath is touched once every inner image is loaded into the box's Docker.
	ImagesReadyPath = "/run/box/images-ready"
	// AfterClonePath is where the revision image keeps its after-clone script.
	AfterClonePath = "/etc/outpost/" + AfterCloneFile
	mirrorsDir     = "/opt/outpost-mirrors"
)

// Limits on a revision's files, so one fits a link frame with room to spare.
const (
	MaxSetupFiles = 32
	MaxSetupBytes = 512 << 10
)

// SetupRev is one revision of a repo's declarative setup: the files, and how its build went.
// Machine, Image, Digest, Inner and Commit describe the latest build; Log is its output's tail.
type SetupRev struct {
	Repo      RepoName
	Rev       int
	Files     map[string]string
	Status    SetupStatus
	Machine   MachineID
	Image     string
	Digest    string
	Inner     string
	Commit    string
	Log       string
	CreatedAt time.Time
	CreatedBy Login
	BuiltAt   time.Time
}

// SetupBuild is a ready revision's build on one machine: what a box starts from.
type SetupBuild struct {
	Rev     int
	Image   string
	Inner   string // the inner images' key on Machine, "" for none
	Machine MachineID
}

// Build is the revision's build, for a repo row to point at.
func (r SetupRev) Build() SetupBuild {
	return SetupBuild{Rev: r.Rev, Image: r.Image, Inner: r.Inner, Machine: r.Machine}
}

var setupFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// reservedSetupNames are what the build makes next to the files.
var reservedSetupNames = map[string]bool{"outpost.hcl": true, "src": true, "out": true, "ctx": true}

// ValidSetupFiles checks a revision's files: flat names, the bake file present, and the limits.
func ValidSetupFiles(files map[string]string) error {
	if _, ok := files[SetupBake]; !ok {
		return errors.New("setup: " + SetupBake + " is required")
	}
	if len(files) > MaxSetupFiles {
		return fmt.Errorf("setup: %d files, over %d", len(files), MaxSetupFiles)
	}
	size := 0
	for name, body := range files {
		if !setupFileName.MatchString(name) || reservedSetupNames[name] {
			return fmt.Errorf("setup: bad file name %q", name)
		}
		size += len(body)
	}
	if size > MaxSetupBytes {
		return fmt.Errorf("setup: files are %d bytes, over %d", size, MaxSetupBytes)
	}
	return nil
}

// RepoSlug is a repo's name as one path or image name part: owner--repo.
func RepoSlug(r RepoName) string { return strings.Replace(string(r), "/", "--", 1) }

// MirrorDir is where a repo's machine mirror is mounted in a box, when the machine has one.
func MirrorDir(r RepoName) string { return mirrorsDir + "/" + RepoSlug(r) + ".git" }

// SetupOn is the repo's ready setup build a task of kind k on machine m starts from. A setup task
// always starts fresh, and a build is used only on the machine that holds it.
func (r Repo) SetupOn(k Kind, m MachineID) (SetupBuild, bool) {
	if k == KindSetup || r.Setup.Image == "" || r.Setup.Machine != m {
		return SetupBuild{}, false
	}
	return r.Setup, true
}
