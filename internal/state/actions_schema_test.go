package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestActionsSchemaUpgrade(t *testing.T) {
	for _, test := range []struct {
		name string
		fail bool
	}{{name: "populated released state"}, {name: "failure rolls back schema 16", fail: true}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			directory := filepath.Join(t.TempDir(), "state")
			loadReleasedDump(t, directory, "schema16-1.1.6-populated.sql")
			db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
			columns, before := schemaTableRecords(t, db, nil)
			fingerprint, _, err := schemaFingerprint(ctx, db)
			noErr(t, err)
			noErr(t, db.Close())
			if test.fail {
				original := schemaSteps
				schemaSteps = slices.Clone(original)
				last := &schemaSteps[len(schemaSteps)-1]
				last.statements = append(slices.Clone(last.statements), `CREATE TABLE failed_migration(value TEXT CHECK(value IS NULL))`, `INSERT INTO failed_migration(value) SELECT id FROM check_jobs`)
				t.Cleanup(func() { schemaSteps = original })
			}
			store, err := Open(ctx, directory)
			if test.fail {
				if store != nil {
					store.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "apply state schema migration 18") {
				t.Fatalf("failed migration=%v", err)
			}
			db = openSchemaDatabase(t, filepath.Join(directory, databaseName))
				defer db.Close()
				afterFingerprint, _, err := schemaFingerprint(ctx, db)
				noErr(t, err)
				version, _, err := readSchemaVersion(ctx, db)
				_, after := schemaTableRecords(t, db, columns)
				if err != nil || version != 16 || afterFingerprint != fingerprint || !reflect.DeepEqual(before, after) {
					t.Fatalf("rollback version=%d catalog=%s err=%v", version, afterFingerprint, err)
				}
				return
			}
			noErr(t, err)
			defer store.Close()
			_, after := schemaTableRecords(t, store.db, columns)
			if !reflect.DeepEqual(before, after) {
				for table, rows := range before {
					if !reflect.DeepEqual(rows, after[table]) {
						t.Errorf("migration changed rows in %s", table)
					}
				}
			}
			if got, objects, err := schemaFingerprint(ctx, store.db); err != nil || got != currentSchemaFingerprint || objects != currentSchemaObjects {
				t.Fatalf("catalog=%s objects=%d err=%v", got, objects, err)
			}
			snapshot, err := store.RecoverySnapshot(ctx)
			noErr(t, err)
			legacy, current := 0, 0
			for _, job := range snapshot.CheckJobs {
				if job.Execution.Legacy {
					legacy++
				} else {
					current++
				}
				if job.RunID != "" || job.JobKey != "" || job.PlanDigest != "" || job.Tolerated {
					t.Fatalf("migration invented workflow facts: %+v", job)
				}
			}
			if legacy == 0 || current == 0 || len(snapshot.CheckAttempts) == 0 || len(snapshot.CheckResults) == 0 {
				t.Fatalf("populated evidence missing: legacy=%d current=%d attempts=%d results=%d", legacy, current, len(snapshot.CheckAttempts), len(snapshot.CheckResults))
			}
			for _, policy := range snapshot.CheckPolicies {
				if policy.RunWorkflows || !policy.ConsentActive {
					t.Fatalf("migration changed consent or workflow opt-in: %+v", policy)
				}
			}
			rows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
			noErr(t, err)
			if rows.Next() {
				rows.Close()
				t.Fatal("migration left foreign key violations")
			}
			noErr(t, closeRows(rows))
		})
	}
}

// renamedByMigration tables are machine-local backup tables that schema 18
// renames; the fixture holds none of their rows, and the conversion is
// covered by the backup plan migration tests.
var renamedByMigration = map[string]bool{"backup_schedule": true, "backup_plans": true}

func schemaTableRecords(t *testing.T, db queryRower, columns map[string][]string) (map[string][]string, map[string][]string) {
	t.Helper()
	ctx := context.Background()
	if columns == nil {
		columns = make(map[string][]string)
		for table := range tableRowCounts(t, db) {
			if renamedByMigration[table] {
				continue
			}
			rows, err := db.QueryContext(ctx, `SELECT * FROM "`+table+`" LIMIT 0`)
			noErr(t, err)
			columns[table], err = rows.Columns()
			noErr(t, err)
			noErr(t, closeRows(rows))
		}
	}
	records := make(map[string][]string, len(columns))
	for table, names := range columns {
		if renamedByMigration[table] {
			continue
		}
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = `"` + name + `"`
		}
		query := `SELECT ` + strings.Join(quoted, ",") + ` FROM "` + table + `"`
		if table == "metadata" {
			query += ` WHERE key!='schema_version'`
		}
		rows, err := db.QueryContext(ctx, query)
		noErr(t, err)
		for rows.Next() {
			values := make([]any, len(names))
			destinations := make([]any, len(names))
			for i := range values {
				destinations[i] = &values[i]
			}
			noErr(t, rows.Scan(destinations...))
			encoded, err := json.Marshal(values)
			noErr(t, err)
			records[table] = append(records[table], string(encoded))
		}
		noErr(t, closeRows(rows))
		sort.Strings(records[table])
	}
	return columns, records
}

func TestStateQueryIndexes(t *testing.T) {
	for _, origin := range []string{"fresh", "released schema 16"} {
		t.Run(origin, func(t *testing.T) {
			var store *Store
			var before *sql.DB
			var directory string
			if origin == "fresh" {
				store = openTestStore(t)
			} else {
				directory = filepath.Join(t.TempDir(), "state")
				loadReleasedDump(t, directory, "schema16-1.1.6-populated.sql")
				before = openSchemaDatabase(t, filepath.Join(directory, databaseName))
			}
			queries := []struct {
				name, query, index, baseline string
				args                         []any
				noSort                       bool
			}{
				{"live leases", `UPDATE check_jobs SET status='ambiguous',lease_lost_at=lease_expires_at WHERE status IN ('claimed','started') AND lease_expires_at IS NOT NULL AND lease_expires_at<=?`, "check_jobs_live_leases", "SCAN check_jobs", []any{int64(1800000000000000000)}, false},
				{"pending repositories", `SELECT repository_id FROM check_jobs WHERE status='pending' GROUP BY repository_id ORDER BY MIN(admitted_at),repository_id`, "check_jobs_pending", "SCAN check_jobs", nil, false},
				{"import failures", feedQueries[NotifyImportFailed], "import_runs_finished", "SCAN i", []any{int64(1800000000), int64(1800000060)}, true},
				{"recent jobs", `SELECT * FROM check_jobs WHERE repository_id=? ORDER BY admitted_at DESC,id DESC LIMIT ?`, "check_jobs_recent", "USE TEMP B-TREE FOR ORDER BY", []any{"alpha", 20}, true},
			}
			if before != nil {
				for _, query := range queries {
					plan := stateQueryPlan(t, before, query.query, query.args)
					t.Logf("Before migration %s: %s", query.name, plan)
					if !strings.Contains(plan, query.baseline) || strings.Contains(plan, query.index) {
						t.Fatalf("baseline plan=%s", plan)
					}
				}
				noErr(t, before.Close())
				var err error
				store, err = Open(context.Background(), directory)
				noErr(t, err)
				t.Cleanup(func() { store.Close() })
			}
			for _, query := range queries {
				t.Run(query.name, func(t *testing.T) {
					plan := stateQueryPlan(t, store.db, query.query, query.args)
					t.Logf("Current plan: %s", plan)
					if !strings.Contains(plan, query.index) || query.noSort && strings.Contains(plan, "TEMP B-TREE") {
						t.Fatalf("query plan=%s, want %s", plan, query.index)
					}
				})
			}
		})
	}
}

func stateQueryPlan(t *testing.T, db queryRower, query string, args []any) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	noErr(t, err)
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		noErr(t, rows.Scan(&id, &parent, &unused, &detail))
		plans = append(plans, detail)
	}
	noErr(t, closeRows(rows))
	return strings.Join(plans, "\n")
}

func TestReleasedReaderRefusesActionsSchemaUnchanged(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, directory)
	noErr(t, err)
	noErr(t, store.Close())
	before := captureSchemaDirectory(t, directory)
	original := schemaSteps
	schemaSteps = slices.Clone(original[:len(original)-1])
	t.Cleanup(func() { schemaSteps = original })
	store, err = Open(ctx, directory)
	if store != nil {
		store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "schema version 18 is newer") {
		t.Fatalf("schema-17 reader=%v", err)
	}
	assertSchemaDirectoryUnchanged(t, directory, before)
}
