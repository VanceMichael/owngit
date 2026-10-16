package state

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReleasedSchemasUpgradeAndKeepPullRequestHistory(t *testing.T) {
	var released []int
	for _, step := range schemaSteps {
		if step.released {
			released = append(released, step.version)
		}
	}
	if !slices.Equal(released, []int{14, 15, 16}) {
		t.Fatalf("released schemas=%v", released)
	}
	for _, version := range released {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			createReleasedSchemaWithPullRequest(t, directory, version)
			store := openUpgradedSchema(t, directory, currentSchemaVersion())
			defer store.Close()
			if fingerprint, objects, err := schemaFingerprint(context.Background(), store.db); err != nil || fingerprint != currentSchemaFingerprint || objects != currentSchemaObjects {
				t.Fatalf("upgraded schema fingerprint=%s objects=%d err=%v", fingerprint, objects, err)
			}
			assertPullRequestHistoryKept(t, store)
		})
	}
}

// A 1.1.2 import source upgrades with no sign-in change recorded, so the
// deletions it follows are those of any run, as before.
func TestReleasedImportSourceUpgradesWithoutASignInRevision(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	loadReleasedDump(t, directory, "schema15-1.1.2.sql")
	db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
	_, err := db.Exec(`INSERT INTO import_sources(repository_id,url,source_generation,authority_revision,mode,created_at,updated_at)
		VALUES('project','https://example.invalid/team/project.git',1,3,'standalone',1800000000,1800000000)`)
	noErr(t, err)
	noErr(t, db.Close())
	store := openUpgradedSchema(t, directory, currentSchemaVersion())
	defer store.Close()
	var revision int64
	noErr(t, store.db.QueryRowContext(ctx, `SELECT sign_in_revision FROM import_sources WHERE repository_id='project'`).Scan(&revision))
	if revision != 0 {
		t.Fatalf("upgraded sign_in_revision=%d", revision)
	}
}

func TestSchemaChainRunsEveryLaterStep(t *testing.T) {
	original := schemaSteps
	schemaSteps = append(slices.Clone(original), schemaStep{version: 19, statements: []string{
		`CREATE TABLE synthetic_step(title TEXT NOT NULL)`,
		`INSERT INTO synthetic_step(title) SELECT title FROM pull_requests`,
	}})
	t.Cleanup(func() { schemaSteps = original })
	ctx := context.Background()

	for _, version := range []int{14, 15, 16} {
		t.Run("upgrade from "+strconv.Itoa(version), func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			createReleasedSchemaWithPullRequest(t, directory, version)
			store := openUpgradedSchema(t, directory, 19)
			defer store.Close()
			if upgrade, want := store.SchemaUpgrade(), "state database upgraded from schema "+strconv.Itoa(version)+" to 19"; upgrade != want {
				t.Fatalf("upgrade reported %q, want %q", upgrade, want)
			}
			var copied int
			noErr(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM synthetic_step WHERE title='Kept'`).Scan(&copied))
			if copied != 1 {
				t.Fatalf("synthetic step copied %d rows", copied)
			}
			// Closing needs the schema 15 constraint, so step 15 ran too.
			assertPullRequestHistoryKept(t, store)
		})
	}

	t.Run("refusals", func(t *testing.T) {
		for version, want := range map[int]string{
			18: "state database uses the unreleased development schema 18; this build upgrades only the committed baseline (no schema version) and released schemas 14, 15 and 16, and opens schema 19",
			20: "state database schema version 20 is newer than this OwnGit build supports (19)",
		} {
			directory := filepath.Join(t.TempDir(), "state")
			createNumberedSchemaDatabase(t, directory, version)
			before := captureSchemaDirectory(t, directory)
			store, err := Open(ctx, directory)
			if store != nil {
				_ = store.Close()
			}
			if err == nil || err.Error() != want {
				t.Fatalf("schema %d error=%v", version, err)
			}
			assertSchemaDirectoryUnchanged(t, directory, before)
		}
	})

	// An opener that inspected schema 14 refuses a database that another
	// opener left at schema 15 instead of migrating it under the wrong start.
	t.Run("changed released version", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "state")
		createMigratedSchemaDatabase(t, directory, 15)
		db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
		defer db.Close()
		migrating := &Store{db: db, dir: directory}
		if err := migrating.migrate(ctx, schemaReleased(14)); !errors.Is(err, ErrInspectionUnstable) {
			t.Fatalf("changed released version error=%v", err)
		}
		if version, _, err := readSchemaVersion(ctx, db); err != nil || version != 15 {
			t.Fatalf("refused migration left schema %d err=%v", version, err)
		}
	})
}

func createReleasedSchemaWithPullRequest(t *testing.T, directory string, version int) {
	t.Helper()
	createMigratedSchemaDatabase(t, directory, version)
	db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
	defer db.Close()
	source, target := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	for _, statement := range []string{
		`INSERT INTO repositories(id,name,description,created_at) VALUES('project','project','',1)`,
		`INSERT INTO pull_requests(repository_id,number,title,source_branch,target_branch,status,created_at,updated_at) VALUES('project',1,'Kept','feature','main','open',1,2)`,
		`INSERT INTO pull_request_revisions(repository_id,pull_request_number,source_oid,target_oid,recorded_at) VALUES('project',1,'` + source + `','` + target + `',1)`,
		`INSERT INTO pull_request_reviews(repository_id,pull_request_number,sequence,source_oid,target_oid,status,reviewer_label,provenance,created_at) VALUES('project',1,1,'` + source + `','` + target + `','not_requested','','default',1)`,
		`INSERT INTO pull_request_merge_intents(repository_id,pull_request_number,source_oid,target_oid,mode,tree_oid,result_oid,receipt_ref,status,created_at,updated_at) VALUES('project',1,'` + source + `','` + target + `','','','','refs/owngit/pull-requests/1/merge-receipt','preparing',1,1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func openUpgradedSchema(t *testing.T, directory string, want int) *Store {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, directory)
	if err != nil {
		t.Fatalf("open released schema: %v", err)
	}
	if version, _, err := readSchemaVersion(ctx, store.db); err != nil || version != want {
		store.Close()
		t.Fatalf("upgraded schema version=%d err=%v", version, err)
	}
	return store
}

// assertPullRequestHistoryKept checks the rows createReleasedSchemaWithPullRequest
// wrote, closes the pull request, and deletes the repository through the
// cascading foreign keys.
func assertPullRequestHistoryKept(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	var requests, revisions, reviews, intents int
	noErr(t, store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM pull_requests),(SELECT COUNT(*) FROM pull_request_revisions),(SELECT COUNT(*) FROM pull_request_reviews),(SELECT COUNT(*) FROM pull_request_merge_intents)`).Scan(&requests, &revisions, &reviews, &intents))
	if requests != 1 || revisions != 1 || reviews != 1 || intents != 1 {
		t.Fatalf("upgrade lost pull request history: requests=%d revisions=%d reviews=%d intents=%d", requests, revisions, reviews, intents)
	}
	var enforced int
	noErr(t, store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enforced))
	if enforced != 1 {
		t.Fatal("foreign key enforcement was not restored after the migration")
	}
	closed, err := store.SetPullRequestClosed(ctx, "project", 1, true, time.Unix(3, 0))
	if err != nil || closed.Status != PullRequestClosed {
		t.Fatalf("close after upgrade=%+v err=%v", closed, err)
	}
	noErr(t, store.Exec(ctx, `DELETE FROM repositories WHERE id='project'`))
	var remaining int
	noErr(t, store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM pull_requests)+(SELECT COUNT(*) FROM pull_request_revisions)+(SELECT COUNT(*) FROM pull_request_reviews)+(SELECT COUNT(*) FROM pull_request_merge_intents)`).Scan(&remaining))
	if remaining != 0 {
		t.Fatalf("cascading delete left %d pull request rows", remaining)
	}
}

func TestPullRequestCloseAndReopenTransitions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	noErr(t, err)
	defer store.Close()
	noErr(t, store.CompleteSetup(ctx, t.TempDir(), "open", "", "synthetic-admin-hash", true))
	noErr(t, store.Exec(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES('project','project','',1)`))
	record, err := store.CreatePullRequest(ctx, "project", "Close me", "feature", "main", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", ReviewNotRequested, time.Unix(10, 0))
	noErr(t, err)
	if _, err := store.SetPullRequestClosed(ctx, "project", record.Number, false, time.Unix(11, 0)); err != ErrPullRequestStateChanged {
		t.Fatalf("reopen of an open pull request err=%v", err)
	}
	closed, err := store.SetPullRequestClosed(ctx, "project", record.Number, true, time.Unix(12, 0))
	if err != nil || closed.Status != PullRequestClosed || closed.UpdatedAt.Unix() != 12 {
		t.Fatalf("close=%+v err=%v", closed, err)
	}
	if open, more, err := store.OpenPullRequests(ctx, "project", 10); err != nil || more || len(open) != 0 {
		t.Fatalf("a closed pull request is listed as open: %d err=%v", len(open), err)
	}
	if _, exists, err := store.OpenPullRequestForBranches(ctx, "project", "feature", "main"); err != nil || exists {
		t.Fatalf("a closed pull request still holds its branch pair: exists=%v err=%v", exists, err)
	}
	if _, err := store.AppendPullRequestReview(ctx, PullRequestReview{RepositoryID: "project", PullRequestNumber: record.Number,
		SourceOID: "1111111111111111111111111111111111111111", TargetOID: "2222222222222222222222222222222222222222",
		Status: ReviewSkipped, Provenance: ReviewProvenanceSkip, CreatedAt: time.Unix(13, 0)}); err == nil {
		t.Fatal("a review was recorded on a closed pull request")
	}
	snapshot, err := store.RecoverySnapshot(ctx)
	noErr(t, err)
	noErr(t, ValidatePullRequestRecovery(snapshot))
	reopened, err := store.SetPullRequestClosed(ctx, "project", record.Number, false, time.Unix(14, 0))
	if err != nil || reopened.Status != PullRequestOpen {
		t.Fatalf("reopen=%+v err=%v", reopened, err)
	}
}

// Open describes the upgrade it applied, once. A new database, a current one,
// and one that another opener already upgraded report nothing.
func TestSchemaUpgradeIsReportedOnce(t *testing.T) {
	ctx := context.Background()
	fresh, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	noErr(t, err)
	if upgrade := fresh.SchemaUpgrade(); upgrade != "" {
		t.Fatalf("new database reported %q", upgrade)
	}
	noErr(t, fresh.Close())

	directory := filepath.Join(t.TempDir(), "state")
	createMigratedSchemaDatabase(t, directory, 14)
	store, err := Open(ctx, directory)
	noErr(t, err)
	if upgrade := store.SchemaUpgrade(); upgrade != "state database upgraded from schema 14 to 18" {
		t.Fatalf("released schema upgrade reported %q", upgrade)
	}
	noErr(t, store.Close())

	reopened, err := Open(ctx, directory)
	noErr(t, err)
	defer reopened.Close()
	if upgrade := reopened.SchemaUpgrade(); upgrade != "" {
		t.Fatalf("current database reported %q", upgrade)
	}
	// An opener that inspected the released schema but finds it current in
	// the migration transaction, because another opener migrated first,
	// reports nothing.
	noErr(t, reopened.migrate(ctx, schemaReleased(14)))
	if upgrade := reopened.SchemaUpgrade(); upgrade != "" {
		t.Fatalf("already migrated database reported %q", upgrade)
	}
}

// The steps run in one transaction, so a step that fails leaves a released
// database at its schema with every row.
func TestFailingStepLeavesTheReleasedSchema(t *testing.T) {
	original := schemaSteps
	schemaSteps = append(slices.Clone(original), schemaStep{version: 19, statements: []string{
		// Fails only on a database that holds a pull request, so the
		// steps' own catalog (built on an empty database) succeeds and the
		// migration itself fails.
		`CREATE TABLE step_19(value TEXT CHECK(value IS NULL))`,
		`INSERT INTO step_19(value) SELECT title FROM pull_requests`,
	}})
	t.Cleanup(func() { schemaSteps = original })
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	createReleasedSchemaWithPullRequest(t, directory, 15)
	store, err := Open(ctx, directory)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "apply state schema migration 19") {
		t.Fatalf("failing step error=%v", err)
	}
	db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
	defer db.Close()
	if version, _, err := readSchemaVersion(ctx, db); err != nil || version != 15 {
		t.Fatalf("failed migration left schema %d err=%v", version, err)
	}
	var added, requests int
	noErr(t, db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sqlite_master WHERE name IN ('share_links','step_19')),(SELECT COUNT(*) FROM pull_requests)`).Scan(&added, &requests))
	if added != 0 || requests != 1 {
		t.Fatalf("failed migration left %d new tables and %d pull requests", added, requests)
	}
}

// A migration commits the current catalog or nothing. This synthetic step
// adds a view only when the database holds a repository, so it ends at a
// catalog other than the one the steps build on an empty database, and the
// migration is rolled back.
func TestMigrationEndingAtAnotherCatalogIsRolledBack(t *testing.T) {
	original := schemaSteps
	schemaSteps = append(slices.Clone(original), schemaStep{version: 19, statements: []string{
		`PRAGMA writable_schema=ON`,
		`INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql) SELECT 'view','step_19','repositories',0,'CREATE VIEW step_19 AS SELECT id FROM repositories' FROM repositories LIMIT 1`,
		`PRAGMA writable_schema=OFF`,
	}})
	t.Cleanup(func() { schemaSteps = original })
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	createReleasedSchemaWithPullRequest(t, directory, 15)
	store, err := Open(ctx, directory)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || err.Error() != "state schema migration to 19 produced a schema other than schema 19; nothing was changed" {
		t.Fatalf("migration error=%v", err)
	}
	db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
	defer db.Close()
	if version, _, err := readSchemaVersion(ctx, db); err != nil || version != 15 {
		t.Fatalf("rolled-back migration left schema %d err=%v", version, err)
	}
	var added int
	noErr(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('share_links','step_19')`).Scan(&added))
	if added != 0 {
		t.Fatalf("rolled-back migration left %d new objects", added)
	}
}
