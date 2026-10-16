package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RecoveryState struct {
	AccessMode              string
	AccessPasswordHash      string
	AdminPasswordHash       string
	Repositories            []Repository
	RepositoryNames         []RepositoryName
	RepositoryPolicies      []RepositoryPolicy
	PullRequests            []PullRequest
	PullRequestRevisions    []PullRequestRevision
	PullRequestReviews      []PullRequestReview
	PullRequestMergeIntents []PullRequestMergeIntent
	PullRequestAutoMerges   []PullRequestAutoMerge
	Tasks                   []RecoveryTask
	CheckPolicies           []CheckPolicy
	CheckJobs               []CheckJob
	ActionsRuns             []ActionsRun
	CheckConfigurations     []CheckConfiguration
	CheckCycles             []RecoveryCheckCycle
	CheckAttempts           []CheckAttempt
	CheckResults            []CheckResultRecord
	ImportSources           []ImportSource
	ImportRuns              []ImportRun
	ImportObservations      []ImportObservation
	ImportIntents           []ImportIntent
	// ImportSchedules and ImportInitialDestinations are machine-local. A
	// portable snapshot leaves them empty. Validation rejects a schedule that
	// does not reference a source and any initial-destination row.
	ImportSchedules           []ImportSchedule
	ImportInitialDestinations []ImportInitialDestination
}

// RecoveryTask contains only the task facts stored in the tasks table.
// Runtime presentation fields are derived from attempts and reservations.
type RecoveryTask struct {
	ID           string
	RepositoryID string
	Title        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RecoveryCheckCycle contains one reservation. Its displayed attempt is
// reconstructed from attempts that refer to the cycle.
type RecoveryCheckCycle struct {
	ID                    string
	TaskID                string
	RepositoryID          string
	Sequence              int64
	ReservedAt            time.Time
	ReservedAfterSequence int64
}

// CheckResultRecord is one portable per-check result row. It carries the
// attempt identifier because results are stored separately from attempts.
type CheckResultRecord struct {
	AttemptID string
	CheckResult
}

// CheckDatabase runs SQLite's integrity and foreign key checks on the whole
// database, as backup verification does on the state it restored.
func (s *Store) CheckDatabase(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("check database integrity: %w", err)
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return fmt.Errorf("check database integrity: %w", err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("check database integrity: %w", err)
	}
	if len(problems) != 0 {
		return fmt.Errorf("the database is damaged: %s", strings.Join(problems, "; "))
	}
	violations, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check database foreign keys: %w", err)
	}
	violated := violations.Next()
	if err := closeRows(violations); err != nil {
		return fmt.Errorf("check database foreign keys: %w", err)
	}
	if violated {
		return errors.New("the database has a record that refers to a missing record")
	}
	return nil
}

// RecoverySnapshot reads all portable state in one database transaction.
// Portable state describes the repositories and their history. Everything
// that grants authority or describes how this computer runs is excluded:
// raw check logs, sessions, setup capabilities, login attempts, trusted
// hosts, repository-root paths, credentials, share links, consents,
// schedules, recent pushes, and every setting kept in metadata other than
// the access mode and the two password hashes.
func (s *Store) RecoverySnapshot(ctx context.Context) (RecoveryState, error) {
	read, err := s.BeginPortableRead(ctx)
	if err != nil {
		return RecoveryState{}, err
	}
	return read.Finish(ctx)
}

// PortableRead is a portable state snapshot in progress: a read-only
// transaction on a connection of its own, so the store's connection stays
// free and writers continue while it is open (write-ahead logging gives the
// reader one unchanging view). Its first read, the repository list, fixes
// the instant the snapshot describes. It never writes.
type PortableRead struct {
	db           *sql.DB
	tx           *sql.Tx
	repositories []Repository
}

// BeginPortableRead opens the snapshot and reads the recorded repositories.
// Finish reads the rest; Close ends a read that is not finished.
func (s *Store) BeginPortableRead(ctx context.Context) (*PortableRead, error) {
	// The same driver and pragmas as the store's own connection, read-only
	// and without an immediate transaction, which would take the write lock.
	path := filepath.Join(s.dir, databaseName)
	if err := s.sameDatabase(path); err != nil {
		return nil, err
	}
	db := sql.OpenDB(dsnConnector{dsn: sqliteURI(path, "mode=ro"), driver: s.db.Driver()})
	db.SetMaxOpenConns(1)
	read := &PortableRead{db: db}
	if err := read.begin(ctx); err != nil {
		return nil, errors.Join(err, read.Close())
	}
	// The connection opened its file by path during its first read. The
	// same file at the path before and after that read is the file the
	// store opened; anything else fails.
	if err := s.sameDatabase(path); err != nil {
		return nil, errors.Join(err, read.Close())
	}
	return read, nil
}

// ErrDatabaseReplaced reports a state database path that no longer names
// the file this store opened: the state folder was moved or replaced
// while OwnGit ran.
var ErrDatabaseReplaced = errors.New("the state database is no longer the file OwnGit opened; the state folder was moved or replaced while OwnGit ran. Restart OwnGit with the intended state folder")

func (s *Store) sameDatabase(path string) error {
	current, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrDatabaseReplaced, err)
		}
		return fmt.Errorf("could not check the state database: %w", err)
	}
	if s.database == nil || !os.SameFile(current, s.database) {
		return ErrDatabaseReplaced
	}
	return nil
}

func (read *PortableRead) begin(ctx context.Context) error {
	var err error
	if read.tx, err = read.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err != nil {
		return err
	}
	rows, err := read.tx.QueryContext(ctx, `SELECT r.id,r.name,r.description,r.created_at,COALESCE(c.attempt_sequence,0)
		FROM repositories r LEFT JOIN repository_attempt_counters c ON c.repository_id=r.id ORDER BY r.id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var repository Repository
		var createdAt int64
		if err := rows.Scan(&repository.ID, &repository.Name, &repository.Description, &createdAt, &repository.AttemptSequence); err != nil {
			rows.Close()
			return err
		}
		repository.CreatedAt = unixTime(createdAt)
		read.repositories = append(read.repositories, repository)
	}
	return closeRows(rows)
}

// Repositories returns the repositories recorded at the snapshot's instant.
func (read *PortableRead) Repositories() []Repository { return read.repositories }

// Close ends the snapshot. It may be called more than once.
func (read *PortableRead) Close() error {
	var err error
	if read.tx != nil {
		err = read.tx.Rollback()
		read.tx = nil
	}
	if read.db != nil {
		err = errors.Join(err, read.db.Close())
		read.db = nil
	}
	return err
}

// Finish reads the rest of the portable state from the same snapshot,
// validates it and ends the snapshot. Live import authority is settled in
// the copy the way a restore settles it (settleImportAuthority), so the
// state itself is never changed and a running import continues.
func (read *PortableRead) Finish(ctx context.Context) (RecoveryState, error) {
	defer read.Close()
	snapshot, err := read.finish(ctx)
	if err != nil {
		return RecoveryState{}, err
	}
	if err := read.Close(); err != nil {
		return RecoveryState{}, err
	}
	return snapshot, nil
}

func (read *PortableRead) finish(ctx context.Context) (RecoveryState, error) {
	tx := read.tx
	if tx == nil {
		return RecoveryState{}, errors.New("the portable state snapshot is closed")
	}
	values := make(map[string]string)
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM metadata WHERE key IN ('initialized','access_mode')`)
	if err != nil {
		return RecoveryState{}, err
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return RecoveryState{}, err
		}
		values[key] = value
	}
	if err := closeRows(rows); err != nil {
		return RecoveryState{}, err
	}
	if values["initialized"] != "true" {
		return RecoveryState{}, errors.New("setup is not complete")
	}

	snapshot := RecoveryState{AccessMode: values["access_mode"], Repositories: read.repositories}
	passwords, err := tx.QueryContext(ctx, `SELECT kind,encoded FROM passwords ORDER BY kind`)
	if err != nil {
		return RecoveryState{}, err
	}
	for passwords.Next() {
		var kind, encoded string
		if err := passwords.Scan(&kind, &encoded); err != nil {
			passwords.Close()
			return RecoveryState{}, err
		}
		switch kind {
		case "access":
			snapshot.AccessPasswordHash = encoded
		case "admin":
			snapshot.AdminPasswordHash = encoded
		}
	}
	if err := closeRows(passwords); err != nil {
		return RecoveryState{}, err
	}
	if snapshot.AdminPasswordHash == "" || (snapshot.AccessMode == "password" && snapshot.AccessPasswordHash == "") {
		return RecoveryState{}, errors.New("portable password state is incomplete")
	}
	if snapshot.AccessMode != "open" && snapshot.AccessMode != "password" {
		return RecoveryState{}, errors.New("portable access mode is invalid")
	}
	if err := readRepositoryRecords(ctx, tx, &snapshot); err != nil {
		return RecoveryState{}, err
	}
	if err := readPullRequestRecovery(ctx, tx, &snapshot); err != nil {
		return RecoveryState{}, err
	}
	if err := readCheckRecovery(ctx, tx, &snapshot); err != nil {
		return RecoveryState{}, err
	}
	if err := refuseDirectReviewRecords(ctx, tx); err != nil {
		return RecoveryState{}, err
	}
	if err := readImportRecovery(ctx, tx, &snapshot); err != nil {
		return RecoveryState{}, err
	}
	settleImportAuthority(&snapshot)
	if err := ValidateRepositoryRecords(snapshot); err != nil {
		return RecoveryState{}, fmt.Errorf("portable repository state is invalid: %w", err)
	}
	if err := ValidatePullRequestRecovery(snapshot); err != nil {
		return RecoveryState{}, fmt.Errorf("portable pull request state is invalid: %w", err)
	}
	if err := ValidateCheckRecovery(snapshot); err != nil {
		return RecoveryState{}, fmt.Errorf("portable check state is invalid: %w", err)
	}
	if err := ValidateImportRecovery(snapshot); err != nil {
		return RecoveryState{}, fmt.Errorf("portable import state is invalid: %w", err)
	}
	return snapshot, nil
}

// dsnConnector opens connections of driver to one database name.
type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.driver }

// ErrDirectReviewRecords refuses a backup of a database that still holds
// records of the removed built-in review. Only unreleased development builds
// wrote them, and a backup never drops records silently.
var ErrDirectReviewRecords = errors.New("state contains direct-review records written by an unreleased development build; this build cannot back them up")

// refuseDirectReviewRecords checks the portable direct-review tables. The
// schema keeps them, but nothing reads or writes their rows any more.
func refuseDirectReviewRecords(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM direct_review_repository_settings)
		OR EXISTS(SELECT 1 FROM direct_review_task_contexts) OR EXISTS(SELECT 1 FROM direct_review_requests)`).Scan(&present); err != nil {
		return fmt.Errorf("inspect direct-review records: %w", err)
	}
	if present {
		return ErrDirectReviewRecords
	}
	return nil
}

// RestoreRecoveryState replaces a new store's portable state in one
// transaction. Every machine-bound or live credential table is cleared.
func (s *Store) RestoreRecoveryState(ctx context.Context, repositoryRoot string, snapshot RecoveryState) error {
	if repositoryRoot == "" {
		return errors.New("repository root is required")
	}
	if snapshot.AccessMode != "open" && snapshot.AccessMode != "password" {
		return errors.New("invalid recovery access mode")
	}
	if snapshot.AdminPasswordHash == "" || (snapshot.AccessMode == "password" && snapshot.AccessPasswordHash == "") {
		return errors.New("recovery password hashes are incomplete")
	}
	if err := ValidateRepositoryRecords(snapshot); err != nil {
		return fmt.Errorf("invalid recovered repository state: %w", err)
	}
	if err := ValidatePullRequestRecovery(snapshot); err != nil {
		return fmt.Errorf("invalid recovered pull request state: %w", err)
	}
	if err := ValidateCheckRecovery(snapshot); err != nil {
		return fmt.Errorf("invalid recovered check state: %w", err)
	}
	if err := ValidateImportRecovery(snapshot); err != nil {
		return fmt.Errorf("invalid recovered import state: %w", err)
	}
	s.credentialRestoreMu.Lock()
	defer s.credentialRestoreMu.Unlock()
	for _, source := range snapshot.ImportSources {
		s.beginImportCredentialMutationLocked(source.RepositoryID)
		if err := s.removeImportCredentialFile(source.RepositoryID); err != nil {
			return fmt.Errorf("invalidate recovered import credential %q: %w", source.RepositoryID, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='initialized'`).Scan(&initialized); err != nil {
		return err
	}
	var repositoryCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories`).Scan(&repositoryCount); err != nil {
		return err
	}
	if initialized != "false" || repositoryCount != 0 {
		return errors.New("recovery state destination is not new")
	}

	metadata := map[string]string{
		"initialized":            "true",
		"repository_root":        repositoryRoot,
		"access_mode":            snapshot.AccessMode,
		"access_session_version": "1",
		"admin_session_version":  "1",
		"insecure_http_accepted": "false",
	}
	for key, value := range metadata {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	for _, table := range []string{"push_events", "backup_runs", "backup_schedule", "share_links", "repository_policies", "repository_names", "import_initial_destinations", "import_stagings", "import_schedules", "import_publication_intents", "import_ref_observations", "import_runs", "import_sources", "check_observations", "check_runner_credentials", "actions_job_plans", "actions_schedules", "check_jobs", "actions_runs", "check_policies", "direct_review_requests", "direct_review_task_contexts", "direct_review_probes", "direct_review_repository_settings", "direct_review_credentials", "check_raw_log_starts", "check_raw_logs", "check_results", "check_attempts", "check_cycles", "check_configurations", "tasks", "repository_attempt_counters", "pull_request_auto_merges", "pull_request_merge_intents", "pull_request_reviews", "pull_request_revisions", "pull_requests", "sessions", "bootstrap", "login_attempts", "trusted_hosts", "passwords", "repositories"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO passwords(kind,encoded) VALUES('admin',?)`, snapshot.AdminPasswordHash); err != nil {
		return err
	}
	if snapshot.AccessMode == "password" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO passwords(kind,encoded) VALUES('access',?)`, snapshot.AccessPasswordHash); err != nil {
			return err
		}
	}
	for _, repository := range snapshot.Repositories {
		if repository.ID == "" || repository.Name == "" {
			return errors.New("invalid recovered repository metadata")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES(?,?,?,?)`, repository.ID, repository.Name, repository.Description, repository.CreatedAt.Unix()); err != nil {
			return fmt.Errorf("restore repository %q metadata: %w", repository.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO repository_attempt_counters(repository_id,attempt_sequence) VALUES(?,?)`, repository.ID, repository.AttemptSequence); err != nil {
			return fmt.Errorf("restore repository %q attempt counter: %w", repository.ID, err)
		}
	}
	if err := restoreRepositoryRecords(ctx, tx, snapshot); err != nil {
		return err
	}
	if err := restorePullRequestRecovery(ctx, tx, snapshot); err != nil {
		return err
	}
	if err := restoreCheckRecovery(ctx, tx, snapshot); err != nil {
		return err
	}
	if err := restoreImportRecovery(ctx, tx, snapshot); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, source := range snapshot.ImportSources {
		s.completeImportCredentialMutationLocked(source.RepositoryID)
	}
	return nil
}

func unixTime(seconds int64) time.Time {
	return time.Unix(seconds, 0)
}

// unixNanoTime reads the sub-second attempt ordering columns.
func unixNanoTime(nanoseconds int64) time.Time {
	return time.Unix(0, nanoseconds)
}
