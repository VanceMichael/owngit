package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"owngit/internal/importgit"
	"owngit/internal/repository"
	"owngit/internal/state"
)

type CurrentRevision struct {
	PullRequest   state.PullRequest
	SourceOID     string
	TargetOID     string
	NewlyObserved bool
}

// Service owns pull request records and their Git effects. Store and
// Repositories are required: every production constructor sets them, so no
// method treats one as absent.
type Service struct {
	Store         *state.Store
	Repositories  *repository.Manager
	Now           func() time.Time
	CompleteMerge func(context.Context, state.PullRequestMergeIntent, time.Time) error
	// OnChange wakes advisory check reconciliation. It must not block or run
	// repository commands because mutations may still hold the repository lock.
	OnChange func(string)
	// beforeAutoMergePublish, when set, runs inside an automatic merge's lock
	// hold immediately before the Git publication. Production leaves it nil;
	// tests use it as a deterministic publication fence.
	beforeAutoMergePublish func(repositoryID string, number int64)
}

func (service *Service) Create(ctx context.Context, input CreateInput) (*View, error) {
	return service.create(ctx, input, false)
}

// CreateExact accepts full source and target refs from the browser's pickers.
// The JSON API continues to use Create and its legacy operand rule.
func (service *Service) CreateExact(ctx context.Context, input CreateInput) (*View, error) {
	return service.create(ctx, input, true)
}

func (service *Service) create(ctx context.Context, input CreateInput, exact bool) (*View, error) {
	if service.OnChange != nil {
		defer service.OnChange(input.Repository)
	}
	if err := repository.ValidateID(input.Repository); err != nil {
		return nil, NewProblem("invalid_repository", "The repository identifier is invalid.")
	}
	input, err := input.CheckText()
	if err != nil {
		return nil, err
	}
	var reviewStatus string
	switch input.ReviewChoice {
	case "request":
		reviewStatus = state.ReviewPending
	case "skip":
		reviewStatus = state.ReviewSkipped
	case "":
		// Review is optional. Omitting it must not create a hidden waiting
		// state that forces an explicit skip later.
		reviewStatus = state.ReviewNotRequested
	default:
		return nil, NewProblem("invalid_review_choice", "Review must be request, skip, or omitted.")
	}
	repositoryPath, err := service.repositoryPath(ctx, input.Repository)
	if err != nil {
		return nil, err
	}
	lock := service.Repositories.Locks.For(input.Repository)
	if err := lockForRequest(ctx, lock.LockContext); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	source, err := service.normalizeBranch(ctx, repositoryPath, input.SourceBranch, exact)
	if err != nil {
		return nil, err
	}
	target, err := service.normalizeBranch(ctx, repositoryPath, input.TargetBranch, exact)
	if err != nil {
		return nil, err
	}
	if source == target {
		return nil, NewProblem("same_branch", "The source and target branches must be different.")
	}
	sourceHead, err := service.resolveBranch(ctx, repositoryPath, source)
	if err != nil {
		return nil, err
	}
	targetHead, err := service.resolveBranch(ctx, repositoryPath, target)
	if err != nil {
		return nil, err
	}
	if err := requireCommitHead("source", sourceHead); err != nil {
		return nil, err
	}
	if err := requireCommitHead("target", targetHead); err != nil {
		return nil, err
	}
	// An expected head is checked inside the lock, so a branch that moved
	// between the form render and the submit is reported instead of silently
	// creating a pull request for a different revision.
	if (input.SourceOID != "" && input.SourceOID != sourceHead.OID) || (input.TargetOID != "" && input.TargetOID != targetHead.OID) {
		return nil, staleRevisionProblem(sourceHead.OID, targetHead.OID)
	}
	// One open pull request per source and target branch pair. A second one
	// would compete for the same merge, so the caller is sent to the first.
	if problem, err := service.openPairProblem(ctx, input.Repository, source, target); err != nil || problem != nil {
		if err != nil {
			return nil, err
		}
		return nil, problem
	}
	return service.createForHeadsLocked(ctx, repositoryPath, state.PullRequestCreation{
		RepositoryID: input.Repository, Title: input.Title, Body: input.Body, SourceBranch: source, TargetBranch: target,
		SourceOID: sourceHead.OID, TargetOID: targetHead.OID, InitialReview: reviewStatus, CreatedBy: input.Actor,
	}, sourceHead, targetHead)
}

func (service *Service) createForHeadsLocked(ctx context.Context, repositoryPath string, creation state.PullRequestCreation, sourceHead, targetHead branchHead) (*View, error) {
	record, err := service.Store.BeginPullRequestCreation(ctx, creation, service.now())
	if err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "The provisional pull request metadata could not be saved.", Cause: err}
	}
	if err := service.ensureRevisionRefs(ctx, repositoryPath, record, sourceHead.OID, targetHead.OID); err != nil {
		reconciled, activated, reconcileErr := service.reconcileProvisionalCreationLocked(ctx, repositoryPath, record, service.readRef)
		if reconcileErr != nil {
			return nil, reconcileErr
		}
		if !activated {
			return nil, err
		}
		return service.viewForHeads(ctx, repositoryPath, reconciled, sourceHead, targetHead, false)
	}
	record, err = service.Store.ActivatePullRequestCreation(ctx, creation.RepositoryID, record.Number, service.now())
	if err != nil {
		return nil, &Problem{Code: "pull_request_creation_reconciliation_pending", Message: "The pull request revision was retained, but its visible state still needs reconciliation.", Cause: err}
	}
	return service.viewForHeads(ctx, repositoryPath, record, sourceHead, targetHead, false)
}

func (service *Service) reconcileProvisionalCreationLocked(ctx context.Context, repositoryPath string, record state.PullRequest, readRef refReader) (state.PullRequest, bool, error) {
	revisions, err := service.Store.PullRequestRevisionsFor(ctx, record.RepositoryID, record.Number)
	if err != nil {
		return state.PullRequest{}, false, &Problem{Code: "state_unavailable", Message: "The provisional pull request revision could not be read.", Cause: err}
	}
	if len(revisions) != 1 {
		return state.PullRequest{}, false, NewProblem("repository_integrity_error", "The provisional pull request does not have exactly one retained revision.")
	}
	revision := revisions[0]
	sourceRef, targetRef := RevisionRefNames(record.Number, revision.SourceOID, revision.TargetOID)
	sourceOID, sourceExists, err := readRef(ctx, repositoryPath, sourceRef)
	if err != nil {
		return state.PullRequest{}, false, NewProblem("pull_request_creation_reconciliation_pending", "The provisional pull request source revision could not be read safely.")
	}
	targetOID, targetExists, err := readRef(ctx, repositoryPath, targetRef)
	if err != nil {
		return state.PullRequest{}, false, NewProblem("pull_request_creation_reconciliation_pending", "The provisional pull request target revision could not be read safely.")
	}
	if !sourceExists && !targetExists {
		if err := service.Store.DeletePullRequestCreation(ctx, record.RepositoryID, record.Number); err != nil {
			return state.PullRequest{}, false, &Problem{Code: "state_unavailable", Message: "The failed pull request creation could not be discarded.", Cause: err}
		}
		return state.PullRequest{}, false, nil
	}
	if !sourceExists || !targetExists || sourceOID != revision.SourceOID || targetOID != revision.TargetOID {
		return state.PullRequest{}, false, NewProblem("repository_integrity_error", "The provisional pull request revision refs are incomplete or have unexpected values.")
	}
	activated, err := service.Store.ActivatePullRequestCreation(ctx, record.RepositoryID, record.Number, service.now())
	if err != nil {
		return state.PullRequest{}, false, &Problem{Code: "pull_request_creation_reconciliation_pending", Message: "The retained pull request creation could not be made visible.", Cause: err}
	}
	return activated, true, nil
}

// List returns one newest-first page of the repository's pull requests. A page
// that does not reach the oldest one carries Next, the number to continue
// below.
func (service *Service) List(ctx context.Context, repositoryID string, input ListInput) (*ListResult, error) {
	status, limit, err := input.check()
	if err != nil {
		return nil, err
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.RLockContext); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
	records, more, err := service.Store.PullRequestSummaries(ctx, repositoryID, status, input.Before, limit)
	if err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "Pull request metadata could not be read.", Cause: err}
	}
	// One ref listing serves every pull request, so the list starts the same
	// number of ref reads however many pull requests it shows. A list leaves
	// out each description and its review notes, so its size stays bounded.
	var heads map[string]branchHead
	result := &ListResult{Items: make([]*View, 0, len(records))}
	for _, record := range records {
		if heads == nil && record.Status != state.PullRequestMerged {
			if heads, err = service.branchHeads(ctx, repositoryPath); err != nil {
				return nil, err
			}
		}
		source, target := recordHeads(record, heads)
		view, err := service.summaryForHeads(ctx, repositoryPath, record, source, target)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, view)
	}
	if more {
		result.Next = records[len(records)-1].Number
	}
	return result, nil
}

func (service *Service) Show(ctx context.Context, repositoryID string, number int64) (*View, error) {
	if number <= 0 {
		return nil, NewProblem("invalid_pull_request_number", "The pull request number must be positive.")
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.RLockContext); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
	record, err := service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return nil, err
	}
	return service.readViewLocked(ctx, repositoryPath, record, true)
}

// Edit replaces the title, the description, or both, when the pull request
// is still at the edit revision the caller read. A later edit is never
// overwritten: the caller gets stale_edit and the current revision. An edit
// that changes nothing records nothing. Revisions, reviews, checks and merge
// records stay as they are, in every state of the pull request.
func (service *Service) Edit(ctx context.Context, repositoryID string, number int64, input EditInput) (*View, error) {
	if input.EditRevision == nil || *input.EditRevision < 0 {
		return nil, NewProblem("invalid_edit_revision", "An edit needs the edit_revision of the pull request it changes, from show.")
	}
	if input.Title == nil && input.Body == nil {
		return nil, NewProblem("invalid_edit", "An edit needs a new title, a new description, or both.")
	}
	input, err := input.CheckText()
	if err != nil {
		return nil, err
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	// Text changes no ref, so reading the branches is all the lock is for.
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.RLockContext); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
	record, err := service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return nil, err
	}
	if record.EditRevision != *input.EditRevision {
		return nil, staleEditProblem(record.EditRevision)
	}
	edit := state.PullRequestEdit{
		RepositoryID: repositoryID, Number: number, BasedOn: record.EditRevision,
		Title: record.Title, Body: record.Body, EditedBy: input.Actor,
	}
	if input.Title != nil {
		edit.Title = *input.Title
	}
	if input.Body != nil {
		edit.Body = *input.Body
	}
	if edit.Title != record.Title || edit.Body != record.Body {
		edited, err := service.Store.EditPullRequest(ctx, edit, service.now())
		if errors.Is(err, state.ErrPullRequestEdited) {
			current, readErr := service.requirePullRequest(ctx, repositoryID, number)
			if readErr != nil {
				return nil, readErr
			}
			return nil, staleEditProblem(current.EditRevision)
		}
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The pull request text could not be saved.", Cause: err}
		}
		record = edited
	}
	return service.readViewLocked(ctx, repositoryPath, record, false)
}

func (service *Service) RequestReview(ctx context.Context, repositoryID string, number int64, input RevisionInput) (*View, error) {
	return service.recordReview(ctx, repositoryID, number, state.PullRequestReview{
		SourceOID: input.SourceOID, TargetOID: input.TargetOID, Status: state.ReviewPending, Actor: input.Actor,
	})
}

func (service *Service) SkipReview(ctx context.Context, repositoryID string, number int64, input RevisionInput) (*View, error) {
	return service.recordReview(ctx, repositoryID, number, state.PullRequestReview{
		SourceOID: input.SourceOID, TargetOID: input.TargetOID, Status: state.ReviewSkipped, Actor: input.Actor,
	})
}

func (service *Service) SubmitReview(ctx context.Context, repositoryID string, number int64, input ReviewSubmitInput) (*View, error) {
	if input.Decision != state.ReviewApproved && input.Decision != state.ReviewChangesRequested {
		return nil, NewProblem("invalid_review_decision", "A submitted review must be approved or changes_requested.")
	}
	input, err := input.CheckText()
	if err != nil {
		return nil, err
	}
	return service.recordReview(ctx, repositoryID, number, state.PullRequestReview{
		SourceOID: input.SourceOID, TargetOID: input.TargetOID, Status: input.Decision, ReviewerLabel: input.ReviewerLabel, Note: input.Note, Actor: input.Actor,
	})
}

// recordReview appends review, which names the exact revision pair, its
// status and what was supplied with it, when that pair is still current.
func (service *Service) recordReview(ctx context.Context, repositoryID string, number int64, review state.PullRequestReview) (*View, error) {
	sourceOID, targetOID := review.SourceOID, review.TargetOID
	if err := validateExpectedRevision(sourceOID, targetOID); err != nil {
		return nil, err
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.LockContext); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	record, err := service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return nil, err
	}
	if record.Status != state.PullRequestOpen {
		return nil, notOpenProblem(record.Status)
	}
	sourceHead, targetHead, err := service.currentHeads(ctx, repositoryPath, record)
	if err != nil {
		return nil, err
	}
	if sourceHead.OID != sourceOID || targetHead.OID != targetOID {
		return nil, staleRevisionProblem(sourceHead.OID, targetHead.OID)
	}
	if err := service.bindRevision(ctx, repositoryPath, record, sourceOID, targetOID); err != nil {
		return nil, err
	}
	review.RepositoryID, review.PullRequestNumber, review.CreatedAt = repositoryID, number, service.now()
	switch review.Status {
	case state.ReviewPending:
		review.Provenance = state.ReviewProvenanceRequest
	case state.ReviewSkipped:
		review.Provenance = state.ReviewProvenanceSkip
	default:
		review.Provenance = state.ReviewProvenanceExternalTool
	}
	if _, err := service.Store.AppendPullRequestReview(ctx, review); err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "The review decision could not be saved.", Cause: err}
	}
	record, err = service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return nil, err
	}
	return service.viewForHeads(ctx, repositoryPath, record, sourceHead, targetHead, false)
}

func (service *Service) Merge(ctx context.Context, repositoryID string, number int64, input RevisionInput) (*View, error) {
	if service.OnChange != nil {
		defer service.OnChange(repositoryID)
	}
	if err := validateExpectedRevision(input.SourceOID, input.TargetOID); err != nil {
		return nil, err
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.LockContext); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	record, err := service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return nil, err
	}
	if record.Status == state.PullRequestOpen {
		record, err = service.completePublishedMergeLocked(ctx, repositoryPath, record)
		if err != nil {
			return nil, err
		}
	}
	if record.Status == state.PullRequestMerged {
		if record.MergeSourceOID != input.SourceOID || record.MergeTargetOID != input.TargetOID {
			return nil, NewProblem("stale_revision", "The pull request was merged for a different source and target revision.")
		}
		intent, ok, err := service.Store.PullRequestMergeIntent(ctx, repositoryID, number, input.SourceOID, input.TargetOID)
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The merge receipt metadata could not be read.", Cause: err}
		}
		if !ok || intent.ResultOID != record.MergeOID {
			return nil, NewProblem("repository_integrity_error", "The merged pull request does not have matching merge intent metadata.")
		}
		published, err := service.validateReceipt(ctx, repositoryPath, intent)
		if err != nil {
			return nil, err
		}
		if !published {
			return nil, NewProblem("repository_integrity_error", "The merged pull request receipt is missing.")
		}
		return service.readViewLocked(ctx, repositoryPath, record, false)
	}
	if record.Status != state.PullRequestOpen {
		return nil, notOpenProblem(record.Status)
	}

	sourceHead, targetHead, err := service.currentHeads(ctx, repositoryPath, record)
	if err != nil {
		return nil, err
	}
	if sourceHead.OID != input.SourceOID || targetHead.OID != input.TargetOID {
		return nil, staleRevisionProblem(sourceHead.OID, targetHead.OID)
	}
	return service.mergeCurrentPairLocked(ctx, repositoryPath, record, sourceHead, targetHead, mergeOptions{actor: input.Actor})
}

// mergeOptions carries the caller identity and the optional guard of the
// background auto-merge path. Manual merges leave requireIntent nil.
type mergeOptions struct {
	actor state.Actor
	// requireIntent is checked after the merge gate and again immediately
	// before publication, both while the repository write lock is held. A
	// false answer aborts the merge without publishing anything.
	requireIntent func() (bool, error)
}

// errAutoMergeAborted marks an auto-merge that reached the merge machinery
// but whose intent disappeared before publication. The caller published
// nothing and reports no failure.
var errAutoMergeAborted = NewProblem("auto_merge_aborted", "The automatic merge was canceled before publication.")

// mergeCurrentPairLocked binds the current pair, plans it, publishes the merge
// transaction and reconciles its receipt. The caller holds the repository
// write lock and has already established that the request is open and that
// the heads are the revision this merge acts on. Manual and automatic merges
// share every step so both produce one identical, idempotent receipt.
func (service *Service) mergeCurrentPairLocked(ctx context.Context, repositoryPath string, record state.PullRequest, sourceHead, targetHead branchHead, options mergeOptions) (*View, error) {
	number := record.Number
	input := RevisionInput{SourceOID: sourceHead.OID, TargetOID: targetHead.OID, Actor: options.actor}
	if err := service.bindRevision(ctx, repositoryPath, record, input.SourceOID, input.TargetOID); err != nil {
		return nil, err
	}
	view, err := service.viewForHeads(ctx, repositoryPath, record, sourceHead, targetHead, false)
	if err != nil {
		return nil, err
	}
	if !view.MergeEligibility.Eligible {
		return nil, &Problem{Code: "merge_blocked", Message: "The pull request is not eligible to merge.", Details: view.MergeEligibility}
	}

	if options.requireIntent != nil {
		enabled, err := options.requireIntent()
		if err != nil {
			return nil, err
		}
		if !enabled {
			return nil, errAutoMergeAborted
		}
	}

	if err := service.discardSupersededMergePlansLocked(ctx, repositoryPath, record, input); err != nil {
		return nil, err
	}
	intent, exists, err := service.Store.PullRequestMergeIntent(ctx, record.RepositoryID, number, input.SourceOID, input.TargetOID)
	if err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "The durable merge intent could not be read.", Cause: err}
	}
	if !exists {
		intent = state.PullRequestMergeIntent{
			RepositoryID: record.RepositoryID, PullRequestNumber: number, SourceOID: input.SourceOID, TargetOID: input.TargetOID,
			ReceiptRef: MergeReceiptRef(number), CreatedAt: service.now(), Status: state.MergeIntentPreparing,
		}
	}
	if intent.Status == state.MergeIntentReady {
		published, reconcileErr := service.reconcileIntentLocked(ctx, repositoryPath, intent, input.Actor)
		if reconcileErr != nil {
			return nil, reconcileErr
		}
		if published {
			record, err = service.requirePullRequest(ctx, record.RepositoryID, number)
			if err != nil {
				return nil, err
			}
			return service.readViewLocked(ctx, repositoryPath, record, false)
		}
	}
	if err := service.requireMergeCapability(ctx); err != nil {
		return nil, err
	}
	if intent.Status == state.MergeIntentPreparing {
		planned, err := service.planMerge(ctx, repositoryPath, intent)
		if err != nil {
			return nil, err
		}
		if err := service.ensurePlannedMergeTree(ctx, repositoryPath, record, planned, true); err != nil {
			return nil, err
		}
		// Planning cannot publish a receipt. Store only an accepted plan.
		if !exists {
			if _, err := service.Store.BeginPullRequestMerge(ctx, intent); err != nil {
				return nil, &Problem{Code: "state_unavailable", Message: "The durable merge intent could not be saved.", Cause: err}
			}
		}
		intent, err = service.Store.UpdatePullRequestMergeIntent(ctx, planned)
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The merge plan could not be saved.", Cause: err}
		}
	}
	switch intent.Status {
	case state.MergeIntentPlanned:
		intent.ResultOID, err = service.ensureMergeResult(ctx, repositoryPath, record, intent, true)
		if err != nil {
			return nil, err
		}
		intent.Status = state.MergeIntentReady
		intent.UpdatedAt = service.now()
		intent, err = service.Store.UpdatePullRequestMergeIntent(ctx, intent)
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The merge result could not be saved before publication.", Cause: err}
		}
	case state.MergeIntentReady:
		if _, err := service.ensureMergeResult(ctx, repositoryPath, record, intent, true); err != nil {
			return nil, err
		}
	default:
		return nil, NewProblem("repository_integrity_error", "The merge intent is in an unexpected state.")
	}

	// The last word before the Git transaction belongs to the durable
	// auto-merge intent, re-read under the same write lock the cancel and
	// close paths take.
	if options.requireIntent != nil {
		enabled, err := options.requireIntent()
		if err != nil {
			return nil, err
		}
		if service.beforeAutoMergePublish != nil {
			service.beforeAutoMergePublish(record.RepositoryID, number)
		}
		if !enabled {
			if discardErr := service.discardUnpublishedMergePlansLocked(ctx, repositoryPath, record, nil); discardErr != nil {
				return nil, discardErr
			}
			return nil, errAutoMergeAborted
		}
	}

	publishErr := service.publishMerge(ctx, repositoryPath, record, intent)
	published, reconcileErr := service.reconcileIntentLocked(ctx, repositoryPath, intent, input.Actor)
	if reconcileErr != nil {
		return nil, reconcileErr
	}
	if !published {
		currentSource, currentTarget, readErr := service.currentHeads(ctx, repositoryPath, record)
		if readErr != nil {
			return nil, NewProblem("merge_reconciliation_pending", "The merge result could not be read back safely. Retry after the repository is available.")
		}
		if currentSource.OID != intent.SourceOID || currentTarget.OID != intent.TargetOID {
			return nil, staleRevisionProblem(currentSource.OID, currentTarget.OID)
		}
		return nil, &Problem{Code: "git_update_failed", Message: "Git did not publish the merge transaction.", Cause: publishErr}
	}
	record, err = service.requirePullRequest(ctx, record.RepositoryID, number)
	if err != nil {
		return nil, err
	}
	return service.readViewLocked(ctx, repositoryPath, record, false)
}

// Close closes an open pull request without merging it. It stays in the
// history and can be reopened. Closing a closed pull request returns it
// unchanged, and a merged one cannot be closed.
func (service *Service) Close(ctx context.Context, repositoryID string, number int64) (*View, error) {
	return service.setClosed(ctx, repositoryID, number, true)
}

// Reopen reopens a closed pull request. It is refused with
// pull_request_exists while another pull request is open for the same branch
// pair. Reopening an open pull request returns it unchanged.
func (service *Service) Reopen(ctx context.Context, repositoryID string, number int64) (*View, error) {
	return service.setClosed(ctx, repositoryID, number, false)
}

func (service *Service) setClosed(ctx context.Context, repositoryID string, number int64, closed bool) (*View, error) {
	if service.OnChange != nil {
		defer service.OnChange(repositoryID)
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.LockContext); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	record, err := service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return nil, err
	}
	if closed && record.Status == state.PullRequestOpen {
		// A merge whose Git update was published but not yet recorded
		// completes first, so a published merge is never recorded as closed.
		record, err = service.completePublishedMergeLocked(ctx, repositoryPath, record)
		if err != nil {
			return nil, err
		}
	}
	wanted, from := state.PullRequestClosed, state.PullRequestOpen
	if !closed {
		wanted, from = state.PullRequestOpen, state.PullRequestClosed
	}
	switch record.Status {
	case wanted:
		return service.readViewLocked(ctx, repositoryPath, record, false)
	case state.PullRequestMerged:
		return nil, NewProblem("pull_request_merged", "A merged pull request cannot be closed or reopened.")
	case from:
	default:
		return nil, NewProblem("repository_integrity_error", "The pull request has an unexpected state.")
	}
	if !closed {
		if problem, err := service.openPairProblem(ctx, record.RepositoryID, record.SourceBranch, record.TargetBranch); err != nil || problem != nil {
			if err != nil {
				return nil, err
			}
			return nil, problem
		}
	}
	record, err = service.Store.SetPullRequestClosed(ctx, repositoryID, number, closed, service.now())
	if err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "The pull request state could not be saved.", Cause: err}
	}
	// A close canceled any auto-merge intent in the same state change; plans
	// the intent prepared but never published are abandoned with it. A reopen
	// restores neither.
	if closed {
		if err := service.discardUnpublishedMergePlansLocked(ctx, repositoryPath, record, nil); err != nil {
			return nil, err
		}
	}
	return service.readViewLocked(ctx, repositoryPath, record, false)
}

// completePublishedMergeLocked records a merge of this pull request that Git
// already published. It returns the current record, merged or not. Who asked
// for that merge is not known here, so nobody is recorded.
func (service *Service) completePublishedMergeLocked(ctx context.Context, repositoryPath string, record state.PullRequest) (state.PullRequest, error) {
	intents, err := service.Store.PullRequestMergeIntents(ctx, record.RepositoryID, record.Number, true)
	if err != nil {
		return state.PullRequest{}, &Problem{Code: "state_unavailable", Message: "The merge metadata could not be read.", Cause: err}
	}
	for _, intent := range intents {
		if intent.RepositoryID != record.RepositoryID || intent.PullRequestNumber != record.Number || intent.Status != state.MergeIntentReady {
			continue
		}
		published, err := service.reconcileIntentLocked(ctx, repositoryPath, intent, state.Actor{})
		if err != nil {
			return state.PullRequest{}, err
		}
		if published {
			return service.requirePullRequest(ctx, record.RepositoryID, record.Number)
		}
	}
	return record, nil
}

// openPairProblem reports the open pull request that already covers a branch
// pair, as pull_request_exists, or nil when there is none.
func (service *Service) openPairProblem(ctx context.Context, repositoryID, source, target string) (*Problem, error) {
	existing, exists, err := service.Store.OpenPullRequestForBranches(ctx, repositoryID, source, target)
	if err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "Pull request metadata could not be read.", Cause: err}
	}
	if !exists {
		return nil, nil
	}
	return &Problem{
		Code:    "pull_request_exists",
		Message: fmt.Sprintf("Pull request #%d is already open for this source and target branch.", existing.Number),
		Details: ExistingPullRequest{Number: existing.Number},
	}, nil
}

func notOpenProblem(status string) *Problem {
	if status == state.PullRequestClosed {
		return NewProblem("pull_request_not_open", "The pull request is closed. Reopen it first.")
	}
	return NewProblem("pull_request_not_open", "The pull request is already merged.")
}

// ObserveCurrentRevisionsAfter durably binds one bounded circular page of
// current open pull-request head pairs and returns those exact current pairs.
// Advancing after to the last returned request prevents a busy request or the
// newest fixed page from permanently starving older open requests.
func (service *Service) ObserveCurrentRevisionsAfter(ctx context.Context, repositoryID string, after int64, limit int) ([]CurrentRevision, bool, error) {
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return nil, false, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lock.LockContext(ctx); err != nil {
		return nil, false, err
	}
	// The configured-check poller calls this every interval and after every
	// push to any repository. A call that binds nothing changes no ref, so it
	// releases the lock without invalidating the cached ref snapshot. The mark
	// is set before binding starts, so a failed or partial bind still does.
	mayChangeRefs := false
	defer func() {
		if mayChangeRefs {
			lock.Unlock()
		} else {
			lock.UnlockWithoutRefChanges()
		}
	}()
	records, more, err := service.Store.OpenPullRequestsAfter(ctx, repositoryID, after, limit)
	if err != nil {
		return nil, false, &Problem{Code: "state_unavailable", Message: "Open pull requests could not be read for revision observation.", Cause: err}
	}
	revisions := make([]CurrentRevision, 0, len(records))
	for _, record := range records {
		source, target, err := service.readHeads(ctx, repositoryPath, record)
		if err != nil {
			return revisions, more, err
		}
		if source.Status != "commit" || target.Status != "commit" {
			revisions = append(revisions, CurrentRevision{PullRequest: record})
			continue
		}
		exists, err := service.Store.HasPullRequestRevision(ctx, repositoryID, record.Number, source.OID, target.OID)
		if err != nil {
			return revisions, more, &Problem{Code: "state_unavailable", Message: "Pull request revision history could not be read.", Cause: err}
		}
		if !exists {
			mayChangeRefs = true
			if err := service.bindRevision(ctx, repositoryPath, record, source.OID, target.OID); err != nil {
				return revisions, more, err
			}
		}
		revisions = append(revisions, CurrentRevision{
			PullRequest: record, SourceOID: source.OID, TargetOID: target.OID, NewlyObserved: !exists,
		})
	}
	return revisions, more, nil
}

// ReconcileAll recovers the pull request state of every repository. Offline
// backup and restore use it; a serving process recovers each repository as
// part of its preparation instead (see RecoverRepositoryLocked).
func (service *Service) ReconcileAll(ctx context.Context) error {
	repositories, err := service.Store.Repositories(ctx)
	if err != nil {
		return fmt.Errorf("read repositories for pull request reconciliation: %w", err)
	}
	for _, stored := range repositories {
		repositoryPath, err := service.repositoryPath(ctx, stored.ID)
		if err != nil {
			return err
		}
		lock := service.Repositories.Locks.For(stored.ID)
		if err := lock.LockContext(ctx); err != nil {
			return err
		}
		err = service.RecoverRepositoryLocked(ctx, stored.ID, repositoryPath)
		lock.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// RecoverRepositoryLocked recovers one repository's pull request state:
// provisional creations, protected revision refs, and unfinished merges. The
// caller holds the repository write lock and supplies the repository path,
// so repository preparation can run it before the repository is served.
func (service *Service) RecoverRepositoryLocked(ctx context.Context, repositoryID, repositoryPath string) error {
	provisional, err := service.Store.ProvisionalPullRequests(ctx, repositoryID)
	if err != nil {
		return fmt.Errorf("read provisional pull requests for %s: %w", repositoryID, err)
	}
	revisions, err := service.Store.PullRequestRevisions(ctx, repositoryID)
	if err != nil {
		return fmt.Errorf("read pull request revisions for %s: %w", repositoryID, err)
	}
	if len(provisional) != 0 || len(revisions) != 0 {
		// One listing answers every pull request ref read below, instead of
		// one Git process per ref.
		known, err := service.listPullRequestRefs(ctx, repositoryPath)
		if err != nil {
			return fmt.Errorf("read pull request refs for %s: %w", repositoryID, err)
		}
		for _, record := range provisional {
			if _, _, err := service.reconcileProvisionalCreationLocked(ctx, repositoryPath, record, known.read); err != nil {
				return fmt.Errorf("reconcile pull request creation %s#%d: %w", repositoryID, record.Number, err)
			}
		}
		for _, revision := range revisions {
			if err := service.ensureStoredRevisionRefs(ctx, repositoryPath, revision, known.read); err != nil {
				return fmt.Errorf("repair pull request revision %s#%d: %w", repositoryID, revision.PullRequestNumber, err)
			}
		}
	}

	intents, err := service.Store.PullRequestMergeIntents(ctx, repositoryID, 0, true)
	if err != nil {
		return fmt.Errorf("read incomplete pull request merges: %w", err)
	}
	for _, intent := range intents {
		if intent.RepositoryID != repositoryID || intent.Status == state.MergeIntentPreparing {
			continue
		}
		_, ok, err := service.Store.PullRequest(ctx, intent.RepositoryID, intent.PullRequestNumber)
		if err != nil {
			return fmt.Errorf("read pull request %s#%d for merge reconciliation: %w", intent.RepositoryID, intent.PullRequestNumber, err)
		}
		if !ok {
			return fmt.Errorf("merge intent refers to missing pull request %s#%d", intent.RepositoryID, intent.PullRequestNumber)
		}
		switch intent.Status {
		case state.MergeIntentReady:
			_, err = service.reconcileIntentLocked(ctx, repositoryPath, intent, state.Actor{})
		case state.MergeIntentPlanned:
			err = service.validateProtectedMergeObjects(ctx, repositoryPath, intent)
		default:
			err = NewProblem("repository_integrity_error", "The merge intent is in an unexpected state.")
		}
		if err != nil {
			return fmt.Errorf("reconcile pull request %s#%d: %w", intent.RepositoryID, intent.PullRequestNumber, err)
		}
	}
	return nil
}

// reconcileIntentLocked records a merge that Git published for intent, made
// by mergedBy, and reports whether Git published it.
func (service *Service) reconcileIntentLocked(ctx context.Context, repositoryPath string, intent state.PullRequestMergeIntent, mergedBy state.Actor) (bool, error) {
	published, err := service.validateReceipt(ctx, repositoryPath, intent)
	if err != nil || !published {
		return published, err
	}
	completedAt := service.now()
	complete := service.CompleteMerge
	if complete == nil {
		complete = func(ctx context.Context, intent state.PullRequestMergeIntent, completedAt time.Time) error {
			return service.Store.CompletePullRequestMerge(ctx, intent, completedAt, mergedBy)
		}
	}
	if err := complete(ctx, intent, completedAt); err != nil {
		return true, &Problem{Code: "merge_reconciliation_pending", Message: "Git published the merge, but its durable state still needs reconciliation. Retry the same merge command.", Cause: err}
	}
	return true, nil
}

func (service *Service) validateReceipt(ctx context.Context, repositoryPath string, intent state.PullRequestMergeIntent) (bool, error) {
	if err := service.validateProtectedMergeObjects(ctx, repositoryPath, intent); err != nil {
		return false, err
	}
	receiptOID, exists, err := service.readRef(ctx, repositoryPath, intent.ReceiptRef)
	if err != nil {
		return false, NewProblem("merge_reconciliation_pending", "The merge receipt could not be read safely. Retry after the repository is available.")
	}
	if !exists {
		return false, nil
	}
	record, err := service.requirePullRequest(ctx, intent.RepositoryID, intent.PullRequestNumber)
	if err != nil {
		return false, err
	}
	intents, err := service.Store.PullRequestMergeIntents(ctx, intent.RepositoryID, intent.PullRequestNumber, false)
	if err != nil {
		return false, &Problem{Code: "state_unavailable", Message: "The merge receipt metadata could not be read.", Cause: err}
	}
	owner, err := state.PullRequestMergeReceiptOwner(record, intents, receiptOID)
	if err != nil {
		return false, &Problem{Code: "repository_integrity_error", Message: "The protected merge receipt does not match the durable merge intent.", Cause: err}
	}
	if owner.SourceOID != intent.SourceOID || owner.TargetOID != intent.TargetOID {
		// The receipt belongs to another plan, whose protected result must
		// still be intact before this plan can be left unpublished.
		if err := service.validateProtectedMergeObjects(ctx, repositoryPath, owner); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (service *Service) discardSupersededMergePlansLocked(ctx context.Context, repositoryPath string, record state.PullRequest, revision RevisionInput) error {
	return service.discardUnpublishedMergePlansLocked(ctx, repositoryPath, record, &revision)
}

// discardUnpublishedMergePlansLocked abandons the pull request's merge plans
// that published no receipt. With keep set it preserves one revision's plan
// (the merge in progress); with keep nil it discards every plan (an
// auto-merge cancel or close). Callers must have completed or refused a
// published receipt under the lock before this runs.
func (service *Service) discardUnpublishedMergePlansLocked(ctx context.Context, repositoryPath string, record state.PullRequest, keep *RevisionInput) error {
	receiptOID, exists, err := service.readRef(ctx, repositoryPath, MergeReceiptRef(record.Number))
	if err != nil {
		return err
	}
	intents, err := service.Store.PullRequestMergeIntents(ctx, record.RepositoryID, record.Number, true)
	if err != nil {
		return &Problem{Code: "state_unavailable", Message: "The merge metadata could not be read.", Cause: err}
	}
	if exists {
		if _, err := state.PullRequestMergeReceiptOwner(record, intents, receiptOID); err != nil {
			return &Problem{Code: "repository_integrity_error", Message: "The protected merge receipt does not match the durable merge intent.", Cause: err}
		}
		return NewProblem("merge_reconciliation_pending", "The published merge still needs reconciliation. Retry after the repository is available.")
	}
	for _, intent := range intents {
		if intent.RepositoryID != record.RepositoryID || intent.PullRequestNumber != record.Number {
			continue
		}
		if keep != nil && intent.SourceOID == keep.SourceOID && intent.TargetOID == keep.TargetOID {
			continue
		}
		if intent.Status != state.MergeIntentPreparing {
			if err := service.validateProtectedMergeObjects(ctx, repositoryPath, intent); err != nil {
				return err
			}
		}
		if err := service.Store.DeleteUnpublishedPullRequestMerge(ctx, intent); err != nil {
			return &Problem{Code: "state_unavailable", Message: "The superseded merge plan could not be discarded.", Cause: err}
		}
	}
	return nil
}

// readViewLocked builds a view from the current heads without retaining a new
// revision pair. Callers that record a decision or a merge bind the pair
// explicitly, so passive reads stay read-only.
func (service *Service) readViewLocked(ctx context.Context, repositoryPath string, record state.PullRequest, deepAutoMerge bool) (*View, error) {
	var heads map[string]branchHead
	if record.Status != state.PullRequestMerged {
		var err error
		if heads, err = service.branchHeads(ctx, repositoryPath); err != nil {
			return nil, err
		}
	}
	source, target := recordHeads(record, heads)
	return service.viewForHeads(ctx, repositoryPath, record, source, target, deepAutoMerge)
}

// recordHeads returns the pair a pull request is about: its current branch
// heads from heads, which branchHeads read, or the recorded merge revisions
// of a merged pull request, which needs no heads.
func recordHeads(record state.PullRequest, heads map[string]branchHead) (branchHead, branchHead) {
	if record.Status == state.PullRequestMerged {
		return branchHead{Branch: record.SourceBranch, OID: record.MergeSourceOID, Status: "commit"},
			branchHead{Branch: record.TargetBranch, OID: record.MergeTargetOID, Status: "commit"}
	}
	return headOf(heads, record.SourceBranch), headOf(heads, record.TargetBranch)
}

// viewForHeads is the whole view of a pull request at source and target:
// its summary with the description and the newest review notes. The shallow
// auto-merge projection is attached while the summary is built; when
// deepAutoMerge is set (show only), the projection additionally works out the
// Git mergeability of the current pair under the held lock.
func (service *Service) viewForHeads(ctx context.Context, repositoryPath string, record state.PullRequest, source, target branchHead, deepAutoMerge bool) (*View, error) {
	view, err := service.summaryForHeads(ctx, repositoryPath, record, source, target)
	if err != nil {
		return nil, err
	}
	if deepAutoMerge && view.AutoMerge != nil {
		intent, ok, err := service.Store.PullRequestAutoMerge(ctx, record.RepositoryID, record.Number)
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be read.", Cause: err}
		}
		if ok {
			view.AutoMerge = service.deepAutoMergeViewLocked(ctx, repositoryPath, record, intent, source, target, view.Checks)
		}
	}
	body := record.Body
	view.Body = &body
	notes, more, err := service.Store.PullRequestReviewNotes(ctx, record.RepositoryID, record.Number, MaximumReviewNotes)
	if err != nil {
		// Review notes are review evidence: a failed read is reported like
		// one of the review record, never as no notes.
		view.Review = Review{ReadFailure: &ReadFailure{Code: ReadFailureReviewEvidence}}
		return view, nil
	}
	for _, note := range notes {
		view.ReviewNotes = append(view.ReviewNotes, ReviewNote{
			Decision: note.Status, ReviewerLabel: note.ReviewerLabel, SourceOID: note.SourceOID, TargetOID: note.TargetOID,
			Current: source.Status == "commit" && target.Status == "commit" && note.SourceOID == source.OID && note.TargetOID == target.OID,
			Note:    note.Note, SubmittedAt: note.CreatedAt, Actor: recordedActor(note.Actor),
		})
	}
	view.ReviewNotesTruncated = more
	return view, nil
}

// recordedActor is actor, or nil when nobody was recorded.
func recordedActor(actor state.Actor) *state.Actor {
	if actor == (state.Actor{}) {
		return nil
	}
	return &actor
}

// summaryForHeads is the view of a pull request at source and target without
// its description and review notes, as a list shows it.
func (service *Service) summaryForHeads(ctx context.Context, repositoryPath string, record state.PullRequest, source, target branchHead) (*View, error) {
	view := &View{
		Repository: record.RepositoryID, Number: record.Number, Title: record.Title, State: record.Status,
		EditRevision: record.EditRevision, EditedAt: record.EditedAt,
		Source:    Revision{Branch: record.SourceBranch, OID: source.OID, Status: source.Status},
		Target:    Revision{Branch: record.TargetBranch, OID: target.OID, Status: target.Status},
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		CreatedBy: recordedActor(record.CreatedBy), EditedBy: recordedActor(record.EditedBy), MergedBy: recordedActor(record.MergedBy),
	}
	// Checks and review are advisory, so an advisory read failure is reported
	// in the view instead of failing the whole operation, including a merge.
	view.Checks = service.checksForRevision(ctx, repositoryPath, record, source)
	if source.Status == "commit" && target.Status == "commit" {
		review, exists, err := service.Store.PullRequestReviewForRevision(ctx, record.RepositoryID, record.Number, source.OID, target.OID)
		if err != nil {
			view.Review = Review{
				Independent: false, ExecutedChecks: false,
				ReadFailure: &ReadFailure{Code: ReadFailureReviewEvidence},
			}
		} else if exists {
			submitted := review.CreatedAt
			view.Review = Review{
				Status: review.Status, SourceOID: review.SourceOID, TargetOID: review.TargetOID,
				ReviewerLabel: review.ReviewerLabel, Provenance: review.Provenance,
				Independent: false, ExecutedChecks: false, SubmittedAt: &submitted, Actor: recordedActor(review.Actor),
			}
		} else {
			view.Review = Review{Status: "decision_required", Independent: false, ExecutedChecks: false}
		}
	} else {
		view.Review = Review{Status: "decision_required", Independent: false, ExecutedChecks: false}
	}
	view.MergeEligibility = evaluateEligibility(record.Status, source, target)
	if record.Status == state.PullRequestOpen {
		intent, ok, err := service.Store.PullRequestAutoMerge(ctx, record.RepositoryID, record.Number)
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be read.", Cause: err}
		}
		if ok {
			view.AutoMerge = service.autoMergeView(intent, record.Status, source, target, view.Checks)
		}
	}
	if record.Status == state.PullRequestMerged {
		intent, ok, err := service.Store.PullRequestMergeIntent(ctx, record.RepositoryID, record.Number, record.MergeSourceOID, record.MergeTargetOID)
		if err != nil {
			return nil, &Problem{Code: "state_unavailable", Message: "The merge metadata could not be read.", Cause: err}
		}
		if !ok || intent.ResultOID != record.MergeOID || record.MergedAt == nil {
			return nil, NewProblem("repository_integrity_error", "The merged pull request metadata is incomplete.")
		}
		view.Merge = &MergeResult{Mode: intent.Mode, OID: record.MergeOID, ReceiptRef: record.MergeReceipt, MergedAt: *record.MergedAt}
	}
	return view, nil
}

// checksForRevision reports the real evidence bound to the current source
// revision. When only an older source revision recorded for this pull request
// has evidence, or the evidence ran checks other than those committed in the
// source revision's workflow file, the result is stale rather than a reused
// success. Configurations recorded for other branches do not apply. Evidence
// for other branches is never shown. Checks are advisory: they never block a
// merge, and an advisory read failure is reported as unavailable instead of
// failing the caller.
func (service *Service) checksForRevision(ctx context.Context, repositoryPath string, record state.PullRequest, source branchHead) Checks {
	repositoryID := record.RepositoryID
	checks := Checks{Status: "absent", Advisory: true}
	_, configured, err := service.Store.LatestCheckConfiguration(ctx, repositoryID)
	if err != nil {
		checks.Status = ""
		checks.ReadFailure = &ReadFailure{Code: ReadFailureCheckConfiguration}
		return checks
	}
	if configured {
		checks.Configured = true
	}
	if source.Status != "commit" {
		return checks
	}
	attempt, exists, err := service.Store.LatestCheckAttemptForRevision(ctx, repositoryID, source.OID)
	if err != nil {
		checks.Status = ""
		checks.ReadFailure = &ReadFailure{Code: ReadFailureCheckEvidence}
		return checks
	}
	if !exists {
		latest, hasLatest, err := service.Store.LatestCheckAttemptForPullRequestHistory(ctx, repositoryID, record.Number)
		if err != nil {
			checks.Status = ""
			checks.ReadFailure = &ReadFailure{Code: ReadFailureCheckEvidence}
			return checks
		}
		if hasLatest {
			checks = service.checksFromAttempt(ctx, latest, source.OID)
			checks.Status = "stale"
			checks.Stale = true
			checks.Summary = "Latest check ran for " + shortOID(latest.RevisionOID) + " (" + latest.Status + ")"
		}
		return checks
	}
	checks = service.checksFromAttempt(ctx, attempt, source.OID)
	// The configuration that applies is the one committed in the source
	// revision. Without one, the evidence ran explicitly chosen checks and
	// nothing newer can replace them.
	committed, hasCommitted, err := service.committedChecks(ctx, repositoryPath, source.OID)
	if err != nil {
		return Checks{Advisory: true, Configured: true, ReadFailure: &ReadFailure{Code: ReadFailureCheckConfiguration}}
	}
	if !hasCommitted {
		return checks
	}
	used, exists, err := service.Store.CheckConfiguration(ctx, repositoryID, attempt.ConfigurationVersion)
	if err != nil {
		return Checks{Advisory: true, Configured: true, ReadFailure: &ReadFailure{Code: ReadFailureCheckConfiguration}}
	}
	if !exists || !slices.Equal(used.Checks, committed) {
		checks.Status = "stale"
		checks.Stale = true
		checks.Summary = attempt.Summary + "; the check configuration changed"
	}
	return checks
}

func (service *Service) checksFromAttempt(ctx context.Context, attempt state.CheckAttempt, sourceOID string) Checks {
	worktreeState := attempt.EffectiveWorktreeState()
	registered := attempt.CreatedAt
	checks := Checks{
		Status: attempt.Status, Advisory: true, Configured: true,
		RevisionOID: attempt.RevisionOID, WorktreeState: worktreeState,
		AttemptID: attempt.ID, TaskID: attempt.TaskID, ConfigurationVersion: attempt.ConfigurationVersion,
		Summary: attempt.Summary, Protection: attempt.Protection, ExecutionScope: attempt.ExecutionScope,
		CredentialID: attempt.CredentialID, JobID: attempt.JobID,
		LogTruncated: attempt.LogTruncated, LogError: attempt.LogError,
		RegisteredAt: &registered, CleanupFailed: attempt.CleanupFailed(),
	}
	if attempt.Status != state.AttemptPending {
		finished := attempt.FinishedAt
		if !finished.IsZero() {
			checks.FinishedAt = &finished
		}
		// Whether the log is kept follows the retention saved now; while
		// that cannot be read, neither can the log.
		checks.LogStatus = state.CheckLogUnavailable
		if retention, err := service.Store.CheckLogRetention(ctx); err == nil {
			checks.LogStatus = service.Store.CheckLogState(attempt, retention, service.now())
			checks.LogExpiresAt = retention.LogExpiry(attempt)
		}
	}
	checks.Passed = attempt.Status == state.AttemptPassed
	checks.TestedCommit = attempt.Status != state.AttemptPending && attempt.RevisionOID == sourceOID && worktreeState == state.WorktreeClean && !checks.CleanupFailed
	return checks
}

func shortOID(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

// evaluateEligibility keeps only hard Git and state blockers. Review and check
// results are advisory, so a pending or changes-requested review no longer
// holds a merge.
func evaluateEligibility(requestStatus string, source, target branchHead) Eligibility {
	eligibility := Eligibility{}
	if requestStatus == state.PullRequestMerged {
		eligibility.Blockers = append(eligibility.Blockers, Blocker{Code: "already_merged", Message: "The pull request is already merged."})
		return eligibility
	}
	if requestStatus == state.PullRequestClosed {
		eligibility.Blockers = append(eligibility.Blockers, Blocker{Code: "pull_request_not_open", Message: "The pull request is closed."})
		return eligibility
	}
	for _, head := range []struct {
		name string
		head branchHead
	}{{"source", source}, {"target", target}} {
		switch head.head.Status {
		case "missing":
			eligibility.Blockers = append(eligibility.Blockers, Blocker{Code: head.name + "_branch_missing", Message: "The " + head.name + " branch no longer exists."})
		case "not_commit":
			eligibility.Blockers = append(eligibility.Blockers, Blocker{Code: head.name + "_not_commit", Message: "The " + head.name + " branch does not point to a commit."})
		}
	}
	eligibility.Eligible = len(eligibility.Blockers) == 0
	return eligibility
}

func (service *Service) readHeads(ctx context.Context, repositoryPath string, record state.PullRequest) (branchHead, branchHead, error) {
	source, err := service.resolveBranch(ctx, repositoryPath, record.SourceBranch)
	if err != nil {
		return branchHead{}, branchHead{}, err
	}
	target, err := service.resolveBranch(ctx, repositoryPath, record.TargetBranch)
	if err != nil {
		return branchHead{}, branchHead{}, err
	}
	return source, target, nil
}

func (service *Service) currentHeads(ctx context.Context, repositoryPath string, record state.PullRequest) (branchHead, branchHead, error) {
	source, target, err := service.readHeads(ctx, repositoryPath, record)
	if err != nil {
		return branchHead{}, branchHead{}, err
	}
	if err := requireCommitHead("source", source); err != nil {
		return branchHead{}, branchHead{}, err
	}
	if err := requireCommitHead("target", target); err != nil {
		return branchHead{}, branchHead{}, err
	}
	return source, target, nil
}

func (service *Service) bindRevision(ctx context.Context, repositoryPath string, record state.PullRequest, sourceOID, targetOID string) error {
	if err := service.ensureRevisionRefs(ctx, repositoryPath, record, sourceOID, targetOID); err != nil {
		return err
	}
	if err := service.Store.RecordPullRequestRevision(ctx, state.PullRequestRevision{
		RepositoryID: record.RepositoryID, PullRequestNumber: record.Number, SourceOID: sourceOID, TargetOID: targetOID, RecordedAt: service.now(),
	}); err != nil {
		return &Problem{Code: "state_unavailable", Message: "The pull request revision binding could not be saved.", Cause: err}
	}
	return nil
}

func (service *Service) requirePullRequest(ctx context.Context, repositoryID string, number int64) (state.PullRequest, error) {
	if number <= 0 {
		return state.PullRequest{}, NewProblem("invalid_pull_request_number", "The pull request number must be positive.")
	}
	record, exists, err := service.Store.PullRequest(ctx, repositoryID, number)
	if err != nil {
		return state.PullRequest{}, &Problem{Code: "state_unavailable", Message: "Pull request metadata could not be read.", Cause: err}
	}
	if !exists {
		return state.PullRequest{}, NewProblem("pull_request_not_found", "The pull request does not exist.")
	}
	return record, nil
}

// lockForRequest takes a repository lock with take unless ctx ends first,
// and then reports the repository as busy.
func lockForRequest(ctx context.Context, take func(context.Context) error) error {
	if err := take(ctx); err != nil {
		if errors.Is(err, repository.ErrStorageChanged) {
			return &Problem{Code: "storage_changed", Message: "The repository folder changed after OwnGit started, so OwnGit stopped writing to it. Put the original folder back to continue, or restart OwnGit to use the folder now in its place.", Cause: err}
		}
		if errors.Is(err, repository.ErrStorageUnavailable) {
			return &Problem{Code: "repository_unavailable", Message: "The repository storage is unavailable.", Cause: err}
		}
		return &Problem{Code: "repository_busy", Message: "Another Git operation, such as a push or a clone, is using the repository. Try again in a moment.", Cause: errors.Join(repository.ErrRepositoryInUse, err)}
	}
	return nil
}

func (service *Service) repositoryPath(ctx context.Context, repositoryID string) (string, error) {
	if err := repository.ValidateID(repositoryID); err != nil {
		return "", NewProblem("invalid_repository", "The repository identifier is invalid.")
	}
	path, _, exists, err := service.Repositories.ExistingPath(ctx, repositoryID)
	if errors.Is(err, repository.ErrRepositoryPreparing) {
		return "", &Problem{Code: "repository_preparing", Message: "The repository is being prepared. Try again later.", Cause: err}
	}
	if err != nil {
		return "", &Problem{Code: "repository_unavailable", Message: "The repository storage is unavailable.", Cause: err}
	}
	if !exists {
		return "", NewProblem("repository_not_found", "The repository does not exist.")
	}
	return path, nil
}

// ValidateBranchInput checks the caller's branch operand syntax and limits,
// returning its branch-name suffix without checking repository membership.
func (service *Service) ValidateBranchInput(ctx context.Context, value string, exact bool) (string, error) {
	const legacyInputBytes = 255
	if value != strings.TrimSpace(value) || value == "" || (!exact && len(value) > legacyInputBytes) || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", NewProblem("invalid_branch", "The branch name is invalid.")
	}
	if strings.HasPrefix(value, "refs/owngit/") {
		return "", NewProblem("reserved_ref", "The refs/owngit namespace is reserved for OwnGit.")
	}
	if strings.HasPrefix(value, "refs/heads/") {
		value = strings.TrimPrefix(value, "refs/heads/")
	} else if exact {
		return "", NewProblem("invalid_branch", "Pull requests require branch refs under refs/heads.")
	}
	if value == "HEAD" || value == "" || len(value) > 255 || !importgit.ValidBranchName(value) {
		return "", NewProblem("invalid_branch", "The branch name is invalid.")
	}
	return value, nil
}

func (service *Service) normalizeBranch(ctx context.Context, repositoryPath, input string, exact bool) (string, error) {
	value, err := service.ValidateBranchInput(ctx, input, exact)
	if err != nil {
		return "", err
	}
	full, err := service.Repositories.SelectBranchRefWithEligibility(ctx, repositoryPath, input, exact, func(operand string) (bool, error) {
		_, err := service.ValidateBranchInput(ctx, operand, false)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return err == nil, nil
	})
	var ambiguous *repository.AmbiguousBranchError
	if errors.As(err, &ambiguous) {
		return "", &Problem{Code: "ambiguous_branch", Message: ambiguous.Error(), Cause: ambiguous}
	}
	if errors.Is(err, repository.ErrNotFound) {
		// Keep missing-source and missing-target status classification below.
		return value, nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(full, "refs/heads/"), nil
}

func requireCommitHead(name string, head branchHead) error {
	switch head.Status {
	case "commit":
		return nil
	case "missing":
		return NewProblem(name+"_branch_missing", "The "+name+" branch does not exist.")
	case "not_commit":
		return NewProblem(name+"_not_commit", "The "+name+" branch does not point to a commit.")
	default:
		return NewProblem("repository_integrity_error", "The "+name+" branch has an unknown state.")
	}
}

func validateExpectedRevision(sourceOID, targetOID string) error {
	if !validOID(sourceOID) || !validOID(targetOID) {
		return NewProblem("invalid_revision", "Exact lowercase source and target commit object IDs are required.")
	}
	return nil
}

// StaleEdit is the detail of a stale_edit problem: the edit revision the pull
// request has now.
type StaleEdit struct {
	CurrentEditRevision int64 `json:"current_edit_revision"`
}

func staleEditProblem(current int64) *Problem {
	return &Problem{
		Code: "stale_edit", Message: "The pull request was edited after you read it. Show it again and reapply your change.",
		Details: StaleEdit{CurrentEditRevision: current},
	}
}

func staleRevisionProblem(sourceOID, targetOID string) *Problem {
	return &Problem{
		Code: "stale_revision", Message: "The source or target branch changed. Inspect the pull request and use its current object IDs.",
		Details: map[string]string{"current_source_oid": sourceOID, "current_target_oid": targetOID},
	}
}

func (service *Service) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}
