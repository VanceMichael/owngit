package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"owngit/internal/statepath"

	_ "modernc.org/sqlite"
)

const (
	databaseName                = statepath.Database
	IncompleteRestoreMarkerName = statepath.IncompleteRestore

	// The committed baseline wrote no schema version marker; numbered
	// schemas are defined by schemaSteps. This is the schemaFingerprint of
	// the catalog that commit 8fdefd1bf10bd4a41b7261131efc119d46666260
	// created.
	committedBaselineSchemaFingerprint = "db9b1e019e93bef8c876e693049084504fa9715d8e6db0f29d96294439c0f5a8"
)

var ErrSetupComplete = errors.New("setup is already complete")

type Store struct {
	db                 *sql.DB
	dir                string
	releaseWay         func()
	observationDir     *os.File
	releaseObservation func() error
	protectionChanges  []ProtectionChange
	// database identifies the database file the store opened, so a later
	// connection to it by path can prove it reached the same file.
	database os.FileInfo
	// schemaUpgrade describes the upgrade this Open applied, for SchemaUpgrade.
	schemaUpgrade string

	// Credential transitions are repository-scoped. credentialRestoreMu stops
	// all of them only while a whole-store restore replaces portable state.
	credentialRegistryMu sync.Mutex
	credentialLocks      map[string]*sync.Mutex
	credentialRestoreMu  sync.RWMutex
	credentialAuthority  sync.Map
	workflowSecretLocks  sync.Map
}

type Settings struct {
	Initialized          bool
	RepositoryRoot       string
	AccessMode           string
	AccessSessionVersion int64
	AdminSessionVersion  int64
	InsecureHTTPAccepted bool
	// UpdateCheck allows the daily new-release check. It is machine-local:
	// a missing key (older state, a restored backup) means on.
	UpdateCheck bool
}

type Session struct {
	Kind    string
	CSRF    string
	Version int64
	Expires time.Time
}

// MaximumRepositoryDescriptionBytes bounds a repository description wherever
// one is held: the repository rules, a recorded import destination and a
// backup manifest. The migration that created the import destination table
// states the same bound in its SQL.
const MaximumRepositoryDescriptionBytes = 500

type Repository struct {
	ID   string
	Name string
	// Address is where the repository answers: its current name, or its ID
	// when it was never renamed. Links use it; storage and records use ID.
	// It is read from repository_names and not written through this field.
	Address     string
	Description string
	CreatedAt   time.Time
	// AttemptSequence is the repository-wide counter that issues check attempt
	// sequences. It is portable so a restore keeps issuing higher sequences.
	AttemptSequence int64
}

type BootstrapSnapshot struct {
	Present   bool   `json:"present"`
	TokenHash []byte `json:"token_hash,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// OtherAccountError refuses a folder for OwnGit's files, such as its state or
// log, that root would open or create in a folder that another account owns.
// The folder, and OwnGit's files in it, belong to that account, so the
// command must run as that account.
type OtherAccountError struct {
	// Path is the folder that belongs to the account.
	Path string
	// Account is the account's name, or "" when its ID has no name here.
	Account string
	UID     uint32
}

func (e *OtherAccountError) Error() string {
	if e.Account == "" {
		return fmt.Sprintf("%s belongs to the account with ID %d, so OwnGit's files there are that account's; run the command as that account", e.Path, e.UID)
	}
	return fmt.Sprintf("%s belongs to the account %s, so OwnGit's files there are that account's; run the command as %s", e.Path, e.Account, e.Account)
}

// CreateDirectory creates the state directory dir and its missing parents
// through OpenDirectory and returns it held open, named by its resolved
// path. Every folder on the way, for every account, must be on a local
// filesystem, which is checked before anything is created, so a refused
// directory leaves nothing behind. The folders it creates are private to
// this account from the moment they exist. Open
// and the locks in it use the held directory, not whatever its path names
// later.
func CreateDirectory(dir string) (*os.File, error) {
	return openStateDirectory(dir, true)
}

// CreateDirectoryForStart opens owned state for startup protection.
// Existing permissions are inspected and narrowed by OpenIn before use.
func CreateDirectoryForStart(dir string) (*os.File, error) {
	return createStateDirectoryForStart(dir)
}

// OpenStateDirectory opens the existing state directory dir like
// CreateDirectory, creating nothing.
func OpenStateDirectory(dir string) (*os.File, error) {
	return openStateDirectory(dir, false)
}

// Open opens the state in the directory dir, creating it with
// CreateDirectoryForStart. An older schema is upgraded with no backup; see OpenIn.
func Open(ctx context.Context, dir string) (*Store, error) {
	held, err := CreateDirectoryForStart(dir)
	if err != nil {
		return nil, err
	}
	defer held.Close()
	return OpenIn(ctx, held, nil)
}

// OpenIn opens the state in the directory held, which CreateDirectory,
// CreateDirectoryForStart or OpenStateDirectory returned. The inspection binds the directory by its
// path and refuses one that is not the held directory any more, as a change
// during inspection (ErrInspectionUnstable). SQLite opens the state by that
// path, so the way to it stays held until Store.Close. When the database
// has an older schema, beforeUpgrade, unless nil, runs after the inspection
// and before anything in the directory changes.
func OpenIn(ctx context.Context, held *os.File, beforeUpgrade BeforeUpgrade) (result *Store, err error) {
	absolute := held.Name()
	release, err := holdWay(held)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			release()
		}
	}()
	if err := requireCompleteRestore(absolute); err != nil {
		return nil, err
	}
	// An existing database is classified before any permission change or
	// read-write SQLite access, so a refused database keeps its bytes, entries
	// and modes. The inspection holds the source handles until acceptance and
	// releases them before SQLite opens the same files. A release failure
	// blocks the writable open because the bound objects are no longer
	// reliably known.
	inspected, err := inspectState(ctx, held)
	if err != nil {
		return nil, err
	}
	// Keep the release guard until acceptance finishes normally, so an
	// interrupted acceptance cannot leave source handles open.
	defer func() {
		if inspected != nil {
			err = errors.Join(err, inspected.release())
			if err != nil {
				result = nil
			}
		}
	}()
	if beforeUpgrade != nil && inspected.class != schemaEmpty && inspected.class != schemaCurrent {
		upgrade := &Upgrade{From: inspected.class.version, To: currentSchemaVersion(), in: inspected}
		if err := beforeUpgrade(ctx, upgrade); err != nil {
			return nil, err
		}
	}
	if err := inspected.accept(ctx); err != nil {
		return nil, err
	}
	class := inspected.class
	create := inspected.main == nil
	if err := inspected.release(); err != nil {
		return nil, err
	}
	inspected = nil
	if preflightHooks.afterRelease != nil {
		preflightHooks.afterRelease(absolute)
	}
	path := filepath.Join(absolute, databaseName)
	if create {
		if err := createDatabase(ctx, path); err != nil {
			return nil, err
		}
	}
	// The database exists now, so SQLite must not create one in rollback
	// journal mode if the file disappeared since.
	db, err := sql.Open("sqlite", sqliteURI(path, "_txlock=immediate&mode=rw"))
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, dir: absolute}
	if err := store.initialize(ctx, class); err != nil {
		db.Close()
		return nil, err
	}
	if err := protectSQLiteFilesAfterOpen(path); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect state database file: %w", err)
	}
	if store.database, err = os.Stat(path); err != nil {
		db.Close()
		return nil, fmt.Errorf("identify state database file: %w", err)
	}
	store.releaseWay = sync.OnceFunc(release)
	return store, nil
}

// SchemaUpgrade returns a one-line description of the schema upgrade that Open
// applied to an existing database, for the caller to log, for example "state
// database upgraded from schema 14 to 15". It returns "" when Open created a
// new database or found the schema current, including one that another opener
// had already upgraded.
func (s *Store) SchemaUpgrade() string { return s.schemaUpgrade }

func sqliteFileURI(path string) string {
	return sqliteURI(path, "_txlock=immediate")
}

// sqliteURI names the state database at path for every connection OwnGit
// opens to it. Each connection turns trusted_schema off, so that nothing in
// the database's own schema may call a function with side effects or use a
// virtual table; OwnGit's schema needs neither. This backs up the catalog
// check (classifySchema), which is what refuses statements no build wrote.
// Each connection also enforces foreign keys and waits up to 5 seconds for
// a lock. They are set here, not once after opening, because database/sql
// replaces a connection that an interrupted statement left unusable, and
// the replacement has only what this URI gives it.
func sqliteURI(path, query string) string {
	slashPath := filepath.ToSlash(path)
	if len(slashPath) >= 3 && slashPath[1] == ':' && slashPath[2] == '/' &&
		(('a' <= slashPath[0] && slashPath[0] <= 'z') || ('A' <= slashPath[0] && slashPath[0] <= 'Z')) {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath, RawQuery: query + "&_pragma=trusted_schema(0)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}).String()
}

// initialize prepares the accepted database. The inspection result selects
// the path, but the real connection reclassifies before every persistent
// write. A migration candidate is reclassified again inside the immediate
// write transaction that applies the authorized schema steps.
func (s *Store) initialize(ctx context.Context, expected schemaClass) error {
	// classifySchema reads the schema in several queries. One read
	// transaction gives them one snapshot, so a migration that another
	// process commits in between cannot mix an empty and a finished schema.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("initialize state database: %w", err)
	}
	class, err := classifySchema(ctx, tx)
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		err = errors.Join(err, fmt.Errorf("end state database classification: %w", rollbackErr))
	}
	if err != nil {
		return err
	}
	if class != schemaCurrent && class != expected {
		return unstable("state database changed between inspection and open")
	}
	// SQLite reports the mode it kept when it cannot switch. Writing in
	// rollback journal mode would leave journals that the preflight refuses.
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("initialize state database: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("initialize state database: journal mode is %q, not write-ahead logging", mode)
	}
	if class == schemaCurrent {
		// Another accepted opener may have completed the migration first, so
		// a current database is served without repeating its statements.
		return nil
	}
	return s.migrate(ctx, expected)
}

// queryRower is the read surface shared by a connection and a transaction.
type queryRower interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// closeRows ends a row loop and reports why iteration stopped. When a step
// fails mid-scan, database/sql closes the rows itself and keeps the error only
// in Err, so a later Close returns nil and a truncated read would look
// complete.
func closeRows(rows *sql.Rows) error {
	return errors.Join(rows.Err(), rows.Close())
}

// schemaHooks is a test seam: versionRead runs after classifySchema found no
// schema version and before it reads the rest of the schema. Production
// leaves it nil.
var schemaHooks struct {
	versionRead func()
}

// classifySchema returns the accepted class of the visible schema or the
// compatibility error that refuses it. It runs several queries, so a database
// that another connection can change must be classified inside one
// transaction. The preflight reads an immutable view or a private copy.
func classifySchema(ctx context.Context, db queryRower) (schemaClass, error) {
	version, versioned, err := readSchemaVersion(ctx, db)
	if err != nil {
		return schemaClass{}, err
	}
	if !versioned {
		if schemaHooks.versionRead != nil {
			schemaHooks.versionRead()
		}
		return validateUnversionedSchema(ctx, db)
	}
	current := currentSchemaVersion()
	switch {
	case version > current:
		return schemaClass{}, fmt.Errorf("state database schema version %d is newer than this OwnGit build supports (%d)", version, current)
	case version == current || isReleasedSchema(version):
		return validateSteppedSchema(ctx, db, version)
	case version >= 1:
		return schemaClass{}, fmt.Errorf("state database uses the unreleased development schema %d; this build upgrades only the committed baseline (no schema version) and %s, and opens schema %d", version, describeReleasedSchemas(), current)
	default:
		return schemaClass{}, fmt.Errorf("unsupported state database schema version %d", version)
	}
}

// currentSchemaVersion is the schema this build writes.
func currentSchemaVersion() int {
	return schemaSteps[len(schemaSteps)-1].version
}

// isReleasedSchema reports whether version is a released schema below the
// current one, which upgrades in place.
func isReleasedSchema(version int) bool {
	for _, step := range schemaSteps[:len(schemaSteps)-1] {
		if step.version == version {
			return step.released
		}
	}
	return false
}

// describeReleasedSchemas names the released schemas below the current one,
// for example "released schema 14" or "released schemas 14 and 15".
func describeReleasedSchemas() string {
	var versions []string
	for _, step := range schemaSteps[:len(schemaSteps)-1] {
		if step.released {
			versions = append(versions, strconv.Itoa(step.version))
		}
	}
	switch len(versions) {
	case 0:
		return "no released schema"
	case 1:
		return "released schema " + versions[0]
	default:
		return "released schemas " + strings.Join(versions[:len(versions)-1], ", ") + " and " + versions[len(versions)-1]
	}
}

func readSchemaVersion(ctx context.Context, db queryRower) (int, bool, error) {
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='metadata'`).Scan(&tables); err != nil {
		return 0, false, fmt.Errorf("inspect state database: %w", err)
	}
	if tables == 0 {
		return 0, false, nil
	}
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema_version'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read state schema version: %w", err)
	}
	version, err := strconv.Atoi(value)
	if err != nil || version < 0 {
		return 0, false, fmt.Errorf("invalid state schema version %q", value)
	}
	return version, true, nil
}

// validateSteppedSchema accepts a database that says it is at the current
// or a released version only when its catalog is exactly the one the schema
// steps up to that version create. The catalog decides, never the version
// marker alone: a table, column, index, trigger or view that someone added
// or changed, under any name, is refused before anything changes, instead
// of being opened or upgraded with statements that no build wrote.
func validateSteppedSchema(ctx context.Context, db queryRower, version int) (schemaClass, error) {
	fingerprint, _, err := schemaFingerprint(ctx, db)
	if err != nil {
		return schemaClass{}, fmt.Errorf("inspect state database schema %d: %w", version, err)
	}
	expected, err := stepsFingerprint(ctx, version)
	if err != nil {
		return schemaClass{}, err
	}
	if fingerprint != expected {
		return schemaClass{}, fmt.Errorf("state database says schema %d but its tables, indexes, triggers or views differ from the ones OwnGit creates at schema %d, so this build does not open it; undo the change, or restore a backup of the state with the OwnGit version that made the backup", version, version)
	}
	if version == currentSchemaVersion() {
		return schemaCurrent, nil
	}
	return schemaReleased(version), nil
}

// stepCatalogs caches stepsFingerprint by the text of the statements it
// ran, which alone decides the result.
var stepCatalogs sync.Map

// stepsFingerprint returns the catalog fingerprint that the schema steps up
// to version produce on an empty in-memory database.
func stepsFingerprint(ctx context.Context, version int) (string, error) {
	statements := sha256.New()
	for _, step := range schemaSteps {
		if step.version > version {
			break
		}
		for _, statement := range step.statements {
			_, _ = fmt.Fprintf(statements, "%d:%s", len(statement), statement)
		}
	}
	key := string(statements.Sum(nil))
	if fingerprint, ok := stepCatalogs.Load(key); ok {
		return fingerprint.(string), nil
	}
	fingerprint, err := buildStepsFingerprint(ctx, version)
	if err != nil {
		return "", err
	}
	stepCatalogs.Store(key, fingerprint)
	return fingerprint, nil
}

func buildStepsFingerprint(ctx context.Context, version int) (string, error) {
	db, err := sql.Open("sqlite", "file:owngit-schema-steps?mode=memory")
	if err != nil {
		return "", fmt.Errorf("prepare the schema %d catalog: %w", version, err)
	}
	defer db.Close()
	// Every connection has its own memory database, so all statements use
	// this one.
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("prepare the schema %d catalog: %w", version, err)
	}
	defer conn.Close()
	for _, step := range schemaSteps {
		if step.version > version {
			break
		}
		for _, statement := range step.statements {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return "", fmt.Errorf("prepare the schema %d catalog: %w", version, err)
			}
		}
	}
	fingerprint, _, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return "", fmt.Errorf("prepare the schema %d catalog: %w", version, err)
	}
	return fingerprint, nil
}

// validateUnversionedSchema accepts an empty database or the exact committed
// baseline catalog.
func validateUnversionedSchema(ctx context.Context, db queryRower) (schemaClass, error) {
	fingerprint, objects, err := schemaFingerprint(ctx, db)
	if err != nil {
		return schemaClass{}, fmt.Errorf("inspect unversioned state database: %w", err)
	}
	if objects == 0 {
		return schemaEmpty, nil
	}
	if fingerprint == committedBaselineSchemaFingerprint {
		return schemaBaseline, nil
	}
	return schemaClass{}, errors.New("state database has no schema version and does not match the committed baseline")
}

// schemaFingerprint hashes every entry of the catalog (sqlite_master): its
// type, name, table and statement with runs of white space made one space.
// No entry is left out by its name. SQLite's own entries, such as the
// indexes of primary keys and sqlite_sequence, follow from the tables, so
// the expected catalogs have them too, and an entry that someone added under
// a name SQLite reserves, which SQLite still loads and runs, is counted like
// any other.
func schemaFingerprint(ctx context.Context, db queryRower) (string, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name,tbl_name,sql`)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	digest := sha256.New()
	objects := 0
	for rows.Next() {
		var objectType, name, table string
		var statement sql.NullString
		if err := rows.Scan(&objectType, &name, &table, &statement); err != nil {
			return "", 0, err
		}
		normalized := ""
		if statement.Valid {
			normalized = strings.Join(strings.Fields(statement.String), " ")
		}
		for _, field := range []string{objectType, name, table, normalized} {
			_, _ = fmt.Fprintf(digest, "%d:", len(field))
			_, _ = digest.Write([]byte(field))
		}
		objects++
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), objects, nil
}

// migrate applies every authorized step in one immediate transaction. The
// classification inside that transaction is the migration authority. A
// completed concurrent migration is accepted, while any other owner change is
// refused before a migration statement runs.
func (s *Store) migrate(ctx context.Context, expected schemaClass) (err error) {
	// A step that rebuilds a parent table drops it, and with foreign keys on
	// SQLite would first delete every child row through ON DELETE CASCADE.
	// Enforcement is therefore off for the migration connection, which is the
	// only one, and is checked before commit and restored afterwards. The
	// pragma has no effect inside a transaction, so it is set around it. The
	// migration holds that one connection throughout: a new connection would
	// enforce foreign keys again (sqliteURI), and a held one that breaks
	// fails instead of being replaced.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("prepare state schema migration: %w", err)
	}
	defer conn.Close()
	// The restore is registered first: a statement that ctx interrupts may
	// already have turned enforcement off when it reports the error.
	defer func() {
		if _, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore foreign key enforcement after schema migration: %w", restoreErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("prepare state schema migration: %w", err)
	}
	// The catalog the migration must produce is built before the write
	// transaction begins: building it runs every schema statement on an
	// in-memory database, and other openers wait for the write lock.
	current, err := stepsFingerprint(ctx, currentSchemaVersion())
	if err != nil {
		return err
	}
	// Each statement stops on ctx, but the transaction itself does not end
	// with it: database/sql would roll it back on another goroutine, and
	// the restore below could then run while SQLite still has it open,
	// where the pragma does nothing. Only the deferred Rollback or the
	// Commit ends it, before the restore.
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	class, err := classifySchema(ctx, tx)
	if err != nil {
		return err
	}
	if class == schemaCurrent {
		return nil
	}
	if class != expected {
		return unstable("state database changed before schema migration")
	}
	// Only the committed baseline, an empty database and a released schema
	// reach this point. The first two run every step, a released schema every
	// step after its own version.
	previous := schemaSteps[0].version - 1
	if class.kind == kindReleased {
		previous = class.version
	}
	for _, step := range schemaSteps {
		if step.version <= previous {
			continue
		}
		if step.version != previous+1 {
			return fmt.Errorf("missing state schema migration %d", previous+1)
		}
		for _, statement := range step.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply state schema migration %d: %w", step.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(step.version)); err != nil {
			return fmt.Errorf("record state schema migration %d: %w", step.version, err)
		}
		previous = step.version
	}
	// The steps produce the current catalog from the one classifySchema
	// accepted; the result is compared as well, so that a migration commits
	// exactly the current schema or nothing.
	migrated, _, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return fmt.Errorf("check the schema after migration: %w", err)
	}
	if migrated != current {
		return fmt.Errorf("state schema migration to %d produced a schema other than schema %d; nothing was changed", previous, previous)
	}
	violations, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check foreign keys after schema migration: %w", err)
	}
	violated := violations.Next()
	if err := closeRows(violations); err != nil {
		return fmt.Errorf("check foreign keys after schema migration: %w", err)
	}
	if violated {
		return errors.New("state schema migration left a foreign key violation")
	}
	// The transaction ignores ctx, so a migration cancelled after its last
	// statement is refused here rather than committed.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	switch class.kind {
	case kindReleased:
		s.schemaUpgrade = fmt.Sprintf("state database upgraded from schema %d to %d", class.version, previous)
	case kindBaseline:
		s.schemaUpgrade = fmt.Sprintf("state database upgraded from the committed baseline (no schema version) to schema %d", previous)
	}
	return nil
}

// schemaStep upgrades the schema from version-1 to version.
type schemaStep struct {
	version int
	// released marks a version that an OwnGit release wrote. A database at a
	// released version below the current one upgrades in place through every
	// later step. Any other numbered version below the current one came from
	// an unreleased development build and is refused.
	released   bool
	statements []string
}

// schemaSteps is the ordered migration chain and the single source of truth
// for schema versions: its last step is the schema this build writes, and its
// released steps are the versions it upgrades. The committed baseline and an
// empty database run every step from the first. Together the steps produce the
// current schema catalog, so their text must not change. A new schema appends
// one step, and a release that writes it marks that step released. Version 6
// upgrades the committed baseline, version 7 adds local raw check logs, version
// 8 adds the unused direct-review tables, version 9 adds the automatic-check
// foundation, version 10 binds executable settings and executor roles, version
// 11 adds inbound import state, version 12 records structured HEAD ownership,
// version 13 records machine-local ownership of unpublished initial
// destinations, version 14 admits the owner_resolved intent status, and
// version 15 admits closed pull requests, and version 16 adds the durable
// fields of 1.1.3: pull request text and actors, repository names and
// policies, import options, share links, scheduled backups, recent pushes,
// and the start time of each raw check log for its cleanup.
// The 1.0.0 to 1.0.2 releases wrote schema 14, and 1.0.3 to 1.1.2 wrote
// schema 15.
// Schema 12 has no ownership rows. A missing row never authorizes removal or
// publication of a look-alike directory.
var schemaSteps = []schemaStep{
	{version: 6, statements: []string{
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS passwords (
			kind TEXT PRIMARY KEY CHECK (kind IN ('access','admin')),
			encoded TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash BLOB PRIMARY KEY,
			kind TEXT NOT NULL CHECK (kind IN ('setup','general','admin')),
			csrf TEXT NOT NULL,
			version INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS bootstrap (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			token_hash BLOB NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS login_attempts (
			kind TEXT NOT NULL,
			address TEXT NOT NULL,
			window_started_at INTEGER NOT NULL,
			attempts INTEGER NOT NULL,
			blocked_until INTEGER NOT NULL,
			PRIMARY KEY (kind, address)
		)`,
		`CREATE TABLE IF NOT EXISTS repositories (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pull_requests (
			repository_id TEXT NOT NULL,
			number INTEGER NOT NULL CHECK (number > 0),
			title TEXT NOT NULL,
			source_branch TEXT NOT NULL,
			target_branch TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('creating','open','merged')),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			merge_source_oid TEXT NOT NULL DEFAULT '',
			merge_target_oid TEXT NOT NULL DEFAULT '',
			merge_oid TEXT NOT NULL DEFAULT '',
			merge_receipt_ref TEXT NOT NULL DEFAULT '',
			merged_at INTEGER,
			PRIMARY KEY (repository_id, number),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS pull_request_revisions (
			repository_id TEXT NOT NULL,
			pull_request_number INTEGER NOT NULL,
			source_oid TEXT NOT NULL,
			target_oid TEXT NOT NULL,
			recorded_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id, pull_request_number, source_oid, target_oid),
			FOREIGN KEY (repository_id, pull_request_number) REFERENCES pull_requests(repository_id, number) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS pull_request_reviews (
			repository_id TEXT NOT NULL,
			pull_request_number INTEGER NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence > 0),
			source_oid TEXT NOT NULL,
			target_oid TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending','approved','changes_requested','skipped','not_requested')),
			reviewer_label TEXT NOT NULL,
			provenance TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id, pull_request_number, sequence),
			FOREIGN KEY (repository_id, pull_request_number) REFERENCES pull_requests(repository_id, number) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS pull_request_merge_intents (
			repository_id TEXT NOT NULL,
			pull_request_number INTEGER NOT NULL,
			source_oid TEXT NOT NULL,
			target_oid TEXT NOT NULL,
			mode TEXT NOT NULL,
			tree_oid TEXT NOT NULL,
			result_oid TEXT NOT NULL,
			receipt_ref TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('preparing','planned','ready','complete')),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id, pull_request_number, source_oid, target_oid),
			FOREIGN KEY (repository_id, pull_request_number) REFERENCES pull_requests(repository_id, number) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS trusted_hosts (
			host TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO metadata(key,value) VALUES
			('initialized','false'),
			('access_mode','open'),
			('access_session_version','1'),
			('admin_session_version','1'),
			('insecure_http_accepted','false')`,
		// Tasks keep only authoritative identity, title, and server-observed
		// timestamps. Status, budget, applied attempt, and pending attempt are
		// derived from attempts and reservations.
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			title TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		// One repository-wide counter issues attempt sequences. It advances
		// only with a new accepted registration.
		`CREATE TABLE IF NOT EXISTS repository_attempt_counters (
			repository_id TEXT PRIMARY KEY,
			attempt_sequence INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS check_configurations (
			repository_id TEXT NOT NULL,
			version INTEGER NOT NULL CHECK (version > 0),
			config_hash TEXT NOT NULL,
			checks_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id, version),
			UNIQUE (repository_id, config_hash),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		// The registration worktree observation and the submitted completion
		// observation are stored separately, so a clean registration followed
		// by a dirty completion stays verifiable.
		`CREATE TABLE IF NOT EXISTS check_attempts (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			revision_oid TEXT NOT NULL,
			registration_worktree_state TEXT NOT NULL CHECK (registration_worktree_state IN ('clean','dirty','unknown')),
			submitted_worktree_state TEXT NOT NULL DEFAULT '' CHECK (submitted_worktree_state IN ('','clean','dirty','unknown')),
			configuration_version INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending','passed','failed','error','cancelled','incomplete','unavailable')),
			exit_code INTEGER,
			started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT '',
			protection TEXT NOT NULL DEFAULT 'unknown',
			execution_scope TEXT NOT NULL DEFAULT 'inherited',
			credential_id TEXT NOT NULL DEFAULT '',
			timeout_ms INTEGER NOT NULL DEFAULT 0,
			output_limit_bytes INTEGER NOT NULL DEFAULT 0,
			log_id TEXT NOT NULL DEFAULT '',
			log_expires_at INTEGER,
			log_truncated INTEGER NOT NULL DEFAULT 0,
			log_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			sequence INTEGER NOT NULL,
			cycle_id TEXT NOT NULL DEFAULT '',
			registration_digest TEXT NOT NULL,
			completion_digest TEXT NOT NULL DEFAULT '',
			submitted_log_digest TEXT NOT NULL DEFAULT '',
			submitted_truncated INTEGER NOT NULL DEFAULT 0,
			submitted_cancelled INTEGER NOT NULL DEFAULT 0,
			log_digest TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS check_results (
			attempt_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			name TEXT NOT NULL,
			command TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('passed','failed','error','cancelled','incomplete','unavailable')),
			exit_code INTEGER,
			duration_ms INTEGER NOT NULL,
			output_excerpt TEXT NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			cleanup_error TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (attempt_id, position),
			FOREIGN KEY (attempt_id) REFERENCES check_attempts(id) ON DELETE CASCADE
		)`,
		// A cycle records the repository attempt counter observed inside its
		// reservation transaction. The boundary is immutable, so a
		// pre-reservation attempt finishing later cannot certify the round.
		`CREATE TABLE IF NOT EXISTS check_cycles (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence > 0),
			reserved_at INTEGER NOT NULL,
			reserved_after_sequence INTEGER NOT NULL,
			UNIQUE (task_id, sequence),
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS helper_credentials (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			label TEXT NOT NULL,
			creation_id TEXT NOT NULL DEFAULT '',
			token_hash BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER,
			last_used_at INTEGER,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS helper_credentials_creation ON helper_credentials(repository_id, creation_id) WHERE creation_id != ''`,
		`CREATE INDEX IF NOT EXISTS check_attempts_revision ON check_attempts(repository_id, revision_oid, sequence)`,
		`CREATE INDEX IF NOT EXISTS check_attempts_task ON check_attempts(task_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS check_attempts_pending ON check_attempts(task_id, status, sequence)`,
		`CREATE INDEX IF NOT EXISTS check_cycles_task ON check_cycles(task_id, sequence)`,
		// The committed baseline review table predates the optional review
		// status, and SQLite cannot alter a CHECK constraint in place.
		`CREATE TABLE pull_request_reviews_v6 (
			repository_id TEXT NOT NULL,
			pull_request_number INTEGER NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence > 0),
			source_oid TEXT NOT NULL,
			target_oid TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending','approved','changes_requested','skipped','not_requested')),
			reviewer_label TEXT NOT NULL,
			provenance TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id, pull_request_number, sequence),
			FOREIGN KEY (repository_id, pull_request_number) REFERENCES pull_requests(repository_id, number) ON DELETE CASCADE
		)`,
		`INSERT INTO pull_request_reviews_v6(repository_id,pull_request_number,sequence,source_oid,target_oid,status,reviewer_label,provenance,created_at)
			SELECT repository_id,pull_request_number,sequence,source_oid,target_oid,status,reviewer_label,provenance,created_at FROM pull_request_reviews`,
		`DROP TABLE pull_request_reviews`,
		`ALTER TABLE pull_request_reviews_v6 RENAME TO pull_request_reviews`,
	}},
	{version: 7, statements: []string{
		`CREATE TABLE check_raw_logs (
			attempt_id TEXT PRIMARY KEY,
			content BLOB NOT NULL CHECK (length(content) <= 262144),
			expires_at INTEGER NOT NULL,
			FOREIGN KEY (attempt_id) REFERENCES check_attempts(id) ON DELETE CASCADE
		) WITHOUT ROWID`,
		`CREATE INDEX check_raw_logs_expiry ON check_raw_logs(expires_at,attempt_id)`,
	}},
	{version: 8, statements: []string{
		`ALTER TABLE pull_request_reviews ADD COLUMN review_event_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX pull_request_reviews_event ON pull_request_reviews(repository_id,review_event_id) WHERE review_event_id != ''`,
		`CREATE TABLE direct_review_credentials (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL UNIQUE,
			label TEXT NOT NULL CHECK (length(CAST(label AS BLOB)) BETWEEN 1 AND 100),
			value TEXT NOT NULL CHECK (length(CAST(value AS BLOB)) BETWEEN 1 AND 16384),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE direct_review_repository_settings (
			repository_id TEXT PRIMARY KEY,
			configuration_version INTEGER NOT NULL CHECK (configuration_version > 0),
			protocol TEXT NOT NULL CHECK (protocol IN ('openai_responses','anthropic_messages','compatible_chat_completions')),
			endpoint TEXT NOT NULL CHECK (length(CAST(endpoint AS BLOB)) BETWEEN 1 AND 2048),
			model TEXT NOT NULL CHECK (length(CAST(model AS BLOB)) BETWEEN 1 AND 200),
			authentication_mode TEXT NOT NULL CHECK (authentication_mode IN ('stored_credential','none')),
			provider_limits_json TEXT NOT NULL CHECK (length(CAST(provider_limits_json AS BLOB)) BETWEEN 2 AND 4096),
			repository_limits_json TEXT NOT NULL CHECK (length(CAST(repository_limits_json AS BLOB)) BETWEEN 2 AND 4096),
			instruction_version TEXT NOT NULL CHECK (length(CAST(instruction_version AS BLOB)) BETWEEN 1 AND 100),
			credential_id TEXT NOT NULL DEFAULT '',
			connection_version INTEGER NOT NULL DEFAULT 0 CHECK (connection_version >= 0),
			authority_epoch TEXT NOT NULL CHECK (length(authority_epoch) = 32),
			probe_fingerprint TEXT NOT NULL DEFAULT '',
			probe_request_id TEXT NOT NULL DEFAULT '',
			probe_updated_at INTEGER NOT NULL DEFAULT 0,
			automatic_pr_enabled INTEGER NOT NULL DEFAULT 0 CHECK (automatic_pr_enabled IN (0,1)),
			automatic_task_enabled INTEGER NOT NULL DEFAULT 0 CHECK (automatic_task_enabled IN (0,1)),
			automatic_consent_version INTEGER NOT NULL DEFAULT 0 CHECK (automatic_consent_version >= 0),
			automatic_consent_digest TEXT NOT NULL DEFAULT '',
			automatic_consent_active INTEGER NOT NULL DEFAULT 0 CHECK (automatic_consent_active IN (0,1)),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE direct_review_probes (
			request_id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			registration_digest TEXT NOT NULL,
			capability_fingerprint TEXT NOT NULL,
			configuration_version INTEGER NOT NULL CHECK (configuration_version > 0),
			connection_version INTEGER NOT NULL CHECK (connection_version >= 0),
			connection_fingerprint TEXT NOT NULL,
			protocol TEXT NOT NULL,
			endpoint TEXT NOT NULL,
			model TEXT NOT NULL,
			authentication_mode TEXT NOT NULL,
			provider_limits_json TEXT NOT NULL CHECK (length(CAST(provider_limits_json AS BLOB)) BETWEEN 2 AND 4096),
			disclosure_version TEXT NOT NULL,
			disclosure_digest TEXT NOT NULL,
			phase TEXT NOT NULL CHECK (phase IN ('preparing','running','terminal')),
			cancel_requested_at INTEGER,
			created_at INTEGER NOT NULL,
			running_at INTEGER,
			terminal_at INTEGER,
			result_json TEXT NOT NULL DEFAULT '' CHECK (length(CAST(result_json AS BLOB)) <= 1048576),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX direct_review_probes_repository ON direct_review_probes(repository_id,created_at,request_id)`,
		`CREATE TABLE direct_review_task_contexts (
			context_id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			attempt_id TEXT NOT NULL UNIQUE,
			credential_id TEXT NOT NULL,
			base_oid TEXT NOT NULL,
			head_oid TEXT NOT NULL,
			pull_request_number INTEGER NOT NULL DEFAULT 0 CHECK (pull_request_number >= 0),
			observed_source_oid TEXT NOT NULL DEFAULT '',
			observed_target_oid TEXT NOT NULL DEFAULT '',
			registration_digest TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE,
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
			FOREIGN KEY (attempt_id) REFERENCES check_attempts(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX direct_review_task_contexts_task ON direct_review_task_contexts(repository_id,task_id,created_at,context_id)`,
		`CREATE TABLE direct_review_requests (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL UNIQUE,
			repository_id TEXT NOT NULL,
			trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('manual','automatic_pr','automatic_task')),
			source_event_key TEXT NOT NULL DEFAULT '',
			pull_request_number INTEGER NOT NULL DEFAULT 0 CHECK (pull_request_number >= 0),
			task_id TEXT NOT NULL DEFAULT '',
			attempt_id TEXT NOT NULL DEFAULT '',
			observed_source_oid TEXT NOT NULL DEFAULT '',
			observed_target_oid TEXT NOT NULL DEFAULT '',
			effective_base_oid TEXT NOT NULL DEFAULT '',
			effective_head_oid TEXT NOT NULL DEFAULT '',
			diff_mode TEXT NOT NULL DEFAULT '' CHECK (diff_mode IN ('','two_commit')),
			configuration_version INTEGER NOT NULL CHECK (configuration_version > 0),
			connection_version INTEGER NOT NULL CHECK (connection_version >= 0),
			connection_fingerprint TEXT NOT NULL,
			protocol TEXT NOT NULL,
			endpoint TEXT NOT NULL,
			model TEXT NOT NULL,
			authentication_mode TEXT NOT NULL,
			provider_limits_json TEXT NOT NULL CHECK (length(CAST(provider_limits_json AS BLOB)) BETWEEN 2 AND 4096),
			repository_limits_json TEXT NOT NULL CHECK (length(CAST(repository_limits_json AS BLOB)) BETWEEN 2 AND 4096),
			instruction_version TEXT NOT NULL,
			consent_version TEXT NOT NULL DEFAULT '',
			consent_digest TEXT NOT NULL DEFAULT '',
			disclosure_version TEXT NOT NULL,
			disclosure_digest TEXT NOT NULL,
			registration_digest TEXT NOT NULL,
			phase TEXT NOT NULL CHECK (phase IN ('observed','preparing','running','terminal')),
			cancel_requested_at INTEGER,
			created_at INTEGER NOT NULL,
			preparing_at INTEGER,
			running_at INTEGER,
			terminal_at INTEGER,
			initial_context_bytes INTEGER NOT NULL DEFAULT 0 CHECK (initial_context_bytes >= 0),
			initial_context_truncated INTEGER NOT NULL DEFAULT 0 CHECK (initial_context_truncated IN (0,1)),
			result_json TEXT NOT NULL DEFAULT '' CHECK (length(CAST(result_json AS BLOB)) <= 1048576),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX direct_review_requests_source_event ON direct_review_requests(repository_id,trigger_kind,source_event_key) WHERE source_event_key != ''`,
		`CREATE INDEX direct_review_requests_pull_request ON direct_review_requests(repository_id,pull_request_number,sequence) WHERE pull_request_number > 0`,
		`CREATE INDEX direct_review_requests_task ON direct_review_requests(repository_id,task_id,sequence) WHERE task_id != ''`,
	}},
	{version: 9, statements: []string{
		// Attempts gain a server-owned job link. Empty keeps the exact helper
		// behavior and every historical digest shape.
		`ALTER TABLE check_attempts ADD COLUMN job_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX check_attempts_job ON check_attempts(job_id) WHERE job_id != ''`,
		// One current policy row per repository. It holds the operator-selected
		// executor, the limits that cap workflow requests, the local consent
		// generation, and the local authority epoch. Queue and lease management
		// are server-owned and are not visible to the committed workflow.
		`CREATE TABLE check_policies (
			repository_id TEXT PRIMARY KEY,
			policy_version INTEGER NOT NULL CHECK (policy_version > 0),
			policy_digest TEXT NOT NULL CHECK (length(policy_digest) = 64),
			executor TEXT NOT NULL CHECK (executor IN ('host','container','external_runner')),
			allowed_events TEXT NOT NULL CHECK (length(CAST(allowed_events AS BLOB)) BETWEEN 2 AND 100),
			max_timeout_ms INTEGER NOT NULL CHECK (max_timeout_ms > 0),
			max_output_limit_bytes INTEGER NOT NULL CHECK (max_output_limit_bytes > 0),
			queue_limit INTEGER NOT NULL CHECK (queue_limit > 0),
			max_active_jobs INTEGER NOT NULL CHECK (max_active_jobs > 0),
			max_lease_ms INTEGER NOT NULL CHECK (max_lease_ms > 0),
			consent_version INTEGER NOT NULL DEFAULT 0 CHECK (consent_version >= 0),
			consent_digest TEXT NOT NULL DEFAULT '' CHECK (consent_digest = '' OR length(consent_digest) = 64),
			consent_active INTEGER NOT NULL DEFAULT 0 CHECK (consent_active IN (0,1)),
			runner_generation INTEGER NOT NULL DEFAULT 0 CHECK (runner_generation >= 0),
			authority_epoch TEXT NOT NULL CHECK (length(authority_epoch) = 32),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		// One durable job per effective execution condition. The dedup digest
		// covers the source, workflow, checks, limits, executor, policy, consent,
		// and rerun generation, so a repeated observation cannot double-run and an
		// uncertain loss requires an explicit rerun.
		`CREATE TABLE check_jobs (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('push','pull_request')),
			event_key TEXT NOT NULL CHECK (length(CAST(event_key AS BLOB)) BETWEEN 1 AND 200),
			source_oid TEXT NOT NULL,
			base_oid TEXT NOT NULL DEFAULT '',
			pull_request_number INTEGER NOT NULL DEFAULT 0 CHECK (pull_request_number >= 0),
			trigger_ref TEXT NOT NULL CHECK (length(CAST(trigger_ref AS BLOB)) BETWEEN 1 AND 200),
			workflow_path TEXT NOT NULL,
			workflow_oid TEXT NOT NULL DEFAULT '',
			workflow_digest TEXT NOT NULL CHECK (length(workflow_digest) = 64),
			configuration_version INTEGER NOT NULL CHECK (configuration_version > 0),
			executor TEXT NOT NULL CHECK (executor IN ('host','container','external_runner')),
			policy_version INTEGER NOT NULL CHECK (policy_version > 0),
			consent_version INTEGER NOT NULL CHECK (consent_version > 0),
			limits_json TEXT NOT NULL CHECK (length(CAST(limits_json AS BLOB)) BETWEEN 2 AND 4096),
			dedup_digest TEXT NOT NULL CHECK (length(dedup_digest) = 64),
			rerun_root TEXT NOT NULL DEFAULT '',
			rerun_generation INTEGER NOT NULL DEFAULT 0 CHECK (rerun_generation >= 0),
			status TEXT NOT NULL CHECK (status IN ('pending','claimed','started','passed','failed','error','cancelled','incomplete','unavailable','ambiguous','interrupted')),
			attempt_id TEXT NOT NULL DEFAULT '',
			lease_id TEXT NOT NULL DEFAULT '',
			lease_expires_at INTEGER,
			credential_id TEXT NOT NULL DEFAULT '',
			credential_generation INTEGER NOT NULL DEFAULT 0 CHECK (credential_generation >= 0),
			protection TEXT NOT NULL DEFAULT 'unknown' CHECK (protection IN ('unknown','host','container','runner_reported')),
			admitted_at INTEGER NOT NULL,
			claimed_at INTEGER,
			started_at INTEGER,
			finished_at INTEGER,
			lease_lost_at INTEGER,
			cancel_requested_at INTEGER,
			interrupted_at INTEGER,
			summary TEXT NOT NULL DEFAULT '' CHECK (length(CAST(summary AS BLOB)) <= 500),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE,
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX check_jobs_dedup ON check_jobs(repository_id,dedup_digest)`,
		`CREATE INDEX check_jobs_queue ON check_jobs(repository_id,status,admitted_at,id)`,
		`CREATE INDEX check_jobs_attempt ON check_jobs(attempt_id) WHERE attempt_id != ''`,
		// Dedicated revocable runner authority. Only the verifier is stored. The
		// issued token carries its local epoch, identity, and generation, so a
		// pre-restore bearer cannot become valid under new local authority.
		`CREATE TABLE check_runner_credentials (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			label TEXT NOT NULL CHECK (length(CAST(label AS BLOB)) BETWEEN 1 AND 100),
			creation_id TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL CHECK (generation > 0),
			token_hash BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER,
			last_used_at INTEGER,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX check_runner_credentials_creation ON check_runner_credentials(repository_id,creation_id) WHERE creation_id != ''`,
		`CREATE UNIQUE INDEX check_runner_credentials_hash ON check_runner_credentials(repository_id,token_hash) WHERE revoked_at IS NULL`,
		// Latest observed refs per repository. Observations are machine-local
		// capture evidence, so a restore starts without them.
		`CREATE TABLE check_observations (
			repository_id TEXT NOT NULL,
			ref_name TEXT NOT NULL CHECK (length(CAST(ref_name AS BLOB)) BETWEEN 1 AND 500),
			oid TEXT NOT NULL,
			observed_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id,ref_name),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX check_observations_recent ON check_observations(repository_id,observed_at,ref_name)`,
	}},
	{version: 10, statements: []string{
		// Schema 9 did not persist source/container bounds. Its rows remain valid
		// history under their original v1 digests, but consent is cleared and the
		// operator must save a complete v2 policy before new execution.
		`ALTER TABLE check_policies ADD COLUMN execution_json TEXT NOT NULL DEFAULT '{"legacy":true}' CHECK (length(CAST(execution_json AS BLOB)) BETWEEN 2 AND 4096)`,
		`UPDATE check_policies SET consent_active=0`,
		// Rebuild the job table to add captured execution authority and to widen
		// the accepted trigger set with the released merge event. Exact schema 9
		// classification makes this copy shape deterministic.
		`ALTER TABLE check_jobs RENAME TO check_jobs_v9`,
		`CREATE TABLE check_jobs (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('push','pull_request')),
			event_key TEXT NOT NULL CHECK (length(CAST(event_key AS BLOB)) BETWEEN 1 AND 200),
			source_oid TEXT NOT NULL,
			base_oid TEXT NOT NULL DEFAULT '',
			pull_request_number INTEGER NOT NULL DEFAULT 0 CHECK (pull_request_number >= 0),
			trigger_ref TEXT NOT NULL CHECK (length(CAST(trigger_ref AS BLOB)) BETWEEN 1 AND 200),
			workflow_path TEXT NOT NULL,
			workflow_oid TEXT NOT NULL DEFAULT '',
			workflow_digest TEXT NOT NULL CHECK (length(workflow_digest) = 64),
			configuration_version INTEGER NOT NULL CHECK (configuration_version > 0),
			executor TEXT NOT NULL CHECK (executor IN ('host','container','external_runner')),
			policy_version INTEGER NOT NULL CHECK (policy_version > 0),
			consent_version INTEGER NOT NULL CHECK (consent_version > 0),
			limits_json TEXT NOT NULL CHECK (length(CAST(limits_json AS BLOB)) BETWEEN 2 AND 4096),
			execution_json TEXT NOT NULL CHECK (length(CAST(execution_json AS BLOB)) BETWEEN 2 AND 4096),
			dedup_digest TEXT NOT NULL CHECK (length(dedup_digest) = 64),
			rerun_root TEXT NOT NULL DEFAULT '',
			rerun_generation INTEGER NOT NULL DEFAULT 0 CHECK (rerun_generation >= 0),
			status TEXT NOT NULL CHECK (status IN ('pending','claimed','started','passed','failed','error','cancelled','incomplete','unavailable','ambiguous','interrupted')),
			attempt_id TEXT NOT NULL DEFAULT '',
			lease_id TEXT NOT NULL DEFAULT '',
			lease_expires_at INTEGER,
			credential_id TEXT NOT NULL DEFAULT '',
			credential_generation INTEGER NOT NULL DEFAULT 0 CHECK (credential_generation >= 0),
			credential_role TEXT NOT NULL DEFAULT '' CHECK (credential_role IN ('','server','external_runner')),
			protection TEXT NOT NULL DEFAULT 'unknown' CHECK (protection IN ('unknown','host','container','runner_reported')),
			admitted_at INTEGER NOT NULL,
			claimed_at INTEGER,
			started_at INTEGER,
			finished_at INTEGER,
			lease_lost_at INTEGER,
			cancel_requested_at INTEGER,
			interrupted_at INTEGER,
			summary TEXT NOT NULL DEFAULT '' CHECK (length(CAST(summary AS BLOB)) <= 500),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE,
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
		)`,
		`INSERT INTO check_jobs(
			id,repository_id,task_id,trigger_kind,event_key,source_oid,base_oid,pull_request_number,trigger_ref,workflow_path,
			workflow_oid,workflow_digest,configuration_version,executor,policy_version,consent_version,limits_json,execution_json,
			dedup_digest,rerun_root,rerun_generation,status,attempt_id,lease_id,lease_expires_at,credential_id,credential_generation,credential_role,
			protection,admitted_at,claimed_at,started_at,finished_at,lease_lost_at,cancel_requested_at,interrupted_at,summary
		) SELECT
			id,repository_id,task_id,trigger_kind,event_key,source_oid,base_oid,pull_request_number,trigger_ref,workflow_path,
			workflow_oid,workflow_digest,configuration_version,executor,policy_version,consent_version,limits_json,'{"legacy":true}',
			dedup_digest,rerun_root,rerun_generation,status,attempt_id,lease_id,lease_expires_at,credential_id,credential_generation,'',
			protection,admitted_at,claimed_at,started_at,finished_at,lease_lost_at,cancel_requested_at,interrupted_at,summary
		FROM check_jobs_v9`,
		`DROP TABLE check_jobs_v9`,
		`CREATE UNIQUE INDEX check_jobs_dedup ON check_jobs(repository_id,dedup_digest)`,
		`CREATE INDEX check_jobs_queue ON check_jobs(repository_id,status,admitted_at,id)`,
		`CREATE INDEX check_jobs_attempt ON check_jobs(attempt_id) WHERE attempt_id != ''`,
		// Every schema 9 runner credential was external authority. Server-local
		// execution uses the policy epoch directly and never receives a bearer.
		`ALTER TABLE check_runner_credentials ADD COLUMN role TEXT NOT NULL DEFAULT 'external_runner' CHECK (role IN ('server','external_runner'))`,
		// Active container identity is machine-local cleanup authority and never
		// enters backup manifests. The daemon ID prevents cleanup against a
		// different mutable local Docker context after restart.
		`CREATE TABLE check_job_runtime_ownership (
			job_id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			container_name TEXT NOT NULL CHECK (length(CAST(container_name AS BLOB)) BETWEEN 1 AND 200),
			container_id TEXT NOT NULL CHECK (length(container_id)=0 OR length(container_id) BETWEEN 12 AND 64),
			daemon_id TEXT NOT NULL CHECK (length(CAST(daemon_id AS BLOB)) BETWEEN 1 AND 200),
			created_at INTEGER NOT NULL,
			FOREIGN KEY (job_id) REFERENCES check_jobs(id) ON DELETE CASCADE,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
	}},
	{version: 11, statements: []string{
		// One inbound import source per repository. No foreign key is declared
		// because a configured source can precede the repository row: the
		// destination object format is only known after the first advertisement.
		// allow_private_network is machine-local transport consent, so a restore
		// clears it while source identity and mode stay portable.
		`CREATE TABLE import_sources (
			repository_id TEXT PRIMARY KEY,
			url TEXT NOT NULL CHECK (length(CAST(url AS BLOB)) BETWEEN 1 AND 8192),
			source_generation INTEGER NOT NULL CHECK (source_generation > 0),
			authority_revision INTEGER NOT NULL CHECK (authority_revision > 0),
			credential_generation TEXT NOT NULL DEFAULT '' CHECK (length(credential_generation) IN (0,32)),
			mode TEXT NOT NULL CHECK (mode IN ('standalone','coexistence')),
			git_only_consent INTEGER NOT NULL DEFAULT 0 CHECK (git_only_consent IN (0,1)),
			allow_private_network INTEGER NOT NULL DEFAULT 0 CHECK (allow_private_network IN (0,1)),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// Append-only refresh history. Counts and inspection facts describe what
		// actually happened, including divergence and truncation.
		`CREATE TABLE import_runs (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			source_generation INTEGER NOT NULL CHECK (source_generation > 0),
			authority_revision INTEGER NOT NULL CHECK (authority_revision > 0),
			kind TEXT NOT NULL CHECK (kind IN ('initial','refresh','scheduled')),
			status TEXT NOT NULL CHECK (status IN ('preparing','fetching','indexing','inspecting','publishing','complete','failed','cancelled','superseded','interrupted','unresolved')),
			started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL DEFAULT 0,
			cancel_requested_at INTEGER,
			object_format TEXT NOT NULL DEFAULT '' CHECK (object_format IN ('','sha1','sha256')),
			refs_seen INTEGER NOT NULL DEFAULT 0 CHECK (refs_seen >= 0),
			refs_created INTEGER NOT NULL DEFAULT 0 CHECK (refs_created >= 0),
			refs_updated INTEGER NOT NULL DEFAULT 0 CHECK (refs_updated >= 0),
			refs_unchanged INTEGER NOT NULL DEFAULT 0 CHECK (refs_unchanged >= 0),
			refs_divergent INTEGER NOT NULL DEFAULT 0 CHECK (refs_divergent >= 0),
			refs_deleted_upstream INTEGER NOT NULL DEFAULT 0 CHECK (refs_deleted_upstream >= 0),
			refs_skipped INTEGER NOT NULL DEFAULT 0 CHECK (refs_skipped >= 0),
			pack_bytes INTEGER NOT NULL DEFAULT 0 CHECK (pack_bytes >= 0),
			http_body_bytes INTEGER NOT NULL DEFAULT 0 CHECK (http_body_bytes >= 0),
			head_advertised INTEGER NOT NULL DEFAULT 0 CHECK (head_advertised IN (0,1)),
			head_symref TEXT NOT NULL DEFAULT '' CHECK (length(CAST(head_symref AS BLOB)) <= 500),
			error_class TEXT NOT NULL DEFAULT '' CHECK (length(CAST(error_class AS BLOB)) <= 100),
			message TEXT NOT NULL DEFAULT '' CHECK (length(CAST(message AS BLOB)) <= 500),
			lfs_detected INTEGER NOT NULL DEFAULT 0 CHECK (lfs_detected >= 0),
			lfs_inspection_complete INTEGER NOT NULL DEFAULT 0 CHECK (lfs_inspection_complete IN (0,1)),
			lfs_scanned_blobs INTEGER NOT NULL DEFAULT 0 CHECK (lfs_scanned_blobs >= 0),
			lfs_scanned_bytes INTEGER NOT NULL DEFAULT 0 CHECK (lfs_scanned_bytes >= 0),
			staging_name TEXT NOT NULL DEFAULT '' CHECK (length(CAST(staging_name AS BLOB)) <= 100),
			cleanup_error TEXT NOT NULL DEFAULT '' CHECK (length(CAST(cleanup_error AS BLOB)) <= 500),
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX import_runs_repository ON import_runs(repository_id,started_at,id)`,
		// Last source fact per ref and generation. The observation baseline is
		// bound to the source generation so a new URL cannot inherit authority
		// over refs published by the previous URL.
		`CREATE TABLE import_ref_observations (
			repository_id TEXT NOT NULL,
			source_generation INTEGER NOT NULL CHECK (source_generation > 0),
			ref_name TEXT NOT NULL CHECK (length(CAST(ref_name AS BLOB)) BETWEEN 1 AND 500),
			oid TEXT NOT NULL DEFAULT '' CHECK (oid = '' OR length(oid) IN (40,64)),
			symref_target TEXT NOT NULL DEFAULT '' CHECK (length(CAST(symref_target AS BLOB)) <= 500),
			observed_at INTEGER NOT NULL,
			run_id TEXT NOT NULL DEFAULT '' CHECK (length(CAST(run_id AS BLOB)) <= 64),
			PRIMARY KEY (repository_id, source_generation, ref_name)
		)`,
		`CREATE INDEX import_ref_observations_recent ON import_ref_observations(repository_id,observed_at,ref_name)`,
		// Durable publication intent and receipt. expected/desired/retained are
		// JSON objects of ref to object ID, so reconciliation can verify actual
		// refs instead of trusting a receipt.
		`CREATE TABLE import_publication_intents (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			source_generation INTEGER NOT NULL CHECK (source_generation > 0),
			authority_revision INTEGER NOT NULL CHECK (authority_revision > 0),
			status TEXT NOT NULL CHECK (status IN ('planning','applied','complete','not_applied','abandoned','unresolved','invalidated')),
			expected_json TEXT NOT NULL CHECK (length(CAST(expected_json AS BLOB)) BETWEEN 2 AND 8388608),
			desired_json TEXT NOT NULL CHECK (length(CAST(desired_json AS BLOB)) BETWEEN 2 AND 8388608),
			observed_json TEXT NOT NULL CHECK (length(CAST(observed_json AS BLOB)) BETWEEN 2 AND 8388608),
			retained_json TEXT NOT NULL CHECK (length(CAST(retained_json AS BLOB)) BETWEEN 2 AND 8388608),
			head_symref TEXT NOT NULL DEFAULT '' CHECK (length(CAST(head_symref AS BLOB)) <= 500),
			head_detach TEXT NOT NULL DEFAULT '' CHECK (length(CAST(head_detach AS BLOB)) <= 64),
			receipt_json TEXT NOT NULL DEFAULT '' CHECK (length(CAST(receipt_json AS BLOB)) <= 8388608),
			receipt_digest TEXT NOT NULL DEFAULT '' CHECK (receipt_digest = '' OR length(receipt_digest) = 64),
			reason TEXT NOT NULL DEFAULT '' CHECK (length(CAST(reason AS BLOB)) <= 500),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX import_publication_intents_repository ON import_publication_intents(repository_id,created_at,id)`,
		// Task-owned staging ownership. The on-disk marker must agree with this
		// row before cleanup deletes anything, so a name alone is not proof.
		`CREATE TABLE import_stagings (
			name TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			token TEXT NOT NULL CHECK (length(token) BETWEEN 16 AND 64),
			state TEXT NOT NULL CHECK (state IN ('active','released','cleanup_failed','unknown')),
			issue TEXT NOT NULL DEFAULT '' CHECK (length(CAST(issue AS BLOB)) <= 500),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX import_stagings_run ON import_stagings(run_id)`,
		// Machine-local opt-in schedule. A restore drops these rows.
		`CREATE TABLE import_schedules (
			repository_id TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
			interval_seconds INTEGER NOT NULL CHECK (interval_seconds BETWEEN 60 AND 604800),
			last_started_at INTEGER,
			last_finished_at INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX import_schedules_due ON import_schedules(enabled,last_started_at,repository_id)`,
	}},
	{version: 12, statements: []string{
		`ALTER TABLE import_publication_intents ADD COLUMN head_owned INTEGER NOT NULL DEFAULT 0 CHECK (head_owned IN (0,1))`,
	}},
	{version: 13, statements: []string{
		`CREATE TABLE import_initial_destinations (
			name TEXT PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 80),
			repository_id TEXT NOT NULL CHECK (length(repository_id) <= 100),
			run_id TEXT NOT NULL CHECK (length(run_id) <= 64),
			root_id TEXT NOT NULL CHECK (length(root_id) = 32),
			token TEXT NOT NULL CHECK (length(token) BETWEEN 16 AND 64),
			display_name TEXT NOT NULL DEFAULT '' CHECK (length(display_name) <= 100),
			description TEXT NOT NULL DEFAULT '' CHECK (length(CAST(description AS BLOB)) <= 500),
			state TEXT NOT NULL CHECK (state IN ('preparing','ready','published','released','cleanup_failed','unknown')),
			issue TEXT NOT NULL DEFAULT '' CHECK (length(CAST(issue AS BLOB)) <= 500),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX import_initial_destinations_run ON import_initial_destinations(run_id)`,
	}},
	// SQLite cannot change a CHECK constraint in place, so the intent table is
	// rebuilt with every row and column kept. owner_resolved is terminal: the
	// owner accepted the destination as found after an unresolved outcome.
	{version: 14, released: true, statements: []string{
		`CREATE TABLE import_publication_intents_v14 (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			source_generation INTEGER NOT NULL CHECK (source_generation > 0),
			authority_revision INTEGER NOT NULL CHECK (authority_revision > 0),
			status TEXT NOT NULL CHECK (status IN ('planning','applied','complete','not_applied','abandoned','unresolved','invalidated','owner_resolved')),
			expected_json TEXT NOT NULL CHECK (length(CAST(expected_json AS BLOB)) BETWEEN 2 AND 8388608),
			desired_json TEXT NOT NULL CHECK (length(CAST(desired_json AS BLOB)) BETWEEN 2 AND 8388608),
			observed_json TEXT NOT NULL CHECK (length(CAST(observed_json AS BLOB)) BETWEEN 2 AND 8388608),
			retained_json TEXT NOT NULL CHECK (length(CAST(retained_json AS BLOB)) BETWEEN 2 AND 8388608),
			head_symref TEXT NOT NULL DEFAULT '' CHECK (length(CAST(head_symref AS BLOB)) <= 500),
			head_detach TEXT NOT NULL DEFAULT '' CHECK (length(CAST(head_detach AS BLOB)) <= 64),
			receipt_json TEXT NOT NULL DEFAULT '' CHECK (length(CAST(receipt_json AS BLOB)) <= 8388608),
			receipt_digest TEXT NOT NULL DEFAULT '' CHECK (receipt_digest = '' OR length(receipt_digest) = 64),
			reason TEXT NOT NULL DEFAULT '' CHECK (length(CAST(reason AS BLOB)) <= 500),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			head_owned INTEGER NOT NULL DEFAULT 0 CHECK (head_owned IN (0,1))
		)`,
		// rowid order is the pending-intent page order, so it is copied too.
		`INSERT INTO import_publication_intents_v14(rowid,id,repository_id,run_id,source_generation,authority_revision,status,expected_json,desired_json,observed_json,retained_json,
			head_symref,head_detach,receipt_json,receipt_digest,reason,created_at,updated_at,head_owned)
			SELECT rowid,id,repository_id,run_id,source_generation,authority_revision,status,expected_json,desired_json,observed_json,retained_json,
			head_symref,head_detach,receipt_json,receipt_digest,reason,created_at,updated_at,head_owned FROM import_publication_intents`,
		`DROP TABLE import_publication_intents`,
		`ALTER TABLE import_publication_intents_v14 RENAME TO import_publication_intents`,
		`CREATE INDEX import_publication_intents_repository ON import_publication_intents(repository_id,created_at,id)`,
	}},
	// A pull request can be closed without merging. The table is rebuilt with
	// every row and column kept; its revisions, reviews and merge intents
	// refer to it by name and keep their rows (see migrate).
	{version: 15, released: true, statements: []string{
		`CREATE TABLE pull_requests_v15 (
			repository_id TEXT NOT NULL,
			number INTEGER NOT NULL CHECK (number > 0),
			title TEXT NOT NULL,
			source_branch TEXT NOT NULL,
			target_branch TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('creating','open','merged','closed')),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			merge_source_oid TEXT NOT NULL DEFAULT '',
			merge_target_oid TEXT NOT NULL DEFAULT '',
			merge_oid TEXT NOT NULL DEFAULT '',
			merge_receipt_ref TEXT NOT NULL DEFAULT '',
			merged_at INTEGER,
			PRIMARY KEY (repository_id, number),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`INSERT INTO pull_requests_v15(repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,
			merge_source_oid,merge_target_oid,merge_oid,merge_receipt_ref,merged_at)
			SELECT repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,
			merge_source_oid,merge_target_oid,merge_oid,merge_receipt_ref,merged_at FROM pull_requests`,
		`DROP TABLE pull_requests`,
		`ALTER TABLE pull_requests_v15 RENAME TO pull_requests`,
	}},
	// Schema 16 holds every durable field of the 1.1.3 release. Columns that
	// hold an actor keep canonical Actor JSON, or '' when nobody was recorded.
	{version: 16, released: true, statements: []string{
		// Pull request text, its edits, and who created, edited and merged it.
		`ALTER TABLE pull_requests ADD COLUMN body TEXT NOT NULL DEFAULT '' CHECK (length(CAST(body AS BLOB)) <= 65536)`,
		`ALTER TABLE pull_requests ADD COLUMN edit_revision INTEGER NOT NULL DEFAULT 0 CHECK (edit_revision >= 0)`,
		`ALTER TABLE pull_requests ADD COLUMN edited_at INTEGER`,
		`ALTER TABLE pull_requests ADD COLUMN created_by TEXT NOT NULL DEFAULT '' CHECK (created_by = '' OR (json_valid(created_by) AND length(CAST(created_by AS BLOB)) <= 1024))`,
		`ALTER TABLE pull_requests ADD COLUMN edited_by TEXT NOT NULL DEFAULT '' CHECK (edited_by = '' OR (json_valid(edited_by) AND length(CAST(edited_by AS BLOB)) <= 1024))`,
		`ALTER TABLE pull_requests ADD COLUMN merged_by TEXT NOT NULL DEFAULT '' CHECK (merged_by = '' OR (json_valid(merged_by) AND length(CAST(merged_by AS BLOB)) <= 1024))`,
		// A review row binds the source and target revisions it reviewed, so
		// its note is bound to both.
		`ALTER TABLE pull_request_reviews ADD COLUMN note TEXT NOT NULL DEFAULT '' CHECK (length(CAST(note AS BLOB)) <= 65536)`,
		`ALTER TABLE pull_request_reviews ADD COLUMN actor TEXT NOT NULL DEFAULT '' CHECK (actor = '' OR (json_valid(actor) AND length(CAST(actor AS BLOB)) <= 1024))`,
		// A repository answers at its ID until it is renamed. The ID and the
		// storage folder never change; a rename records the current name and
		// keeps the previous one as an alias until alias_until.
		`CREATE TABLE repository_names (
			name TEXT PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 100),
			repository_id TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('current','alias')),
			created_at INTEGER NOT NULL,
			alias_until INTEGER,
			CHECK ((kind = 'current') = (alias_until IS NULL)),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX repository_names_current ON repository_names(repository_id) WHERE kind = 'current'`,
		`CREATE INDEX repository_names_repository ON repository_names(repository_id,kind,name)`,
		// A repository's own policies. No row means every default; a NULL
		// retain_history follows the server default.
		`CREATE TABLE repository_policies (
			repository_id TEXT PRIMARY KEY,
			retain_history INTEGER CHECK (retain_history IN (0,1)),
			protect_default_branch INTEGER NOT NULL DEFAULT 0 CHECK (protect_default_branch IN (0,1)),
			extra_ref_prefixes TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(extra_ref_prefixes) AND length(CAST(extra_ref_prefixes AS BLOB)) <= 4096),
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		// Import refresh behaviour is portable. Connection choices are
		// machine-local like allow_private_network: a restore resets them.
		`ALTER TABLE import_sources ADD COLUMN overwrite_diverged INTEGER NOT NULL DEFAULT 0 CHECK (overwrite_diverged IN (0,1))`,
		`ALTER TABLE import_sources ADD COLUMN follow_upstream_deletions INTEGER NOT NULL DEFAULT 0 CHECK (follow_upstream_deletions IN (0,1))`,
		`ALTER TABLE import_sources ADD COLUMN extra_ref_prefixes TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(extra_ref_prefixes) AND length(CAST(extra_ref_prefixes AS BLOB)) <= 4096)`,
		`ALTER TABLE import_sources ADD COLUMN allow_plain_http INTEGER NOT NULL DEFAULT 0 CHECK (allow_plain_http IN (0,1))`,
		`ALTER TABLE import_sources ADD COLUMN redirect_policy TEXT NOT NULL DEFAULT 'refuse' CHECK (redirect_policy IN ('refuse','same_origin','approved'))`,
		`ALTER TABLE import_sources ADD COLUMN approved_redirect_origin TEXT NOT NULL DEFAULT '' CHECK (length(CAST(approved_redirect_origin AS BLOB)) <= 2048)`,
		`ALTER TABLE import_sources ADD COLUMN allow_reserved_addresses INTEGER NOT NULL DEFAULT 0 CHECK (allow_reserved_addresses IN (0,1))`,
		`ALTER TABLE import_sources ADD COLUMN limits_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(limits_json) AND length(CAST(limits_json AS BLOB)) <= 4096)`,
		// The authority revision of the latest sign-in change or restore. A
		// refresh follows an upstream deletion only for a ref a run at or
		// after it observed. Machine-local: a restore sets it.
		`ALTER TABLE import_sources ADD COLUMN sign_in_revision INTEGER NOT NULL DEFAULT 0 CHECK (sign_in_revision >= 0)`,
		// Read-only share links are machine-local authority. Only a hash of
		// the secret is stored, and a restore brings no link back.
		`CREATE TABLE share_links (
			id TEXT PRIMARY KEY CHECK (length(id) = 32),
			repository_id TEXT NOT NULL,
			secret_hash BLOB NOT NULL UNIQUE CHECK (length(secret_hash) = 32),
			scope TEXT NOT NULL CHECK (scope IN ('browse','clone')),
			label TEXT NOT NULL DEFAULT '' CHECK (length(CAST(label AS BLOB)) <= 100),
			password_hash TEXT NOT NULL DEFAULT '' CHECK (length(password_hash) <= 512),
			created_by TEXT NOT NULL DEFAULT '' CHECK (created_by = '' OR (json_valid(created_by) AND length(CAST(created_by AS BLOB)) <= 1024)),
			created_at INTEGER NOT NULL,
			expires_at INTEGER,
			revoked_at INTEGER,
			last_used_at INTEGER,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX share_links_repository ON share_links(repository_id,created_at,id)`,
		// Scheduled backups and their runs are machine-local. At most one run
		// is running, so a restart cannot start a second one beside it.
		`CREATE TABLE backup_schedule (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
			interval_seconds INTEGER NOT NULL CHECK (interval_seconds BETWEEN 3600 AND 2592000),
			destination TEXT NOT NULL CHECK (length(CAST(destination AS BLOB)) BETWEEN 1 AND 4096),
			keep INTEGER NOT NULL CHECK (keep BETWEEN 1 AND 1000),
			verify INTEGER NOT NULL DEFAULT 1 CHECK (verify IN (0,1)),
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE backup_runs (
			id TEXT PRIMARY KEY CHECK (length(id) = 32),
			kind TEXT NOT NULL CHECK (kind IN ('scheduled','manual')),
			status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','interrupted')),
			destination TEXT NOT NULL CHECK (length(CAST(destination AS BLOB)) BETWEEN 1 AND 4096),
			backup_name TEXT NOT NULL DEFAULT '' CHECK (length(CAST(backup_name AS BLOB)) <= 255),
			verification TEXT NOT NULL DEFAULT 'not_run' CHECK (verification IN ('not_run','passed','failed')),
			message TEXT NOT NULL DEFAULT '' CHECK (length(CAST(message AS BLOB)) <= 500),
			started_at INTEGER NOT NULL,
			finished_at INTEGER,
			-- The longest time the backup kept one repository's Git writes
			-- waiting and that repository, once its capture finished.
			longest_hold_ms INTEGER CHECK (longest_hold_ms IS NULL OR longest_hold_ms >= 0),
			longest_hold_repository TEXT CHECK (longest_hold_repository IS NULL OR length(longest_hold_repository) BETWEEN 1 AND 100),
			-- The SHA-256 of the manifest the run wrote, which names its
			-- backup: only the folder that holds this manifest is the run's.
			manifest_sha256 TEXT CHECK (manifest_sha256 IS NULL OR (length(manifest_sha256) = 64 AND manifest_sha256 NOT GLOB '*[^0-9a-f]*'))
		)`,
		`CREATE UNIQUE INDEX backup_runs_running ON backup_runs(status) WHERE status = 'running'`,
		`CREATE INDEX backup_runs_recent ON backup_runs(started_at,id)`,
		// Recent successful pushes for the tray, machine-local and bounded by
		// the writer. It is not a delivery queue.
		`CREATE TABLE push_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			repository_id TEXT NOT NULL,
			ref_name TEXT NOT NULL CHECK (length(CAST(ref_name AS BLOB)) BETWEEN 1 AND 500),
			old_oid TEXT NOT NULL CHECK (old_oid = '' OR length(old_oid) IN (40,64)),
			new_oid TEXT NOT NULL CHECK (new_oid = '' OR length(new_oid) IN (40,64)),
			refs_updated INTEGER NOT NULL CHECK (refs_updated >= 1),
			actor TEXT NOT NULL DEFAULT '' CHECK (actor = '' OR (json_valid(actor) AND length(CAST(actor AS BLOB)) <= 1024)),
			pushed_at INTEGER NOT NULL,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX push_events_repository ON push_events(repository_id,sequence)`,
		// Each raw check log by its attempt's created_at, the time its check
		// started, which never changes, so a cleanup reads the logs it
		// removes in order and stops at the first it keeps. A row goes with
		// its raw log.
		`CREATE TABLE check_raw_log_starts (
			created_at INTEGER NOT NULL,
			attempt_id TEXT NOT NULL UNIQUE,
			PRIMARY KEY (created_at, attempt_id),
			FOREIGN KEY (attempt_id) REFERENCES check_raw_logs(attempt_id) ON DELETE CASCADE
		) WITHOUT ROWID`,
		`INSERT INTO check_raw_log_starts(created_at,attempt_id)
			SELECT a.created_at,r.attempt_id FROM check_raw_logs r JOIN check_attempts a ON a.id=r.attempt_id`,
	}},
	{version: 17, statements: []string{
		`ALTER TABLE sessions ADD COLUMN verified_at INTEGER`,
		`UPDATE sessions SET verified_at=expires_at-CASE COALESCE(
			(SELECT value FROM metadata WHERE key='admin_confirmation'),'30m')
			WHEN '30m' THEN 1800 WHEN '1h' THEN 3600 WHEN '8h' THEN 28800
			WHEN '1d' THEN 86400 WHEN '7d' THEN 604800 WHEN '30d' THEN 2592000
			END WHERE kind='admin'`,
		// Rebuild parents under new names so child foreign keys keep their targets.
		`CREATE TABLE check_attempts_v17 (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			revision_oid TEXT NOT NULL,
			registration_worktree_state TEXT NOT NULL CHECK (registration_worktree_state IN ('clean','dirty','unknown')),
			submitted_worktree_state TEXT NOT NULL DEFAULT '' CHECK (submitted_worktree_state IN ('','clean','dirty','unknown')),
			configuration_version INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending','passed','failed','error','cancelled','incomplete','unavailable','skipped')),
			exit_code INTEGER,
			started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT '',
			protection TEXT NOT NULL DEFAULT 'unknown',
			execution_scope TEXT NOT NULL DEFAULT 'inherited',
			credential_id TEXT NOT NULL DEFAULT '',
			timeout_ms INTEGER NOT NULL DEFAULT 0,
			output_limit_bytes INTEGER NOT NULL DEFAULT 0,
			log_id TEXT NOT NULL DEFAULT '',
			log_expires_at INTEGER,
			log_truncated INTEGER NOT NULL DEFAULT 0,
			log_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			sequence INTEGER NOT NULL,
			cycle_id TEXT NOT NULL DEFAULT '',
			registration_digest TEXT NOT NULL,
			completion_digest TEXT NOT NULL DEFAULT '',
			submitted_log_digest TEXT NOT NULL DEFAULT '',
			submitted_truncated INTEGER NOT NULL DEFAULT 0,
			submitted_cancelled INTEGER NOT NULL DEFAULT 0,
			log_digest TEXT NOT NULL DEFAULT '',
			job_id TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`INSERT INTO check_attempts_v17(id,task_id,repository_id,revision_oid,registration_worktree_state,submitted_worktree_state,configuration_version,status,exit_code,started_at,finished_at,duration_ms,summary,protection,execution_scope,credential_id,timeout_ms,output_limit_bytes,log_id,log_expires_at,log_truncated,log_error,created_at,sequence,cycle_id,registration_digest,completion_digest,submitted_log_digest,submitted_truncated,submitted_cancelled,log_digest,job_id) SELECT id,task_id,repository_id,revision_oid,registration_worktree_state,submitted_worktree_state,configuration_version,status,exit_code,started_at,finished_at,duration_ms,summary,protection,execution_scope,credential_id,timeout_ms,output_limit_bytes,log_id,log_expires_at,log_truncated,log_error,created_at,sequence,cycle_id,registration_digest,completion_digest,submitted_log_digest,submitted_truncated,submitted_cancelled,log_digest,job_id FROM check_attempts`,
		`DROP TABLE check_attempts`,
		`ALTER TABLE check_attempts_v17 RENAME TO check_attempts`,
		`CREATE INDEX check_attempts_revision ON check_attempts(repository_id,revision_oid,sequence)`,
		`CREATE INDEX check_attempts_task ON check_attempts(task_id,sequence)`,
		`CREATE INDEX check_attempts_pending ON check_attempts(task_id,status,sequence)`,
		`CREATE INDEX check_attempts_job ON check_attempts(job_id) WHERE job_id != ''`,
		`CREATE TABLE check_results_v17 (
			attempt_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			name TEXT NOT NULL,
			command TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('passed','failed','error','cancelled','incomplete','unavailable','skipped','not_run')),
			exit_code INTEGER,
			duration_ms INTEGER NOT NULL,
			output_excerpt TEXT NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			cleanup_error TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '' CHECK (role IN ('','run','tolerated','builtin')),
			PRIMARY KEY (attempt_id, position),
			FOREIGN KEY (attempt_id) REFERENCES check_attempts(id) ON DELETE CASCADE
		)`,
		`INSERT INTO check_results_v17(attempt_id,position,name,command,status,exit_code,duration_ms,output_excerpt,truncated,cleanup_error) SELECT attempt_id,position,name,command,status,exit_code,duration_ms,output_excerpt,truncated,cleanup_error FROM check_results`,
		`DROP TABLE check_results`,
		`ALTER TABLE check_results_v17 RENAME TO check_results`,
		`CREATE TABLE check_jobs_v17 (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('push','pull_request','workflow_dispatch','schedule')),
			event_key TEXT NOT NULL CHECK (length(CAST(event_key AS BLOB)) BETWEEN 1 AND 200),
			source_oid TEXT NOT NULL,
			base_oid TEXT NOT NULL DEFAULT '',
			pull_request_number INTEGER NOT NULL DEFAULT 0 CHECK (pull_request_number >= 0),
			trigger_ref TEXT NOT NULL CHECK (length(CAST(trigger_ref AS BLOB)) BETWEEN 1 AND 200),
			workflow_path TEXT NOT NULL,
			workflow_oid TEXT NOT NULL DEFAULT '',
			workflow_digest TEXT NOT NULL CHECK (length(workflow_digest) = 64),
			configuration_version INTEGER NOT NULL CHECK (configuration_version > 0),
			executor TEXT NOT NULL CHECK (executor IN ('host','container','external_runner')),
			policy_version INTEGER NOT NULL CHECK (policy_version > 0),
			consent_version INTEGER NOT NULL CHECK (consent_version > 0),
			limits_json TEXT NOT NULL CHECK (length(CAST(limits_json AS BLOB)) BETWEEN 2 AND 4096),
			execution_json TEXT NOT NULL CHECK (length(CAST(execution_json AS BLOB)) BETWEEN 2 AND 4096),
			dedup_digest TEXT NOT NULL CHECK (length(dedup_digest) = 64),
			rerun_root TEXT NOT NULL DEFAULT '',
			rerun_generation INTEGER NOT NULL DEFAULT 0 CHECK (rerun_generation >= 0),
			status TEXT NOT NULL CHECK (status IN ('pending','claimed','started','passed','failed','error','cancelled','incomplete','unavailable','ambiguous','interrupted','waiting','skipped')),
			attempt_id TEXT NOT NULL DEFAULT '',
			lease_id TEXT NOT NULL DEFAULT '',
			lease_expires_at INTEGER,
			credential_id TEXT NOT NULL DEFAULT '',
			credential_generation INTEGER NOT NULL DEFAULT 0 CHECK (credential_generation >= 0),
			credential_role TEXT NOT NULL DEFAULT '' CHECK (credential_role IN ('','server','external_runner')),
			protection TEXT NOT NULL DEFAULT 'unknown' CHECK (protection IN ('unknown','host','container','runner_reported')),
			admitted_at INTEGER NOT NULL,
			claimed_at INTEGER,
			started_at INTEGER,
			finished_at INTEGER,
			lease_lost_at INTEGER,
			cancel_requested_at INTEGER,
			interrupted_at INTEGER,
			summary TEXT NOT NULL DEFAULT '' CHECK (length(CAST(summary AS BLOB)) <= 500),
			run_id TEXT NOT NULL DEFAULT '',
			job_key TEXT NOT NULL DEFAULT '' CHECK (length(CAST(job_key AS BLOB)) <= 100),
			matrix_index INTEGER NOT NULL DEFAULT 0 CHECK (matrix_index >= 0),
			plan_digest TEXT NOT NULL DEFAULT '' CHECK (length(plan_digest) IN (0,64)),
			tolerated INTEGER NOT NULL DEFAULT 0 CHECK (tolerated IN (0,1)),
			concurrency_group TEXT NOT NULL DEFAULT '' CHECK (length(CAST(concurrency_group AS BLOB)) <= 200),
			max_parallel INTEGER NOT NULL DEFAULT 0 CHECK (max_parallel >= 0),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE,
			FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
		)`,
		`INSERT INTO check_jobs_v17(id,repository_id,task_id,trigger_kind,event_key,source_oid,base_oid,pull_request_number,trigger_ref,workflow_path,workflow_oid,workflow_digest,configuration_version,executor,policy_version,consent_version,limits_json,execution_json,dedup_digest,rerun_root,rerun_generation,status,attempt_id,lease_id,lease_expires_at,credential_id,credential_generation,credential_role,protection,admitted_at,claimed_at,started_at,finished_at,lease_lost_at,cancel_requested_at,interrupted_at,summary) SELECT id,repository_id,task_id,trigger_kind,event_key,source_oid,base_oid,pull_request_number,trigger_ref,workflow_path,workflow_oid,workflow_digest,configuration_version,executor,policy_version,consent_version,limits_json,execution_json,dedup_digest,rerun_root,rerun_generation,status,attempt_id,lease_id,lease_expires_at,credential_id,credential_generation,credential_role,protection,admitted_at,claimed_at,started_at,finished_at,lease_lost_at,cancel_requested_at,interrupted_at,summary FROM check_jobs`,
		`DROP TABLE check_jobs`,
		`ALTER TABLE check_jobs_v17 RENAME TO check_jobs`,
		`CREATE UNIQUE INDEX check_jobs_dedup ON check_jobs(repository_id,dedup_digest)`,
		`CREATE INDEX check_jobs_queue ON check_jobs(repository_id,status,admitted_at,id)`,
		`CREATE INDEX check_jobs_attempt ON check_jobs(attempt_id) WHERE attempt_id != ''`,
		`CREATE INDEX check_jobs_run ON check_jobs(run_id) WHERE run_id != ''`,
		`CREATE INDEX check_jobs_waiting ON check_jobs(repository_id) WHERE status='waiting'`,
		`CREATE INDEX check_jobs_group ON check_jobs(repository_id,concurrency_group) WHERE concurrency_group != '' AND status IN ('claimed','started')`,
		`CREATE INDEX check_jobs_live_leases ON check_jobs(lease_expires_at) WHERE status IN ('claimed','started')`,
		`CREATE INDEX check_jobs_pending ON check_jobs(repository_id,admitted_at,id) WHERE status='pending'`,
		`CREATE INDEX check_jobs_recent ON check_jobs(repository_id,admitted_at DESC,id DESC)`,
		`CREATE INDEX import_runs_finished ON import_runs(finished_at,id)`,
		`ALTER TABLE check_policies ADD COLUMN run_workflows INTEGER NOT NULL DEFAULT 0 CHECK (run_workflows IN (0,1))`,
		`CREATE TABLE actions_runs (
			id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			workflow_path TEXT NOT NULL CHECK (length(CAST(workflow_path AS BLOB)) <= 118),
			number INTEGER NOT NULL CHECK (number > 0),
			event TEXT NOT NULL CHECK (event IN ('push','pull_request','workflow_dispatch','schedule')),
			event_key TEXT NOT NULL CHECK (length(CAST(event_key AS BLOB)) BETWEEN 1 AND 200),
			source_oid TEXT NOT NULL,
			base_oid TEXT NOT NULL DEFAULT '',
			pull_request_number INTEGER NOT NULL DEFAULT 0 CHECK (pull_request_number >= 0),
			trigger_ref TEXT NOT NULL CHECK (length(CAST(trigger_ref AS BLOB)) BETWEEN 1 AND 200),
			inputs_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(inputs_json) AND length(CAST(inputs_json AS BLOB)) <= 65536),
			scheduled_for INTEGER,
			rerun_root TEXT NOT NULL DEFAULT '',
			rerun_generation INTEGER NOT NULL DEFAULT 0 CHECK (rerun_generation >= 0),
			outcome TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','refused','not_run')),
			reason TEXT NOT NULL DEFAULT '',
			concurrency_group TEXT NOT NULL DEFAULT '' CHECK (length(CAST(concurrency_group AS BLOB)) <= 200),
			cancel_in_progress INTEGER NOT NULL DEFAULT 0 CHECK (cancel_in_progress IN (0,1)),
			concurrency_queue TEXT NOT NULL DEFAULT 'single' CHECK (concurrency_queue IN ('single','max')),
			cancel_requested_at INTEGER,
			facts_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(facts_json) AND length(CAST(facts_json AS BLOB)) <= 65536),
			policy_version INTEGER NOT NULL CHECK (policy_version > 0),
			consent_version INTEGER NOT NULL CHECK (consent_version > 0),
			actor TEXT NOT NULL DEFAULT '' CHECK (actor = '' OR (json_valid(actor) AND length(CAST(actor AS BLOB)) <= 1024)),
			created_at INTEGER NOT NULL,
			UNIQUE (repository_id,event,event_key,workflow_path,rerun_generation),
			UNIQUE (repository_id,workflow_path,number),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX actions_runs_revision ON actions_runs(repository_id,source_oid)`,
		`CREATE INDEX actions_runs_group ON actions_runs(repository_id,concurrency_group) WHERE concurrency_group != ''`,
		`CREATE TABLE actions_job_plans (
			job_id TEXT PRIMARY KEY,
			plan_json TEXT NOT NULL CHECK (json_valid(plan_json) AND length(CAST(plan_json AS BLOB)) <= 2097152),
			plan_digest TEXT NOT NULL CHECK (length(plan_digest) = 64),
			FOREIGN KEY (job_id) REFERENCES check_jobs(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE actions_schedules (
			repository_id TEXT NOT NULL,
			workflow_path TEXT NOT NULL,
			cron TEXT NOT NULL,
			source_oid TEXT NOT NULL,
			next_due_at INTEGER NOT NULL,
			last_slot_at INTEGER,
			last_admitted_at INTEGER,
			last_run_id TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (repository_id,workflow_path,cron),
			FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX actions_schedules_due ON actions_schedules(next_due_at)`,
	}},
	// Schema 18 turns the one backup schedule into uniquely named backup
	// plans. The schedule a released build wrote becomes the plan named
	// "default", and the runs that the single schedule made belong to it.
	// Runs keep their global one-running index across all plans: only one
	// backup, verification or upload runs at a time. Plans and runs stay
	// machine-local, like the singleton schedule they replace.
	{version: 18, statements: []string{
		`CREATE TABLE backup_plans (
			name TEXT PRIMARY KEY CHECK (length(CAST(name AS BLOB)) BETWEEN 1 AND 64 AND name GLOB '[A-Za-z0-9_-]*'),
			enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
			interval_seconds INTEGER NOT NULL CHECK (interval_seconds BETWEEN 3600 AND 2592000),
			destination TEXT NOT NULL CHECK (length(CAST(destination AS BLOB)) BETWEEN 1 AND 4096),
			keep INTEGER NOT NULL CHECK (keep BETWEEN 1 AND 1000),
			verify INTEGER NOT NULL DEFAULT 1 CHECK (verify IN (0,1)),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`INSERT INTO backup_plans(name,enabled,interval_seconds,destination,keep,verify,created_at,updated_at)
			SELECT 'default',enabled,interval_seconds,destination,keep,verify,updated_at,updated_at FROM backup_schedule`,
		`DROP TABLE backup_schedule`,
		`ALTER TABLE backup_runs ADD COLUMN plan TEXT NOT NULL DEFAULT 'default' CHECK (length(CAST(plan AS BLOB)) BETWEEN 1 AND 64)`,
		`CREATE INDEX backup_runs_plan ON backup_runs(plan,started_at,id)`,
	}},
}

// Exec runs one statement against the state database. It is used for targeted
// repair and for tests that need to break one read on purpose.
func (s *Store) Exec(ctx context.Context, statement string, args ...any) error {
	_, err := s.db.ExecContext(ctx, statement, args...)
	return err
}

func (s *Store) Close() error {
	if s.releaseObservation != nil {
		return s.releaseObservation()
	}
	err := s.db.Close()
	if s.releaseWay != nil {
		s.releaseWay()
	}
	return err
}
func (s *Store) Dir() string { return s.dir }

// TableRowCount reports the row count of an existing table. It covers narrow
// checks that have no dedicated accessor, such as confirming an unrelated
// feature left no state behind. A missing table is reported as an error so
// callers can skip tables from later migrations.
func (s *Store) TableRowCount(ctx context.Context, table string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// ErrNotExist means that directory holds no OwnGit state yet, as before the
// first start.
var ErrNotExist = errors.New("OwnGit state does not exist")

// RequireExisting refuses a state directory without an OwnGit state
// database (ErrNotExist) or with one that is not a regular file.
func RequireExisting(directory string) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(absolute, databaseName))
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotExist
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("OwnGit state database must be a regular file")
	}
	return nil
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM metadata`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Settings{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return Settings{}, err
	}
	accessVersion, err := strconv.ParseInt(values["access_session_version"], 10, 64)
	if err != nil {
		return Settings{}, fmt.Errorf("invalid access session version: %w", err)
	}
	adminVersion, err := strconv.ParseInt(values["admin_session_version"], 10, 64)
	if err != nil {
		return Settings{}, fmt.Errorf("invalid admin session version: %w", err)
	}
	updateCheck := true
	switch raw := values[updateCheckKey]; raw {
	case "", "on":
	case "off":
		updateCheck = false
	default:
		return Settings{}, fmt.Errorf("invalid update check setting %q", raw)
	}
	return Settings{
		Initialized:          values["initialized"] == "true",
		RepositoryRoot:       values["repository_root"],
		AccessMode:           values["access_mode"],
		AccessSessionVersion: accessVersion,
		AdminSessionVersion:  adminVersion,
		InsecureHTTPAccepted: values["insecure_http_accepted"] == "true",
		UpdateCheck:          updateCheck,
	}, nil
}

// updateCheckKey is an optional metadata key, so the setting needs no schema
// change and older OwnGit builds ignore it.
const updateCheckKey = "update_check"

// Network settings are optional metadata keys like updateCheckKey, so they
// need no schema change and older OwnGit builds ignore them. They are
// machine-local: backups do not carry them. The allowed Host names are the
// trusted_hosts table.
const (
	networkListenKey  = "network_listen"
	networkBaseURLKey = "network_base_url"
	// The public share address: a second listener that answers share links
	// only, and the URL visitors use to reach it.
	networkPublicShareListenKey = "network_public_share_listen"
	networkPublicShareURLKey    = "network_public_share_url"
	// networkTrustedProxiesKey holds the trusted reverse proxies as a JSON
	// list of canonical addresses and ranges.
	networkTrustedProxiesKey = "network_trusted_proxies"
	// networkRunningKey holds the RunningNetwork record of the server that
	// currently serves this state directory.
	networkRunningKey = "network_running"
)

// NetworkSettings are the saved network settings that serve applies at its
// next start when no flag overrides them. An empty value is not saved, so
// the default applies. The store does not validate them; callers do.
type NetworkSettings struct {
	Listen  string
	BaseURL string
	// PublicShareListen and PublicShareURL are both set or both empty; the
	// public share address is off while they are empty.
	PublicShareListen string
	PublicShareURL    string
}

// NetworkSettings returns the saved network settings.
func (s *Store) NetworkSettings(ctx context.Context) (NetworkSettings, error) {
	values, err := s.metadataValues(ctx, networkListenKey, networkBaseURLKey, networkPublicShareListenKey, networkPublicShareURLKey)
	if err != nil {
		return NetworkSettings{}, err
	}
	return NetworkSettings{
		Listen: values[networkListenKey], BaseURL: values[networkBaseURLKey],
		PublicShareListen: values[networkPublicShareListenKey], PublicShareURL: values[networkPublicShareURLKey],
	}, nil
}

// TrustedProxies returns the saved trusted reverse proxies, sorted.
func (s *Store) TrustedProxies(ctx context.Context) ([]string, error) {
	return trustedProxies(ctx, s.db)
}

func trustedProxies(ctx context.Context, db queryRower) ([]string, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, networkTrustedProxiesKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	proxies := []string{}
	if err := json.Unmarshal([]byte(raw), &proxies); err != nil {
		return nil, fmt.Errorf("invalid saved trusted proxies: %w", err)
	}
	return proxies, nil
}

// NetworkUpdate replaces the saved network settings and changes the allowed
// Host names and trusted proxies in one transaction. Host names and proxies
// are stored as given; callers pass normalized names and canonical proxies.
type NetworkUpdate struct {
	Settings      NetworkSettings
	AddHosts      []string
	RemoveHosts   []string
	AddProxies    []string
	RemoveProxies []string
	// Tailscale, when set, replaces the Tailscale sharing record in the same
	// transaction; ClearTailscale removes it.
	Tailscale      *TailscaleServe
	ClearTailscale bool
	// AcknowledgeInsecureHTTP records the plain HTTP acknowledgement in the
	// same transaction, so a change that is not saved leaves it unrecorded.
	AcknowledgeInsecureHTTP bool
}

// tailscaleServeKey holds the TailscaleServe record. Like the network
// settings it is optional metadata and machine-local.
const tailscaleServeKey = "tailscale_serve"

// TailscaleServe records what OwnGit changed to share this computer's OwnGit
// on the tailnet: the Tailscale Serve endpoint it wrote and the OwnGit
// settings it changed. Turning sharing off removes the endpoint only when
// Tailscale still has exactly this endpoint, and takes back only these
// settings.
type TailscaleServe struct {
	// Name is the computer's MagicDNS name, such as box.tail1234.ts.net.
	Name string `json:"name"`
	// HTTPSPort is the port Tailscale answers HTTPS on, and Target the local
	// address it proxies to, such as http://127.0.0.1:7654.
	HTTPSPort int    `json:"https_port"`
	Target    string `json:"target"`
	// Created is false when Tailscale already had exactly this endpoint, so
	// turning sharing off leaves it in place.
	Created bool `json:"created"`
	// Confirmed is true once the endpoint was read back from Tailscale and
	// the settings below were saved. An endpoint OwnGit is about to write is
	// recorded first with Confirmed false.
	Confirmed bool  `json:"confirmed"`
	CreatedAt int64 `json:"created_at"`
	// BaseURL is the base URL OwnGit saved and PreviousBaseURL the one saved
	// before; turning off restores it while BaseURL is still saved. BaseURL
	// is empty until the settings are saved (SavedSettings).
	BaseURL         string `json:"base_url"`
	PreviousBaseURL string `json:"previous_base_url,omitempty"`
	// AddedProxy and AddedHost are the trusted proxy and allowed Host name
	// OwnGit added, or empty when they were already saved.
	AddedProxy string `json:"added_proxy,omitempty"`
	AddedHost  string `json:"added_host,omitempty"`
}

// TailscaleServe returns the Tailscale sharing record.
func (s *Store) TailscaleServe(ctx context.Context) (TailscaleServe, bool, error) {
	values, err := s.metadataValues(ctx, tailscaleServeKey)
	if err != nil {
		return TailscaleServe{}, false, err
	}
	raw, found := values[tailscaleServeKey]
	if !found {
		return TailscaleServe{}, false, nil
	}
	var record TailscaleServe
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return TailscaleServe{}, false, fmt.Errorf("invalid Tailscale sharing record: %w", err)
	}
	return record, true, nil
}

// TailscaleChangeLockFile serializes changes of Tailscale sharing between
// the serve process and "owngit tailscale".
const TailscaleChangeLockFile = statepath.TailscaleLock

// LockTailscaleChange waits until no other process is changing Tailscale
// sharing and holds the lock until the release is called. It gives up when
// ctx ends.
func (s *Store) LockTailscaleChange(ctx context.Context) (func(), error) {
	path := filepath.Join(s.dir, TailscaleChangeLockFile)
	for {
		release, err := AcquireExclusiveFileLock(path)
		if !errors.Is(err, ErrInstanceRunning) {
			return release, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("another change of Tailscale sharing is still running; try again later")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SavedSettings reports whether the settings the record names were saved. A
// confirmed record's were. A record that is not confirmed either never saved
// them, when a first turning on was interrupted, or keeps those of an
// earlier confirmation while a new endpoint is not yet confirmed.
func (record TailscaleServe) SavedSettings() bool {
	return record.BaseURL != ""
}

// SaveTailscaleServe replaces the Tailscale sharing record.
func (s *Store) SaveTailscaleServe(ctx context.Context, record TailscaleServe) error {
	return putTailscaleServe(ctx, s.db, record)
}

// ClearTailscaleServe removes the Tailscale sharing record.
func (s *Store) ClearTailscaleServe(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, tailscaleServeKey)
	return err
}

func putTailscaleServe(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record TailscaleServe) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, tailscaleServeKey, string(encoded))
	return err
}

// UpdateNetwork applies update atomically.
func (s *Store) UpdateNetwork(ctx context.Context, update NetworkUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range map[string]string{
		networkListenKey: update.Settings.Listen, networkBaseURLKey: update.Settings.BaseURL,
		networkPublicShareListenKey: update.Settings.PublicShareListen, networkPublicShareURLKey: update.Settings.PublicShareURL,
	} {
		if value == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, key)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		}
		if err != nil {
			return err
		}
	}
	for _, host := range update.RemoveHosts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM trusted_hosts WHERE host=?`, host); err != nil {
			return err
		}
	}
	for _, host := range update.AddHosts {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trusted_hosts(host,created_at) VALUES(?,?)`, host, time.Now().Unix()); err != nil {
			return err
		}
	}
	if len(update.AddProxies) > 0 || len(update.RemoveProxies) > 0 {
		proxies, err := trustedProxies(ctx, tx)
		if err != nil {
			return err
		}
		proxies = slices.DeleteFunc(proxies, func(proxy string) bool { return slices.Contains(update.RemoveProxies, proxy) })
		for _, proxy := range update.AddProxies {
			if !slices.Contains(proxies, proxy) {
				proxies = append(proxies, proxy)
			}
		}
		slices.Sort(proxies)
		if len(proxies) == 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, networkTrustedProxiesKey)
		} else {
			encoded, _ := json.Marshal(proxies)
			_, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, networkTrustedProxiesKey, string(encoded))
		}
		if err != nil {
			return err
		}
	}
	switch {
	case update.ClearTailscale:
		_, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, tailscaleServeKey)
	case update.Tailscale != nil:
		err = putTailscaleServe(ctx, tx, *update.Tailscale)
	}
	if err != nil {
		return err
	}
	if update.AcknowledgeInsecureHTTP {
		if _, err := tx.ExecContext(ctx, acknowledgeInsecureHTTP); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RunningNetwork is what a serving process actually uses. serve publishes it
// after its listener is bound and clears it when it stops. A process that
// ends without stopping, such as after a crash, leaves the record behind, so
// the record is trustworthy only while the process that published it holds
// the running-record lock in the state directory; read it through
// ObserveRunningNetwork or OwnRunningNetwork, never directly. Holding the
// offline lock alone proves nothing: an older OwnGit or an offline backup
// holds it too. Saved settings (NetworkSettings and the allowed Hosts)
// differ from the running values until the next start.
type RunningNetwork struct {
	PID       int   `json:"pid"`
	StartedAt int64 `json:"started_at"`
	// Listen is the requested listen address and Address the bound one; they
	// differ when the port is 0.
	Listen  string `json:"listen"`
	Address string `json:"address"`
	// BaseURL is the configured base URL, or "" when the server derives the
	// address from each request. Origin is the owner-facing origin used for
	// setup links.
	BaseURL string `json:"base_url"`
	Origin  string `json:"origin"`
	// ListenSource and BaseURLSource name where each value came from:
	// "flag", "saved", or "default".
	ListenSource  string `json:"listen_source"`
	BaseURLSource string `json:"base_url_source"`
	// SavedHosts are the allowed Host names loaded from storage at start;
	// AcceptedHosts are every Host name the running Host check accepts,
	// loopback names included.
	SavedHosts    []string `json:"saved_hosts"`
	AcceptedHosts []string `json:"accepted_hosts"`
	// TrustedProxies are the reverse proxies whose forwarded headers the
	// server believes, and TrustedProxiesSource where the list came from.
	TrustedProxies       []string `json:"trusted_proxies"`
	TrustedProxiesSource string   `json:"trusted_proxies_source"`
	// PublicShareListen and PublicShareURL are the public share address the
	// server started with, or empty when it is off. PublicShareAddress is
	// the bound address, or empty when listening failed; PublicShareError
	// then says why.
	PublicShareListen  string `json:"public_share_listen"`
	PublicShareURL     string `json:"public_share_url"`
	PublicShareAddress string `json:"public_share_address"`
	PublicShareError   string `json:"public_share_error,omitempty"`
}

// PublishRunningNetwork records what this serving process uses.
func (s *Store) PublishRunningNetwork(ctx context.Context, running RunningNetwork) error {
	encoded, err := json.Marshal(running)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, networkRunningKey, string(encoded))
	return err
}

// ClearRunningNetwork removes the running record when a server stops.
func (s *Store) ClearRunningNetwork(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, networkRunningKey)
	return err
}

// RunningNetwork returns the last published running record. It does not
// check whether that server still runs; ObserveRunningNetwork and
// OwnRunningNetwork do.
func (s *Store) RunningNetwork(ctx context.Context) (RunningNetwork, bool, error) {
	values, err := s.metadataValues(ctx, networkRunningKey)
	if err != nil {
		return RunningNetwork{}, false, err
	}
	raw, found := values[networkRunningKey]
	if !found {
		return RunningNetwork{}, false, nil
	}
	var running RunningNetwork
	if err := json.Unmarshal([]byte(raw), &running); err != nil {
		return RunningNetwork{}, false, fmt.Errorf("invalid running network record: %w", err)
	}
	return running, true, nil
}

// metadataValues reads the present values of keys.
func (s *Store) metadataValues(ctx context.Context, keys ...string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		var value string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, nil
}

// CompleteSetup saves the first-run answers. trustedHosts, when given, are
// normalized Host names saved as allowed Hosts in the same transaction.
func (s *Store) CompleteSetup(ctx context.Context, repositoryRoot, accessMode, accessHash, adminHash string, insecureAccepted bool, trustedHosts ...string) error {
	if accessMode != "open" && accessMode != "password" {
		return errors.New("invalid access mode")
	}
	if accessMode == "password" && accessHash == "" {
		return errors.New("access password hash is required")
	}
	if adminHash == "" {
		return errors.New("administrator password hash is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='initialized'`).Scan(&initialized); err != nil {
		return err
	}
	if initialized == "true" {
		return ErrSetupComplete
	}
	updates := map[string]string{
		"repository_root":        repositoryRoot,
		"access_mode":            accessMode,
		"initialized":            "true",
		"insecure_http_accepted": strconv.FormatBool(insecureAccepted),
	}
	for key, value := range updates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO passwords(kind,encoded) VALUES('admin',?) ON CONFLICT(kind) DO UPDATE SET encoded=excluded.encoded`, adminHash); err != nil {
		return err
	}
	if accessMode == "password" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO passwords(kind,encoded) VALUES('access',?) ON CONFLICT(kind) DO UPDATE SET encoded=excluded.encoded`, accessHash); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM passwords WHERE kind='access'`); err != nil {
		return err
	}
	for _, host := range trustedHosts {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trusted_hosts(host,created_at) VALUES(?,?)`, host, time.Now().Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bootstrap`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE kind='setup'`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PasswordHash(ctx context.Context, kind string) (string, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT encoded FROM passwords WHERE kind=?`, kind).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return encoded, err
}

// PasswordCredential reads a password and its session version from one state
// snapshot. The version identifies the credential the caller checks.
func (s *Store) PasswordCredential(ctx context.Context, kind string) (string, int64, error) {
	passwordKind, versionKey, err := credentialKeys(kind)
	if err != nil {
		return "", 0, err
	}
	var encoded, mode, rawVersion string
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT encoded FROM passwords WHERE kind=?), ''), mode.value, version.value
		FROM metadata AS mode, metadata AS version
		WHERE mode.key='access_mode' AND version.key=?`, passwordKind, versionKey).Scan(&encoded, &mode, &rawVersion)
	if err != nil {
		return "", 0, err
	}
	if kind == "general" && mode != "password" {
		return "", 0, ErrAccessChanged
	}
	version, err := strconv.ParseInt(rawVersion, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid %s session version: %w", kind, err)
	}
	return encoded, version, nil
}

func credentialKeys(kind string) (string, string, error) {
	switch kind {
	case "general":
		return "access", "access_session_version", nil
	case "admin":
		return "admin", "admin_session_version", nil
	default:
		return "", "", errors.New("invalid credential kind")
	}
}

// CredentialVersionCurrent checks that a password still uses the credential
// version the caller verified. General access must also remain in password mode.
func (s *Store) CredentialVersionCurrent(ctx context.Context, kind string, version int64) (bool, error) {
	return credentialVersionCurrent(ctx, s.db, kind, version)
}

func credentialVersionCurrent(ctx context.Context, db queryRower, kind string, version int64) (bool, error) {
	_, key, err := credentialKeys(kind)
	if err != nil {
		return false, err
	}
	var mode, rawVersion string
	err = db.QueryRowContext(ctx, `SELECT mode.value, version.value FROM metadata AS mode, metadata AS version
		WHERE mode.key='access_mode' AND version.key=?`, key).Scan(&mode, &rawVersion)
	if err != nil {
		return false, err
	}
	current, err := strconv.ParseInt(rawVersion, 10, 64)
	if err != nil {
		return false, fmt.Errorf("invalid %s session version: %w", kind, err)
	}
	return current == version && (kind != "general" || mode == "password"), nil
}

func (s *Store) SetAccessPassword(ctx context.Context, encoded string) error {
	if encoded == "" {
		return errors.New("password hash is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO passwords(kind,encoded) VALUES('access',?) ON CONFLICT(kind) DO UPDATE SET encoded=excluded.encoded`, encoded); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value='password' WHERE key='access_mode'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT) WHERE key='access_session_version'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE kind='general'`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DisableAccessPassword(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM passwords WHERE kind='access'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value='open' WHERE key='access_mode'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT) WHERE key='access_session_version'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE kind='general'`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AcknowledgeInsecureHTTP(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, acknowledgeInsecureHTTP)
	return err
}

// acknowledgeInsecureHTTP records that plain HTTP was acknowledged.
const acknowledgeInsecureHTTP = `UPDATE metadata SET value='true' WHERE key='insecure_http_accepted'`

// SetAdminPassword replaces the administrator password whatever it was. It
// is local recovery for an owner who no longer knows it.
func (s *Store) SetAdminPassword(ctx context.Context, encoded string) error {
	return s.replaceAdminPassword(ctx, encoded, nil)
}

// ChangeAdminPassword replaces the administrator password only while the
// administrator version the caller verified is current, and returns
// ErrAccessChanged, changing nothing, once another change replaced it.
func (s *Store) ChangeAdminPassword(ctx context.Context, encoded string, verified int64) error {
	return s.replaceAdminPassword(ctx, encoded, &verified)
}

// replaceAdminPassword stores encoded, ends every administrator session
// and moves to the next version, after comparing verified when given.
func (s *Store) replaceAdminPassword(ctx context.Context, encoded string, verified *int64) error {
	if encoded == "" {
		return errors.New("password hash is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if verified != nil {
		current, err := credentialVersionCurrent(ctx, tx, "admin", *verified)
		if err != nil {
			return err
		}
		if !current {
			return ErrAccessChanged
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO passwords(kind,encoded) VALUES('admin',?) ON CONFLICT(kind) DO UPDATE SET encoded=excluded.encoded`, encoded); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT) WHERE key='admin_session_version'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE kind='admin'`); err != nil {
		return err
	}
	// A new password also ends every administrator pause, so the owner who
	// runs reset-admin on this computer can sign in at once.
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_attempts WHERE kind='admin'`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutBootstrap(ctx context.Context, token string, expires time.Time) error {
	hash := sha256.Sum256([]byte(token))
	return s.putBootstrapHash(ctx, hash[:], expires.Unix())
}

func (s *Store) putBootstrapHash(ctx context.Context, hash []byte, expiresAt int64) error {
	var singleton int
	err := s.db.QueryRowContext(ctx, `INSERT INTO bootstrap(singleton,token_hash,expires_at)
		SELECT 1,?,? WHERE (SELECT value FROM metadata WHERE key='initialized')='false'
		ON CONFLICT(singleton) DO UPDATE SET token_hash=excluded.token_hash,expires_at=excluded.expires_at
		WHERE (SELECT value FROM metadata WHERE key='initialized')='false'
		RETURNING singleton`, hash, expiresAt).Scan(&singleton)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSetupComplete
	}
	return err
}

func (s *Store) BootstrapSnapshot(ctx context.Context) (BootstrapSnapshot, error) {
	var snapshot BootstrapSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT token_hash,expires_at FROM bootstrap WHERE singleton=1`).Scan(&snapshot.TokenHash, &snapshot.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return BootstrapSnapshot{}, err
	}
	snapshot.Present = true
	return snapshot, nil
}

func (s *Store) RestoreBootstrap(ctx context.Context, snapshot BootstrapSnapshot) error {
	if !snapshot.Present {
		_, err := s.db.ExecContext(ctx, `DELETE FROM bootstrap WHERE singleton=1`)
		return err
	}
	if len(snapshot.TokenHash) != sha256.Size {
		return errors.New("invalid bootstrap snapshot")
	}
	return s.putBootstrapHash(ctx, snapshot.TokenHash, snapshot.ExpiresAt)
}

func BootstrapMatches(snapshot BootstrapSnapshot, token string) bool {
	if !snapshot.Present || len(snapshot.TokenHash) != sha256.Size {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	return equalHash(snapshot.TokenHash, hash[:])
}

// RedeemBootstrap consumes a valid token and creates the only setup session atomically.
func (s *Store) RedeemBootstrap(ctx context.Context, token, sessionToken, csrf string, now, expires time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var expected []byte
	var expiresAt int64
	if err := tx.QueryRowContext(ctx, `SELECT token_hash,expires_at FROM bootstrap WHERE singleton=1`).Scan(&expected, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	actual := sha256.Sum256([]byte(token))
	if expiresAt < now.Unix() || !equalHash(expected, actual[:]) {
		return false, nil
	}
	if err := replaceSetupSession(ctx, tx, sessionToken, csrf, expires); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// StartApprovedSetupSession creates the only setup session for a browser the
// owner approved from the terminal running OwnGit. It grants exactly what
// redeeming the setup capability grants: any unredeemed capability and any
// other setup session end. It fails with ErrSetupComplete after setup.
func (s *Store) StartApprovedSetupSession(ctx context.Context, sessionToken, csrf string, expires time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='initialized'`).Scan(&initialized); err != nil {
		return err
	}
	if initialized == "true" {
		return ErrSetupComplete
	}
	if err := replaceSetupSession(ctx, tx, sessionToken, csrf, expires); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceSetupSession consumes the setup capability and makes sessionToken
// the only setup session.
func replaceSetupSession(ctx context.Context, tx *sql.Tx, sessionToken, csrf string, expires time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM bootstrap WHERE singleton=1`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE kind='setup'`); err != nil {
		return err
	}
	sessionHash := sha256.Sum256([]byte(sessionToken))
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,kind,csrf,version,expires_at) VALUES(?,'setup',?,1,?)`, sessionHash[:], csrf, expires.Unix())
	return err
}

// EndSetupSessions ends every browser setup session, for example when the
// owner switches to setup in the terminal.
func (s *Store) EndSetupSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE kind='setup'`)
	return err
}

func (s *Store) CreateSession(ctx context.Context, token, kind, csrf string, version int64, expires time.Time) error {
	if kind != "general" && kind != "admin" {
		return errors.New("invalid session kind")
	}
	hash := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,kind,csrf,version,expires_at) VALUES(?,?,?,?,?)`, hash[:], kind, csrf, version, expires.Unix())
	return err
}

// ErrEndedSessionKept reports that a session found no longer valid could not
// be removed. The session is not valid either way; only its removal failed.
var ErrEndedSessionKept = errors.New("the ended session could not be removed")

// Session returns the session of kind with token. An expired session is not
// valid and is removed; ErrEndedSessionKept, with ok false, reports that the
// removal failed.
func (s *Store) Session(ctx context.Context, token, kind string, now time.Time) (Session, bool, error) {
	hash := sha256.Sum256([]byte(token))
	var session Session
	var expiresAt int64
	err := s.db.QueryRowContext(ctx, `SELECT kind,csrf,version,expires_at FROM sessions WHERE token_hash=? AND kind=?`, hash[:], kind).Scan(&session.Kind, &session.CSRF, &session.Version, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	session.Expires = time.Unix(expiresAt, 0)
	if !session.Expires.After(now) {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, hash[:]); err != nil {
			return Session{}, false, fmt.Errorf("%w: %w", ErrEndedSessionKept, err)
		}
		return Session{}, false, nil
	}
	return session, true, nil
}

func (s *Store) DeleteSession(ctx context.Context, token, kind string) error {
	hash := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=? AND kind=?`, hash[:], kind)
	return err
}

// AttemptBlocked returns how much longer authentication of kind from
// address is refused at now because of earlier failures, or zero. A pause
// keeps the end recorded when it started, whatever the limits are now.
func (s *Store) AttemptBlocked(ctx context.Context, kind, address string, now time.Time) (time.Duration, error) {
	var blocked int64
	err := s.db.QueryRowContext(ctx, `SELECT blocked_until FROM login_attempts WHERE kind=? AND address=?`, kind, address).Scan(&blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return max(time.Unix(blocked, 0).Sub(now), 0), nil
}

// ServerWideAddress is the address under which the server-wide count of
// administrator failures is kept. No client address can equal it.
const ServerWideAddress = "*"

// ServerWideFailureFactor times the per-address attempts is the number of
// wrong administrator passwords from any addresses, within the login window,
// that pauses every administrator password check for the login pause. It
// keeps guessing bounded when a client can choose its address.
const ServerWideFailureFactor = 5

// AdminServerBlocked returns how much longer every administrator password
// check is refused at now because of the server-wide cap, or zero.
func (s *Store) AdminServerBlocked(ctx context.Context, now time.Time) (time.Duration, error) {
	return s.AttemptBlocked(ctx, "admin", ServerWideAddress, now)
}

// RecordFailedAttempt counts one wrong password from address under the
// login limits saved now (LoginLimits), read in the same transaction: the
// failure that reaches their attempts within their window pauses the
// address for their pause. A wrong administrator password also counts
// toward the server-wide cap (ServerWideFailureFactor). Limits that cannot
// be read fail with their PolicyError, and nothing is counted.
func (s *Store) RecordFailedAttempt(ctx context.Context, kind, address string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	limits, err := loginLimits(ctx, tx)
	if err != nil {
		return err
	}
	if err := countFailure(ctx, tx, kind, address, now, limits, limits.Attempts); err != nil {
		return err
	}
	if kind == "admin" {
		if err := countFailure(ctx, tx, kind, ServerWideAddress, now, limits, limits.Attempts*ServerWideFailureFactor); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// countFailure adds one failure to the row of kind and address and starts
// the pause when the row reaches attempts failures within the window.
func countFailure(ctx context.Context, tx *sql.Tx, kind, address string, now time.Time, limits LoginLimits, attempts int) error {
	var started, blocked int64
	var failures int
	err := tx.QueryRowContext(ctx, `SELECT window_started_at,attempts,blocked_until FROM login_attempts WHERE kind=? AND address=?`, kind, address).Scan(&started, &failures, &blocked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) || now.Sub(time.Unix(started, 0)) >= limits.Window {
		started, failures, blocked = now.Unix(), 0, 0
	}
	failures++
	if failures >= attempts {
		blocked = now.Add(limits.Pause).Unix()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO login_attempts(kind,address,window_started_at,attempts,blocked_until) VALUES(?,?,?,?,?)
		ON CONFLICT(kind,address) DO UPDATE SET window_started_at=excluded.window_started_at,attempts=excluded.attempts,blocked_until=excluded.blocked_until`, kind, address, started, failures, blocked)
	return err
}

// ClearAttempts forgets the failures of kind from address after a right
// password. Administrator failures are cleared with ClearAdminAttempts.
func (s *Store) ClearAttempts(ctx context.Context, kind, address string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE kind=? AND address=?`, kind, address)
	return err
}

// ClearAdminAttempts forgets the administrator failures from address and
// the server-wide count after a right administrator password, only while
// version is still the current administrator password version. A password
// replaced meanwhile clears nothing. Only the owner can send a right
// password, and it never follows a pause.
func (s *Store) ClearAdminAttempts(ctx context.Context, address string, version int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := credentialVersionCurrent(ctx, tx, "admin", version)
	if err != nil || !current {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_attempts WHERE kind='admin' AND address IN (?,?)`, address, ServerWideAddress); err != nil {
		return err
	}
	return tx.Commit()
}

// AddRepository records a repository. It refuses an ID whose earlier deletion
// is unfinished, so no path can claim a name before that deletion completes,
// and an ID that is another repository's current name or unexpired alias.
// An expired alias left for the ID is removed in the same transaction.
func (s *Store) AddRepository(ctx context.Context, repository Repository) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata WHERE key=?`, repositoryDeletionKey(repository.ID)).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return ErrRepositoryDeletionPending
	}
	if err := takeRepositoryName(ctx, tx, "", repository.ID, time.Now()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repositories(id,name,description,created_at) VALUES(?,?,?,?)`,
		repository.ID, repository.Name, repository.Description, repository.CreatedAt.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// repositoryColumns reads a repository with its address.
const repositoryColumns = `SELECT r.id,r.name,COALESCE(c.name,r.id),r.description,r.created_at FROM repositories r
	LEFT JOIN repository_names c ON c.repository_id=r.id AND c.kind='current'`

func scanRepository(row interface{ Scan(...any) error }) (Repository, error) {
	var repository Repository
	var created int64
	err := row.Scan(&repository.ID, &repository.Name, &repository.Address, &repository.Description, &created)
	repository.CreatedAt = time.Unix(created, 0)
	return repository, err
}

// Repository reads the repository with ID id.
func (s *Store) Repository(ctx context.Context, id string) (Repository, bool, error) {
	return readRepository(ctx, s.db, id)
}

func readRepository(ctx context.Context, queryer queryRower, id string) (Repository, bool, error) {
	repository, err := scanRepository(queryer.QueryRowContext(ctx, repositoryColumns+` WHERE r.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, false, nil
	}
	if err != nil {
		return Repository{}, false, err
	}
	return repository, true, nil
}

func (s *Store) Repositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.db.QueryContext(ctx, repositoryColumns+` ORDER BY lower(r.name),r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repositories []Repository
	for rows.Next() {
		repository, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

func (s *Store) RepositoryIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM repositories`)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, closeRows(rows))
		}
		present[id] = true
	}
	return present, closeRows(rows)
}

func (s *Store) AddTrustedHost(ctx context.Context, host string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO trusted_hosts(host,created_at) VALUES(?,?)`, host, time.Now().Unix())
	return err
}

func (s *Store) TrustedHosts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host FROM trusted_hosts ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

func equalHash(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}

// StoppedByContext reports whether err is how the database answers work its
// context stopped when the answer does not carry the context's error: SQLite
// reports a statement interrupted for the context as SQLITE_INTERRUPT, and
// database/sql reports a transaction it rolled back for the context as
// sql.ErrTxDone. Only a caller whose context has ended may take either for
// that end.
func StoppedByContext(err error) bool {
	const sqliteInterrupt = 9
	var sqliteErr sqliteCodeError
	return errors.Is(err, sql.ErrTxDone) || (errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteInterrupt)
}
