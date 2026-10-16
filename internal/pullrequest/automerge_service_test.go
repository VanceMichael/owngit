package pullrequest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"owngit/internal/state"
	"owngit/internal/testfixture"
)

func TestAutoMergeEvidenceStatus(t *testing.T) {
	for _, test := range []struct {
		name     string
		checks   Checks
		eligible bool
		blocker  string
		status   string
		reason   string
	}{
		{name: "no evidence", checks: Checks{Status: "absent"}, eligible: true, status: AutoMergeWaiting, reason: AutoMergeWaitingChecks},
		{name: "empty status", checks: Checks{Status: ""}, eligible: true, status: AutoMergeWaiting, reason: AutoMergeWaitingChecks},
		{name: "running", checks: Checks{Status: state.AttemptPending}, eligible: true, status: AutoMergeWaiting, reason: AutoMergeChecksRunning},
		{name: "evidence unreadable", checks: Checks{Status: state.AttemptPassed, ReadFailure: &ReadFailure{Code: ReadFailureCheckEvidence}}, eligible: true, status: AutoMergeWaiting, reason: AutoMergeEvidenceUnreadable},
		{name: "older revision evidence", checks: Checks{Status: "stale", TestedCommit: false, Stale: true}, eligible: true, status: AutoMergeWaiting, reason: AutoMergeWaitingChecks},
		{name: "config changed", checks: Checks{Status: "stale", TestedCommit: true, Stale: true}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksConfigurationChanged},
		{name: "passed but dirty worktree", checks: Checks{Status: state.AttemptPassed, TestedCommit: false, WorktreeState: state.WorktreeDirty}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksUntrustedWorktree},
		{name: "passed with stale flag", checks: Checks{Status: state.AttemptPassed, TestedCommit: true, Stale: true}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksConfigurationChanged},
		{name: "clean pass", checks: Checks{Status: state.AttemptPassed, TestedCommit: true}, eligible: true, status: AutoMergeWaiting, reason: AutoMergeReconciling},
		{name: "failed", checks: Checks{Status: state.AttemptFailed}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksFailed},
		{name: "error verdict", checks: Checks{Status: state.AttemptError}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksFailed},
		{name: "cancelled verdict", checks: Checks{Status: state.AttemptCancelled}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksFailed},
		{name: "incomplete verdict", checks: Checks{Status: state.AttemptIncomplete}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksFailed},
		{name: "unavailable verdict", checks: Checks{Status: state.AttemptUnavailable}, eligible: true, status: AutoMergeBlocked, reason: AutoMergeChecksFailed},
		{name: "source missing", checks: Checks{Status: state.AttemptPassed, TestedCommit: true}, eligible: false, blocker: "source_branch_missing", status: AutoMergeBlocked, reason: "source_branch_missing"},
		{name: "target not a commit", checks: Checks{Status: state.AttemptPassed, TestedCommit: true}, eligible: false, blocker: "target_not_commit", status: AutoMergeBlocked, reason: "target_not_commit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, reason := autoMergeEvidenceStatus(test.checks, test.eligible, test.blocker)
			if status != test.status || reason != test.reason {
				t.Fatalf("got %s/%s, want %s/%s", status, reason, test.status, test.reason)
			}
			if autoMergeMessage(reason) == "" {
				t.Fatalf("reason %s has no message", reason)
			}
		})
	}
}

func newAutoMergeFixture(t *testing.T) (*serviceFixture, *View) {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.commitFile("base.txt", "base\n", "base")
	fixture.push("HEAD:refs/heads/main")
	fixture.git("checkout", "-b", "feature")
	source := fixture.commitFile("feature.txt", "feature\n", "feature")
	fixture.push("HEAD:refs/heads/feature")
	created, err := fixture.service.Create(fixture.ctx, CreateInput{
		Repository: fixture.repositoryID, Title: "Auto merge", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	target := fixture.ref("refs/heads/main")
	if created.Source.OID != source || created.Target.OID != target {
		t.Fatalf("created pair=%s/%s want %s/%s", created.Source.OID, created.Target.OID, source, target)
	}
	return fixture, created
}

func TestEnableAutoMergeIsIdempotentAndRefusesNonOpen(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	actor := state.Actor{Kind: state.ActorAccess}
	enabled, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, actor)
	noErr(t, err)
	if enabled.AutoMerge == nil || !enabled.AutoMerge.Enabled || enabled.AutoMerge.Reason != AutoMergeWaitingChecks ||
		enabled.AutoMerge.SourceOID != created.Source.OID || enabled.AutoMerge.TargetOID != created.Target.OID ||
		enabled.AutoMerge.EnabledBy == nil || *enabled.AutoMerge.EnabledBy != actor {
		t.Fatalf("enabled view=%+v", enabled.AutoMerge)
	}
	enabledAt := enabled.AutoMerge.EnabledAt

	again, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	if again.AutoMerge == nil || !again.AutoMerge.EnabledAt.Equal(enabledAt) {
		t.Fatalf("second enable changed the intent: %+v", again.AutoMerge)
	}
	row, ok, err := fixture.store.PullRequestAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if !ok || !row.EnabledAt.Equal(enabledAt) {
		t.Fatalf("stored intent=%+v ok=%v", row, ok)
	}

	closed, err := fixture.service.Close(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if closed.AutoMerge != nil {
		t.Fatal("a closed view carried an auto-merge intent")
	}
	if _, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, actor); err == nil {
		t.Fatal("enable on a closed request succeeded")
	} else if AsProblem(err).Code != "pull_request_not_open" {
		t.Fatalf("closed enable error=%v", err)
	}

	reopened, err := fixture.service.Reopen(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if reopened.AutoMerge != nil {
		t.Fatal("reopen restored the auto-merge intent")
	}
}

func TestCancelAutoMergeRemovesIntentAndPlans(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	if _, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{}); err != nil {
		t.Fatal(err)
	}
	// A plan the intent prepared but never published is abandoned with it.
	_, err := fixture.store.BeginPullRequestMerge(fixture.ctx, state.PullRequestMergeIntent{
		RepositoryID: fixture.repositoryID, PullRequestNumber: created.Number,
		SourceOID: created.Source.OID, TargetOID: created.Target.OID,
		ReceiptRef: MergeReceiptRef(created.Number), CreatedAt: time.Now().UTC(),
	})
	noErr(t, err)

	canceled, err := fixture.service.CancelAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if canceled.AutoMerge != nil {
		t.Fatal("canceled view still carried the intent")
	}
	if _, err := fixture.service.CancelAutoMerge(fixture.ctx, fixture.repositoryID, created.Number); err == nil {
		t.Fatal("a second cancel succeeded")
	} else if AsProblem(err).Code != "auto_merge_not_enabled" {
		t.Fatalf("second cancel error=%v", err)
	}
	plans, err := fixture.store.PullRequestMergeIntents(fixture.ctx, fixture.repositoryID, created.Number, true)
	noErr(t, err)
	if len(plans) != 0 {
		t.Fatalf("cancel left %d unpublished merge plans", len(plans))
	}
	if _, ok, err := fixture.store.PullRequestAutoMerge(fixture.ctx, fixture.repositoryID, created.Number); err != nil || ok {
		t.Fatalf("intent after cancel ok=%v err=%v", ok, err)
	}
}

func TestPublishedMergeWinsOverEnableAndCancel(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	// Publish a receipt while the pull request state is still open, as a
	// background convergence that loses the lock would leave it.
	receiptOID := publishReceiptOnly(t, fixture, created.Number, created.Source.OID, created.Target.OID)
	if fixture.refExists(MergeReceiptRef(created.Number)) == false {
		t.Fatal("receipt was not published")
	}

	if _, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{}); err == nil {
		t.Fatal("enable after a published merge succeeded")
	} else if AsProblem(err).Code != "pull_request_not_open" {
		t.Fatalf("enable after receipt error=%v", err)
	}
	merged, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if merged.State != state.PullRequestMerged || merged.Merge == nil || merged.Merge.OID != receiptOID {
		t.Fatalf("enable did not reconcile the published merge: %+v", merged)
	}
	if _, ok, err := fixture.store.PullRequestAutoMerge(fixture.ctx, fixture.repositoryID, created.Number); err != nil || ok {
		t.Fatalf("an intent row survived the published merge: ok=%v", ok)
	}
}

// publishReceiptOnly drives the existing plan and publish machinery to the
// point where Git has the merge receipt but the durable state still says open.
func publishReceiptOnly(t *testing.T, fixture *serviceFixture, number int64, sourceOID, targetOID string) string {
	t.Helper()
	lock := fixture.manager.Locks.For(fixture.repositoryID)
	noErr(t, lock.LockContext(fixture.ctx))
	defer lock.Unlock()
	record, err := fixture.service.requirePullRequest(fixture.ctx, fixture.repositoryID, number)
	noErr(t, err)
	path, _, exists, err := fixture.manager.ExistingPath(fixture.ctx, fixture.repositoryID)
	noErr(t, err)
	if !exists {
		t.Fatal("repository path missing")
	}
	intent, err := fixture.store.BeginPullRequestMerge(fixture.ctx, state.PullRequestMergeIntent{
		RepositoryID: fixture.repositoryID, PullRequestNumber: number,
		SourceOID: sourceOID, TargetOID: targetOID, ReceiptRef: MergeReceiptRef(number),
		CreatedAt: time.Now().UTC(),
	})
	noErr(t, err)
	planned, err := fixture.service.planMerge(fixture.ctx, path, intent)
	noErr(t, err)
	noErr(t, fixture.service.ensurePlannedMergeTree(fixture.ctx, path, record, planned, true))
	resultOID, err := fixture.service.ensureMergeResult(fixture.ctx, path, record, planned, true)
	noErr(t, err)
	planned.ResultOID = resultOID
	planned.Status = state.MergeIntentReady
	intent, err = fixture.store.UpdatePullRequestMergeIntent(fixture.ctx, planned)
	noErr(t, err)
	noErr(t, fixture.service.publishMerge(fixture.ctx, path, record, intent))
	return resultOID
}

func TestClosingPullRequestCancelsAutoMergeAndPlans(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	_, err = fixture.store.BeginPullRequestMerge(fixture.ctx, state.PullRequestMergeIntent{
		RepositoryID: fixture.repositoryID, PullRequestNumber: created.Number,
		SourceOID: created.Source.OID, TargetOID: created.Target.OID,
		ReceiptRef: MergeReceiptRef(created.Number), CreatedAt: time.Now().UTC(),
	})
	noErr(t, err)

	closed, err := fixture.service.Close(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if closed.AutoMerge != nil {
		t.Fatal("closed view carried the intent")
	}
	if _, ok, err := fixture.store.PullRequestAutoMerge(fixture.ctx, fixture.repositoryID, created.Number); err != nil || ok {
		t.Fatalf("intent survived close: ok=%v", ok)
	}
	plans, err := fixture.store.PullRequestMergeIntents(fixture.ctx, fixture.repositoryID, created.Number, true)
	noErr(t, err)
	if len(plans) != 0 {
		t.Fatalf("close left %d unpublished plans", len(plans))
	}
}

func TestAutoMergeProjectionReflectsEvidenceAndConflict(t *testing.T) {
	fixture := newConflictingAutoMergeFixture(t)
	created, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, 1)
	noErr(t, err)
	_, err = fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, 1, state.Actor{})
	noErr(t, err)

	// Before evidence, both the list and the page wait for checks.
	listed, err := fixture.service.List(fixture.ctx, fixture.repositoryID, ListInput{State: ListStateAll, Limit: 5})
	noErr(t, err)
	if len(listed.Items) != 1 || listed.Items[0].AutoMerge == nil || listed.Items[0].AutoMerge.Reason != AutoMergeWaitingChecks {
		t.Fatalf("list before evidence=%+v", listed.Items)
	}

	// A passed, clean attempt at the conflicting current source commit: the
	// shallow list projection cannot know the conflict and reports
	// reconciling, while the show detail runs the mergeability decision and
	// reports a retryable conflict.
	recordCheckResult(t, fixture, created.Source.OID, state.AttemptPassed, state.WorktreeClean)
	listed, err = fixture.service.List(fixture.ctx, fixture.repositoryID, ListInput{State: ListStateAll, Limit: 5})
	noErr(t, err)
	if reason := listed.Items[0].AutoMerge.Reason; reason != AutoMergeReconciling {
		t.Fatalf("shallow list reason=%s, want reconciling", reason)
	}
	shown, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, 1)
	noErr(t, err)
	if shown.AutoMerge == nil || shown.AutoMerge.Status != AutoMergeWaiting || shown.AutoMerge.Reason != AutoMergeMergeConflict {
		t.Fatalf("deep show=%+v", shown.AutoMerge)
	}
}

func TestAutoMergeProjectionBlocksFailedAndDirtyEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   string
		worktree string
		reason   string
	}{
		{name: "failed", status: state.AttemptFailed, worktree: state.WorktreeClean, reason: AutoMergeChecksFailed},
		{name: "passed on dirty worktree", status: state.AttemptPassed, worktree: state.WorktreeDirty, reason: AutoMergeChecksUntrustedWorktree},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, created := newAutoMergeFixture(t)
			_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
			noErr(t, err)
			recordCheckResult(t, fixture, created.Source.OID, test.status, test.worktree)
			shown, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, created.Number)
			noErr(t, err)
			if shown.AutoMerge == nil || shown.AutoMerge.Status != AutoMergeBlocked || shown.AutoMerge.Reason != test.reason {
				t.Fatalf("projection=%+v, want blocked/%s", shown.AutoMerge, test.reason)
			}
			if fixture.refExists(MergeReceiptRef(created.Number)) {
				t.Fatal("blocked evidence published a merge")
			}
		})
	}
}

func newConflictingAutoMergeFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.commitFile("conflict.txt", "base\n", "base")
	fixture.push("HEAD:refs/heads/main")
	fixture.git("checkout", "-b", "feature")
	fixture.commitFile("conflict.txt", "feature side\n", "feature side")
	fixture.push("HEAD:refs/heads/feature")
	fixture.git("checkout", "main")
	fixture.commitFile("conflict.txt", "target side\n", "target side")
	fixture.push("HEAD:refs/heads/main")
	_, err := fixture.service.Create(fixture.ctx, CreateInput{
		Repository: fixture.repositoryID, Title: "Conflict", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	return fixture
}

func TestConvergeAutoMergePublishesCleanPassExactlyOnce(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	actor := state.Actor{Kind: state.ActorAccess}
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, actor)
	noErr(t, err)
	recordPassedCheck(t, fixture, created.Source.OID)

	merged, view, err := fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if !merged || view == nil || view.State != state.PullRequestMerged || view.Merge == nil {
		t.Fatalf("convergence did not merge: merged=%v view=%+v err=%v", merged, view, err)
	}
	if view.MergedBy == nil || *view.MergedBy != actor {
		t.Fatalf("auto merge merged_by=%v", view.MergedBy)
	}
	if got := fixture.ref(MergeReceiptRef(created.Number)); got != view.Merge.OID {
		t.Fatalf("receipt=%s merge oid=%s", got, view.Merge.OID)
	}
	if got := fixture.ref("refs/heads/main"); got != view.Merge.OID {
		t.Fatalf("target moved to %s, want %s", got, view.Merge.OID)
	}

	// A second convergence is a no-op and leaves exactly one receipt and one
	// completed merge intent.
	again, _, err := fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if again {
		t.Fatal("convergence merged twice")
	}
	intents, err := fixture.store.PullRequestMergeIntents(fixture.ctx, fixture.repositoryID, created.Number, false)
	noErr(t, err)
	if len(intents) != 1 || intents[0].Status != state.MergeIntentComplete {
		t.Fatalf("merge intents=%+v", intents)
	}
}

func TestConvergeAutoMergeWaitsWithoutGitEffects(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	targetBefore := fixture.ref("refs/heads/main")

	merged, view, err := fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if merged || view == nil || view.AutoMerge == nil || view.AutoMerge.Reason != AutoMergeWaitingChecks {
		t.Fatalf("convergence without evidence=%v %+v err=%v", merged, view, err)
	}
	if got := fixture.ref("refs/heads/main"); got != targetBefore {
		t.Fatalf("waiting moved the target: %s", got)
	}
	if fixture.refExists(MergeReceiptRef(created.Number)) {
		t.Fatal("waiting published a receipt")
	}

	recordCheckResult(t, fixture, created.Source.OID, state.AttemptFailed, state.WorktreeClean)
	merged, view, err = fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if merged || view.AutoMerge == nil || view.AutoMerge.Status != AutoMergeBlocked || view.AutoMerge.Reason != AutoMergeChecksFailed {
		t.Fatalf("convergence on failure=%v %+v", merged, view.AutoMerge)
	}
	if got := fixture.ref("refs/heads/main"); got != targetBefore {
		t.Fatalf("blocked moved the target: %s", got)
	}
	if fixture.refExists(MergeReceiptRef(created.Number)) {
		t.Fatal("blocked published a receipt")
	}
}

func TestConvergeAutoMergeReEvaluatesSourceAndTargetMoves(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	recordPassedCheck(t, fixture, created.Source.OID)

	// The source moves to a new commit with no evidence. The old pass must
	// not merge the new pair.
	fixture.git("checkout", "feature")
	newSource := fixture.commitFile("feature2.txt", "feature 2\n", "feature 2")
	fixture.push("HEAD:refs/heads/feature")
	fixture.git("checkout", "main")
	merged, view, err := fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if merged || view.AutoMerge.Reason != AutoMergeWaitingChecks || view.AutoMerge.SourceOID != newSource {
		t.Fatalf("old evidence merged a new source: %+v", view.AutoMerge)
	}

	// The target moves to a conflicting commit while evidence is missing, then
	// the new source gets a clean pass: convergence reports the retryable
	// conflict and publishes nothing.
	fixture.commitFile("feature.txt", "target changed\n", "target changed")
	fixture.push("HEAD:refs/heads/main")
	recordCheckResult(t, fixture, newSource, state.AttemptPassed, state.WorktreeClean)
	merged, view, err = fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if merged || view.AutoMerge.Reason != AutoMergeMergeConflict {
		t.Fatalf("conflicting pair converged=%v: %+v", merged, view.AutoMerge)
	}
	if fixture.refExists(MergeReceiptRef(created.Number)) {
		t.Fatal("conflict published a receipt")
	}

	// Resolve the conflict on the target: merge the feature into main in the
	// working copy and push the merge. The current pair is now up to date and
	// convergence records it against the newest target.
	// The merge stops on the conflict both branches changed; resolve it.
	merge := exec.Command("git", "merge", "feature")
	merge.Dir = fixture.work
	merge.Env = testfixture.GitEnvironment(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"))
	if err := merge.Run(); err == nil {
		t.Fatal("expected a conflicting merge")
	}
	noErr(t, os.WriteFile(filepath.Join(fixture.work, "feature.txt"), []byte("resolved\n"), 0o600))
	fixture.git("add", "feature.txt")
	fixture.git("commit", "-m", "resolve feature")
	resolvedTarget := fixture.gitOutput("rev-parse", "HEAD")
	fixture.push("HEAD:refs/heads/main")
	merged, view, err = fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if !merged || view.Merge == nil || view.Merge.OID != resolvedTarget {
		t.Fatalf("resolved pair did not converge: merged=%v view=%+v", merged, view)
	}
	if got := fixture.ref(MergeReceiptRef(created.Number)); got != resolvedTarget {
		t.Fatalf("receipt=%s, want resolved target %s", got, resolvedTarget)
	}
}

// The in-lock guard aborts a merge whose intent disappears after planning but
// before publication: no Git transaction runs and the prepared plan is
// discarded.
func TestAutoMergeGuardAbortsWithoutPublishing(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	recordPassedCheck(t, fixture, created.Source.OID)
	targetBefore := fixture.ref("refs/heads/main")

	lock := fixture.manager.Locks.For(fixture.repositoryID)
	noErr(t, lock.LockContext(fixture.ctx))
	defer lock.Unlock()
	record, err := fixture.service.requirePullRequest(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	path, _, exists, err := fixture.manager.ExistingPath(fixture.ctx, fixture.repositoryID)
	noErr(t, err)
	if !exists {
		t.Fatal("repository path missing")
	}
	source, target, err := fixture.service.currentHeads(fixture.ctx, path, record)
	noErr(t, err)
	reads := 0
	_, err = fixture.service.mergeCurrentPairLocked(fixture.ctx, path, record, source, target, mergeOptions{
		requireIntent: func() (bool, error) {
			reads++
			return reads == 1, nil
		},
	})
	if problemCode(err) != "auto_merge_aborted" {
		t.Fatalf("guard error=%v, want auto_merge_aborted", err)
	}
	if got := fixture.ref("refs/heads/main"); got != targetBefore {
		t.Fatalf("aborted merge moved the target: %s", got)
	}
	if fixture.refExists(MergeReceiptRef(created.Number)) {
		t.Fatal("aborted merge published a receipt")
	}
	plans, err := fixture.store.PullRequestMergeIntents(fixture.ctx, fixture.repositoryID, created.Number, true)
	noErr(t, err)
	if len(plans) != 0 {
		t.Fatalf("aborted merge left %d plans", len(plans))
	}
}

// A cancel that reaches the lock while a convergence is publishing cannot win
// the ordering: the merge completes with one receipt and the canceled view is
// the merged pull request.
func TestPublishedConvergenceWinsOverQueuedCancel(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	recordPassedCheck(t, fixture, created.Source.OID)
	hooked := make(chan struct{})
	release := make(chan struct{})
	fixture.service.beforeAutoMergePublish = func(string, int64) {
		close(hooked)
		<-release
	}

	finished := make(chan error, 1)
	go func() {
		_, _, err := fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
		finished <- err
	}()
	<-hooked
	cancelDone := make(chan error, 1)
	go func() {
		_, err := fixture.service.CancelAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
		cancelDone <- err
	}()
	// Give the queued cancel time to block on the repository lock; it cannot
	// pass until the convergence releases it.
	close(release)
	noErr(t, <-finished)
	cancelErr := <-cancelDone
	if problemCode(cancelErr) != "pull_request_not_open" {
		t.Fatalf("queued cancel error=%v, want the merged problem", cancelErr)
	}
	shown, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if shown.State != state.PullRequestMerged || shown.Merge == nil {
		t.Fatalf("convergence did not win: %+v", shown)
	}
	intents, err := fixture.store.PullRequestMergeIntents(fixture.ctx, fixture.repositoryID, created.Number, false)
	noErr(t, err)
	if len(intents) != 1 || intents[0].Status != state.MergeIntentComplete {
		t.Fatalf("merge intents after the race=%+v", intents)
	}
}

// A convergence whose Git capability is unavailable reports a retryable wait
// and merges once the repository answers again.
func TestConvergeAutoMergeRetriesAfterUnavailableRepository(t *testing.T) {
	fixture, created := newAutoMergeFixture(t)
	_, err := fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	recordPassedCheck(t, fixture, created.Source.OID)
	targetBefore := fixture.ref("refs/heads/main")

	oldRunner, _ := newOldGitRunner(t, filepath.Join(t.TempDir(), "runtime-old"))
	realRunner := fixture.manager.Git
	fixture.manager.Git = oldRunner
	merged, view, err := fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	if err != nil {
		t.Fatalf("unavailable convergence returned an error: %v", err)
	}
	if merged || view.AutoMerge.Reason != AutoMergeRepositoryUnavailable {
		t.Fatalf("unavailable convergence=%v %+v", merged, view.AutoMerge)
	}
	if got := fixture.ref("refs/heads/main"); got != targetBefore {
		t.Fatalf("unavailable convergence moved the target: %s", got)
	}

	fixture.manager.Git = realRunner
	merged, view, err = fixture.service.ConvergeAutoMerge(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if !merged || view.Merge == nil {
		t.Fatalf("convergence after recovery=%v %+v", merged, view)
	}
}

// checkAttemptSerial gives every fixture attempt a distinct identity without
// relying on the revision's bytes.
var checkAttemptSerial int

// recordCheckResult registers and completes one attempt for revision with the
// given verdict and worktree state.
func recordCheckResult(t *testing.T, fixture *serviceFixture, revision, status, worktree string) {
	t.Helper()
	now := time.Now().UTC()
	task, err := fixture.store.CreateTask(fixture.ctx, fixture.repositoryID, "Check the change", now.Add(-time.Second))
	noErr(t, err)
	checkAttemptSerial++
	attemptID := fmt.Sprintf("%032d", checkAttemptSerial)
	_, _, err = fixture.store.RegisterCheckAttempt(fixture.ctx, state.CheckAttempt{
		ID: attemptID, TaskID: task.ID, RepositoryID: fixture.repositoryID, RevisionOID: revision,
		WorktreeState: worktree, StartedAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Second),
		Protection: state.ProtectionUnknown, ExecutionScope: state.ExecutionScopeInherited,
		Checks: []state.CheckDefinition{{Name: "unit", Command: "go test ./..."}},
	})
	noErr(t, err)
	exitCode := 0
	if status != state.AttemptPassed {
		exitCode = 1
	}
	_, _, err = fixture.store.CompleteCheckAttempt(fixture.ctx, state.CheckCompletion{
		AttemptID: attemptID, RepositoryID: fixture.repositoryID, TaskID: task.ID, FinishedAt: now, WorktreeState: worktree,
		Results: []state.CheckResult{{Name: "unit", Command: "go test ./...", Status: status, ExitCode: &exitCode, DurationMS: 5}},
	}, now)
	noErr(t, err)
}
