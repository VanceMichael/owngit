package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAutoMergeStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	noErr(t, err)
	t.Cleanup(func() { noErr(t, store.Close()) })
	now := time.Unix(1_800_000_000, 0).UTC()
	noErr(t, store.AddRepository(ctx, Repository{ID: "project", Name: "Project", CreatedAt: now}))
	noErr(t, store.AddRepository(ctx, Repository{ID: "other", Name: "Other", CreatedAt: now}))
	return store
}

func createOpenRequest(t *testing.T, store *Store, repositoryID string, number int, now time.Time) {
	t.Helper()
	_, err := store.CreatePullRequest(context.Background(), repositoryID,
		"Request "+string(rune('0'+number)), "feature-"+string(rune('0'+number)), "main",
		strings.Repeat("a", 40), strings.Repeat("b", 40), ReviewSkipped, now.Add(time.Duration(number)*time.Second))
	noErr(t, err)
}

func TestEnablePullRequestAutoMergeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	createOpenRequest(t, store, "project", 1, now)
	actor := Actor{Kind: ActorAccess}

	first, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, actor, now)
	noErr(t, err)
	if first.RepositoryID != "project" || first.PullRequestNumber != 1 || !first.EnabledAt.Equal(now) || first.EnabledBy != actor {
		t.Fatalf("first enable recorded %+v", first)
	}
	// A repeated enable keeps the first timestamp and actor even when the
	// caller names a later time.
	later := now.Add(time.Minute)
	second, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, later)
	noErr(t, err)
	if !second.EnabledAt.Equal(first.EnabledAt) || second.EnabledBy != actor || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("second enable rewrote the intent: %+v first=%+v", second, first)
	}
	read, ok, err := store.PullRequestAutoMerge(ctx, "project", 1)
	noErr(t, err)
	if !ok || read.EnabledAt != first.EnabledAt {
		t.Fatalf("stored intent=%+v ok=%v", read, ok)
	}
	var count int
	noErr(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pull_request_auto_merges`).Scan(&count))
	if count != 1 {
		t.Fatalf("idempotent enable left %d rows", count)
	}
}

func TestEnablePullRequestAutoMergeRejectsNonOpenRequests(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	createOpenRequest(t, store, "project", 1, now)

	if _, err := store.EnablePullRequestAutoMerge(ctx, "project", 99, Actor{}, now); !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("missing request error=%v, want ErrPullRequestNotFound", err)
	}
	_, err := store.SetPullRequestClosed(ctx, "project", 1, true, now.Add(time.Second))
	noErr(t, err)
	if _, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, now.Add(2*time.Second)); !errors.Is(err, ErrPullRequestStateChanged) {
		t.Fatalf("closed request error=%v, want ErrPullRequestStateChanged", err)
	}
	reopened, err := store.SetPullRequestClosed(ctx, "project", 1, false, now.Add(3*time.Second))
	noErr(t, err)
	if reopened.Status != PullRequestOpen {
		t.Fatalf("reopen status=%s", reopened.Status)
	}
	// Merge the request through the durable completion path.
	source, target := strings.Repeat("a", 40), strings.Repeat("b", 40)
	intent, err := store.BeginPullRequestMerge(ctx, PullRequestMergeIntent{
		RepositoryID: "project", PullRequestNumber: 1, SourceOID: source, TargetOID: target,
		ReceiptRef: "refs/owngit/pull-requests/1/merge-receipt", CreatedAt: now.Add(4 * time.Second),
	})
	noErr(t, err)
	intent.Mode, intent.ResultOID, intent.Status = "up_to_date", target, MergeIntentReady
	intent, err = store.UpdatePullRequestMergeIntent(ctx, intent)
	noErr(t, err)
	noErr(t, store.CompletePullRequestMerge(ctx, intent, now.Add(5*time.Second), Actor{}))
	if _, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, now.Add(6*time.Second)); !errors.Is(err, ErrPullRequestStateChanged) {
		t.Fatalf("merged request error=%v, want ErrPullRequestStateChanged", err)
	}
}

func TestClosingPullRequestDeletesAutoMergeAndReopenKeepsItDeleted(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	createOpenRequest(t, store, "project", 1, now)
	_, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, now.Add(time.Second))
	noErr(t, err)
	_, err = store.SetPullRequestClosed(ctx, "project", 1, true, now.Add(2*time.Second))
	noErr(t, err)
	if _, ok, err := store.PullRequestAutoMerge(ctx, "project", 1); err != nil || ok {
		t.Fatalf("intent after close ok=%v err=%v", ok, err)
	}
	_, err = store.SetPullRequestClosed(ctx, "project", 1, false, now.Add(3*time.Second))
	noErr(t, err)
	if _, ok, err := store.PullRequestAutoMerge(ctx, "project", 1); err != nil || ok {
		t.Fatalf("reopen restored the intent: ok=%v err=%v", ok, err)
	}
}

func TestCompletingMergeDeletesAutoMergeIntent(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	createOpenRequest(t, store, "project", 1, now)
	_, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, now.Add(time.Second))
	noErr(t, err)
	source, target := strings.Repeat("a", 40), strings.Repeat("b", 40)
	intent, err := store.BeginPullRequestMerge(ctx, PullRequestMergeIntent{
		RepositoryID: "project", PullRequestNumber: 1, SourceOID: source, TargetOID: target,
		ReceiptRef: "refs/owngit/pull-requests/1/merge-receipt", CreatedAt: now.Add(2 * time.Second),
	})
	noErr(t, err)
	intent.Mode, intent.ResultOID, intent.Status = "up_to_date", target, MergeIntentReady
	intent, err = store.UpdatePullRequestMergeIntent(ctx, intent)
	noErr(t, err)
	noErr(t, store.CompletePullRequestMerge(ctx, intent, now.Add(3*time.Second), Actor{}))
	if _, ok, err := store.PullRequestAutoMerge(ctx, "project", 1); err != nil || ok {
		t.Fatalf("intent after merge ok=%v err=%v", ok, err)
	}
}

func TestPullRequestAutoMergeListsAndFairPages(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	for number := 1; number <= 5; number++ {
		createOpenRequest(t, store, "project", number, now)
	}
	createOpenRequest(t, store, "other", 1, now)
	enabled := []int64{1, 3, 5}
	for _, number := range enabled {
		_, err := store.EnablePullRequestAutoMerge(ctx, "project", number, Actor{}, now.Add(time.Duration(number)*time.Second))
		noErr(t, err)
	}
	_, err := store.EnablePullRequestAutoMerge(ctx, "other", 1, Actor{}, now.Add(time.Second))
	noErr(t, err)

	repositories, err := store.RepositoriesWithPullRequestAutoMerges(ctx)
	noErr(t, err)
	if len(repositories) != 2 || repositories[0] != "other" || repositories[1] != "project" {
		t.Fatalf("repositories with intents=%v", repositories)
	}
	oneRepo, err := store.PullRequestAutoMerges(ctx, "project")
	noErr(t, err)
	if len(oneRepo) != 3 || oneRepo[0].PullRequestNumber != 1 || oneRepo[2].PullRequestNumber != 5 {
		t.Fatalf("project intents=%+v", oneRepo)
	}
	all, err := store.PullRequestAutoMerges(ctx, "")
	noErr(t, err)
	if len(all) != 4 {
		t.Fatalf("all intents=%d", len(all))
	}

	// Circular paging rotates fairly: from every cursor the page starts with
	// the next enabled number, wrapping back to the smallest one, so a wake
	// loop that advances the cursor reaches every enabled request.
	for _, expected := range []struct {
		after  int64
		first  int64
		second int64
	}{
		{0, 1, 3}, {1, 3, 5}, {3, 5, 1}, {5, 1, 3},
	} {
		page, more, err := store.PullRequestAutoMergesAfter(ctx, "project", expected.after, 2)
		if err != nil || !more || len(page) != 2 || page[0].PullRequestNumber != expected.first || page[1].PullRequestNumber != expected.second {
			t.Fatalf("page after %d=%+v more=%v err=%v, want %d,%d", expected.after, page, more, err, expected.first, expected.second)
		}
	}
	if _, _, err := store.PullRequestAutoMergesAfter(ctx, "missing", 0, 2); err != nil {
		t.Fatalf("a repository without intents pages as empty: %v", err)
	}

	if err := store.DeletePullRequestAutoMerge(ctx, "project", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePullRequestAutoMerge(ctx, "project", 1); err != nil {
		t.Fatalf("deleting a missing intent should not fail: %v", err)
	}
	remaining, err := store.PullRequestAutoMerges(ctx, "project")
	noErr(t, err)
	if len(remaining) != 2 {
		t.Fatalf("remaining intents=%d", len(remaining))
	}
}

// The portable snapshot validates the same invariants the live database
// enforces: an auto-merge intent exists only for an open pull request.
func TestPullRequestAutoMergeRecoveryValidation(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	noErr(t, store.CompleteSetup(ctx, t.TempDir(), "open", "", "synthetic-admin-hash", false))
	now := time.Unix(1_800_000_000, 0).UTC()
	createOpenRequest(t, store, "project", 1, now)
	_, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, now.Add(time.Second))
	noErr(t, err)
	base, err := store.RecoverySnapshot(ctx)
	noErr(t, err)
	if err := ValidatePullRequestRecovery(base); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}

	corrupt := func(edit func(*RecoveryState)) RecoveryState {
		snapshot := base
		snapshot.PullRequestAutoMerges = append([]PullRequestAutoMerge(nil), base.PullRequestAutoMerges...)
		edit(&snapshot)
		return snapshot
	}
	tests := []struct {
		name     string
		snapshot RecoveryState
	}{
		{name: "intent for a missing request", snapshot: corrupt(func(s *RecoveryState) {
			s.PullRequests = nil
			s.PullRequestRevisions = nil
			s.PullRequestReviews = nil
		})},
		{name: "intent on a closed request", snapshot: corrupt(func(s *RecoveryState) {
			s.PullRequests[0].Status = PullRequestClosed
		})},
		{name: "intent without enabled time", snapshot: corrupt(func(s *RecoveryState) {
			s.PullRequestAutoMerges[0].EnabledAt = time.Time{}
			s.PullRequestAutoMerges[0].UpdatedAt = time.Time{}
		})},
		{name: "duplicate intent", snapshot: corrupt(func(s *RecoveryState) {
			s.PullRequestAutoMerges = append(s.PullRequestAutoMerges, s.PullRequestAutoMerges[0])
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidatePullRequestRecovery(test.snapshot); err == nil {
				t.Fatal("corrupt snapshot was accepted")
			}
		})
	}
}

func TestPullRequestAutoMergeCascadesWithRepository(t *testing.T) {
	ctx := context.Background()
	store := newAutoMergeStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	createOpenRequest(t, store, "project", 1, now)
	_, err := store.EnablePullRequestAutoMerge(ctx, "project", 1, Actor{}, now.Add(time.Second))
	noErr(t, err)
	if _, err := store.db.ExecContext(ctx, `DELETE FROM repositories WHERE id='project'`); err != nil {
		t.Fatal(err)
	}
	var count int
	noErr(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pull_request_auto_merges WHERE repository_id='project'`).Scan(&count))
	if count != 0 {
		t.Fatalf("repository deletion left %d intent rows", count)
	}
}
