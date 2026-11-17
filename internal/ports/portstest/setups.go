package portstest

import (
	"context"
	"maps"
	"slices"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Setups = (*Store)(nil)

// PutSetup stores the repo's next revision.
func (s *Store) PutSetup(_ context.Context, r domain.SetupRev) (domain.SetupRev, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setups == nil {
		s.setups = map[domain.RepoName][]domain.SetupRev{}
	}
	revs := s.setups[r.Repo]
	r.Rev, r.Status, r.Files = len(revs)+1, domain.SetupNew, maps.Clone(r.Files)
	s.setups[r.Repo] = append(revs, r)
	return r, nil
}

// Setups lists the repo's revisions, newest first.
func (s *Store) Setups(_ context.Context, repo domain.RepoName) ([]domain.SetupRev, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.setups[repo])
	slices.Reverse(out)
	return out, nil
}

// Setup returns one revision.
func (s *Store) Setup(_ context.Context, repo domain.RepoName, rev int) (domain.SetupRev, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupLocked(repo, rev)
}

func (s *Store) setupLocked(repo domain.RepoName, rev int) (domain.SetupRev, error) {
	revs := s.setups[repo]
	if rev < 1 || rev > len(revs) {
		return domain.SetupRev{}, ports.ErrNoSetup
	}
	return revs[rev-1], nil
}

// StartBuild marks the revision building on m.
func (s *Store) StartBuild(_ context.Context, repo domain.RepoName, rev int, m domain.MachineID, resume bool) (domain.SetupRev, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.setupLocked(repo, rev)
	if err != nil {
		return domain.SetupRev{}, err
	}
	if r.Status == domain.SetupBuilding && !resume {
		return domain.SetupRev{}, ports.ErrBuilding
	}
	r.Status, r.Machine = domain.SetupBuilding, m
	s.setups[repo][rev-1] = r
	return r, nil
}

// Building lists every revision marked building.
func (s *Store) Building(context.Context) ([]domain.SetupRev, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.SetupRev
	for _, name := range slices.Sorted(maps.Keys(s.setups)) {
		for _, r := range s.setups[name] {
			if r.Status == domain.SetupBuilding {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// EndBuild records the build's end, and points the repo at a ready revision.
func (s *Store) EndBuild(_ context.Context, r domain.SetupRev) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.setupLocked(r.Repo, r.Rev)
	if err != nil {
		return err
	}
	cur.Status, cur.Machine, cur.Image, cur.Digest, cur.Inner = r.Status, r.Machine, r.Image, r.Digest, r.Inner
	cur.Commit, cur.Log, cur.BuiltAt = r.Commit, r.Log, r.BuiltAt
	s.setups[r.Repo][r.Rev-1] = cur
	if cur.Status == domain.SetupReady {
		repo := s.repos[r.Repo]
		repo.Name, repo.Setup = r.Repo, cur.Build()
		s.repos[r.Repo] = repo
	}
	return nil
}
