package recovery

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"owngit/internal/gitexec"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
)

// An enabled auto-merge intent is portable: it makes the backup format 13,
// survives restore unchanged, and stays format 13 on a second backup. A
// backup without one keeps the older format 12.
func TestAutoMergeIntentRoundTripsInBackupVersion13(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, manager := newBackupStore(t, root)
	service := &pullrequest.Service{Store: store, Repositories: manager}
	work := filepath.Join(root, "backup-work")
	remote, err := manager.Path("project")
	noErr(t, err)
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "checkout", "-b", "auto")
	noErr(t, os.WriteFile(filepath.Join(work, "auto.txt"), []byte("auto\n"), 0o600))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "auto change")
	runGit(t, work, "push", "origin", "HEAD:refs/heads/auto")
	openPR, err := service.Create(ctx, pullrequest.CreateInput{
		Repository: "project", Title: "Waits for checks", SourceBranch: "auto", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	enabledAt := time.Unix(1_800_000_000, 0).UTC()
	enabled, err := store.EnablePullRequestAutoMerge(ctx, "project", openPR.Number, state.Actor{Kind: state.ActorAccess}, enabledAt)
	noErr(t, err)

	backup := filepath.Join(root, "auto-backup")
	_, err = CreateWithReport(ctx, store, manager, backup)
	noErr(t, err)
	manifest, err := readManifest(filepath.Join(backup, manifestName))
	noErr(t, err)
	if manifest.Version != autoMergeBackupVersion {
		t.Fatalf("backup with an auto-merge intent wrote version %d, want %d", manifest.Version, autoMergeBackupVersion)
	}
	if len(manifest.PullRequestAutoMerges) != 1 {
		t.Fatalf("manifest holds %d auto-merge intents", len(manifest.PullRequestAutoMerges))
	}
	item := manifest.PullRequestAutoMerges[0]
	if item.RepositoryID != "project" || item.PullRequestNumber != openPR.Number || !item.EnabledAt.Equal(enabledAt) || item.EnabledBy.Kind != state.ActorAccess {
		t.Fatalf("manifest intent=%+v", item)
	}
	noErr(t, store.Close())

	restoredState := canonicalTestTarget(t, filepath.Join(root, "restored-state"))
	restoredRepositories := canonicalTestTarget(t, filepath.Join(root, "restored-repositories"))
	if _, err := RestoreWithReport(ctx, backup, restoredState, restoredRepositories, ""); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := state.Open(ctx, restoredState)
	noErr(t, err)
	t.Cleanup(func() { _ = restoredStore.Close() })
	snapshot, err := restoredStore.RecoverySnapshot(ctx)
	noErr(t, err)
	if len(snapshot.PullRequestAutoMerges) != 1 {
		t.Fatalf("restored snapshot holds %d auto-merge intents", len(snapshot.PullRequestAutoMerges))
	}
	restored := snapshot.PullRequestAutoMerges[0]
	if restored.RepositoryID != enabled.RepositoryID || restored.PullRequestNumber != enabled.PullRequestNumber ||
		!restored.EnabledAt.Equal(enabled.EnabledAt) || !restored.UpdatedAt.Equal(enabled.UpdatedAt) || !reflect.DeepEqual(restored.EnabledBy, enabled.EnabledBy) {
		t.Fatalf("restored intent=%+v, want %+v", restored, enabled)
	}
	read, ok, err := restoredStore.PullRequestAutoMerge(ctx, "project", openPR.Number)
	noErr(t, err)
	if !ok || read.EnabledAt != enabled.EnabledAt {
		t.Fatalf("restored store intent=%+v ok=%v", read, ok)
	}

	// A second backup of the restored state stays format 13 and keeps the
	// exact record.
	second := filepath.Join(root, "second-backup")
	restoredRunner, err := gitexec.New("", filepath.Join(restoredState, "runtime-test"))
	noErr(t, err)
	reopenedManager := &repository.Manager{Store: restoredStore, Git: restoredRunner, Locks: gitexec.NewLocks(), Root: restoredRepositories}
	_, err = CreateWithReport(ctx, restoredStore, reopenedManager, second)
	noErr(t, err)
	secondManifest, err := readManifest(filepath.Join(second, manifestName))
	noErr(t, err)
	if secondManifest.Version != autoMergeBackupVersion || len(secondManifest.PullRequestAutoMerges) != 1 {
		t.Fatalf("second backup version=%d intents=%d", secondManifest.Version, len(secondManifest.PullRequestAutoMerges))
	}
}

// A manifest that holds an auto-merge intent but claims an older format is
// refused, so an older reader never silently drops the intent.
func TestOlderBackupFormatCannotHoldAutoMergeIntent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, manager := newBackupStore(t, root)
	service := &pullrequest.Service{Store: store, Repositories: manager}
	work := filepath.Join(root, "backup-work")
	remote, err := manager.Path("project")
	noErr(t, err)
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "checkout", "-b", "waits")
	runGit(t, work, "push", "origin", "HEAD:refs/heads/waits")
	openPR, err := service.Create(ctx, pullrequest.CreateInput{
		Repository: "project", Title: "Waits", SourceBranch: "waits", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	_, err = store.EnablePullRequestAutoMerge(ctx, "project", openPR.Number, state.Actor{}, time.Unix(1_800_000_000, 0).UTC())
	noErr(t, err)

	backup := filepath.Join(root, "downgraded-backup")
	_, err = CreateWithReport(ctx, store, manager, backup)
	noErr(t, err)
	manifest, err := readManifest(filepath.Join(backup, manifestName))
	noErr(t, err)
	if manifest.Version != autoMergeBackupVersion {
		t.Fatalf("version=%d", manifest.Version)
	}
	// Marking the same records as format 12 must not make them restorable.
	manifest.Version = workflowBackupVersion
	writeManifestFile(t, filepath.Join(backup, manifestName), manifest)
	noErr(t, store.Close())

	stateTarget := canonicalTestTarget(t, filepath.Join(root, "old-state"))
	repositoriesTarget := canonicalTestTarget(t, filepath.Join(root, "old-repositories"))
	_, err = RestoreWithReport(ctx, backup, stateTarget, repositoriesTarget, "")
	if err == nil || !strings.Contains(err.Error(), "only version 13 holds") {
		t.Fatalf("downgraded manifest restore error=%v", err)
	}
}
