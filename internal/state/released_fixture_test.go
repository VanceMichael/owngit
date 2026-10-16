package state

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"owngit/internal/testfixture"
)

// Databases that released builds wrote upgrade once to the current schema,
// end at exactly the catalog of a new database and keep every row. The
// fixtures are SQL dumps of stopped states made from the shared synthetic
// test data (testdata/released/README.md says how), so their catalogs are the
// text those releases stored, not text rebuilt from the schema steps.
func TestReleasedDatabasesUpgradeOnce(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		file    string
		version int
	}{
		{file: "schema14-1.0.1.sql", version: 14},
		{file: "schema14-1.0.2.sql", version: 14},
		{file: "schema15-1.0.3.sql", version: 15},
		{file: "schema15-1.0.3-upgraded-from-1.0.2.sql", version: 15},
		{file: "schema15-baseline-upgraded-by-1.0.3.sql", version: 15},
		{file: "schema15-1.1.2.sql", version: 15},
		{file: "schema15-1.1.2-upgraded-from-1.0.2.sql", version: 15},
		{file: "schema16-1.1.6-populated.sql", version: 16},
	} {
		t.Run(test.file, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			loadReleasedDump(t, directory, test.file)
			db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
			before := tableRowCounts(t, db)
			noErr(t, db.Close())
			if before["repositories"] == 0 || before["pull_requests"] == 0 || before["pull_request_reviews"] == 0 {
				t.Fatalf("fixture has too few records: %v", before)
			}

			store, err := Open(ctx, directory)
			if err != nil {
				t.Fatalf("open released schema %d: %v", test.version, err)
			}
			want := "state database upgraded from schema " + strconv.Itoa(test.version) + " to " + strconv.Itoa(currentSchemaVersion())
			if upgrade := store.SchemaUpgrade(); upgrade != want {
				store.Close()
				t.Fatalf("upgrade reported %q, want %q", upgrade, want)
			}
			if fingerprint, objects, err := schemaFingerprint(ctx, store.db); err != nil || fingerprint != currentSchemaFingerprint || objects != currentSchemaObjects {
				store.Close()
				t.Fatalf("upgraded schema fingerprint=%s objects=%d err=%v", fingerprint, objects, err)
			}
			after := tableRowCounts(t, store.db)
			for table, count := range before {
				if after[table] != count {
					store.Close()
					t.Fatalf("table %s has %d rows after the upgrade, %d before", table, after[table], count)
				}
			}
			// Every raw log kept is indexed by its check's start, so the
			// first cleanup reaches the logs the upgrade kept.
			var unindexed int
			noErr(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_raw_logs r JOIN check_attempts a ON a.id=r.attempt_id
				LEFT JOIN check_raw_log_starts s ON s.attempt_id=r.attempt_id WHERE s.created_at IS NOT a.created_at`).Scan(&unindexed))
			if unindexed != 0 || after["check_raw_log_starts"] != before["check_raw_logs"] {
				store.Close()
				t.Fatalf("raw logs=%d starts=%d unindexed=%d", before["check_raw_logs"], after["check_raw_log_starts"], unindexed)
			}
			if _, err := store.RecoverySnapshot(ctx); err != nil {
				store.Close()
				t.Fatalf("snapshot of the upgraded state: %v", err)
			}
			noErr(t, store.Close())

			reopened, err := Open(ctx, directory)
			noErr(t, err)
			defer reopened.Close()
			if upgrade := reopened.SchemaUpgrade(); upgrade != "" {
				t.Fatalf("second open reported %q", upgrade)
			}
		})
	}
}

// A database whose catalog someone changed is refused before anything
// changes, whatever its version marker says and whatever the name of what
// was added: the catalog decides. A trigger in these databases would make
// Do not ask the confirmation policy; the files stay byte-identical, so
// none ran.
func TestAlteredDatabaseIsRefusedUnchanged(t *testing.T) {
	ctx := context.Background()
	const hostile = `BEGIN INSERT OR REPLACE INTO metadata(key,value) VALUES('admin_confirmation','never'); END`
	hidden := func(name string) []string {
		return []string{`PRAGMA writable_schema=ON`,
			`INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql) VALUES('trigger','` + name + `','metadata',0,'CREATE TRIGGER ` + name + ` AFTER UPDATE ON metadata ` + strings.ReplaceAll(hostile, "'", "''") + `')`}
	}
	changedColumn := []string{`PRAGMA writable_schema=ON`, `UPDATE sqlite_master SET sql=replace(sql,'description TEXT NOT NULL','description BLOB') WHERE name='repositories'`}
	released15 := func(t *testing.T, directory string) { loadReleasedDump(t, directory, "schema15-1.1.2.sql") }
	current := func(t *testing.T, directory string) {
		store, err := Open(ctx, directory)
		noErr(t, err)
		noErr(t, store.Close())
	}
	baseline := func(t *testing.T, directory string) {
		noErr(t, testfixture.CreateCommittedBaselineState(ctx, directory, testfixture.BaselineStateOptions{RepositoryRoot: t.TempDir(), AdminPasswordHash: "synthetic-admin-hash"}))
	}
	altered := func(version int) string {
		return "state database says schema " + strconv.Itoa(version) + " but its tables, indexes, triggers or views differ from the ones OwnGit creates at schema " + strconv.Itoa(version) + ", so this build does not open it; undo the change, or restore a backup of the state with the OwnGit version that made the backup"
	}
	for _, test := range []struct {
		name       string
		prepare    func(*testing.T, string)
		statements []string
		want       string
	}{
		{"released: added table", released15, []string{`CREATE TABLE notes_by_hand(text TEXT)`}, altered(15)},
		{"released: added column", released15, []string{`ALTER TABLE repositories ADD COLUMN colour TEXT`}, altered(15)},
		{"released: added index", released15, []string{`CREATE INDEX pull_requests_title ON pull_requests(title)`}, altered(15)},
		{"released: added view", released15, []string{`CREATE VIEW titles AS SELECT title FROM pull_requests`}, altered(15)},
		{"released: trigger", released15, []string{`CREATE TRIGGER u07_trigger AFTER UPDATE ON metadata ` + hostile}, altered(15)},
		{"released: trigger under a name SQLite reserves", released15, hidden("sqlite_u07_trigger"), altered(15)},
		{"released: changed column", released15, changedColumn, altered(15)},
		{"released catalog marked current", released15, []string{`CREATE TRIGGER u07_trigger AFTER INSERT ON sessions ` + hostile, `UPDATE metadata SET value='18' WHERE key='schema_version'`}, altered(18)},
		{"current: trigger under a name SQLite reserves", current, hidden("sqlite_u07_trigger"), altered(18)},
		{"current: added view", current, []string{`CREATE VIEW titles AS SELECT title FROM pull_requests`}, altered(18)},
		{"current: added table", current, []string{`CREATE TABLE notes_by_hand(text TEXT)`}, altered(18)},
		{"current: changed column", current, changedColumn, altered(18)},
		{"current: step 16 without sign_in_revision", current, []string{`ALTER TABLE import_sources DROP COLUMN sign_in_revision`}, altered(18)},
		{"baseline: trigger under a name SQLite reserves", baseline, hidden("sqlite_u07_trigger"), "state database has no schema version and does not match the committed baseline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			test.prepare(t, directory)
			db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
			for _, statement := range test.statements {
				_, err := db.Exec(statement)
				noErr(t, err)
			}
			noErr(t, db.Close())
			before := captureSchemaDirectory(t, directory)
			store, err := Open(ctx, directory)
			if store != nil {
				_ = store.Close()
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("altered schema error=%v", err)
			}
			assertSchemaDirectoryUnchanged(t, directory, before)
		})
	}
}

func loadReleasedDump(t *testing.T, directory, name string) {
	t.Helper()
	testfixture.LoadReleasedState(t, directory, name)
}

func tableRowCounts(t *testing.T, db queryRower) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT GLOB 'sqlite_*'`)
	noErr(t, err)
	var tables []string
	for rows.Next() {
		var name string
		noErr(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	noErr(t, closeRows(rows))
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		noErr(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&count))
		counts[table] = count
	}
	return counts
}
