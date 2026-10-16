package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"owngit/internal/importgit"
)

const (
	PullRequestCreating = "creating"
	PullRequestOpen     = "open"
	PullRequestMerged   = "merged"
	// PullRequestClosed is a pull request closed without merging. It keeps
	// its history and can be reopened.
	PullRequestClosed = "closed"

	ReviewPending          = "pending"
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes_requested"
	ReviewSkipped          = "skipped"
	// ReviewNotRequested is the default when creation omits a review choice.
	// It is not a waiting state: merge never requires a later decision.
	ReviewNotRequested = "not_requested"

	ReviewProvenanceRequest      = "review_request"
	ReviewProvenanceSkip         = "explicit_skip"
	ReviewProvenanceExternalTool = "supplied_external_tool"
	ReviewProvenanceDefault      = "default"

	MergeIntentPreparing = "preparing"
	MergeIntentPlanned   = "planned"
	MergeIntentReady     = "ready"
	MergeIntentComplete  = "complete"

	// MaximumPullRequestTextBytes bounds a pull request description and a
	// review note. The schema states the same bound.
	MaximumPullRequestTextBytes = 64 << 10
)

type PullRequest struct {
	RepositoryID   string
	Number         int64
	Title          string
	SourceBranch   string
	TargetBranch   string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	MergeSourceOID string
	MergeTargetOID string
	MergeOID       string
	MergeReceipt   string
	MergedAt       *time.Time
	Body           string
	// EditRevision counts title and description edits. An edit names the
	// revision it read, so a stale edit is refused instead of overwriting a
	// newer one. EditedAt is set from the first edit on.
	EditRevision int64
	EditedAt     *time.Time
	CreatedBy    Actor
	EditedBy     Actor
	MergedBy     Actor
}

type PullRequestRevision struct {
	RepositoryID      string
	PullRequestNumber int64
	SourceOID         string
	TargetOID         string
	RecordedAt        time.Time
}

type PullRequestReview struct {
	RepositoryID      string
	PullRequestNumber int64
	Sequence          int64
	SourceOID         string
	TargetOID         string
	Status            string
	ReviewerLabel     string
	Provenance        string
	// ReviewEventID is an optional identity for a pending review request, so a
	// retransmitted request is recognisable. Manual and default rows leave it
	// empty, and a stored value is still validated as a record fact.
	ReviewEventID string
	CreatedAt     time.Time
	// Note is the reviewer's text. The row binds the source and target
	// revisions it reviewed, so the note is bound to both.
	Note  string
	Actor Actor
}

type PullRequestMergeIntent struct {
	RepositoryID      string
	PullRequestNumber int64
	SourceOID         string
	TargetOID         string
	Mode              string
	TreeOID           string
	ResultOID         string
	ReceiptRef        string
	Status            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// PullRequestAutoMerge is an open pull request's opt-in intent to merge
// automatically once the checks of its current source revision pass. A row
// exists only while the intent is enabled and the pull request is open; the
// same transaction that closes the request or records its merge deletes the
// row. It binds to the request, not to a revision: branch moves keep the row
// and the merge decision is always taken for the current source and target.
type PullRequestAutoMerge struct {
	RepositoryID      string
	PullRequestNumber int64
	EnabledAt         time.Time
	UpdatedAt         time.Time
	EnabledBy         Actor
}

// PullRequestCreation is what creating a pull request records: the pull
// request, the revision pair it starts at, and the initial review choice for
// that pair, made by CreatedBy.
type PullRequestCreation struct {
	RepositoryID  string
	Title         string
	Body          string
	SourceBranch  string
	TargetBranch  string
	SourceOID     string
	TargetOID     string
	InitialReview string
	CreatedBy     Actor
}

func (s *Store) CreatePullRequest(ctx context.Context, repositoryID, title, sourceBranch, targetBranch, sourceOID, targetOID, initialReview string, now time.Time) (PullRequest, error) {
	record, err := s.BeginPullRequestCreation(ctx, PullRequestCreation{
		RepositoryID: repositoryID, Title: title, SourceBranch: sourceBranch, TargetBranch: targetBranch,
		SourceOID: sourceOID, TargetOID: targetOID, InitialReview: initialReview,
	}, now)
	if err != nil {
		return PullRequest{}, err
	}
	return s.ActivatePullRequestCreation(ctx, repositoryID, record.Number, now)
}

func (s *Store) BeginPullRequestCreation(ctx context.Context, creation PullRequestCreation, now time.Time) (PullRequest, error) {
	repositoryID, sourceOID, targetOID, initialReview := creation.RepositoryID, creation.SourceOID, creation.TargetOID, creation.InitialReview
	if initialReview != ReviewPending && initialReview != ReviewSkipped && initialReview != ReviewNotRequested {
		return PullRequest{}, errors.New("invalid initial review choice")
	}
	record := PullRequest{
		RepositoryID: repositoryID, Title: creation.Title, Body: creation.Body, SourceBranch: creation.SourceBranch, TargetBranch: creation.TargetBranch,
		Status: PullRequestCreating, CreatedAt: now, UpdatedAt: now, CreatedBy: creation.CreatedBy,
	}
	revision := PullRequestRevision{RepositoryID: repositoryID, SourceOID: sourceOID, TargetOID: targetOID, RecordedAt: now}
	review := PullRequestReview{RepositoryID: repositoryID, SourceOID: sourceOID, TargetOID: targetOID, Status: initialReview, CreatedAt: now, Actor: creation.CreatedBy}
	switch initialReview {
	case ReviewPending:
		review.Provenance = ReviewProvenanceRequest
	case ReviewSkipped:
		review.Provenance = ReviewProvenanceSkip
	default:
		review.Provenance = ReviewProvenanceDefault
	}
	if err := validatePullRequestRecord(record); err != nil {
		return PullRequest{}, err
	}
	if !validObjectID(sourceOID) || !validObjectID(targetOID) {
		return PullRequest{}, errors.New("invalid pull request revision")
	}
	createdBy, err := encodeActor(record.CreatedBy)
	if err != nil {
		return PullRequest{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequest{}, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM pull_requests WHERE repository_id=?`, repositoryID).Scan(&record.Number); err != nil {
		return PullRequest{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pull_requests(
		repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,body,created_by
	) VALUES(?,?,?,?,?,?,?,?,?,?)`, record.RepositoryID, record.Number, record.Title, record.SourceBranch, record.TargetBranch, record.Status, record.CreatedAt.Unix(), record.UpdatedAt.Unix(), record.Body, createdBy); err != nil {
		return PullRequest{}, err
	}
	revision.PullRequestNumber = record.Number
	if _, err := tx.ExecContext(ctx, `INSERT INTO pull_request_revisions(
		repository_id,pull_request_number,source_oid,target_oid,recorded_at
	) VALUES(?,?,?,?,?)`, revision.RepositoryID, revision.PullRequestNumber, revision.SourceOID, revision.TargetOID, revision.RecordedAt.Unix()); err != nil {
		return PullRequest{}, err
	}
	review.PullRequestNumber = record.Number
	review.Sequence = 1
	if err := insertPullRequestReview(ctx, tx, review); err != nil {
		return PullRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequest{}, err
	}
	return record, nil
}

func (s *Store) ActivatePullRequestCreation(ctx context.Context, repositoryID string, number int64, now time.Time) (PullRequest, error) {
	if now.IsZero() {
		return PullRequest{}, errors.New("pull request activation time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequest{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE pull_requests SET status=?,updated_at=? WHERE repository_id=? AND number=? AND status=?`,
		PullRequestOpen, now.Unix(), repositoryID, number, PullRequestCreating)
	if err != nil {
		return PullRequest{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return PullRequest{}, err
		}
		return PullRequest{}, errors.New("provisional pull request was not found")
	}
	row := tx.QueryRowContext(ctx, pullRequestSelect+` WHERE repository_id=? AND number=?`, repositoryID, number)
	record, err := scanPullRequest(row)
	if err != nil {
		return PullRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequest{}, err
	}
	return record, nil
}

// ErrPullRequestStateChanged reports that a pull request was not in the state
// a close or reopen expected.
var ErrPullRequestStateChanged = errors.New("pull request state changed")

// ErrPullRequestNotFound reports that no visible pull request has the
// repository and number an operation named.
var ErrPullRequestNotFound = errors.New("pull request not found")

// SetPullRequestClosed closes an open pull request, or reopens a closed one
// when closed is false. Any other state is left unchanged and reported with
// ErrPullRequestStateChanged.
func (s *Store) SetPullRequestClosed(ctx context.Context, repositoryID string, number int64, closed bool, now time.Time) (PullRequest, error) {
	if now.IsZero() {
		return PullRequest{}, errors.New("pull request state change time is required")
	}
	from, to := PullRequestOpen, PullRequestClosed
	if !closed {
		from, to = PullRequestClosed, PullRequestOpen
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequest{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE pull_requests SET status=?,updated_at=MAX(updated_at,?) WHERE repository_id=? AND number=? AND status=?`,
		to, now.Unix(), repositoryID, number, from)
	if err != nil {
		return PullRequest{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return PullRequest{}, err
		}
		return PullRequest{}, ErrPullRequestStateChanged
	}
	// Closing removes the auto-merge intent in the same state change, so a
	// background merge that takes the repository lock after the close finds
	// no intent to publish. Reopening does not bring it back.
	if closed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pull_request_auto_merges WHERE repository_id=? AND pull_request_number=?`, repositoryID, number); err != nil {
			return PullRequest{}, err
		}
	}
	record, err := scanPullRequest(tx.QueryRowContext(ctx, pullRequestSelect+` WHERE repository_id=? AND number=?`, repositoryID, number))
	if err != nil {
		return PullRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequest{}, err
	}
	return record, nil
}

func (s *Store) DeletePullRequestCreation(ctx context.Context, repositoryID string, number int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM pull_requests WHERE repository_id=? AND number=? AND status=?`, repositoryID, number, PullRequestCreating)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return errors.New("provisional pull request was not found")
	}
	return nil
}

func (s *Store) ProvisionalPullRequests(ctx context.Context, repositoryID string) ([]PullRequest, error) {
	rows, err := s.db.QueryContext(ctx, pullRequestSelect+` WHERE repository_id=? AND status=? ORDER BY number`, repositoryID, PullRequestCreating)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PullRequest
	for rows.Next() {
		record, err := scanPullRequest(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) PullRequest(ctx context.Context, repositoryID string, number int64) (PullRequest, bool, error) {
	row := s.db.QueryRowContext(ctx, pullRequestSelect+` WHERE repository_id=? AND number=? AND status!='creating'`, repositoryID, number)
	record, err := scanPullRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequest{}, false, nil
	}
	return record, err == nil, err
}

// OpenPullRequestForBranches returns the oldest open pull request from source
// into target. Stores written before one open pull request per branch pair was
// enforced can hold several; the oldest one is reported.
func (s *Store) OpenPullRequestForBranches(ctx context.Context, repositoryID, sourceBranch, targetBranch string) (PullRequest, bool, error) {
	row := s.db.QueryRowContext(ctx, pullRequestSelect+` WHERE repository_id=? AND source_branch=? AND target_branch=? AND status=? ORDER BY number LIMIT 1`,
		repositoryID, sourceBranch, targetBranch, PullRequestOpen)
	record, err := scanPullRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequest{}, false, nil
	}
	return record, err == nil, err
}

// OpenPullRequests returns a bounded newest-first set for automatic revision
// observation after Git writes.
func (s *Store) OpenPullRequests(ctx context.Context, repositoryID string, limit int) ([]PullRequest, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("invalid open pull request limit")
	}
	rows, err := s.db.QueryContext(ctx, pullRequestSelect+` WHERE repository_id=? AND status=? ORDER BY number DESC LIMIT ?`, repositoryID, PullRequestOpen, limit+1)
	if err != nil {
		return nil, false, err
	}
	records, err := collectRows(rows, scanPullRequest)
	if err != nil {
		return nil, false, err
	}
	more := len(records) > limit
	if more {
		records = records[:limit]
	}
	return records, more, nil
}

// OpenPullRequestsAfter returns one bounded circular page in ascending number
// order. Advancing the cursor to the last returned number gives every open
// request fair progress without deleting revision history.
func (s *Store) OpenPullRequestsAfter(ctx context.Context, repositoryID string, after int64, limit int) ([]PullRequest, bool, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, false, errors.New("invalid open pull request page")
	}
	rows, err := s.db.QueryContext(ctx, pullRequestSelect+` WHERE repository_id=? AND status=?
		ORDER BY CASE WHEN number>? THEN 0 ELSE 1 END,number ASC LIMIT ?`, repositoryID, PullRequestOpen, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var records []PullRequest
	for rows.Next() {
		record, err := scanPullRequest(rows)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(records) > limit
	if more {
		records = records[:limit]
	}
	return records, more, nil
}

// PullRequestSummaries returns one newest-first page of a repository's
// visible pull requests, optionally of one status, continuing below number
// before (0 starts at the newest). It returns at most limit records and
// whether older ones remain. The records carry no description.
func (s *Store) PullRequestSummaries(ctx context.Context, repositoryID, status string, before int64, limit int) ([]PullRequest, bool, error) {
	if limit < 1 || limit > 1000 || before < 0 {
		return nil, false, errors.New("invalid pull request page")
	}
	switch status {
	case "", PullRequestOpen, PullRequestClosed, PullRequestMerged:
	default:
		return nil, false, errors.New("invalid pull request status")
	}
	query := pullRequestSummarySelect + ` WHERE repository_id=? AND status!='creating'`
	args := []any{repositoryID}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	if before > 0 {
		query += ` AND number<?`
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY number DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var records []PullRequest
	for rows.Next() {
		record, err := scanPullRequest(rows)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(records) > limit
	if more {
		records = records[:limit]
	}
	return records, more, nil
}

const pullRequestSelect = `SELECT repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,
	merge_source_oid,merge_target_oid,merge_oid,merge_receipt_ref,merged_at,
	body,edit_revision,edited_at,created_by,edited_by,merged_by FROM pull_requests`

// pullRequestSummarySelect is pullRequestSelect without the description,
// which a list never shows and which can be 64 KiB a record.
const pullRequestSummarySelect = `SELECT repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,
	merge_source_oid,merge_target_oid,merge_oid,merge_receipt_ref,merged_at,
	'',edit_revision,edited_at,created_by,edited_by,merged_by FROM pull_requests`

func collectRows[T any](rows *sql.Rows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var records []T
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

type rowScanner interface {
	Scan(...any) error
}

func scanPullRequest(scanner rowScanner) (PullRequest, error) {
	var record PullRequest
	var createdAt, updatedAt int64
	var mergedAt, editedAt sql.NullInt64
	var createdBy, editedBy, mergedBy string
	if err := scanner.Scan(
		&record.RepositoryID, &record.Number, &record.Title, &record.SourceBranch, &record.TargetBranch, &record.Status,
		&createdAt, &updatedAt, &record.MergeSourceOID, &record.MergeTargetOID, &record.MergeOID, &record.MergeReceipt, &mergedAt,
		&record.Body, &record.EditRevision, &editedAt, &createdBy, &editedBy, &mergedBy,
	); err != nil {
		return PullRequest{}, err
	}
	record.CreatedAt = unixTime(createdAt)
	record.UpdatedAt = unixTime(updatedAt)
	record.MergedAt = nullableTimePointer(mergedAt)
	record.EditedAt = nullableTimePointer(editedAt)
	var err error
	if record.CreatedBy, err = decodeActor(createdBy); err != nil {
		return PullRequest{}, err
	}
	if record.EditedBy, err = decodeActor(editedBy); err != nil {
		return PullRequest{}, err
	}
	if record.MergedBy, err = decodeActor(mergedBy); err != nil {
		return PullRequest{}, err
	}
	return record, nil
}

func (s *Store) HasPullRequestRevision(ctx context.Context, repositoryID string, number int64, sourceOID, targetOID string) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM pull_request_revisions WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=?`,
		repositoryID, number, sourceOID, targetOID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) RecordPullRequestRevision(ctx context.Context, revision PullRequestRevision) error {
	if !validObjectID(revision.SourceOID) || !validObjectID(revision.TargetOID) || revision.RecordedAt.IsZero() {
		return errors.New("invalid pull request revision")
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO pull_request_revisions(
		repository_id,pull_request_number,source_oid,target_oid,recorded_at
	) VALUES(?,?,?,?,?)`, revision.RepositoryID, revision.PullRequestNumber, revision.SourceOID, revision.TargetOID, revision.RecordedAt.Unix())
	return err
}

// LatestPullRequestRevisions returns a bounded newest-first set for automatic
// event reconciliation. The full recovery reader remains separate.
func (s *Store) LatestPullRequestRevisions(ctx context.Context, repositoryID string, limit int) ([]PullRequestRevision, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid pull request revision limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revisions.repository_id,revisions.pull_request_number,revisions.source_oid,revisions.target_oid,revisions.recorded_at
		FROM pull_request_revisions revisions
		JOIN pull_requests requests ON requests.repository_id=revisions.repository_id AND requests.number=revisions.pull_request_number
		WHERE revisions.repository_id=? AND requests.status!='creating'
		ORDER BY revisions.recorded_at DESC,revisions.pull_request_number DESC,revisions.source_oid DESC,revisions.target_oid DESC LIMIT ?`, repositoryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []PullRequestRevision
	for rows.Next() {
		var revision PullRequestRevision
		var recordedAt int64
		if err := rows.Scan(&revision.RepositoryID, &revision.PullRequestNumber, &revision.SourceOID, &revision.TargetOID, &recordedAt); err != nil {
			return nil, err
		}
		revision.RecordedAt = unixTime(recordedAt)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) PullRequestRevisions(ctx context.Context, repositoryID string) ([]PullRequestRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT revisions.repository_id,revisions.pull_request_number,revisions.source_oid,revisions.target_oid,revisions.recorded_at
		FROM pull_request_revisions revisions
		JOIN pull_requests requests ON requests.repository_id=revisions.repository_id AND requests.number=revisions.pull_request_number
		WHERE revisions.repository_id=? AND requests.status!='creating'
		ORDER BY revisions.pull_request_number,revisions.recorded_at,revisions.source_oid,revisions.target_oid`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []PullRequestRevision
	for rows.Next() {
		var revision PullRequestRevision
		var recordedAt int64
		if err := rows.Scan(&revision.RepositoryID, &revision.PullRequestNumber, &revision.SourceOID, &revision.TargetOID, &recordedAt); err != nil {
			return nil, err
		}
		revision.RecordedAt = unixTime(recordedAt)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) PullRequestRevisionsFor(ctx context.Context, repositoryID string, number int64) ([]PullRequestRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id,pull_request_number,source_oid,target_oid,recorded_at
		FROM pull_request_revisions WHERE repository_id=? AND pull_request_number=? ORDER BY recorded_at,source_oid,target_oid`, repositoryID, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []PullRequestRevision
	for rows.Next() {
		var revision PullRequestRevision
		var recordedAt int64
		if err := rows.Scan(&revision.RepositoryID, &revision.PullRequestNumber, &revision.SourceOID, &revision.TargetOID, &recordedAt); err != nil {
			return nil, err
		}
		revision.RecordedAt = unixTime(recordedAt)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) AppendPullRequestReview(ctx context.Context, review PullRequestReview) (PullRequestReview, error) {
	if err := validateReviewRecord(review, false); err != nil {
		return PullRequestReview{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequestReview{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM pull_requests WHERE repository_id=? AND number=?`, review.RepositoryID, review.PullRequestNumber).Scan(&status); err != nil {
		return PullRequestReview{}, err
	}
	if status != PullRequestOpen {
		return PullRequestReview{}, errors.New("pull request is not open")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM pull_request_reviews WHERE repository_id=? AND pull_request_number=?`, review.RepositoryID, review.PullRequestNumber).Scan(&review.Sequence); err != nil {
		return PullRequestReview{}, err
	}
	if err := insertPullRequestReview(ctx, tx, review); err != nil {
		return PullRequestReview{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pull_requests SET updated_at=? WHERE repository_id=? AND number=?`, review.CreatedAt.Unix(), review.RepositoryID, review.PullRequestNumber); err != nil {
		return PullRequestReview{}, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequestReview{}, err
	}
	return review, nil
}

// insertPullRequestReview writes one review row with every column.
func insertPullRequestReview(ctx context.Context, tx *sql.Tx, review PullRequestReview) error {
	actor, err := encodeActor(review.Actor)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pull_request_reviews(
		repository_id,pull_request_number,sequence,source_oid,target_oid,status,reviewer_label,provenance,review_event_id,created_at,note,actor
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, review.RepositoryID, review.PullRequestNumber, review.Sequence, review.SourceOID, review.TargetOID, review.Status, review.ReviewerLabel, review.Provenance, review.ReviewEventID, review.CreatedAt.Unix(), review.Note, actor)
	return err
}

// PullRequestReviewNotes returns the newest reviews of a pull request that
// carry a note, at most limit of them, newest first, and whether older ones
// exist. Each keeps the revision pair it reviewed.
func (s *Store) PullRequestReviewNotes(ctx context.Context, repositoryID string, number int64, limit int) ([]PullRequestReview, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("invalid review note limit")
	}
	rows, err := s.db.QueryContext(ctx, pullRequestReviewSelect+` WHERE repository_id=? AND pull_request_number=? AND note!='' ORDER BY sequence DESC LIMIT ?`, repositoryID, number, limit+1)
	if err != nil {
		return nil, false, err
	}
	reviews, err := collectRows(rows, scanPullRequestReview)
	if err != nil {
		return nil, false, err
	}
	more := len(reviews) > limit
	if more {
		reviews = reviews[:limit]
	}
	return reviews, more, nil
}

// PullRequestEdit replaces the title and description of the pull request
// whose edit revision is still BasedOn.
type PullRequestEdit struct {
	RepositoryID string
	Number       int64
	BasedOn      int64
	Title        string
	Body         string
	EditedBy     Actor
}

// ErrPullRequestEdited reports that a pull request was edited after the
// revision an edit was based on. Nothing was changed.
var ErrPullRequestEdited = errors.New("pull request was edited since it was read")

// EditPullRequest applies edit when the pull request is still at the edit
// revision it was based on, and counts one more edit. Otherwise it changes
// nothing and returns ErrPullRequestEdited. Revisions, reviews, checks and
// merge records are not touched.
func (s *Store) EditPullRequest(ctx context.Context, edit PullRequestEdit, now time.Time) (PullRequest, error) {
	if now.IsZero() || edit.BasedOn < 0 || !validText(edit.Title, 500) || !ValidPullRequestText(edit.Body) {
		return PullRequest{}, errors.New("invalid pull request edit")
	}
	editedBy, err := encodeActor(edit.EditedBy)
	if err != nil {
		return PullRequest{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequest{}, err
	}
	defer tx.Rollback()
	// An edit is never earlier than the creation, even when the clock moved
	// back, because a record that says so is refused on restore.
	result, err := tx.ExecContext(ctx, `UPDATE pull_requests SET title=?,body=?,edit_revision=edit_revision+1,edited_at=MAX(created_at,COALESCE(edited_at,0),?),
		edited_by=?,updated_at=MAX(updated_at,?) WHERE repository_id=? AND number=? AND status!='creating' AND edit_revision=?`,
		edit.Title, edit.Body, now.Unix(), editedBy, now.Unix(), edit.RepositoryID, edit.Number, edit.BasedOn)
	if err != nil {
		return PullRequest{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return PullRequest{}, err
		}
		return PullRequest{}, ErrPullRequestEdited
	}
	record, err := scanPullRequest(tx.QueryRowContext(ctx, pullRequestSelect+` WHERE repository_id=? AND number=?`, edit.RepositoryID, edit.Number))
	if err != nil {
		return PullRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequest{}, err
	}
	return record, nil
}

func (s *Store) PullRequestReviewForRevision(ctx context.Context, repositoryID string, number int64, sourceOID, targetOID string) (PullRequestReview, bool, error) {
	row := s.db.QueryRowContext(ctx, pullRequestReviewSelect+` WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=? ORDER BY sequence DESC LIMIT 1`, repositoryID, number, sourceOID, targetOID)
	review, err := scanPullRequestReview(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequestReview{}, false, nil
	}
	return review, err == nil, err
}

const pullRequestReviewSelect = `SELECT repository_id,pull_request_number,sequence,source_oid,target_oid,status,reviewer_label,provenance,review_event_id,created_at,note,actor FROM pull_request_reviews`

func scanPullRequestReview(scanner rowScanner) (PullRequestReview, error) {
	var review PullRequestReview
	var createdAt int64
	var actor string
	if err := scanner.Scan(&review.RepositoryID, &review.PullRequestNumber, &review.Sequence, &review.SourceOID, &review.TargetOID, &review.Status, &review.ReviewerLabel, &review.Provenance, &review.ReviewEventID, &createdAt, &review.Note, &actor); err != nil {
		return PullRequestReview{}, err
	}
	review.CreatedAt = unixTime(createdAt)
	var err error
	if review.Actor, err = decodeActor(actor); err != nil {
		return PullRequestReview{}, err
	}
	return review, nil
}

func (s *Store) BeginPullRequestMerge(ctx context.Context, intent PullRequestMergeIntent) (PullRequestMergeIntent, error) {
	if !validObjectID(intent.SourceOID) || !validObjectID(intent.TargetOID) || intent.ReceiptRef == "" || intent.CreatedAt.IsZero() {
		return PullRequestMergeIntent{}, errors.New("invalid merge intent")
	}
	intent.Mode = ""
	intent.TreeOID = ""
	intent.ResultOID = ""
	intent.Status = MergeIntentPreparing
	intent.UpdatedAt = intent.CreatedAt
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO pull_request_merge_intents(
		repository_id,pull_request_number,source_oid,target_oid,mode,tree_oid,result_oid,receipt_ref,status,created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID, intent.Mode, intent.TreeOID, intent.ResultOID, intent.ReceiptRef, intent.Status, intent.CreatedAt.Unix(), intent.UpdatedAt.Unix())
	if err != nil {
		return PullRequestMergeIntent{}, err
	}
	stored, ok, err := s.PullRequestMergeIntent(ctx, intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)
	if err != nil {
		return PullRequestMergeIntent{}, err
	}
	if !ok || stored.ReceiptRef != intent.ReceiptRef {
		return PullRequestMergeIntent{}, errors.New("merge intent collision")
	}
	return stored, nil
}

func (s *Store) UpdatePullRequestMergeIntent(ctx context.Context, intent PullRequestMergeIntent) (PullRequestMergeIntent, error) {
	if err := validateMergeIntentRecord(intent, false); err != nil {
		return PullRequestMergeIntent{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pull_request_merge_intents SET mode=?,tree_oid=?,result_oid=?,status=?,updated_at=?
		WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=? AND receipt_ref=?`,
		intent.Mode, intent.TreeOID, intent.ResultOID, intent.Status, intent.UpdatedAt.Unix(), intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID, intent.ReceiptRef)
	if err != nil {
		return PullRequestMergeIntent{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return PullRequestMergeIntent{}, err
		}
		return PullRequestMergeIntent{}, errors.New("merge intent was not found")
	}
	stored, _, err := s.PullRequestMergeIntent(ctx, intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)
	return stored, err
}

// DeleteUnpublishedPullRequestMerge removes a plan only after its caller,
// holding the repository write lock, has established that no receipt exists.
func (s *Store) DeleteUnpublishedPullRequestMerge(ctx context.Context, intent PullRequestMergeIntent) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pull_request_merge_intents
		WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=? AND status <> 'complete'`,
		intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)
	return err
}

// PullRequestMergeReceiptOwner binds a shared receipt to its published plan.
// Older unpublished plans for other revisions cannot claim that receipt.
func PullRequestMergeReceiptOwner(record PullRequest, intents []PullRequestMergeIntent, receiptOID string) (PullRequestMergeIntent, error) {
	var owner PullRequestMergeIntent
	for _, intent := range intents {
		if intent.RepositoryID != record.RepositoryID || intent.PullRequestNumber != record.Number || intent.ReceiptRef != "refs/owngit/pull-requests/"+strconv.FormatInt(record.Number, 10)+"/merge-receipt" || intent.ResultOID == "" || intent.ResultOID != receiptOID {
			continue
		}
		if record.Status == PullRequestMerged {
			if intent.Status != MergeIntentComplete || intent.SourceOID != record.MergeSourceOID || intent.TargetOID != record.MergeTargetOID || intent.ResultOID != record.MergeOID || intent.ReceiptRef != record.MergeReceipt {
				continue
			}
		} else if record.Status != PullRequestOpen || intent.Status != MergeIntentReady {
			continue
		}
		if owner.RepositoryID != "" {
			return PullRequestMergeIntent{}, errors.New("merge receipt has multiple durable owners")
		}
		owner = intent
	}
	if owner.RepositoryID == "" {
		return PullRequestMergeIntent{}, errors.New("merge receipt does not match its durable intent")
	}
	return owner, nil
}

func (s *Store) PullRequestMergeIntent(ctx context.Context, repositoryID string, number int64, sourceOID, targetOID string) (PullRequestMergeIntent, bool, error) {
	row := s.db.QueryRowContext(ctx, mergeIntentSelect+` WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=?`, repositoryID, number, sourceOID, targetOID)
	intent, err := scanMergeIntent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequestMergeIntent{}, false, nil
	}
	return intent, err == nil, err
}

// PullRequestMergeIntents reads the merge intents of one repository, or of
// one pull request when number is not zero. The primary key serves both reads,
// so a caller never decodes another repository's intents.
func (s *Store) PullRequestMergeIntents(ctx context.Context, repositoryID string, number int64, incompleteOnly bool) ([]PullRequestMergeIntent, error) {
	query := mergeIntentSelect + ` WHERE repository_id=?`
	args := []any{repositoryID}
	if number != 0 {
		query += ` AND pull_request_number=?`
		args = append(args, number)
	}
	if incompleteOnly {
		query += ` AND status!='complete'`
	}
	query += ` ORDER BY pull_request_number,created_at`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []PullRequestMergeIntent
	for rows.Next() {
		intent, err := scanMergeIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

const mergeIntentSelect = `SELECT repository_id,pull_request_number,source_oid,target_oid,mode,tree_oid,result_oid,receipt_ref,status,created_at,updated_at FROM pull_request_merge_intents`

func scanMergeIntent(scanner rowScanner) (PullRequestMergeIntent, error) {
	var intent PullRequestMergeIntent
	var createdAt, updatedAt int64
	if err := scanner.Scan(&intent.RepositoryID, &intent.PullRequestNumber, &intent.SourceOID, &intent.TargetOID, &intent.Mode, &intent.TreeOID, &intent.ResultOID, &intent.ReceiptRef, &intent.Status, &createdAt, &updatedAt); err != nil {
		return PullRequestMergeIntent{}, err
	}
	intent.CreatedAt = unixTime(createdAt)
	intent.UpdatedAt = unixTime(updatedAt)
	return intent, nil
}

// CompletePullRequestMerge records the merge that Git published for intent.
// mergedBy is who asked for the merge, or nobody when OwnGit completes it on
// its own after an interruption.
func (s *Store) CompletePullRequestMerge(ctx context.Context, intent PullRequestMergeIntent, completedAt time.Time, mergedBy Actor) error {
	if intent.Status != MergeIntentReady || intent.ResultOID == "" || completedAt.IsZero() {
		return errors.New("merge intent is not ready")
	}
	merger, err := encodeActor(mergedBy)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var storedStatus, resultOID, receiptRef string
	if err := tx.QueryRowContext(ctx, `SELECT status,result_oid,receipt_ref FROM pull_request_merge_intents
		WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=?`, intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID).Scan(&storedStatus, &resultOID, &receiptRef); err != nil {
		return err
	}
	if resultOID != intent.ResultOID || receiptRef != intent.ReceiptRef || (storedStatus != MergeIntentReady && storedStatus != MergeIntentComplete) {
		return errors.New("stored merge intent does not match the published result")
	}
	var requestStatus, mergedOID, mergedSource, mergedTarget, mergedReceipt string
	if err := tx.QueryRowContext(ctx, `SELECT status,merge_oid,merge_source_oid,merge_target_oid,merge_receipt_ref FROM pull_requests WHERE repository_id=? AND number=?`, intent.RepositoryID, intent.PullRequestNumber).Scan(&requestStatus, &mergedOID, &mergedSource, &mergedTarget, &mergedReceipt); err != nil {
		return err
	}
	if requestStatus == PullRequestMerged {
		if mergedOID != intent.ResultOID || mergedSource != intent.SourceOID || mergedTarget != intent.TargetOID || mergedReceipt != intent.ReceiptRef {
			return errors.New("pull request is already merged for another revision")
		}
	} else if requestStatus == PullRequestOpen {
		if _, err := tx.ExecContext(ctx, `UPDATE pull_requests SET status='merged',updated_at=?,merge_source_oid=?,merge_target_oid=?,merge_oid=?,merge_receipt_ref=?,merged_at=?,merged_by=?
			WHERE repository_id=? AND number=? AND status='open'`, completedAt.Unix(), intent.SourceOID, intent.TargetOID, intent.ResultOID, intent.ReceiptRef, completedAt.Unix(), merger, intent.RepositoryID, intent.PullRequestNumber); err != nil {
			return err
		}
	} else {
		return errors.New("pull request has an invalid state")
	}
	// The published merge ends the auto-merge intent in the same transaction,
	// so a wake that runs after the merge finds nothing left to publish.
	if _, err := tx.ExecContext(ctx, `DELETE FROM pull_request_auto_merges WHERE repository_id=? AND pull_request_number=?`, intent.RepositoryID, intent.PullRequestNumber); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pull_request_merge_intents SET status='complete',updated_at=?
		WHERE repository_id=? AND pull_request_number=? AND source_oid=? AND target_oid=?`, completedAt.Unix(), intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID); err != nil {
		return err
	}
	return tx.Commit()
}

// EnablePullRequestAutoMerge records an auto-merge intent for an open pull
// request. Enabling an intent that already exists is idempotent: the first
// enabled_at and actor are kept and nothing is rewritten. A pull request that
// is missing, provisional, closed, or merged is refused with
// ErrPullRequestNotFound or ErrPullRequestStateChanged.
func (s *Store) EnablePullRequestAutoMerge(ctx context.Context, repositoryID string, number int64, enabledBy Actor, now time.Time) (PullRequestAutoMerge, error) {
	if number <= 0 || now.IsZero() {
		return PullRequestAutoMerge{}, errors.New("invalid pull request auto-merge")
	}
	encoded, err := encodeActor(enabledBy)
	if err != nil {
		return PullRequestAutoMerge{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PullRequestAutoMerge{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM pull_requests WHERE repository_id=? AND number=? AND status!='creating'`, repositoryID, number).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PullRequestAutoMerge{}, ErrPullRequestNotFound
		}
		return PullRequestAutoMerge{}, err
	}
	if status != PullRequestOpen {
		return PullRequestAutoMerge{}, ErrPullRequestStateChanged
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pull_request_auto_merges(
		repository_id,pull_request_number,enabled_at,updated_at,enabled_by
	) VALUES(?,?,?,?,?)`, repositoryID, number, now.Unix(), now.Unix(), encoded); err != nil {
		return PullRequestAutoMerge{}, err
	}
	record, err := scanPullRequestAutoMerge(tx.QueryRowContext(ctx, pullRequestAutoMergeSelect+` WHERE repository_id=? AND pull_request_number=?`, repositoryID, number))
	if err != nil {
		return PullRequestAutoMerge{}, err
	}
	if err := tx.Commit(); err != nil {
		return PullRequestAutoMerge{}, err
	}
	return record, nil
}

// PullRequestAutoMerge reads one enabled intent. ok is false when the pull
// request has no enabled auto-merge.
func (s *Store) PullRequestAutoMerge(ctx context.Context, repositoryID string, number int64) (PullRequestAutoMerge, bool, error) {
	row := s.db.QueryRowContext(ctx, pullRequestAutoMergeSelect+` WHERE repository_id=? AND pull_request_number=?`, repositoryID, number)
	record, err := scanPullRequestAutoMerge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequestAutoMerge{}, false, nil
	}
	return record, err == nil, err
}

// PullRequestAutoMerges lists enabled intents of one repository, or of every
// repository when repositoryID is empty, ordered for stable processing.
func (s *Store) PullRequestAutoMerges(ctx context.Context, repositoryID string) ([]PullRequestAutoMerge, error) {
	query := pullRequestAutoMergeSelect
	var args []any
	if repositoryID != "" {
		query += ` WHERE repository_id=?`
		args = append(args, repositoryID)
	}
	query += ` ORDER BY repository_id,pull_request_number`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, scanPullRequestAutoMerge)
}

// PullRequestAutoMergesAfter returns one bounded circular page of one
// repository's enabled intents in ascending pull request number order.
// Advancing the cursor to the last returned number, as the caller converges
// a page, gives every enabled request fair progress.
func (s *Store) PullRequestAutoMergesAfter(ctx context.Context, repositoryID string, after int64, limit int) ([]PullRequestAutoMerge, bool, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, false, errors.New("invalid pull request auto-merge page")
	}
	rows, err := s.db.QueryContext(ctx, pullRequestAutoMergeSelect+` WHERE repository_id=?
		ORDER BY CASE WHEN pull_request_number>? THEN 0 ELSE 1 END,pull_request_number ASC LIMIT ?`, repositoryID, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	records, err := collectRows(rows, scanPullRequestAutoMerge)
	if err != nil {
		return nil, false, err
	}
	more := len(records) > limit
	if more {
		records = records[:limit]
	}
	return records, more, nil
}

// RepositoriesWithPullRequestAutoMerges lists the repository identifiers that
// currently have an enabled intent, so a wake scan touches only repositories
// that can have background work.
func (s *Store) RepositoriesWithPullRequestAutoMerges(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT repository_id FROM pull_request_auto_merges ORDER BY repository_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []string
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			return nil, err
		}
		repositories = append(repositories, repositoryID)
	}
	return repositories, rows.Err()
}

// DeletePullRequestAutoMerge removes an enabled intent. A missing intent is
// not an error: the caller has already established, under the repository
// lock, whether the row should exist.
func (s *Store) DeletePullRequestAutoMerge(ctx context.Context, repositoryID string, number int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pull_request_auto_merges WHERE repository_id=? AND pull_request_number=?`, repositoryID, number)
	return err
}

const pullRequestAutoMergeSelect = `SELECT repository_id,pull_request_number,enabled_at,updated_at,enabled_by FROM pull_request_auto_merges`

func scanPullRequestAutoMerge(scanner rowScanner) (PullRequestAutoMerge, error) {
	var record PullRequestAutoMerge
	var enabledAt, updatedAt int64
	var enabledBy string
	if err := scanner.Scan(&record.RepositoryID, &record.PullRequestNumber, &enabledAt, &updatedAt, &enabledBy); err != nil {
		return PullRequestAutoMerge{}, err
	}
	record.EnabledAt = unixTime(enabledAt)
	record.UpdatedAt = unixTime(updatedAt)
	var err error
	if record.EnabledBy, err = decodeActor(enabledBy); err != nil {
		return PullRequestAutoMerge{}, err
	}
	return record, nil
}

func readPullRequestRecovery(ctx context.Context, tx *sql.Tx, snapshot *RecoveryState) error {
	rows, err := tx.QueryContext(ctx, pullRequestSelect+` ORDER BY repository_id,number`)
	if err != nil {
		return err
	}
	for rows.Next() {
		record, err := scanPullRequest(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if record.Status == PullRequestCreating {
			rows.Close()
			return errors.New("provisional pull request creation must be reconciled before backup")
		}
		snapshot.PullRequests = append(snapshot.PullRequests, record)
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	rows, err = tx.QueryContext(ctx, `SELECT repository_id,pull_request_number,source_oid,target_oid,recorded_at FROM pull_request_revisions ORDER BY repository_id,pull_request_number,recorded_at,source_oid,target_oid`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var revision PullRequestRevision
		var recordedAt int64
		if err := rows.Scan(&revision.RepositoryID, &revision.PullRequestNumber, &revision.SourceOID, &revision.TargetOID, &recordedAt); err != nil {
			rows.Close()
			return err
		}
		revision.RecordedAt = unixTime(recordedAt)
		snapshot.PullRequestRevisions = append(snapshot.PullRequestRevisions, revision)
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	rows, err = tx.QueryContext(ctx, pullRequestReviewSelect+` ORDER BY repository_id,pull_request_number,sequence`)
	if err != nil {
		return err
	}
	for rows.Next() {
		review, err := scanPullRequestReview(rows)
		if err != nil {
			rows.Close()
			return err
		}
		snapshot.PullRequestReviews = append(snapshot.PullRequestReviews, review)
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	rows, err = tx.QueryContext(ctx, mergeIntentSelect+` ORDER BY repository_id,pull_request_number,created_at,source_oid,target_oid`)
	if err != nil {
		return err
	}
	for rows.Next() {
		intent, err := scanMergeIntent(rows)
		if err != nil {
			rows.Close()
			return err
		}
		snapshot.PullRequestMergeIntents = append(snapshot.PullRequestMergeIntents, intent)
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	autoMergeRows, err := tx.QueryContext(ctx, pullRequestAutoMergeSelect+` ORDER BY repository_id,pull_request_number`)
	if err != nil {
		return err
	}
	for autoMergeRows.Next() {
		record, err := scanPullRequestAutoMerge(autoMergeRows)
		if err != nil {
			autoMergeRows.Close()
			return err
		}
		snapshot.PullRequestAutoMerges = append(snapshot.PullRequestAutoMerges, record)
	}
	return closeRows(autoMergeRows)
}

func restorePullRequestRecovery(ctx context.Context, tx *sql.Tx, snapshot RecoveryState) error {
	for _, record := range snapshot.PullRequests {
		actors := make([]string, 3)
		for index, actor := range []Actor{record.CreatedBy, record.EditedBy, record.MergedBy} {
			encoded, err := encodeActor(actor)
			if err != nil {
				return fmt.Errorf("restore pull request %s#%d: %w", record.RepositoryID, record.Number, err)
			}
			actors[index] = encoded
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_requests(
			repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,merge_source_oid,merge_target_oid,merge_oid,merge_receipt_ref,merged_at,
			body,edit_revision,edited_at,created_by,edited_by,merged_by
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.RepositoryID, record.Number, record.Title, record.SourceBranch, record.TargetBranch, record.Status, record.CreatedAt.Unix(), record.UpdatedAt.Unix(), record.MergeSourceOID, record.MergeTargetOID, record.MergeOID, record.MergeReceipt, nullableUnix(record.MergedAt),
			record.Body, record.EditRevision, nullableUnix(record.EditedAt), actors[0], actors[1], actors[2]); err != nil {
			return fmt.Errorf("restore pull request %s#%d: %w", record.RepositoryID, record.Number, err)
		}
	}
	for _, revision := range snapshot.PullRequestRevisions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_request_revisions(repository_id,pull_request_number,source_oid,target_oid,recorded_at) VALUES(?,?,?,?,?)`, revision.RepositoryID, revision.PullRequestNumber, revision.SourceOID, revision.TargetOID, revision.RecordedAt.Unix()); err != nil {
			return fmt.Errorf("restore pull request revision: %w", err)
		}
	}
	for _, review := range snapshot.PullRequestReviews {
		if err := insertPullRequestReview(ctx, tx, review); err != nil {
			return fmt.Errorf("restore pull request review: %w", err)
		}
	}
	for _, intent := range snapshot.PullRequestMergeIntents {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_request_merge_intents(repository_id,pull_request_number,source_oid,target_oid,mode,tree_oid,result_oid,receipt_ref,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID, intent.Mode, intent.TreeOID, intent.ResultOID, intent.ReceiptRef, intent.Status, intent.CreatedAt.Unix(), intent.UpdatedAt.Unix()); err != nil {
			return fmt.Errorf("restore pull request merge intent: %w", err)
		}
	}
	for _, intent := range snapshot.PullRequestAutoMerges {
		enabledBy, err := encodeActor(intent.EnabledBy)
		if err != nil {
			return fmt.Errorf("restore pull request auto-merge %s#%d: %w", intent.RepositoryID, intent.PullRequestNumber, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_request_auto_merges(repository_id,pull_request_number,enabled_at,updated_at,enabled_by) VALUES(?,?,?,?,?)`, intent.RepositoryID, intent.PullRequestNumber, intent.EnabledAt.Unix(), intent.UpdatedAt.Unix(), enabledBy); err != nil {
			return fmt.Errorf("restore pull request auto-merge: %w", err)
		}
	}
	return nil
}

func ValidatePullRequestRecovery(snapshot RecoveryState) error {
	repositories := make(map[string]bool, len(snapshot.Repositories))
	for _, repository := range snapshot.Repositories {
		repositories[repository.ID] = true
	}
	requests := make(map[string]PullRequest, len(snapshot.PullRequests))
	for _, record := range snapshot.PullRequests {
		if !repositories[record.RepositoryID] || record.Number <= 0 {
			return errors.New("pull request refers to an unknown repository")
		}
		if err := validatePullRequestRecord(record); err != nil {
			return err
		}
		if record.Status == PullRequestCreating {
			return errors.New("backup contains a provisional pull request creation")
		}
		key := pullRequestKey(record.RepositoryID, record.Number)
		if _, exists := requests[key]; exists {
			return errors.New("duplicate pull request record")
		}
		requests[key] = record
	}
	revisions := make(map[string]bool, len(snapshot.PullRequestRevisions))
	requestRevisionCounts := make(map[string]int)
	for _, revision := range snapshot.PullRequestRevisions {
		requestKey := pullRequestKey(revision.RepositoryID, revision.PullRequestNumber)
		if _, exists := requests[requestKey]; !exists || !validObjectID(revision.SourceOID) || !validObjectID(revision.TargetOID) || revision.RecordedAt.IsZero() {
			return errors.New("invalid pull request revision record")
		}
		key := revisionKey(revision.RepositoryID, revision.PullRequestNumber, revision.SourceOID, revision.TargetOID)
		if revisions[key] {
			return errors.New("duplicate pull request revision record")
		}
		revisions[key] = true
		requestRevisionCounts[requestKey]++
	}
	for key := range requests {
		if requestRevisionCounts[key] == 0 {
			return errors.New("pull request has no retained revision binding")
		}
	}
	reviews := make(map[string]bool, len(snapshot.PullRequestReviews))
	reviewEvents := make(map[string]bool)
	reviewCounts := make(map[string]int)
	for _, review := range snapshot.PullRequestReviews {
		if _, exists := requests[pullRequestKey(review.RepositoryID, review.PullRequestNumber)]; !exists || !revisions[revisionKey(review.RepositoryID, review.PullRequestNumber, review.SourceOID, review.TargetOID)] {
			return errors.New("pull request review refers to an unknown revision")
		}
		if err := validateReviewRecord(review, true); err != nil {
			return err
		}
		key := pullRequestKey(review.RepositoryID, review.PullRequestNumber) + "/" + strconv.FormatInt(review.Sequence, 10)
		if reviews[key] {
			return errors.New("duplicate pull request review sequence")
		}
		reviews[key] = true
		if review.ReviewEventID != "" {
			eventKey := review.RepositoryID + "/" + review.ReviewEventID
			if reviewEvents[eventKey] {
				return errors.New("duplicate pull request review event identity")
			}
			reviewEvents[eventKey] = true
		}
		reviewCounts[pullRequestKey(review.RepositoryID, review.PullRequestNumber)]++
	}
	for key := range requests {
		if reviewCounts[key] == 0 {
			return errors.New("pull request has no explicit review choice")
		}
	}
	completed := make(map[string]PullRequestMergeIntent)
	intents := make(map[string]bool, len(snapshot.PullRequestMergeIntents))
	for _, intent := range snapshot.PullRequestMergeIntents {
		requestKey := pullRequestKey(intent.RepositoryID, intent.PullRequestNumber)
		if _, exists := requests[requestKey]; !exists || !revisions[revisionKey(intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)] {
			return errors.New("merge intent refers to an unknown pull request revision")
		}
		if err := validateMergeIntentRecord(intent, true); err != nil {
			return err
		}
		key := revisionKey(intent.RepositoryID, intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)
		if intents[key] {
			return errors.New("duplicate pull request merge intent")
		}
		intents[key] = true
		if intent.Status == MergeIntentComplete {
			if _, exists := completed[requestKey]; exists {
				return errors.New("pull request has multiple completed merge intents")
			}
			completed[requestKey] = intent
		}
	}
	for key := range completed {
		if requests[key].Status != PullRequestMerged {
			return errors.New("completed merge intent belongs to a pull request that is not merged")
		}
	}
	for key, record := range requests {
		if record.Status != PullRequestMerged {
			continue
		}
		intent, ok := completed[key]
		if !ok || intent.SourceOID != record.MergeSourceOID || intent.TargetOID != record.MergeTargetOID || intent.ResultOID != record.MergeOID || intent.ReceiptRef != record.MergeReceipt {
			return errors.New("merged pull request does not match a completed merge intent")
		}
	}
	autoMerges := make(map[string]bool, len(snapshot.PullRequestAutoMerges))
	for _, intent := range snapshot.PullRequestAutoMerges {
		key := pullRequestKey(intent.RepositoryID, intent.PullRequestNumber)
		record, exists := requests[key]
		if !exists {
			return errors.New("auto-merge intent refers to an unknown pull request")
		}
		// The same transaction that closes a request or records its merge
		// removes the intent, so a portable snapshot never carries one for a
		// non-open request.
		if record.Status != PullRequestOpen {
			return errors.New("auto-merge intent belongs to a pull request that is not open")
		}
		if intent.PullRequestNumber <= 0 || intent.EnabledAt.IsZero() || intent.UpdatedAt.IsZero() || intent.UpdatedAt.Before(intent.EnabledAt) {
			return errors.New("invalid auto-merge intent record")
		}
		if err := intent.EnabledBy.Validate(); err != nil {
			return fmt.Errorf("invalid auto-merge intent actor: %w", err)
		}
		if autoMerges[key] {
			return errors.New("duplicate auto-merge intent record")
		}
		autoMerges[key] = true
	}
	return nil
}

func validatePullRequestRecord(record PullRequest) error {
	if record.RepositoryID == "" || record.Number < 0 || !validText(record.Title, 500) || !validBranchText(record.SourceBranch) || !validBranchText(record.TargetBranch) || record.SourceBranch == record.TargetBranch || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return errors.New("invalid pull request metadata")
	}
	switch record.Status {
	case PullRequestCreating, PullRequestOpen, PullRequestClosed:
		if record.MergeSourceOID != "" || record.MergeTargetOID != "" || record.MergeOID != "" || record.MergeReceipt != "" || record.MergedAt != nil {
			return errors.New("unmerged pull request contains merge metadata")
		}
	case PullRequestMerged:
		if !validObjectID(record.MergeSourceOID) || !validObjectID(record.MergeTargetOID) || !validObjectID(record.MergeOID) || record.MergeReceipt == "" || record.MergedAt == nil || record.MergedAt.IsZero() {
			return errors.New("merged pull request metadata is incomplete")
		}
	default:
		return errors.New("invalid pull request status")
	}
	if !ValidPullRequestText(record.Body) {
		return errors.New("invalid pull request description")
	}
	if record.EditRevision < 0 || (record.EditRevision == 0) != (record.EditedAt == nil) || (record.EditedAt != nil && record.EditedAt.Before(record.CreatedAt)) ||
		(record.EditRevision == 0 && record.EditedBy != Actor{}) {
		return errors.New("invalid pull request edit record")
	}
	if record.Status != PullRequestMerged && record.MergedBy != (Actor{}) {
		return errors.New("unmerged pull request names who merged it")
	}
	for _, actor := range []Actor{record.CreatedBy, record.EditedBy, record.MergedBy} {
		if err := actor.Validate(); err != nil {
			return fmt.Errorf("invalid pull request actor: %w", err)
		}
	}
	return nil
}

// ValidPullRequestText accepts a description or review note: empty or UTF-8
// text of at most MaximumPullRequestTextBytes without NUL.
func ValidPullRequestText(value string) bool {
	return len(value) <= MaximumPullRequestTextBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// validReviewEventID accepts the 32 lowercase hexadecimal characters an event
// identity always had.
func validReviewEventID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if character < '0' || (character > '9' && character < 'a') || character > 'f' {
			return false
		}
	}
	return true
}

func validateReviewRecord(review PullRequestReview, requireSequence bool) error {
	if review.RepositoryID == "" || review.PullRequestNumber <= 0 || (requireSequence && review.Sequence <= 0) || !validObjectID(review.SourceOID) || !validObjectID(review.TargetOID) || review.CreatedAt.IsZero() {
		return errors.New("invalid pull request review metadata")
	}
	if review.ReviewEventID != "" && !validReviewEventID(review.ReviewEventID) {
		return errors.New("invalid pull request review event identity")
	}
	if !ValidPullRequestText(review.Note) {
		return errors.New("invalid pull request review note")
	}
	if err := review.Actor.Validate(); err != nil {
		return fmt.Errorf("invalid pull request review actor: %w", err)
	}
	switch review.Status {
	case ReviewPending:
		if review.ReviewerLabel != "" || review.Provenance != ReviewProvenanceRequest {
			return errors.New("invalid pending review provenance")
		}
	case ReviewSkipped:
		if review.ReviewEventID != "" {
			return errors.New("only pending review requests can carry an event identity")
		}
		if review.ReviewerLabel != "" || review.Provenance != ReviewProvenanceSkip {
			return errors.New("invalid skipped review provenance")
		}
	case ReviewNotRequested:
		if review.ReviewEventID != "" {
			return errors.New("only pending review requests can carry an event identity")
		}
		if review.ReviewerLabel != "" || review.Provenance != ReviewProvenanceDefault {
			return errors.New("invalid default review provenance")
		}
	case ReviewApproved, ReviewChangesRequested:
		if review.ReviewEventID != "" {
			return errors.New("only pending review requests can carry an event identity")
		}
		if !validText(review.ReviewerLabel, 200) || review.Provenance != ReviewProvenanceExternalTool {
			return errors.New("invalid submitted review provenance")
		}
	default:
		return errors.New("invalid pull request review status")
	}
	return nil
}

func validateMergeIntentRecord(intent PullRequestMergeIntent, requireTimes bool) error {
	if intent.RepositoryID == "" || intent.PullRequestNumber <= 0 || !validObjectID(intent.SourceOID) || !validObjectID(intent.TargetOID) || intent.ReceiptRef == "" {
		return errors.New("invalid pull request merge intent")
	}
	if requireTimes && (intent.CreatedAt.IsZero() || intent.UpdatedAt.IsZero() || intent.UpdatedAt.Before(intent.CreatedAt)) {
		return errors.New("invalid pull request merge intent time")
	}
	expectedReceipt := "refs/owngit/pull-requests/" + strconv.FormatInt(intent.PullRequestNumber, 10) + "/merge-receipt"
	if intent.ReceiptRef != expectedReceipt {
		return errors.New("invalid pull request merge receipt ref")
	}
	switch intent.Status {
	case MergeIntentPreparing:
		if intent.Mode != "" || intent.TreeOID != "" || intent.ResultOID != "" {
			return errors.New("preparing merge intent contains a result")
		}
	case MergeIntentPlanned:
		switch intent.Mode {
		case "fast_forward":
			if intent.ResultOID != intent.SourceOID || intent.TreeOID != "" {
				return errors.New("invalid planned fast-forward merge intent")
			}
		case "up_to_date":
			if intent.ResultOID != intent.TargetOID || intent.TreeOID != "" {
				return errors.New("invalid planned up-to-date merge intent")
			}
		case "merge_commit":
			if !validObjectID(intent.TreeOID) || intent.ResultOID != "" {
				return errors.New("invalid planned merge-commit intent")
			}
		default:
			return errors.New("invalid planned merge intent mode")
		}
	case MergeIntentReady, MergeIntentComplete:
		switch intent.Mode {
		case "fast_forward":
			if intent.ResultOID != intent.SourceOID || intent.TreeOID != "" {
				return errors.New("invalid fast-forward merge intent")
			}
		case "up_to_date":
			if intent.ResultOID != intent.TargetOID || intent.TreeOID != "" {
				return errors.New("invalid up-to-date merge intent")
			}
		case "merge_commit":
			if !validObjectID(intent.TreeOID) || !validObjectID(intent.ResultOID) {
				return errors.New("invalid merge-commit intent")
			}
		default:
			return errors.New("invalid merge intent mode")
		}
	default:
		return errors.New("invalid merge intent status")
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || (character > '9' && character < 'a') || character > 'f' {
			return false
		}
	}
	return true
}

func validText(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validBranchText(value string) bool {
	return value != "" && len(value) <= 255 && value == strings.TrimSpace(value) && importgit.ValidBranchName(value)
}

func pullRequestKey(repositoryID string, number int64) string {
	return repositoryID + "/" + strconv.FormatInt(number, 10)
}

func revisionKey(repositoryID string, number int64, sourceOID, targetOID string) string {
	return pullRequestKey(repositoryID, number) + "/" + sourceOID + "/" + targetOID
}
