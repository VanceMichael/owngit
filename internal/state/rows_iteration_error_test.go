package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var errInjectedStep = errors.New("injected sqlite step error")

// rowFault makes the next matching query fail after at most one row, the way
// a SQLite step error (I/O error, corruption, interrupt) ends a scan.
// database/sql then closes the rows itself and keeps the error only in Err.
type rowFault struct {
	mu    sync.Mutex
	match func(query string) bool
}

func (f *rowFault) set(match func(string) bool) {
	f.mu.Lock()
	f.match = match
	f.mu.Unlock()
}

func (f *rowFault) pending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.match != nil
}

func (f *rowFault) take(query string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.match == nil || !f.match(query) {
		return false
	}
	f.match = nil
	return true
}

type faultConnector struct {
	dsn    string
	driver driver.Driver
	fault  *rowFault
}

func (c faultConnector) Connect(context.Context) (driver.Conn, error) {
	return c.Driver().Open(c.dsn)
}

// Driver injects the fault into every connection it opens, including the
// portable snapshot's own read-only connection, which uses the store's driver.
func (c faultConnector) Driver() driver.Driver { return faultDriver{driver: c.driver, fault: c.fault} }

type faultDriver struct {
	driver driver.Driver
	fault  *rowFault
}

func (d faultDriver) Open(dsn string) (driver.Conn, error) {
	conn, err := d.driver.Open(dsn)
	if err != nil {
		return nil, err
	}
	return faultConn{Conn: conn, fault: d.fault}, nil
}

type faultConn struct {
	driver.Conn
	fault *rowFault
}

func (c faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil || !c.fault.take(query) {
		return rows, err
	}
	return &faultRows{Rows: rows}, nil
}

func (c faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c faultConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}

func (c faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c faultConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type faultRows struct {
	driver.Rows
	delivered int
}

func (r *faultRows) Next(dest []driver.Value) error {
	if r.delivered >= 1 {
		return errInjectedStep
	}
	if err := r.Rows.Next(dest); err != nil {
		// An empty table fails on its first step instead of ending.
		return errInjectedStep
	}
	r.delivered++
	return nil
}

// openFaultStore opens a store whose single connection can inject a
// mid-iteration step error into one chosen query.
func openFaultStore(t *testing.T) (*Store, *rowFault) {
	t.Helper()
	store := openTestStore(t)
	completeTestSetup(t, store)
	ctx := context.Background()
	fault := &rowFault{}
	db := sql.OpenDB(faultConnector{dsn: sqliteFileURI(filepath.Join(store.dir, databaseName)), driver: store.db.Driver(), fault: fault})
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{`PRAGMA foreign_keys=ON`, `PRAGMA busy_timeout=5000`} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			t.Fatal(err)
		}
	}
	noErr(t, store.db.Close())
	store.db = db
	return store, fault
}

// fullTableRead matches the unfiltered snapshot read of one table.
func fullTableRead(table string) func(string) bool {
	pattern := regexp.MustCompile(`(?s)\bFROM ` + table + `\b`)
	return func(query string) bool {
		return pattern.MatchString(query) && !strings.Contains(query, "WHERE")
	}
}

func TestRowReadsFailOnIterationError(t *testing.T) {
	store, fault := openFaultStore(t)
	ctx := context.Background()
	// Two repositories make the repositories case fail after one delivered row.
	for _, id := range []string{"alpha", "beta"} {
		noErr(t, store.AddRepository(ctx, Repository{ID: id, Name: id, CreatedAt: time.Unix(1_800_000_000, 0)}))
	}
	if snapshot, err := store.RecoverySnapshot(ctx); err != nil || len(snapshot.Repositories) != 2 {
		t.Fatalf("baseline snapshot repositories=%d err=%v", len(snapshot.Repositories), err)
	}
	type rowRead struct {
		match func(string) bool
		read  func() error
	}
	cases := map[string]rowRead{
		"metadata": {match: func(query string) bool { return strings.Contains(query, "FROM metadata WHERE key IN") }},
		"page": {match: fullTableRead("repositories"), read: func() error {
			rows, err := store.db.QueryContext(ctx, `SELECT id FROM repositories ORDER BY id`)
			if err != nil {
				return err
			}
			_, err = collectRows(rows, func(scanner rowScanner) (string, error) {
				var id string
				err := scanner.Scan(&id)
				return id, err
			})
			return err
		}},
	}
	for _, table := range []string{
		"passwords", "repositories",
		"pull_requests", "pull_request_revisions", "pull_request_reviews", "pull_request_merge_intents", "pull_request_auto_merges",
		"tasks", "check_configurations", "check_cycles", "check_attempts", "check_results",
		"check_policies", "check_jobs",
		"import_sources", "import_runs", "import_ref_observations", "import_publication_intents",
	} {
		cases[table] = rowRead{match: fullTableRead(table)}
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fault.set(test.match)
			var err error
			if test.read != nil {
				err = test.read()
			} else {
				_, err = store.RecoverySnapshot(ctx)
			}
			if fault.pending() {
				fault.set(nil)
				t.Fatal("the fault matched no query; the test no longer reaches this read")
			}
			if !errors.Is(err, errInjectedStep) {
				t.Fatalf("failing %s read returned err=%v", name, err)
			}
		})
	}
	if _, err := store.RecoverySnapshot(ctx); err != nil {
		t.Fatalf("snapshot after the faults: %v", err)
	}
}

func TestAttemptReadersUseTheirOwnQuerySurface(t *testing.T) {
	for _, transactional := range []bool{false, true} {
		t.Run(map[bool]string{false: "pool", true: "transaction"}[transactional], func(t *testing.T) {
			store, fault := openFaultStore(t)
			ctx := t.Context()
			now := time.Unix(1_800_000_000, 0)
			noErr(t, store.AddRepository(ctx, Repository{ID: "project", Name: "project", CreatedAt: now}))
			task, err := store.CreateTask(ctx, "project", "Attempt reader", now)
			noErr(t, err)
			_, attempt := recordAttempt(t, store, attemptFor(task, strings.Repeat("a", 40), now, AttemptPassed))
			// Bound the reader checks, not fixture creation on a slow disk.
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var queryer querier = store.db
			read := func(id string) (CheckAttempt, bool, error) { return store.CheckAttemptByID(ctx, "project", id) }
			if transactional {
				tx, err := store.db.BeginTx(ctx, nil)
				noErr(t, err)
				defer tx.Rollback()
				queryer = tx
				read = func(id string) (CheckAttempt, bool, error) { return readAttemptTx(ctx, tx, id) }
			}
			_, err = queryer.ExecContext(ctx, `UPDATE check_attempts SET summary='private' WHERE id=?`, attempt.ID)
			noErr(t, err)
			_, err = queryer.ExecContext(ctx, `INSERT INTO check_results(attempt_id,position,name,command,status,duration_ms,output_excerpt) VALUES(?,1,'second','true','passed',0,'private')`, attempt.ID)
			noErr(t, err)
			got, found, err := read(attempt.ID)
			if err != nil || !found || got.Summary != "private" || len(got.Results) != 2 || got.Results[0].Position != 0 || got.Results[1].OutputExcerpt != "private" {
				t.Fatalf("row/result visibility or order: found=%v err=%v", found, err)
			}
			if _, found, err := read("missing"); err != nil || found {
				t.Fatalf("missing found=%v err=%v", found, err)
			}
			for _, table := range []string{"check_attempts", "check_results"} {
				fault.set(func(query string) bool { return strings.Contains(query, "FROM "+table) })
				id := attempt.ID
				if table == "check_attempts" {
					id = "missing"
				}
				if _, found, err := read(id); found || !errors.Is(err, errInjectedStep) {
					t.Fatalf("%s failure became success/missing: found=%v err=%v", table, found, err)
				}
			}
			for _, query := range []string{"SELECT 1", "SELECT * FROM absent_attempt_table"} {
				if _, found, err := readCheckAttempt(ctx, queryer, query); found || err == nil {
					t.Fatalf("scan/query failure became missing: found=%v err=%v", found, err)
				}
			}
			if !transactional {
				if _, found, err := store.CheckAttemptByID(ctx, "other", attempt.ID); found || err != nil {
					t.Fatalf("repository scope: found=%v err=%v", found, err)
				}
				fault.set(func(query string) bool { return strings.Contains(query, "FROM check_results") })
				if _, found, err := store.CheckAttemptByID(ctx, "other", attempt.ID); found || !errors.Is(err, errInjectedStep) {
					t.Fatalf("repository scope hid result failure: found=%v err=%v", found, err)
				}
			}
			_, err = queryer.ExecContext(ctx, `UPDATE check_results SET duration_ms='invalid' WHERE attempt_id=?`, attempt.ID)
			noErr(t, err)
			if _, found, err := read(attempt.ID); found || err == nil {
				t.Fatalf("result scan failure became missing: found=%v err=%v", found, err)
			}
			cancelled, stop := context.WithCancel(ctx)
			stop()
			if _, found, err := readCheckAttempt(cancelled, queryer, attemptSelect); found || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation became missing: found=%v err=%v", found, err)
			}
		})
	}
}

// The stale-job sweep must not treat a failed read as "no stale jobs".
func TestInterruptStalePendingCheckJobsFailsOnIterationError(t *testing.T) {
	store, fault := openFaultStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	noErr(t, err)
	defer tx.Rollback()
	fault.set(func(query string) bool {
		return strings.Contains(query, "FROM check_jobs WHERE repository_id=? AND status IN")
	})
	if err := interruptStalePendingCheckJobsTx(ctx, tx, CheckPolicy{RepositoryID: "project"}, time.Now()); !errors.Is(err, errInjectedStep) {
		t.Fatalf("stale job sweep err=%v", err)
	}
}
