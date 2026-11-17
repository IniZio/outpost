package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ports"
)

var _ ports.Setups = (*Store)(nil)

const setupCols = `repo, rev, files, status, machine, image, digest, inner_key, commit_sha, log, created_at, created_by, built_at`

// PutSetup stores the repo's next revision. The primary key makes a racing put of the same number
// fail rather than overwrite, and the caller sees the error.
func (s *Store) PutSetup(ctx context.Context, r domain.SetupRev) (domain.SetupRev, error) {
	files, err := json.Marshal(r.Files)
	if err != nil {
		return domain.SetupRev{}, fmt.Errorf("postgres: setup files: %w", err)
	}
	r.Status = domain.SetupNew
	err = s.pool.QueryRow(ctx, `INSERT INTO repo_setups (repo, rev, files, status, created_at, created_by)
		SELECT $1, COALESCE(MAX(rev), 0) + 1, $2, $3, $4, $5 FROM repo_setups WHERE repo = $1
		RETURNING rev`, string(r.Repo), files, string(r.Status), r.CreatedAt, string(r.CreatedBy)).Scan(&r.Rev)
	if err != nil {
		return domain.SetupRev{}, fmt.Errorf("postgres: put setup: %w", err)
	}
	return r, nil
}

// Setups lists the repo's revisions, newest first.
func (s *Store) Setups(ctx context.Context, repo domain.RepoName) ([]domain.SetupRev, error) {
	return s.querySetups(ctx, `SELECT `+setupCols+` FROM repo_setups WHERE repo = $1 ORDER BY rev DESC`, string(repo))
}

// Building lists every revision marked building.
func (s *Store) Building(ctx context.Context) ([]domain.SetupRev, error) {
	return s.querySetups(ctx, `SELECT `+setupCols+` FROM repo_setups WHERE status = 'building' ORDER BY repo, rev`)
}

func (s *Store) querySetups(ctx context.Context, q string, args ...any) ([]domain.SetupRev, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: setups: %w", err)
	}
	defer rows.Close()
	var out []domain.SetupRev
	for rows.Next() {
		r, err := scanSetup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, wrap(rows.Err(), "postgres: read setups")
}

// Setup returns one revision.
func (s *Store) Setup(ctx context.Context, repo domain.RepoName, rev int) (domain.SetupRev, error) {
	r, err := scanSetup(s.pool.QueryRow(ctx, `SELECT `+setupCols+` FROM repo_setups WHERE repo = $1 AND rev = $2`, string(repo), rev))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SetupRev{}, ports.ErrNoSetup
	}
	return r, err
}

func scanSetup(row pgx.Row) (domain.SetupRev, error) {
	var (
		r     domain.SetupRev
		files []byte
		built *time.Time
	)
	err := row.Scan(&r.Repo, &r.Rev, &files, &r.Status, &r.Machine, &r.Image, &r.Digest, &r.Inner, &r.Commit,
		&r.Log, &r.CreatedAt, &r.CreatedBy, &built)
	if err != nil {
		return domain.SetupRev{}, err //nolint:wrapcheck // pgx.ErrNoRows is matched by the caller
	}
	if err := json.Unmarshal(files, &r.Files); err != nil {
		return domain.SetupRev{}, fmt.Errorf("postgres: setup files: %w", err)
	}
	if built != nil {
		r.BuiltAt = *built
	}
	return r, nil
}

// StartBuild marks the revision building on m, compare-and-swap on its status.
func (s *Store) StartBuild(ctx context.Context, repo domain.RepoName, rev int, m domain.MachineID, resume bool) (domain.SetupRev, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE repo_setups SET status = 'building', machine = $3
		WHERE repo = $1 AND rev = $2 AND (status <> 'building' OR $4)`, string(repo), rev, string(m), resume)
	if err != nil {
		return domain.SetupRev{}, fmt.Errorf("postgres: start build: %w", err)
	}
	r, err := s.Setup(ctx, repo, rev)
	if err != nil {
		return domain.SetupRev{}, err
	}
	if tag.RowsAffected() == 0 {
		return domain.SetupRev{}, ports.ErrBuilding
	}
	return r, nil
}

// EndBuild records the build's end. A ready revision becomes the repo's current one.
func (s *Store) EndBuild(ctx context.Context, r domain.SetupRev) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE repo_setups SET status=$3, machine=$4, image=$5, digest=$6, inner_key=$7,
			commit_sha=$8, log=$9, built_at=$10 WHERE repo=$1 AND rev=$2`, string(r.Repo), r.Rev, string(r.Status),
			string(r.Machine), r.Image, r.Digest, r.Inner, r.Commit, r.Log, r.BuiltAt)
		if err != nil {
			return fmt.Errorf("postgres: end build: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ports.ErrNoSetup
		}
		if r.Status != domain.SetupReady {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO repos (name, setup_rev) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET setup_rev = $2`, string(r.Repo), r.Rev)
		return wrap(err, "postgres: point the repo at its setup")
	})
}
