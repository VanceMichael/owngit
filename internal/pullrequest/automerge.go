package pullrequest

import (
	"context"

	"owngit/internal/state"
)

// autoMergeEvidenceStatus maps the existing advisory check projection of the
// current source revision to the auto-merge gate conclusion. It never looks
// at review: reviews stay advisory. When the heads are not eligible, the
// first stable merge blocker code is the blocked reason.
func autoMergeEvidenceStatus(checks Checks, eligible bool, blocker string) (status string, reason string) {
	if !eligible {
		return AutoMergeBlocked, blocker
	}
	if checks.ReadFailure != nil {
		return AutoMergeWaiting, AutoMergeEvidenceUnreadable
	}
	switch checks.Status {
	case state.AttemptPending:
		return AutoMergeWaiting, AutoMergeChecksRunning
	case "", "absent":
		return AutoMergeWaiting, AutoMergeWaitingChecks
	case state.AttemptPassed:
		if !checks.TestedCommit {
			return AutoMergeBlocked, AutoMergeChecksUntrustedWorktree
		}
		if checks.Stale {
			return AutoMergeBlocked, AutoMergeChecksConfigurationChanged
		}
		return AutoMergeWaiting, AutoMergeReconciling
	case "stale":
		// A stale projection is evidence for an older revision or evidence
		// recorded under a different configuration. The former waits for
		// evidence of the current commit; the latter blocks the current
		// commit.
		if checks.TestedCommit {
			return AutoMergeBlocked, AutoMergeChecksConfigurationChanged
		}
		return AutoMergeWaiting, AutoMergeWaitingChecks
	default:
		// Every other status is a terminal non-passing verdict.
		return AutoMergeBlocked, AutoMergeChecksFailed
	}
}

func autoMergeMessage(reason string) string {
	switch reason {
	case AutoMergeWaitingChecks:
		return "Waiting for passing checks of the current source commit."
	case AutoMergeChecksRunning:
		return "Checks of the current source commit are still running."
	case AutoMergeEvidenceUnreadable:
		return "The check records could not be read, so the automatic merge waits and retries."
	case AutoMergeMergeConflict:
		return "The current source and target revisions do not merge cleanly, so the automatic merge waits for a ref change that resolves them."
	case AutoMergeRepositoryUnavailable:
		return "The repository is temporarily unavailable, so the automatic merge waits and retries."
	case AutoMergeReconciling:
		return "The current source commit passed its checks and merges cleanly; OwnGit is publishing the merge."
	case AutoMergeChecksFailed:
		return "The latest checks of the current source commit did not pass."
	case AutoMergeChecksUntrustedWorktree:
		return "The passing checks were not recorded from a clean worktree of the current source commit, so they cannot publish an automatic merge."
	case AutoMergeChecksConfigurationChanged:
		return "The passing checks used a different check configuration than the workflow committed at the current source commit."
	case "source_branch_missing":
		return "The source branch no longer exists."
	case "target_branch_missing":
		return "The target branch no longer exists."
	case "source_not_commit":
		return "The source branch does not point to a commit."
	case "target_not_commit":
		return "The target branch does not point to a commit."
	case "already_merged":
		return "The pull request is already merged."
	default:
		return ""
	}
}

// autoMergeView builds the shallow projection: it costs no trial merge, so
// list rendering and the views returned by mutations use it.
func (service *Service) autoMergeView(intent state.PullRequestAutoMerge, status string, source, target branchHead, checks Checks) *AutoMerge {
	eligibility := evaluateEligibility(status, source, target)
	blocker := ""
	if !eligibility.Eligible {
		blocker = eligibility.Blockers[0].Code
	}
	state2, reason := autoMergeEvidenceStatus(checks, eligibility.Eligible, blocker)
	view := &AutoMerge{
		Enabled:   true,
		Status:    state2,
		Reason:    reason,
		Message:   autoMergeMessage(reason),
		SourceOID: source.OID,
		TargetOID: target.OID,
		EnabledAt: intent.EnabledAt,
		EnabledBy: recordedActor(intent.EnabledBy),
	}
	return view
}

// deepAutoMergeViewLocked runs the Git mergeability decision on top of the
// shallow evidence gate. It is used only by the show detail of an enabled
// intent while holding a repository lock. A conflict is waiting, not blocked:
// a later target move can resolve it.
func (service *Service) deepAutoMergeViewLocked(ctx context.Context, repositoryPath string, record state.PullRequest, intent state.PullRequestAutoMerge, source, target branchHead, checks Checks) *AutoMerge {
	_, view := service.autoMergeDecisionLocked(ctx, repositoryPath, record, intent, source, target, checks)
	return view
}

// autoMergeDecisionLocked reports whether the current pair may merge now and
// the waiting/blocked view that explains it. ready means the evidence gate
// passed and the pair merges cleanly.
func (service *Service) autoMergeDecisionLocked(ctx context.Context, repositoryPath string, record state.PullRequest, intent state.PullRequestAutoMerge, source, target branchHead, checks Checks) (bool, *AutoMerge) {
	shallow := service.autoMergeView(intent, record.Status, source, target, checks)
	if shallow.Status == AutoMergeBlocked || shallow.Reason != AutoMergeReconciling {
		return false, shallow
	}
	switch service.autoMergeMergeabilityLocked(ctx, repositoryPath, record, source.OID, target.OID) {
	case autoMergeReady:
		return true, shallow
	case autoMergeConflict:
		shallow.Reason = AutoMergeMergeConflict
		shallow.Message = autoMergeMessage(AutoMergeMergeConflict)
		return false, shallow
	default:
		shallow.Reason = AutoMergeRepositoryUnavailable
		shallow.Message = autoMergeMessage(AutoMergeRepositoryUnavailable)
		return false, shallow
	}
}

const (
	autoMergeReady = iota
	autoMergeConflict
	autoMergeUnavailable
)

// autoMergeMergeabilityLocked mirrors the mergeability answer a merge would
// act on. It takes no lock and writes nothing.
func (service *Service) autoMergeMergeabilityLocked(ctx context.Context, repositoryPath string, record state.PullRequest, sourceOID, targetOID string) int {
	if err := service.requireMergeCapability(ctx); err != nil {
		return autoMergeUnavailable
	}
	method, err := service.mergeMethod(ctx, repositoryPath, sourceOID, targetOID)
	if err != nil {
		return autoMergeUnavailable
	}
	if method == "" {
		return autoMergeConflict
	}
	if method != "merge_commit" {
		return autoMergeReady
	}
	var leftover error
	if _, _, conflicted, err := service.trialMerge(ctx, repositoryPath, targetOID, sourceOID, &leftover); err != nil || conflicted {
		if err == nil || conflicted {
			return autoMergeConflict
		}
		return autoMergeUnavailable
	}
	return autoMergeReady
}

// EnableAutoMerge records the opt-in intent for an open pull request. When a
// merge was already published, the receipt is reconciled to merged and no
// intent is recorded. Enabling twice is idempotent.
func (service *Service) EnableAutoMerge(ctx context.Context, repositoryID string, number int64, actor state.Actor) (*View, error) {
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
	if record, err = service.completePublishedMergeLocked(ctx, repositoryPath, record); err != nil {
		return nil, err
	}
	if record.Status != state.PullRequestOpen {
		return nil, notOpenProblem(record.Status)
	}
	if _, err := service.Store.EnablePullRequestAutoMerge(ctx, repositoryID, number, actor, service.now()); err != nil {
		switch {
		case err == state.ErrPullRequestStateChanged:
			current, readErr := service.requirePullRequest(ctx, repositoryID, number)
			if readErr != nil {
				return nil, &Problem{Code: "state_unavailable", Message: "The pull request state could not be saved.", Cause: err}
			}
			return nil, notOpenProblem(current.Status)
		case err == state.ErrPullRequestNotFound:
			return nil, NewProblem("pull_request_not_found", "The pull request does not exist.")
		default:
			return nil, &Problem{Code: "state_unavailable", Message: "The pull request state could not be saved.", Cause: err}
		}
	}
	return service.readViewLocked(ctx, repositoryPath, record, false)
}

// CancelAutoMerge removes the intent and abandons merge plans that published
// no receipt. When a merge was already published, the receipt is reconciled
// to merged and the cancel reports the pull request as not open instead of
// claiming the intent was canceled.
func (service *Service) CancelAutoMerge(ctx context.Context, repositoryID string, number int64) (*View, error) {
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
	if record, err = service.completePublishedMergeLocked(ctx, repositoryPath, record); err != nil {
		return nil, err
	}
	if record.Status != state.PullRequestOpen {
		return nil, notOpenProblem(record.Status)
	}
	_, enabled, err := service.Store.PullRequestAutoMerge(ctx, repositoryID, number)
	if err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be read.", Cause: err}
	}
	if !enabled {
		return nil, NewProblem("auto_merge_not_enabled", "Automatic merging is not enabled for this pull request.")
	}
	if err := service.Store.DeletePullRequestAutoMerge(ctx, repositoryID, number); err != nil {
		return nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be canceled.", Cause: err}
	}
	// Any plan this intent prepared but never published is abandoned now,
	// while a plan with a receipt (already handled above) is never touched.
	if err := service.discardUnpublishedMergePlansLocked(ctx, repositoryPath, record, nil); err != nil {
		return nil, err
	}
	return service.readViewLocked(ctx, repositoryPath, record, false)
}

// ConvergeAutoMerge evaluates one enabled intent under the repository write
// lock and, when the current source revision passed its checks and the
// current pair merges cleanly, publishes the merge using the same machinery
// as a manual merge. It returns merged=true only when this call published (or
// reconciled) the merge. Waiting and blocked conclusions are reported in the
// view with a nil error; a lock, Git, or state failure returns an error the
// background loop retries after the next event.
func (service *Service) ConvergeAutoMerge(ctx context.Context, repositoryID string, number int64) (bool, *View, error) {
	_, enabled, err := service.Store.PullRequestAutoMerge(ctx, repositoryID, number)
	if err != nil {
		return false, nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be read.", Cause: err}
	}
	if !enabled {
		return false, nil, nil
	}
	repositoryPath, err := service.repositoryPath(ctx, repositoryID)
	if err != nil {
		return false, nil, err
	}
	lock := service.Repositories.Locks.For(repositoryID)
	if err := lockForRequest(ctx, lock.LockContext); err != nil {
		return false, nil, err
	}
	defer lock.Unlock()
	merged, view, err := service.convergeAutoMergeLocked(ctx, repositoryID, repositoryPath, number)
	if merged && service.OnChange != nil {
		service.OnChange(repositoryID)
	}
	return merged, view, err
}

func (service *Service) convergeAutoMergeLocked(ctx context.Context, repositoryID, repositoryPath string, number int64) (bool, *View, error) {
	// Re-read the intent under the lock so a cancel followed by a new enable
	// during lock wait publishes with the current enabler and guard.
	intent, enabled, err := service.Store.PullRequestAutoMerge(ctx, repositoryID, number)
	if err != nil {
		return false, nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be read.", Cause: err}
	}
	if !enabled {
		return false, nil, nil
	}
	record, err := service.requirePullRequest(ctx, repositoryID, number)
	if err != nil {
		return false, nil, err
	}
	// Finish a merge a previous convergence published but did not record.
	if record, err = service.completePublishedMergeLocked(ctx, repositoryPath, record); err != nil {
		return false, nil, err
	}
	if record.Status != state.PullRequestOpen {
		// A close or merge that committed while a wake was queued removed the
		// intent in its transaction; clear any defensive residue.
		if err := service.Store.DeletePullRequestAutoMerge(ctx, repositoryID, number); err != nil {
			return false, nil, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be removed.", Cause: err}
		}
		return false, nil, nil
	}
	source, target, err := service.currentHeads(ctx, repositoryPath, record)
	if err != nil {
		return false, nil, err
	}
	checks := service.checksForRevision(ctx, repositoryPath, record, source)
	ready, autoMerge := service.autoMergeDecisionLocked(ctx, repositoryPath, record, intent, source, target, checks)
	if !ready {
		view := &View{}
		view.AutoMerge = autoMerge
		return false, view, nil
	}
	guard := func() (bool, error) {
		_, enabled, err := service.Store.PullRequestAutoMerge(ctx, record.RepositoryID, number)
		if err != nil {
			return false, &Problem{Code: "state_unavailable", Message: "The automatic merge could not be read.", Cause: err}
		}
		return enabled, nil
	}
	mergedView, err := service.mergeCurrentPairLocked(ctx, repositoryPath, record, source, target, mergeOptions{
		actor:         intent.EnabledBy,
		requireIntent: guard,
	})
	if err != nil {
		if AsProblem(err) == errAutoMergeAborted {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, mergedView, nil
}

// ConvergeAutoMerges processes one bounded, fair page of one repository's
// enabled intents, in ascending pull request number after after. The next
// cursor is the last processed number; more is true when another page may
// follow. A blocked or waiting request never interrupts the page. The error
// of one convergence (a busy or unavailable repository) is returned after the
// page completes, so the loop retries the repository.
func (service *Service) ConvergeAutoMerges(ctx context.Context, repositoryID string, after int64, limit int) (next int64, more bool, err error) {
	page, pageMore, err := service.Store.PullRequestAutoMergesAfter(ctx, repositoryID, after, limit)
	if err != nil {
		return 0, false, &Problem{Code: "state_unavailable", Message: "The automatic merges could not be read.", Cause: err}
	}
	var convergenceErr error
	for _, intent := range page {
		if _, _, convergeErr := service.ConvergeAutoMerge(ctx, repositoryID, intent.PullRequestNumber); convergeErr != nil && convergenceErr == nil {
			convergenceErr = convergeErr
		}
	}
	if len(page) == 0 {
		return 0, false, convergenceErr
	}
	return page[len(page)-1].PullRequestNumber, pageMore, convergenceErr
}
