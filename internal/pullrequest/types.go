package pullrequest

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"owngit/internal/repository"
	"owngit/internal/state"
)

const (
	// DefaultListLimit and MaximumListLimit are the page sizes of List.
	DefaultListLimit = 50
	MaximumListLimit = 100
	// ListStateAll lists every state.
	ListStateAll = "all"
)

// ListInput selects a page of List: pull requests in State (open, closed,
// merged, or all when empty), at most Limit of them (DefaultListLimit when
// zero), all with a number below Before (the newest when zero).
type ListInput struct {
	State  string
	Limit  int
	Before int64
}

// ListResult is one page, newest first. Next is the Before value of the
// following page and is zero on the last page.
type ListResult struct {
	Items []*View
	Next  int64
}

func (input ListInput) check() (status string, limit int, err error) {
	switch input.State {
	case "", ListStateAll:
	case state.PullRequestOpen, state.PullRequestClosed, state.PullRequestMerged:
		status = input.State
	default:
		return "", 0, NewProblem("invalid_list_state", "The state must be open, closed, merged, or all.")
	}
	limit = input.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 0 || limit > MaximumListLimit {
		return "", 0, NewProblem("invalid_list_limit", "The page size must be from 1 to "+strconv.Itoa(MaximumListLimit)+".")
	}
	if input.Before < 0 {
		return "", 0, NewProblem("invalid_list_before", "The continuation must be a pull request number.")
	}
	return status, limit, nil
}

const (
	// MaximumTextRequestBytes bounds a JSON request that carries pull request
	// text: a title and a description or review note at their limits, which
	// JSON may escape to six bytes for each byte.
	MaximumTextRequestBytes = 6*(500+state.MaximumPullRequestTextBytes) + 16<<10
	// MaximumReviewNotes is how many review notes a pull request shows, newest
	// first. Older notes stay recorded and backed up.
	MaximumReviewNotes = 5
)

type Problem struct {
	Code    string
	Message string
	Details any
	Cause   error
	// git marks a cause that is the error of a Git command.
	git bool
}

// gitProblem is a repository_unavailable Problem whose cause is a Git command's error.
func gitProblem(message string, cause error) *Problem {
	return &Problem{Code: "repository_unavailable", Message: message, Cause: cause, git: true}
}

func (problem *Problem) Error() string {
	if problem == nil {
		return ""
	}
	if reason := problem.plainReason(); reason != "" {
		return strings.TrimSuffix(problem.Message, ".") + ": " + reason + "."
	}
	return problem.Message
}

// plainReason names a cause the owner can act on in a few words. Only a
// timeout or cancellation is named; other causes stay out of the line, and
// Cause still carries them. The Git and disk hint is only for a Git command
// that ran out of time; a busy repository already names its cause in the
// message.
func (problem *Problem) plainReason() string {
	switch {
	case problem.Code == "repository_busy" || errors.Is(problem.Cause, repository.ErrRepositoryInUse):
		return ""
	case errors.Is(problem.Cause, context.DeadlineExceeded):
		if problem.git {
			return "Git timed out, so the repository folder or its disk may not be answering"
		}
		return "it did not finish in time"
	case errors.Is(problem.Cause, context.Canceled):
		return "the operation was cancelled"
	}
	return ""
}

func (problem *Problem) Unwrap() error {
	if problem == nil {
		return nil
	}
	return problem.Cause
}

func NewProblem(code, message string) *Problem {
	return &Problem{Code: code, Message: message}
}

func AsProblem(err error) *Problem {
	var problem *Problem
	if errors.As(err, &problem) {
		return problem
	}
	return &Problem{Code: "internal_error", Message: "The pull request operation could not be completed.", Cause: err}
}

type Revision struct {
	Branch string `json:"branch"`
	OID    string `json:"oid,omitempty"`
	Status string `json:"status"`
}

const (
	ReadFailureCheckConfiguration = "check_configuration"
	ReadFailureCheckEvidence      = "check_evidence"
	ReadFailureReviewEvidence     = "review_evidence"
)

// ReadFailure identifies an OwnGit evidence query that failed. It is separate
// from an unavailable check or review execution, which is evidence supplied by
// the tool that attempted the work.
type ReadFailure struct {
	Code string `json:"code"`
}

type Review struct {
	Status         string     `json:"status"`
	SourceOID      string     `json:"source_oid,omitempty"`
	TargetOID      string     `json:"target_oid,omitempty"`
	ReviewerLabel  string     `json:"reviewer_label,omitempty"`
	Provenance     string     `json:"provenance,omitempty"`
	Independent    bool       `json:"independent"`
	ExecutedChecks bool       `json:"executed_checks"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	// Actor is the access that recorded this review, request or skip, when
	// OwnGit recorded it. The reviewer label is what the caller supplied.
	Actor *state.Actor `json:"actor,omitempty"`
	// ReadFailure reports that OwnGit could not read its own review evidence,
	// including the review notes. Review remains advisory, so this never
	// becomes a merge blocker.
	ReadFailure *ReadFailure `json:"read_failure,omitempty"`
}

// ReviewNote is what a reviewer wrote with a decision, bound to the exact
// source and target revisions reviewed. It is untrusted text, kept verbatim.
type ReviewNote struct {
	Decision      string `json:"decision"`
	ReviewerLabel string `json:"reviewer_label"`
	SourceOID     string `json:"source_oid"`
	TargetOID     string `json:"target_oid"`
	// Current is true while both revisions are the pull request's current
	// ones (its merged pair once merged). Once either branch moves, the note
	// is about earlier revisions; it is still shown.
	Current     bool         `json:"current"`
	Note        string       `json:"note"`
	SubmittedAt time.Time    `json:"submitted_at"`
	Actor       *state.Actor `json:"actor,omitempty"`
}

type Checks struct {
	// Status is absent when no evidence exists, stale when only an older
	// revision has evidence, or the real attempt status otherwise. It is never
	// a reused success for a different revision.
	Status     string `json:"status"`
	Configured bool   `json:"configured"`
	// Passed carries the raw exit result of the bound attempt. It can be true
	// while TestedCommit is false, so render both.
	Passed bool `json:"passed"`
	// Blocking is retained for compatibility and is always false: checks are
	// advisory and never hold a merge.
	Blocking bool `json:"blocking"`
	// Advisory is always true and states the contract explicitly.
	Advisory bool `json:"advisory"`
	// RevisionOID is the revision the evidence actually tested. It can differ
	// from the current source when Status is stale.
	RevisionOID string `json:"revision_oid,omitempty"`
	// WorktreeState is clean, dirty, or unknown. A dirty or unknown worktree
	// means the tested commit is not proven to be the bytes that ran.
	WorktreeState string `json:"worktree_state,omitempty"`
	// TestedCommit is true only for a clean worktree on the current revision.
	TestedCommit         bool       `json:"tested_commit"`
	Stale                bool       `json:"stale"`
	ConfigurationVersion int64      `json:"configuration_version,omitempty"`
	AttemptID            string     `json:"attempt_id,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	Summary              string     `json:"summary,omitempty"`
	// LogStatus is found, expired, or missing, or unavailable while the
	// saved raw log retention cannot be read. The durable attempt record
	// outlives its disposable log.
	LogStatus    string     `json:"log_status,omitempty"`
	LogExpiresAt *time.Time `json:"log_expires_at,omitempty"`
	LogTruncated bool       `json:"log_truncated"`
	// Protection and ExecutionScope state what was actually established.
	Protection     string `json:"protection,omitempty"`
	ExecutionScope string `json:"execution_scope,omitempty"`
	// CredentialID is the server-authenticated helper credential that
	// submitted the evidence.
	CredentialID string `json:"credential_id,omitempty"`
	// JobID is the automatic job OwnGit admitted for this attempt, empty for a
	// manual or historical helper submission. It is the recorded authority for
	// describing a run as the server's own work rather than the operator's.
	// It is projected for the browser only and stays out of the pull request
	// API response, which this release does not extend.
	JobID string `json:"-"`
	// TaskID links the attempt back to its durable task without requiring a
	// second lookup by the browser layer.
	TaskID string `json:"task_id,omitempty"`
	// RegisteredAt is the server observation available for a pending attempt.
	// FinishedAt remains absent until a completion is accepted.
	RegisteredAt *time.Time `json:"registered_at,omitempty"`
	// LogError records why the disposable raw log could not be stored or read.
	LogError string `json:"log_error,omitempty"`
	// CleanupFailed prevents a raw successful exit from being presented as a
	// clean completed run when owned-process cleanup was not confirmed.
	CleanupFailed bool `json:"cleanup_failed"`
	// ReadFailure reports which OwnGit evidence query failed. Checks remain
	// advisory, so this never becomes a merge blocker.
	ReadFailure *ReadFailure `json:"read_failure,omitempty"`
}

type Blocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Eligibility struct {
	Eligible bool      `json:"eligible"`
	Blockers []Blocker `json:"blockers"`
}

// Auto-merge statuses and the stable reason codes shown while an enabled
// intent waits. Waiting is retryable: a later event converges again. Blocked
// names a fact about the current source revision that publishing must wait
// past; a new check result or a source push re-evaluates it.
const (
	AutoMergeWaiting = "waiting"
	AutoMergeBlocked = "blocked"
)

const (
	// AutoMergeWaitingChecks: the current source commit has no check attempt
	// yet, or only an older revision's evidence.
	AutoMergeWaitingChecks = "waiting_for_checks"
	// AutoMergeChecksRunning: the current source commit's latest attempt is
	// still pending.
	AutoMergeChecksRunning = "checks_running"
	// AutoMergeEvidenceUnreadable: the check configuration or evidence store
	// could not be read; the decision retries.
	AutoMergeEvidenceUnreadable = "evidence_unreadable"
	// AutoMergeMergeConflict: the current pair does not merge cleanly; a ref
	// change can resolve it, so this is waiting rather than blocked.
	AutoMergeMergeConflict = "merge_conflict"
	// AutoMergeRepositoryUnavailable: a Git or state read failed temporarily.
	AutoMergeRepositoryUnavailable = "repository_unavailable"
	// AutoMergeReconciling: every gate passed and the next convergence pass
	// publishes the merge.
	AutoMergeReconciling = "reconciling"
	// AutoMergeChecksFailed: the latest terminal attempt for the current
	// source commit did not pass.
	AutoMergeChecksFailed = "checks_failed"
	// AutoMergeChecksUntrustedWorktree: a passing attempt did not run against
	// a clean worktree of the tested commit (or its cleanup failed).
	AutoMergeChecksUntrustedWorktree = "checks_untrusted_worktree"
	// AutoMergeChecksConfigurationChanged: the passing attempt used a check
	// configuration that differs from the workflow committed at the source.
	AutoMergeChecksConfigurationChanged = "checks_configuration_changed"
)

// AutoMerge is the state of one pull request's opt-in automatic merge. It is
// present only while the pull request is open and the intent is enabled.
type AutoMerge struct {
	Enabled bool `json:"enabled"`
	// Status is waiting or blocked.
	Status string `json:"status,omitempty"`
	// Reason is one of the stable AutoMerge* reason codes.
	Reason string `json:"reason,omitempty"`
	// Message is an English sentence for the reason; browser surfaces
	// localize stable codes themselves.
	Message string `json:"message,omitempty"`
	// SourceOID and TargetOID are the current heads this state was evaluated
	// against.
	SourceOID string    `json:"source_oid,omitempty"`
	TargetOID string    `json:"target_oid,omitempty"`
	EnabledAt time.Time    `json:"enabled_at,omitempty"`
	EnabledBy *state.Actor `json:"enabled_by,omitempty"`
}

// ExistingPullRequest is the detail of a pull_request_exists problem: the open
// pull request that already covers the requested branch pair.
type ExistingPullRequest struct {
	Number int64 `json:"number"`
}

type MergeResult struct {
	// Mode is fast_forward, merge_commit, or up_to_date. up_to_date means the
	// target already contained the source, so no commit was written and OID is
	// the unchanged target commit.
	Mode       string    `json:"mode"`
	OID        string    `json:"oid"`
	ReceiptRef string    `json:"receipt_ref"`
	MergedAt   time.Time `json:"merged_at"`
}

type View struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	// Body is the description. A list leaves it out (nil) to stay bounded;
	// show and every change include it, empty or not.
	Body *string `json:"body,omitempty"`
	// EditRevision counts title and description edits. An edit names the
	// revision it read and is refused once another edit moved it.
	EditRevision     int64        `json:"edit_revision"`
	EditedAt         *time.Time   `json:"edited_at,omitempty"`
	State            string       `json:"state"`
	Source           Revision     `json:"source"`
	Target           Revision     `json:"target"`
	Review           Review       `json:"review"`
	Checks           Checks       `json:"checks"`
	MergeEligibility Eligibility  `json:"merge_eligibility"`
	Merge            *MergeResult `json:"merge,omitempty"`
	// AutoMerge is present only while an automatic merge is enabled for an
	// open pull request.
	AutoMerge *AutoMerge `json:"auto_merge,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// CreatedBy, EditedBy and MergedBy name the access that made each change,
	// when OwnGit recorded it.
	CreatedBy *state.Actor `json:"created_by,omitempty"`
	EditedBy  *state.Actor `json:"edited_by,omitempty"`
	MergedBy  *state.Actor `json:"merged_by,omitempty"`
	// ReviewNotes are the newest review notes, at most MaximumReviewNotes,
	// newest first; ReviewNotesTruncated says that older ones exist. A list
	// leaves them out.
	ReviewNotes          []ReviewNote `json:"review_notes,omitempty"`
	ReviewNotesTruncated bool         `json:"review_notes_truncated,omitempty"`
}

// Diff is what a pull request changes between an exact source and target
// revision, counted from their merge base, as the pull request diff API
// returns it.
type Diff struct {
	OK         bool         `json:"ok"`
	Repository string       `json:"repository"`
	Number     int64        `json:"number"`
	State      string       `json:"state"`
	Source     DiffRevision `json:"source"`
	Target     DiffRevision `json:"target"`
	// MergeBase is the single merge base the changes are counted from. It is
	// empty when Unavailable is set.
	MergeBase string `json:"merge_base,omitempty"`
	// Moved is set when the requested pair is no longer the pull request's
	// current pair. Current then holds the current pair.
	Moved   bool         `json:"moved"`
	Current *DiffCurrent `json:"current,omitempty"`
	// Unavailable is no_merge_base or multiple_merge_bases when there is no
	// single merge base to count from; Files and Patch are then empty.
	Unavailable string     `json:"unavailable,omitempty"`
	Files       []DiffFile `json:"files"`
	// Incomplete is set when Files does not list every changed file. It
	// implies Truncated.
	Incomplete bool `json:"incomplete"`
	// Truncated is set when Patch leaves out the changes of some files. Patch
	// always ends at a file boundary, so each file in it is whole.
	Truncated bool `json:"truncated"`
	// Reason says why output is missing: output_limit (the diff reached its
	// size limit), time_limit (the diff ran out of time; a retry may read
	// more), too_large (too many of the changed files are too large to compare
	// here, so no patch read was attempted and each such file is listed with
	// too_large), or response_limit (cut to fit the API response).
	Reason string `json:"reason,omitempty"`
	Patch  string `json:"patch"`
}

// DiffRevision is one side of the diffed pair.
type DiffRevision struct {
	Branch string `json:"branch"`
	OID    string `json:"oid"`
}

// DiffCurrent is the pull request's current pair when a pinned pair moved.
type DiffCurrent struct {
	Source Revision `json:"source"`
	Target Revision `json:"target"`
}

// DiffFile is one changed file with its line counts.
type DiffFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// Additions and Deletions are the changed line counts. Both are omitted
	// when the counts are unknown, which is the case for a file this computer
	// did not compare as text (see TooLarge); a count of zero is sent.
	Additions *int `json:"additions,omitempty"`
	Deletions *int `json:"deletions,omitempty"`
	// Binary says the file's content is not text, so it has no line counts.
	Binary bool `json:"binary"`
	// TooLarge says this computer did not compare the file as text because
	// reading it would need more memory than it gives Git at once, so its
	// counts and its diff are unknown. Binary stays false then.
	TooLarge bool `json:"too_large,omitempty"`
}

type CreateInput struct {
	Repository string `json:"repository,omitempty"`
	Title      string `json:"title"`
	// Body is the optional Markdown description.
	Body         string `json:"body,omitempty"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	ReviewChoice string `json:"review"`
	// SourceOID and TargetOID are optional expected heads. They are validated
	// inside the repository lock, so a form cannot create a pull request for a
	// revision the user never saw.
	SourceOID string `json:"source_oid,omitempty"`
	TargetOID string `json:"target_oid,omitempty"`
	// Actor, in this input and the others, is the access that authorized
	// the request. The server sets it; it is never read from a request body.
	Actor state.Actor `json:"-"`
}

// EditInput replaces the title, the description, or both. EditRevision is
// the edit revision the caller read; the edit is refused when it moved.
type EditInput struct {
	EditRevision *int64      `json:"edit_revision"`
	Title        *string     `json:"title,omitempty"`
	Body         *string     `json:"body,omitempty"`
	Actor        state.Actor `json:"-"`
}

type RevisionInput struct {
	SourceOID string      `json:"source_oid"`
	TargetOID string      `json:"target_oid"`
	Actor     state.Actor `json:"-"`
}

type ReviewSubmitInput struct {
	SourceOID     string `json:"source_oid"`
	TargetOID     string `json:"target_oid"`
	Decision      string `json:"decision"`
	ReviewerLabel string `json:"reviewer_label"`
	// Note is optional Markdown about the reviewed revisions.
	Note  string      `json:"note,omitempty"`
	Actor state.Actor `json:"-"`
}

type SuccessEnvelope struct {
	OK          bool    `json:"ok"`
	PullRequest *View   `json:"pull_request,omitempty"`
	Items       []*View `json:"pull_requests,omitempty"`
}

type ErrorEnvelope struct {
	OK    bool             `json:"ok"`
	Error ErrorDescription `json:"error"`
}

type ErrorDescription struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

type OperationErrorDetails struct {
	OperationStarted *bool `json:"operation_started"`
}
