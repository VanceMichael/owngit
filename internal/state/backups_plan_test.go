package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A released build's one backup schedule becomes the default plan, and
// the runs it made belong to it; a build that never configured a schedule
// starts with no plans.
func TestBackupPlanMigrationFromSchema16(t *testing.T) {
	ctx := context.Background()
	for _, withSchedule := range []bool{true, false} {
		t.Run(fmt.Sprintf("schedule=%v", withSchedule), func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "state")
			loadReleasedDump(t, directory, "schema16-1.1.6-populated.sql")
			db := openSchemaDatabase(t, filepath.Join(directory, databaseName))
			if withSchedule {
				_, err := db.Exec(`INSERT INTO backup_schedule(singleton,enabled,interval_seconds,destination,keep,verify,updated_at)
					VALUES(1,0,43200,'/home/example/offline',3,0,1800000500)`)
				noErr(t, err)
				_, err = db.Exec(`INSERT INTO backup_runs(id,kind,status,destination,started_at)
					VALUES('00000000000000000000000000000010','scheduled','succeeded','/home/example/offline',1800000400)`)
				noErr(t, err)
			}
			noErr(t, db.Close())

			store := openUpgradedSchema(t, directory, currentSchemaVersion())
			defer store.Close()
			plans, err := store.BackupPlans(ctx)
			noErr(t, err)
			schedule, configured, err := store.BackupSchedule(ctx)
			noErr(t, err)
			if withSchedule {
				if len(plans) != 1 || plans[0].Name != DefaultBackupPlanName {
					t.Fatalf("plans=%+v", plans)
				}
				plan := plans[0]
				if plan.Enabled || plan.Interval != 12*time.Hour || plan.Destination != "/home/example/offline" || plan.Keep != 3 || plan.Verify {
					t.Fatalf("migrated plan=%+v", plan)
				}
				if !plan.CreatedAt.Equal(time.Unix(1800000000+500, 0)) || !plan.UpdatedAt.Equal(plan.CreatedAt) {
					t.Fatalf("migrated plan times=%+v", plan)
				}
				if !configured || schedule.Destination != "/home/example/offline" || schedule.Keep != 3 || schedule.Enabled || schedule.Verify {
					t.Fatalf("default schedule=%+v configured=%v", schedule, configured)
				}
				runs, err := store.BackupRuns(ctx)
				noErr(t, err)
				if len(runs) != 1 || runs[0].Plan != DefaultBackupPlanName {
					t.Fatalf("migrated runs=%+v", runs)
				}
			} else {
				if len(plans) != 0 || configured {
					t.Fatalf("plans=%+v configured=%v, want none", plans, configured)
				}
			}
			if exists := tableExists(t, store.db, "backup_schedule"); exists {
				t.Fatal("the old backup_schedule table survived the migration")
			}
		})
	}
}

func tableExists(t *testing.T, db queryRower, name string) bool {
	t.Helper()
	var count int
	noErr(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count))
	return count == 1
}

// Plan names follow the bounds the schema and ValidateBackupPlanName keep.
func TestBackupPlanNameValidation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	noErr(t, err)
	defer store.Close()
	valid := func(name string) {
		t.Helper()
		if err := ValidateBackupPlanName(name); err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	valid("a")
	valid("off-site_2")
	valid(strings.Repeat("x", MaxBackupPlanName))
	for _, name := range []string{"", strings.Repeat("x", MaxBackupPlanName+1), "with space", "a/b", "a.b", "plan:1", "계획", "a b"} {
		if ValidateBackupPlanName(name) == nil {
			t.Fatalf("name %q was accepted", name)
		}
	}
	plan := testBackupPlan("ok")
	noErr(t, store.SaveBackupPlan(ctx, plan))
	for name, change := range map[string]func(*BackupPlan){
		"bad name":      func(p *BackupPlan) { p.Name = "no good" },
		"short keep":    func(p *BackupPlan) { p.Keep = 0 },
		"short interv":  func(p *BackupPlan) { p.Interval = time.Minute },
		"relative dir":  func(p *BackupPlan) { p.Destination = "relative/backups" },
	} {
		edited := plan
		change(&edited)
		if err := store.SaveBackupPlan(ctx, edited); err == nil {
			t.Fatalf("%s saved an invalid plan", name)
		}
	}
}

// Plans are stored independently, listed in the stable order, and deleted
// without taking their runs.
func TestBackupPlanCRUDAndOrder(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	noErr(t, err)
	defer store.Close()
	first := time.Unix(1800000000, 0)
	second := first.Add(time.Minute)
	noErr(t, store.SaveBackupPlan(ctx, testBackupPlanAt("zebra", second)))
	noErr(t, store.SaveBackupPlan(ctx, testBackupPlanAt("alpha", first)))
	plans, err := store.BackupPlans(ctx)
	noErr(t, err)
	if len(plans) != 2 || plans[0].Name != "alpha" || plans[1].Name != "zebra" {
		t.Fatalf("plans in stable order=%+v", plans)
	}
	// Same creation time orders by name.
	noErr(t, store.SaveBackupPlan(ctx, testBackupPlanAt("beta", second)))
	plans, err = store.BackupPlans(ctx)
	noErr(t, err)
	if len(plans) != 3 || plans[1].Name != "beta" || plans[2].Name != "zebra" {
		t.Fatalf("same-time order=%+v", plans)
	}
	// An update keeps CreatedAt and changes the schedule.
	edited := plans[0]
	edited.Enabled, edited.Keep, edited.UpdatedAt = false, 2, second.Add(time.Hour)
	noErr(t, store.SaveBackupPlan(ctx, edited))
	reread, found, err := store.BackupPlan(ctx, "alpha")
	noErr(t, err)
	if !found || reread.Enabled || reread.Keep != 2 || !reread.CreatedAt.Equal(first) || !reread.UpdatedAt.Equal(second.Add(time.Hour)) {
		t.Fatalf("updated plan=%+v found=%v", reread, found)
	}
	// The default wrapper reads and writes the default plan.
	schedule := testBackupPlan(DefaultBackupPlanName).BackupSchedule
	noErr(t, store.SaveBackupSchedule(ctx, schedule))
	defaultPlan, found, err := store.BackupPlan(ctx, DefaultBackupPlanName)
	noErr(t, err)
	if !found || defaultPlan.Destination != schedule.Destination || !defaultPlan.CreatedAt.Equal(schedule.UpdatedAt) {
		t.Fatalf("wrapped default=%+v schedule=%+v", defaultPlan, schedule)
	}
	// Deleting a plan leaves its runs.
	run := BackupRun{ID: strings.Repeat("a", 32), Plan: "alpha", Kind: BackupRunManual, Status: BackupSucceeded,
		Destination: "/backups-a", BackupName: "owngit-backup-a", StartedAt: first, FinishedAt: first}
	noErr(t, store.Exec(ctx, `INSERT INTO backup_runs(id,plan,kind,status,destination,backup_name,started_at,finished_at) VALUES(?,?,?,?,?,?,?,?)`,
		run.ID, run.Plan, run.Kind, run.Status, run.Destination, run.BackupName, run.StartedAt.Unix(), run.FinishedAt.Unix()))
	noErr(t, store.DeleteBackupPlan(ctx, "alpha"))
	if _, found, err := store.BackupPlan(ctx, "alpha"); err != nil || found {
		t.Fatalf("deleted plan found=%v err=%v", found, err)
	}
	runs, err := store.BackupRunsOfPlan(ctx, "alpha")
	noErr(t, err)
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs of a deleted plan=%+v", runs)
	}
	running, err := store.BackupPlanRunning(ctx, "alpha")
	noErr(t, err)
	if running {
		t.Fatal("a finished plan reported as running")
	}
}

// The newest scheduled run of a plan alone decides when that plan is due.
func TestLastScheduledBackupRunIsPerPlan(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	noErr(t, err)
	defer store.Close()
	start := time.Unix(1800000000, 0)
	insert := func(index int, plan, kind string) {
		run := BackupRun{ID: fmt.Sprintf("%032x", index), Plan: plan, Kind: kind, Status: BackupSucceeded,
			Destination: "/backups-" + plan, StartedAt: start.Add(time.Duration(index) * time.Minute)}
		noErr(t, store.Exec(ctx, `INSERT INTO backup_runs(id,plan,kind,status,destination,started_at,finished_at) VALUES(?,?,?,?,?,?,?)`,
			run.ID, run.Plan, run.Kind, run.Status, run.Destination, run.StartedAt.Unix(), run.StartedAt.Unix()))
	}
	insert(1, "alpha", BackupRunManual)
	insert(2, "alpha", BackupRunScheduled)
	insert(3, "beta", BackupRunScheduled)
	insert(4, "alpha", BackupRunManual)
	last, found, err := store.LastScheduledBackupRun(ctx, "alpha")
	noErr(t, err)
	if !found || last.ID != fmt.Sprintf("%032x", 2) {
		t.Fatalf("last scheduled of alpha=%+v found=%v", last, found)
	}
	if _, found, err := store.LastScheduledBackupRun(ctx, "gamma"); err != nil || found {
		t.Fatalf("gamma scheduled found=%v err=%v", found, err)
	}
	all, err := store.BackupRunsOfPlan(ctx, "beta")
	noErr(t, err)
	if len(all) != 1 || all[0].Plan != "beta" {
		t.Fatalf("runs of beta=%+v", all)
	}
}

// Each plan bounds its own history: the oldest records beyond the limit go
// within one plan while another plan's newest records stay, and each plan's
// newest scheduled run is kept.
func TestBackupRunHistoryIsBoundedPerPlan(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	noErr(t, err)
	defer store.Close()
	start := time.Unix(1800000000, 0)
	finish := func(index int, plan, kind string) {
		run := BackupRun{ID: fmt.Sprintf("%s%031d", plan[:1], index), Plan: plan, Kind: kind, Status: BackupFailed,
			Verification: BackupVerifyNotRun, Destination: "/backups-" + plan,
			StartedAt: start.Add(time.Duration(index) * time.Second), FinishedAt: start.Add(time.Duration(index) * time.Second)}
		noErr(t, store.StartBackupRun(ctx, run))
		noErr(t, store.FinishBackupRun(ctx, run))
	}
	// Alpha gets 105 records, its oldest scheduled first; beta gets 3.
	finish(1, "alpha", BackupRunScheduled)
	for index := 2; index <= 105; index++ {
		finish(index, "alpha", BackupRunManual)
	}
	for index := 1; index <= 3; index++ {
		finish(index, "beta", BackupRunManual)
	}
	alpha, err := store.BackupRunsOfPlan(ctx, "alpha")
	noErr(t, err)
	beta, err := store.BackupRunsOfPlan(ctx, "beta")
	noErr(t, err)
	if len(alpha) != MaxBackupRunRecords+1 { // the newest 100 and the oldest scheduled
		t.Fatalf("alpha records=%d", len(alpha))
	}
	if len(beta) != 3 {
		t.Fatalf("beta records=%d, its small history is untouched", len(beta))
	}
	ids := map[string]bool{}
	for _, run := range alpha {
		ids[run.ID] = true
		if run.Plan != "alpha" {
			t.Fatalf("alpha history holds %q", run.Plan)
		}
	}
	if !ids[fmt.Sprintf("%s%031d", "a", 1)] || !ids[fmt.Sprintf("%s%031d", "a", 105)] {
		t.Fatalf("alpha kept=%v", ids)
	}
}

func testBackupPlan(name string) BackupPlan {
	return testBackupPlanAt(name, time.Unix(1800000000, 0))
}

func testBackupPlanAt(name string, at time.Time) BackupPlan {
	return BackupPlan{
		Name: name,
		BackupSchedule: BackupSchedule{
			Enabled:     true,
			Interval:    24 * time.Hour,
			Destination:  "/backups-" + name,
			Keep:        7,
			Verify:      true,
			UpdatedAt:   at,
		},
		CreatedAt: at,
	}
}
