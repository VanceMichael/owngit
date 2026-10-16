package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Repository deletion intents.
//
// Deleting a repository removes its records in one transaction that also
// writes a deletion intent. The intent names the directory that still has to
// be moved aside or deleted, so an interrupted deletion is completed by the
// next start. It is a machine-local metadata row: the schema 14 catalog does
// not change, older schema 14 builds ignore the row, and backups never copy
// it.
const repositoryDeletionKeyPrefix = "repository_deletion/"

const (
	RepositoryDeletionKeepFiles   = "keep_files"
	RepositoryDeletionDeleteFiles = "delete_files"

	// RepositoryDeletionPending means the records are gone and the directory
	// may still be at its repository path.
	RepositoryDeletionPending = "pending"
	// RepositoryDeletionMoved means a delete_files directory was renamed to
	// its temporary name and only its removal remains.
	RepositoryDeletionMoved = "moved"
)

var (
	ErrRepositoryNotFound             = errors.New("repository does not exist")
	ErrRepositoryDeletionImportActive = errors.New("an import is running for the repository")
	ErrRepositoryDeletionCheckActive  = errors.New("a check job is claimed or running for the repository")
	// ErrRepositoryDeletionCheckCleanup reports a check container whose
	// removal is not yet confirmed. Its ownership row is the only record that
	// lets a later start remove the container, so it must not be deleted.
	ErrRepositoryDeletionCheckCleanup = errors.New("a check container of the repository still awaits cleanup")
	ErrRepositoryDeletionPending      = errors.New("an earlier deletion of this repository is unfinished")
)

// RepositoryDeletion is one durable deletion intent. Root is the canonical
// repository root when deletion began. Moved is the slash-separated rename
// target relative to Root, or ".owngit-missing" for record-only deletion.
// Marker is the random token in the deletion's marker file in Root.
type RepositoryDeletion struct {
	RepositoryID string    `json:"-"`
	Mode         string    `json:"mode"`
	Phase        string    `json:"phase"`
	Root         string    `json:"root"`
	Moved        string    `json:"moved"`
	Marker       string    `json:"marker"`
	CreatedAt    time.Time `json:"-"`
	CreatedUnix  int64     `json:"created_at"`
}

func (d RepositoryDeletion) validate() error {
	if !validText(d.RepositoryID, 100) || strings.ContainsAny(d.RepositoryID, `/\`) {
		return errors.New("invalid repository deletion identifier")
	}
	if d.Mode != RepositoryDeletionKeepFiles && d.Mode != RepositoryDeletionDeleteFiles {
		return errors.New("invalid repository deletion mode")
	}
	if d.Phase != RepositoryDeletionPending && (d.Phase != RepositoryDeletionMoved || d.Mode != RepositoryDeletionDeleteFiles) {
		return errors.New("invalid repository deletion phase")
	}
	if d.Root == "" || !filepath.IsAbs(d.Root) || len(d.Root) > 4096 {
		return errors.New("invalid repository deletion root")
	}
	if !validText(d.Moved, 300) || !strings.HasPrefix(d.Moved, ".owngit-") || strings.Contains(d.Moved, "..") || strings.Contains(d.Moved, `\`) {
		return errors.New("invalid repository deletion target")
	}
	if len(d.Marker) != 32 || strings.Trim(d.Marker, "0123456789abcdef") != "" {
		return errors.New("invalid repository deletion marker")
	}
	return nil
}

func repositoryDeletionKey(repositoryID string) string {
	return repositoryDeletionKeyPrefix + repositoryID
}

func encodeRepositoryDeletion(d RepositoryDeletion) (string, error) {
	d.CreatedUnix = d.CreatedAt.Unix()
	if err := d.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(d)
	return string(encoded), err
}

func decodeRepositoryDeletion(key, value string) (RepositoryDeletion, error) {
	var d RepositoryDeletion
	if err := json.Unmarshal([]byte(value), &d); err != nil {
		return RepositoryDeletion{}, fmt.Errorf("decode repository deletion %q: %w", key, err)
	}
	d.RepositoryID = strings.TrimPrefix(key, repositoryDeletionKeyPrefix)
	d.CreatedAt = time.Unix(d.CreatedUnix, 0)
	if err := d.validate(); err != nil {
		return RepositoryDeletion{}, fmt.Errorf("repository deletion %q: %w", key, err)
	}
	return d, nil
}

// repositoryDeletionStatements remove every record keyed by one repository,
// children before parents, so the result does not depend on foreign-key
// cascades. check_job_runtime_ownership is always empty here because
// repositoryDeletionBusy refuses otherwise; the statement keeps the list
// complete. Staging and initial-destination rows go too: a directory they
// described stays in place and import reconciliation reports it as unknown
// instead of removing it.
var repositoryDeletionStatements = []string{
	`DELETE FROM check_raw_log_starts WHERE attempt_id IN (SELECT id FROM check_attempts WHERE repository_id=?)`,
	`DELETE FROM check_raw_logs WHERE attempt_id IN (SELECT id FROM check_attempts WHERE repository_id=?)`,
	`DELETE FROM check_results WHERE attempt_id IN (SELECT id FROM check_attempts WHERE repository_id=?)`,
	`DELETE FROM check_job_runtime_ownership WHERE repository_id=?`,
	`DELETE FROM direct_review_task_contexts WHERE repository_id=?`,
	`DELETE FROM direct_review_requests WHERE repository_id=?`,
	`DELETE FROM direct_review_probes WHERE repository_id=?`,
	`DELETE FROM direct_review_repository_settings WHERE repository_id=?`,
	`DELETE FROM direct_review_credentials WHERE repository_id=?`,
	`DELETE FROM check_jobs WHERE repository_id=?`,
	`DELETE FROM check_attempts WHERE repository_id=?`,
	`DELETE FROM check_cycles WHERE repository_id=?`,
	`DELETE FROM tasks WHERE repository_id=?`,
	`DELETE FROM check_configurations WHERE repository_id=?`,
	`DELETE FROM check_policies WHERE repository_id=?`,
	`DELETE FROM check_runner_credentials WHERE repository_id=?`,
	`DELETE FROM check_observations WHERE repository_id=?`,
	`DELETE FROM helper_credentials WHERE repository_id=?`,
	`DELETE FROM pull_request_auto_merges WHERE repository_id=?`,
	`DELETE FROM pull_request_merge_intents WHERE repository_id=?`,
	`DELETE FROM pull_request_reviews WHERE repository_id=?`,
	`DELETE FROM pull_request_revisions WHERE repository_id=?`,
	`DELETE FROM pull_requests WHERE repository_id=?`,
	`DELETE FROM repository_attempt_counters WHERE repository_id=?`,
	`DELETE FROM import_publication_intents WHERE repository_id=?`,
	`DELETE FROM import_ref_observations WHERE repository_id=?`,
	`DELETE FROM import_runs WHERE repository_id=?`,
	`DELETE FROM import_schedules WHERE repository_id=?`,
	`DELETE FROM import_stagings WHERE repository_id=?`,
	`DELETE FROM import_initial_destinations WHERE repository_id=?`,
	`DELETE FROM import_sources WHERE repository_id=?`,
	`DELETE FROM repository_names WHERE repository_id=?`,
	`DELETE FROM repository_policies WHERE repository_id=?`,
	`DELETE FROM share_links WHERE repository_id=?`,
	`DELETE FROM push_events WHERE repository_id=?`,
	`DELETE FROM repositories WHERE id=?`,
}

// RepositoryDeletionBusy reports why a repository cannot be deleted now, or
// nil. BeginRepositoryDeletion repeats the check inside its transaction.
func (s *Store) RepositoryDeletionBusy(ctx context.Context, repositoryID string) error {
	return repositoryDeletionBusy(ctx, s.db, repositoryID)
}

func repositoryDeletionBusy(ctx context.Context, queryer queryRower, repositoryID string) error {
	var imports, checks int
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_runs WHERE repository_id=? AND status IN (?,?,?,?,?)`,
		repositoryID, ImportRunPreparing, ImportRunFetching, ImportRunIndexing, ImportRunInspecting, ImportRunPublishing).Scan(&imports); err != nil {
		return err
	}
	if imports > 0 {
		return ErrRepositoryDeletionImportActive
	}
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_jobs WHERE repository_id=? AND status IN ('claimed','started')`, repositoryID).Scan(&checks); err != nil {
		return err
	}
	if checks > 0 {
		return ErrRepositoryDeletionCheckActive
	}
	var containers int
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_job_runtime_ownership WHERE repository_id=?`, repositoryID).Scan(&containers); err != nil {
		return err
	}
	if containers > 0 {
		return ErrRepositoryDeletionCheckCleanup
	}
	return nil
}

// BeginRepositoryDeletion is the commit point of a deletion. In one
// transaction it refuses a busy repository, records the intent, and removes
// the repository row and every record keyed by it. Queued check jobs are
// removed with the rest, so nothing can claim them afterwards. The stored
// import credential and workflow secret files are removed after the commit;
// FinishRepositoryDeletion retries those removals.
func (s *Store) BeginRepositoryDeletion(ctx context.Context, deletion RepositoryDeletion) error {
	if deletion.Phase == "" {
		deletion.Phase = RepositoryDeletionPending
	}
	if deletion.Phase != RepositoryDeletionPending {
		return errors.New("a repository deletion begins in the pending phase")
	}
	encoded, err := encodeRepositoryDeletion(deletion)
	if err != nil {
		return err
	}
	release := s.LockImportCredentialAuthority(deletion.RepositoryID)
	defer release()
	releaseSecrets := s.lockWorkflowSecrets(deletion.RepositoryID)
	defer releaseSecrets()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata WHERE key=?`, repositoryDeletionKey(deletion.RepositoryID)).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		return ErrRepositoryDeletionPending
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories WHERE id=?`, deletion.RepositoryID).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return ErrRepositoryNotFound
	}
	if err := repositoryDeletionBusy(ctx, tx, deletion.RepositoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?)`, repositoryDeletionKey(deletion.RepositoryID), encoded); err != nil {
		return err
	}
	for _, statement := range repositoryDeletionStatements {
		if _, err := tx.ExecContext(ctx, statement, deletion.RepositoryID); err != nil {
			return fmt.Errorf("remove repository records: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Best effort: the deletion is committed, and Finish retries a failure.
	_ = s.discardImportCredentialsLocked(deletion.RepositoryID)
	_ = s.removeWorkflowSecretFileLocked(deletion.RepositoryID)
	return nil
}

// discardImportCredentialsLocked removes the stored import credential file.
// The caller holds LockImportCredentialAuthority for the repository.
func (s *Store) discardImportCredentialsLocked(repositoryID string) error {
	s.beginImportCredentialMutationLocked(repositoryID)
	if err := s.removeImportCredentialFile(repositoryID); err != nil {
		return err
	}
	s.completeImportCredentialMutationLocked(repositoryID)
	return nil
}

// RepositoryDeletion reads one unfinished deletion intent.
func (s *Store) RepositoryDeletion(ctx context.Context, repositoryID string) (RepositoryDeletion, bool, error) {
	var value string
	key := repositoryDeletionKey(repositoryID)
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryDeletion{}, false, nil
	}
	if err != nil {
		return RepositoryDeletion{}, false, err
	}
	deletion, err := decodeRepositoryDeletion(key, value)
	return deletion, err == nil, err
}

// RepositoryDeletions lists every unfinished deletion intent.
func (s *Store) RepositoryDeletions(ctx context.Context) ([]RepositoryDeletion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM metadata WHERE substr(key,1,?)=? ORDER BY key`,
		len(repositoryDeletionKeyPrefix), repositoryDeletionKeyPrefix)
	if err != nil {
		return nil, err
	}
	var deletions []RepositoryDeletion
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return nil, err
		}
		deletion, err := decodeRepositoryDeletion(key, value)
		if err != nil {
			rows.Close()
			return nil, err
		}
		deletions = append(deletions, deletion)
	}
	return deletions, closeRows(rows)
}

// MarkRepositoryDeletionMoved records that a delete_files directory is at its
// temporary name, so later steps never look at the repository path again.
func (s *Store) MarkRepositoryDeletionMoved(ctx context.Context, repositoryID string) error {
	deletion, exists, err := s.RepositoryDeletion(ctx, repositoryID)
	if err != nil {
		return err
	}
	if !exists || deletion.Mode != RepositoryDeletionDeleteFiles {
		return errors.New("no pending delete_files deletion to advance")
	}
	if deletion.Phase == RepositoryDeletionMoved {
		return nil
	}
	previous, err := encodeRepositoryDeletion(deletion)
	if err != nil {
		return err
	}
	deletion.Phase = RepositoryDeletionMoved
	encoded, err := encodeRepositoryDeletion(deletion)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE metadata SET value=? WHERE key=? AND value=?`, encoded, repositoryDeletionKey(repositoryID), previous)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return errors.Join(err, errors.New("repository deletion changed concurrently"))
	}
	return nil
}

// FinishRepositoryDeletion removes leftover credential and secret files and
// then the intent. After it returns, the name has no remaining OwnGit state. The
// deletion removed the old source, so a source row that exists now belongs to
// a newer configuration (for example from an older build that ignores the
// intent), and its credential file is left alone.
func (s *Store) FinishRepositoryDeletion(ctx context.Context, repositoryID string) error {
	release := s.LockImportCredentialAuthority(repositoryID)
	defer release()
	releaseSecrets := s.lockWorkflowSecrets(repositoryID)
	defer releaseSecrets()
	var sources int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_sources WHERE repository_id=?`, repositoryID).Scan(&sources); err != nil {
		return err
	}
	if sources == 0 {
		if err := s.discardImportCredentialsLocked(repositoryID); err != nil {
			return fmt.Errorf("remove stored import credentials: %w", err)
		}
	}
	var repositories int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories WHERE id=?`, repositoryID).Scan(&repositories); err != nil {
		return err
	}
	if repositories == 0 {
		if err := s.removeWorkflowSecretFileLocked(repositoryID); err != nil {
			return fmt.Errorf("remove stored workflow secrets: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, repositoryDeletionKey(repositoryID))
	return err
}
