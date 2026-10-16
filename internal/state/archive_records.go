package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RepositoryArchived reports whether repository id is archived. A repository
// without a policy row is not archived.
func (s *Store) RepositoryArchived(ctx context.Context, id string) (bool, error) {
	var archived int64
	err := s.db.QueryRowContext(ctx, `SELECT archived FROM repository_policies WHERE repository_id=?`, id).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return archived == 1, nil
}

// ArchivedRepositoryIDs lists every archived repository. The repository
// manager loads it once at startup and treats it as the authority until an
// archive or unarchive changes it.
func (s *Store) ArchivedRepositoryIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id FROM repository_policies WHERE archived=1 ORDER BY repository_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, closeRows(rows)
}

// SetRepositoryArchived records the archive state of repository id. It writes
// only its own column and updated_at, so the repository's kept-history,
// default-branch protection and extra ref namespace choices are never
// changed. Other callers create a policy row through SaveRepositoryRefPolicy.
func (s *Store) SetRepositoryArchived(ctx context.Context, id string, archived bool, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repository_policies(repository_id,archived,updated_at) VALUES(?,?,?)
		ON CONFLICT(repository_id) DO UPDATE SET archived=excluded.archived,updated_at=excluded.updated_at`,
		id, boolInt(archived), at.Unix())
	return err
}
