package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Archived defaults to false for a repository without a policy row, flips
// durably, and listing reports every archived repository.
func TestRepositoryArchivedRecord(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	noErr(t, store.Exec(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES('project','project','',1)`))
	noErr(t, store.Exec(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES('other','other','',2)`))

	archived, err := store.RepositoryArchived(ctx, "project")
	noErr(t, err)
	if archived {
		t.Fatal("a repository without a policy row is archived")
	}
	ids, err := store.ArchivedRepositoryIDs(ctx)
	noErr(t, err)
	if len(ids) != 0 {
		t.Fatalf("archived ids without an archive: %v", ids)
	}

	noErr(t, store.SetRepositoryArchived(ctx, "project", true, time.Unix(100, 0)))
	archived, err = store.RepositoryArchived(ctx, "project")
	noErr(t, err)
	if !archived {
		t.Fatal("SetRepositoryArchived(true) did not archive")
	}
	ids, err = store.ArchivedRepositoryIDs(ctx)
	noErr(t, err)
	if len(ids) != 1 || ids[0] != "project" {
		t.Fatalf("archived ids=%v, want [project]", ids)
	}
	noErr(t, store.SetRepositoryArchived(ctx, "other", true, time.Unix(101, 0)))
	if ids, err = store.ArchivedRepositoryIDs(ctx); err != nil || len(ids) != 2 || ids[0] != "other" || ids[1] != "project" {
		t.Fatalf("archived ids=%v err=%v, want [other project]", ids, err)
	}
	noErr(t, store.SetRepositoryArchived(ctx, "project", false, time.Unix(102, 0)))
	archived, err = store.RepositoryArchived(ctx, "project")
	noErr(t, err)
	if archived {
		t.Fatal("SetRepositoryArchived(false) left the repository archived")
	}
	if ids, err = store.ArchivedRepositoryIDs(ctx); err != nil || len(ids) != 1 || ids[0] != "other" {
		t.Fatalf("archived ids after unarchive=%v err=%v", ids, err)
	}
}

// Flipping the archive state changes only its own column: the repository's
// kept-history, protection and extra ref namespace choices stay in place.
func TestSetRepositoryArchivedKeepsTheOtherPolicyColumns(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	noErr(t, store.Exec(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES('project','project','',1)`))
	on := KeptHistoryOn
	protect := true
	prefixes := []string{"refs/notes/"}
	_, err := store.SaveRepositoryRefPolicy(ctx, "project", RepositoryRefPolicyChange{KeptHistory: &on, ProtectDefaultBranch: &protect, ExtraRefPrefixes: &prefixes})
	noErr(t, err)

	noErr(t, store.SetRepositoryArchived(ctx, "project", true, time.Unix(200, 0)))
	saved, err := store.RepositoryRefPolicy(ctx, "project")
	noErr(t, err)
	if saved.KeptHistory != KeptHistoryOn || !saved.ProtectDefaultBranch {
		t.Fatalf("archiving changed the ref choices: %+v", saved)
	}
	gotPrefixes, err := store.RepositoryExtraRefPrefixes(ctx, "project")
	noErr(t, err)
	if len(gotPrefixes) != 1 || gotPrefixes[0] != "refs/notes/" {
		t.Fatalf("archiving changed extra ref prefixes: %v", gotPrefixes)
	}
	archived, err := store.RepositoryArchived(ctx, "project")
	noErr(t, err)
	if !archived {
		t.Fatal("repository is not archived after saving the ref choices")
	}

	noErr(t, store.SetRepositoryArchived(ctx, "project", false, time.Unix(201, 0)))
	saved, err = store.RepositoryRefPolicy(ctx, "project")
	noErr(t, err)
	if saved.KeptHistory != KeptHistoryOn || !saved.ProtectDefaultBranch {
		t.Fatalf("unarchiving changed the ref choices: %+v", saved)
	}
}

// The portable snapshot and its restore carry the archive state.
func TestArchivedTravelsInTheRecoverySnapshot(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	noErr(t, store.CompleteSetup(ctx, t.TempDir(), "open", "", "synthetic-admin-hash", true))
	noErr(t, store.Exec(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES('project','project','',1)`))
	noErr(t, store.SetRepositoryArchived(ctx, "project", true, time.Unix(300, 0)))

	snapshot, err := store.RecoverySnapshot(ctx)
	noErr(t, err)
	var found bool
	for _, policy := range snapshot.RepositoryPolicies {
		if policy.RepositoryID == "project" {
			found = true
			if !policy.Archived {
				t.Fatal("snapshot policy is not archived")
			}
		}
	}
	if !found {
		t.Fatal("snapshot has no policy for project")
	}

	restored, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	noErr(t, err)
	defer restored.Close()
	noErr(t, restored.RestoreRecoveryState(ctx, t.TempDir(), snapshot))
	archived, err := restored.RepositoryArchived(ctx, "project")
	noErr(t, err)
	if !archived {
		t.Fatal("restored repository is not archived")
	}
}
