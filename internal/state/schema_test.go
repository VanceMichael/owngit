package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"owngit/internal/testfixture"
)

func TestUnsupportedSchemaPreservesDirectoryMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode probe; Windows ACL has native coverage")
	}
	ctx := context.Background()
	directory := t.TempDir()
	createNumberedSchemaDatabase(t, directory, 5)
	noErr(t, os.Chmod(directory, 0o750))
	before := captureSchemaDirectory(t, directory)
	store, err := Open(ctx, directory)
	if store != nil {
		_ = store.Close()
	}
	if err == nil {
		t.Fatal("unsupported schema accepted")
	}
	info, statErr := os.Stat(directory)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("unsupported refusal changed directory permissions to %04o", info.Mode().Perm())
	}
	assertSchemaDirectoryUnchanged(t, directory, before)
}

func TestUnsupportedSchemaInCommittedWALPreservesSource(t *testing.T) {
	const fixtureVariable = "OWNGIT_UNSUPPORTED_WAL_FIXTURE"
	if directory := os.Getenv(fixtureVariable); directory != "" {
		db, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(directory, databaseName)))
		noErr(t, err)
		for _, statement := range []string{
			`PRAGMA journal_mode=WAL`,
			`PRAGMA wal_autocheckpoint=0`,
			`CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL)`,
			`INSERT INTO metadata VALUES('schema_version','5')`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		os.Exit(73)
	}

	directory := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestUnsupportedSchemaInCommittedWALPreservesSource$")
	child.Env = append(os.Environ(), fixtureVariable+"="+directory)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("fixture child did not retain its committed WAL: %v %s", err, output)
	}
	before := captureSchemaDirectory(t, directory)
	if wal, exists := before[databaseName+"-wal"]; !exists || len(wal.data) == 0 {
		t.Fatal("fixture did not retain a committed WAL")
	}
	store, err := Open(context.Background(), directory)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "schema 5") {
		t.Fatalf("unsupported WAL schema error=%v", err)
	}
	assertSchemaDirectoryUnchanged(t, directory, before)
}

// currentSchemaFingerprint pins the catalog of schema 18. A changed migration
// statement changes it, so the current schema cannot drift unnoticed.
const (
	currentSchemaFingerprint = "1a6f6339385930ad0b417314c6eb04161c413f5cacc985bad8edb2f8e429da00"
	currentSchemaObjects     = 140
)

func TestFreshSchemaOpen(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "new path"},
		{name: "existing empty database", prepare: createEmptySQLiteDatabase},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			if test.prepare != nil {
				test.prepare(t, directory)
			}
			store, err := Open(ctx, directory)
			noErr(t, err)
			version, _, err := readSchemaVersion(ctx, store.db)
			if err != nil || version != currentSchemaVersion() {
				store.Close()
				t.Fatalf("schema version=%d err=%v", version, err)
			}
			for _, table := range []string{"check_policies", "check_jobs", "check_runner_credentials", "check_observations"} {
				var count int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
					store.Close()
					t.Fatal(err)
				}
				if count != 1 {
					store.Close()
					t.Fatalf("current schema table %q count=%d", table, count)
				}
			}
			for _, table := range []string{"import_sources", "import_runs", "import_ref_observations", "import_publication_intents", "import_stagings", "import_schedules", "import_initial_destinations"} {
				var count int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
					store.Close()
					t.Fatal(err)
				}
				if count != 1 {
					store.Close()
					t.Fatalf("import schema table %q count=%d", table, count)
				}
			}
			var rawTable, expiryIndex int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='check_raw_logs'`).Scan(&rawTable); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='check_raw_logs_expiry'`).Scan(&expiryIndex); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if rawTable != 1 || expiryIndex != 1 {
				store.Close()
				t.Fatalf("raw log schema table=%d index=%d", rawTable, expiryIndex)
			}
			fingerprint, objects, err := schemaFingerprint(ctx, store.db)
			if err != nil || fingerprint != currentSchemaFingerprint || objects != currentSchemaObjects {
				store.Close()
				t.Fatalf("fresh schema fingerprint=%s objects=%d err=%v", fingerprint, objects, err)
			}
			var trusted int
			if err := store.db.QueryRowContext(ctx, `PRAGMA trusted_schema`).Scan(&trusted); err != nil || trusted != 0 {
				store.Close()
				t.Fatalf("trusted_schema=%d err=%v", trusted, err)
			}
			noErr(t, store.Close())
			store, err = Open(ctx, directory)
			if err != nil {
				t.Fatalf("reopen current schema: %v", err)
			}
			noErr(t, store.Close())
		})
	}
}

// A database that changed from empty to the baseline after inspection must
// not be migrated under the empty classification.
func TestSchemaMigrationRefusesAnIncompatibleOwnerChange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	if err := testfixture.CreateCommittedBaselineState(ctx, directory, testfixture.BaselineStateOptions{
		RepositoryRoot: filepath.Join(root, "repositories"), AdminPasswordHash: "synthetic-admin-hash",
	}); err != nil {
		t.Fatal(err)
	}
	db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
	defer db.Close()
	migrating := &Store{db: db, dir: directory}
	if err := migrating.migrate(ctx, schemaEmpty); !errors.Is(err, ErrInspectionUnstable) {
		t.Fatalf("incompatible migration owner error=%v", err)
	}
	if _, versioned, err := readSchemaVersion(ctx, db); err != nil || versioned {
		t.Fatalf("refused migration versioned=%v err=%v", versioned, err)
	}
	if fingerprint, _, err := schemaFingerprint(ctx, db); err != nil || fingerprint != committedBaselineSchemaFingerprint {
		t.Fatalf("refused migration changed the baseline catalog: %s err=%v", fingerprint, err)
	}
}

func TestCommittedBaselineSchemaUpgradesInPlace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	repositoryRoot := filepath.Join(root, "repositories")
	if err := testfixture.CreateCommittedBaselineState(ctx, directory, testfixture.BaselineStateOptions{
		RepositoryRoot: repositoryRoot, AdminPasswordHash: "synthetic-admin-hash",
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, databaseName)
	db := openSchemaDatabase(t, path)
	var baselineReviewDDL string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='pull_request_reviews'`).Scan(&baselineReviewDDL); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if strings.Contains(baselineReviewDDL, ReviewNotRequested) || !strings.Contains(baselineReviewDDL, "'skipped'") {
		db.Close()
		t.Fatalf("fixture does not contain the review constraint from %s: %s", testfixture.CommittedBaselineStateCommit, baselineReviewDDL)
	}
	noErr(t, db.Close())

	store, err := Open(ctx, directory)
	if err != nil {
		t.Fatalf("open committed baseline: %v", err)
	}
	defer store.Close()
	if upgrade := store.SchemaUpgrade(); upgrade != "state database upgraded from the committed baseline (no schema version) to schema 18" {
		t.Fatalf("baseline upgrade reported %q", upgrade)
	}
	if version, _, err := readSchemaVersion(ctx, store.db); err != nil || version != currentSchemaVersion() {
		t.Fatalf("upgraded schema version=%d err=%v", version, err)
	}
	// The only upgrade path must produce exactly the catalog a fresh store has.
	if fingerprint, objects, err := schemaFingerprint(ctx, store.db); err != nil || fingerprint != currentSchemaFingerprint || objects != currentSchemaObjects {
		t.Fatalf("upgraded schema fingerprint=%s objects=%d err=%v", fingerprint, objects, err)
	}
	settings, err := store.Settings(ctx)
	if err != nil || !settings.Initialized || settings.RepositoryRoot != repositoryRoot || settings.AccessMode != "open" || settings.AccessSessionVersion != 3 || settings.AdminSessionVersion != 4 || !settings.InsecureHTTPAccepted || !settings.UpdateCheck {
		t.Fatalf("upgraded settings=%+v err=%v", settings, err)
	}
	if hash, err := store.PasswordHash(ctx, "admin"); err != nil || hash != "synthetic-admin-hash" {
		t.Fatalf("upgraded administrator hash=%q err=%v", hash, err)
	}
	if repository, exists, err := store.Repository(ctx, testfixture.BaselineRepositoryID); err != nil || !exists || repository.Name != "Baseline project" {
		t.Fatalf("upgraded repository=%+v exists=%v err=%v", repository, exists, err)
	}
	if hosts, err := store.TrustedHosts(ctx); err != nil || len(hosts) != 1 || hosts[0] != "baseline.example.invalid" {
		t.Fatalf("upgraded trusted hosts=%v err=%v", hosts, err)
	}
	if session, exists, err := store.Session(ctx, testfixture.BaselineSessionToken, "admin", time.Unix(1_800_000_000, 0)); err != nil || !exists || session.CSRF != "baseline-csrf" || session.Version != 4 {
		t.Fatalf("upgraded session=%+v exists=%v err=%v", session, exists, err)
	}
	snapshot, err := store.RecoverySnapshot(ctx)
	noErr(t, err)
	if len(snapshot.PullRequests) != 1 || len(snapshot.PullRequestRevisions) != 1 || len(snapshot.PullRequestReviews) != 1 {
		t.Fatalf("upgraded pull request history: requests=%d revisions=%d reviews=%d", len(snapshot.PullRequests), len(snapshot.PullRequestRevisions), len(snapshot.PullRequestReviews))
	}
	if review := snapshot.PullRequestReviews[0]; review.Status != ReviewApproved || review.SourceOID != testfixture.BaselineSourceOID || review.TargetOID != testfixture.BaselineTargetOID {
		t.Fatalf("upgraded review=%+v", review)
	}
	if _, err := store.CreateTask(ctx, testfixture.BaselineRepositoryID, "Upgraded task", time.Now()); err != nil {
		t.Fatalf("upgraded baseline cannot store tasks: %v", err)
	}
	if _, err := store.CreatePullRequest(ctx, testfixture.BaselineRepositoryID, "Optional review", "next", "main", strings.Repeat("c", 40), strings.Repeat("d", 40), ReviewNotRequested, time.Now()); err != nil {
		t.Fatalf("upgraded review constraint rejected an optional review: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("state directory mode=%v err=%v", infoMode(info), err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("state database mode=%v err=%v", infoMode(info), err)
		}
	}
}

func TestUnsupportedNumberedSchemasAreRefusedWithoutWrites(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 99} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			if version >= 6 && version < 14 {
				createMigratedSchemaDatabase(t, directory, version)
			} else {
				createNumberedSchemaDatabase(t, directory, version)
			}
			before := captureSchemaDirectory(t, directory)
			store, err := Open(ctx, directory)
			if store != nil {
				_ = store.Close()
			}
			want := "state database uses the unreleased development schema " + strconv.Itoa(version) + "; this build upgrades only the committed baseline (no schema version) and released schemas 14, 15 and 16, and opens schema 18"
			if version > currentSchemaVersion() {
				want = "state database schema version 99 is newer than this OwnGit build supports (18)"
			}
			if err == nil || err.Error() != want {
				t.Fatalf("schema %d error=%v", version, err)
			}
			assertSchemaDirectoryUnchanged(t, directory, before)
		})
	}
}

func TestUnversionedNonBaselineSchemasAreRefusedWithoutWrites(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "unrelated table", prepare: func(t *testing.T, directory string) {
			db := createSchemaDatabase(t, directory)
			if _, err := db.Exec(`CREATE TABLE unrelated(value TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			noErr(t, db.Close())
		}},
		{name: "partial baseline", prepare: func(t *testing.T, directory string) {
			db := createSchemaDatabase(t, directory)
			if _, err := db.Exec(`CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			noErr(t, db.Close())
		}},
		{name: "current schema with marker removed", prepare: func(t *testing.T, directory string) {
			store, err := Open(ctx, directory)
			noErr(t, err)
			if err := store.Exec(ctx, `DELETE FROM metadata WHERE key='schema_version'`); err != nil {
				store.Close()
				t.Fatal(err)
			}
			noErr(t, store.Close())
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			test.prepare(t, directory)
			before := captureSchemaDirectory(t, directory)
			store, err := Open(ctx, directory)
			if store != nil {
				_ = store.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "does not match the committed baseline") {
				t.Fatalf("unversioned schema error=%v", err)
			}
			assertSchemaDirectoryUnchanged(t, directory, before)
		})
	}
}

func createEmptySQLiteDatabase(t *testing.T, directory string) {
	t.Helper()
	db := createSchemaDatabase(t, directory)
	if _, err := db.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	noErr(t, db.Close())
}

func createNumberedSchemaDatabase(t *testing.T, directory string, version int) {
	t.Helper()
	db := createSchemaDatabase(t, directory)
	if _, err := db.Exec(`CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO metadata(key,value) VALUES('schema_version',?)`, version); err != nil {
		t.Fatal(err)
	}
	noErr(t, db.Close())
}

// createMigratedSchemaDatabase builds the catalog an earlier build left at
// version by applying the recorded migration steps.
func createMigratedSchemaDatabase(t *testing.T, directory string, version int) {
	t.Helper()
	db := createSchemaDatabase(t, directory)
	defer db.Close()
	for _, step := range schemaSteps {
		if step.version > version {
			break
		}
		for _, statement := range step.statements {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("apply schema %d fixture: %v", step.version, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO metadata(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, step.version); err != nil {
			t.Fatal(err)
		}
	}
}

// applyCommittedBaselineCatalog copies the committed baseline catalog, without
// rows, into db.
func applyCommittedBaselineCatalog(t *testing.T, db *sql.DB) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "baseline")
	if err := testfixture.CreateCommittedBaselineState(context.Background(), source, testfixture.BaselineStateOptions{
		RepositoryRoot: filepath.Join(root, "repositories"), AdminPasswordHash: "synthetic-admin-hash",
	}); err != nil {
		t.Fatal(err)
	}
	baseline := openSchemaDatabase(t, filepath.Join(source, databaseName))
	defer baseline.Close()
	rows, err := baseline.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type='index',rowid`)
	noErr(t, err)
	var statements []string
	for rows.Next() {
		var statement string
		noErr(t, rows.Scan(&statement))
		statements = append(statements, statement)
	}
	noErr(t, closeRows(rows))
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if fingerprint, _, err := schemaFingerprint(context.Background(), db); err != nil || fingerprint != committedBaselineSchemaFingerprint {
		t.Fatalf("baseline catalog fingerprint=%s err=%v", fingerprint, err)
	}
}

func createSchemaDatabase(t *testing.T, directory string) *sql.DB {
	t.Helper()
	noErr(t, os.MkdirAll(directory, 0o700))
	return openSchemaDatabase(t, filepath.Join(directory, databaseName))
}

func openSchemaDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path))
	noErr(t, err)
	db.SetMaxOpenConns(1)
	return db
}

type schemaFileSnapshot struct {
	mode os.FileMode
	data []byte
}

func captureSchemaDirectory(t *testing.T, directory string) map[string]schemaFileSnapshot {
	t.Helper()
	entries, err := os.ReadDir(directory)
	noErr(t, err)
	snapshot := make(map[string]schemaFileSnapshot, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		noErr(t, err)
		if !info.Mode().IsRegular() {
			t.Fatalf("unexpected schema fixture entry %q", entry.Name())
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		noErr(t, err)
		snapshot[entry.Name()] = schemaFileSnapshot{mode: info.Mode(), data: content}
	}
	return snapshot
}

func assertSchemaDirectoryUnchanged(t *testing.T, directory string, before map[string]schemaFileSnapshot) {
	t.Helper()
	after := captureSchemaDirectory(t, directory)
	if len(after) != len(before) {
		t.Fatalf("refusal changed state directory entries: before=%v after=%v", mapKeys(before), mapKeys(after))
	}
	for name, want := range before {
		got, exists := after[name]
		if !exists || got.mode != want.mode || !bytes.Equal(got.data, want.data) {
			t.Fatalf("refusal changed %q", name)
		}
	}
}

func mapKeys(values map[string]schemaFileSnapshot) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

// TestInitializeClassifiesOneSnapshot lets another connection commit the
// schema migration after initialize read that the new database has no schema
// version and before it reads the rest of the schema. Mixing the two views
// would refuse the database as an unknown unversioned schema; one snapshot
// sees it empty and then finds it current inside the migration.
func TestInitializeClassifiesOneSnapshot(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, databaseName)
	noErr(t, createDatabase(ctx, path))
	db, err := sql.Open("sqlite", sqliteFileURI(path))
	noErr(t, err)
	db.SetMaxOpenConns(1)
	store := &Store{db: db, dir: directory}
	defer store.Close()

	migrated := false
	t.Cleanup(func() { schemaHooks.versionRead = nil })
	schemaHooks.versionRead = func() {
		if migrated {
			return
		}
		migrated = true
		otherDB, err := sql.Open("sqlite", sqliteFileURI(path))
		noErr(t, err)
		otherDB.SetMaxOpenConns(1)
		other := &Store{db: otherDB, dir: directory}
		defer other.Close()
		noErr(t, other.migrate(ctx, schemaEmpty))
	}
	noErr(t, store.initialize(ctx, schemaEmpty))
	if !migrated {
		t.Fatal("the hook did not run, so the race was not exercised")
	}
	version, _, err := readSchemaVersion(ctx, store.db)
	noErr(t, err)
	if version != currentSchemaVersion() {
		t.Fatalf("schema version %d, want %d", version, currentSchemaVersion())
	}
}

// A migration that its context ends at any point fails and leaves the
// schema it started from, and the connection it held still enforces
// foreign keys afterwards: the pragma it turned off for the steps is turned
// on again only after the transaction has ended. The same holds when
// another opener already migrated and migrate returns early.
func TestCancelledMigrationLeavesForeignKeysOn(t *testing.T) {
	open := func(t *testing.T, directory string) (*Store, *sql.DB) {
		t.Helper()
		db, err := sql.Open("sqlite", sqliteURI(filepath.Join(directory, databaseName), "_txlock=immediate&mode=rw"))
		noErr(t, err)
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return &Store{db: db, dir: directory}, db
	}
	foreignKeys := func(t *testing.T, db *sql.DB) int {
		t.Helper()
		var enabled int
		noErr(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled))
		return enabled
	}
	migrateWithin := func(t *testing.T, deadline time.Duration) (error, int, int) {
		directory := filepath.Join(t.TempDir(), "state")
		loadReleasedDump(t, directory, "schema15-1.1.2-upgraded-from-1.0.2.sql")
		store, db := open(t, directory)
		class, err := classifySchema(context.Background(), db)
		noErr(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		migrateErr := store.migrate(ctx, class)
		cancel()
		version, _, err := readSchemaVersion(context.Background(), store.db)
		noErr(t, err)
		return migrateErr, foreignKeys(t, db), version
	}
	started := time.Now()
	if err, enabled, version := migrateWithin(t, time.Minute); err != nil || enabled != 1 || version != currentSchemaVersion() {
		t.Fatalf("uncancelled migration err=%v foreign_keys=%d schema=%d", err, enabled, version)
	}
	full := time.Since(started)
	for step := 0; step <= 24; step++ {
		deadline := time.Duration(step) * full / 16
		err, enabled, version := migrateWithin(t, deadline)
		if enabled != 1 || (err == nil) != (version == currentSchemaVersion()) || (err != nil && version != 15) {
			t.Fatalf("deadline %v: err=%v foreign_keys=%d schema=%d", deadline, err, enabled, version)
		}
	}

	directory := filepath.Join(t.TempDir(), "migrated")
	migrated, err := Open(context.Background(), directory)
	noErr(t, err)
	noErr(t, migrated.Close())
	store, db := open(t, directory)
	started = time.Now()
	noErr(t, store.migrate(context.Background(), schemaCurrent))
	full = time.Since(started)
	for try := 0; try < 200; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(try%40)*full/20)
		_ = store.migrate(ctx, schemaCurrent)
		cancel()
		if enabled := foreignKeys(t, db); enabled != 1 {
			t.Fatalf("try %d: foreign_keys=%d after an already migrated database", try, enabled)
		}
	}
}
