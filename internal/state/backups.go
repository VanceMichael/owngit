package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// DefaultBackupPlanName is the plan the one schedule of older builds
// becomes when this build upgrades, and the plan the schedule, status,
// runs and "back up now" calls that name no plan keep using.
const DefaultBackupPlanName = "default"

// MaxBackupPlans bounds how many backup plans one OwnGit keeps.
const MaxBackupPlans = 50

// BackupSchedule is when and where one backup plan makes backups while
// OwnGit serves. It is machine-local: a backup does not carry it.
type BackupSchedule struct {
	// Enabled is false when scheduled backups are off; the destination
	// still serves backups started by hand.
	Enabled     bool
	Interval    time.Duration
	Destination string
	// Keep is how many of its own backups OwnGit keeps in Destination.
	Keep int
	// Verify rehearses a restore of every new backup.
	Verify    bool
	UpdatedAt time.Time
}

// BackupPlan is a uniquely named backup schedule. Name is its stable
// identity: a run records the name of the plan that started it, and the
// name stays with the run after the plan is deleted.
type BackupPlan struct {
	Name string
	BackupSchedule
	// CreatedAt decides the stable order of plans together with Name; it
	// never changes.
	CreatedAt time.Time
}

// Bounds of a backup schedule, as the schema holds them.
const (
	MinBackupInterval = time.Hour
	MaxBackupInterval = 30 * 24 * time.Hour
	MaxBackupKeep     = 1000
	// MaxBackupPlanName is the most bytes a plan name holds.
	MaxBackupPlanName = 64
	// MaxBackupRunMessage is the most bytes a run's message holds.
	MaxBackupRunMessage = 500
	// MaxBackupRunRecords is how many finished run records OwnGit keeps
	// per plan besides the records of the backups it still keeps: the
	// newest ones, whether their runs succeeded or failed, and the newest
	// scheduled run, whose start decides when the plan's next scheduled
	// backup is due. Every recorded outcome applies the limit, so backups
	// that keep failing do not grow the history without bound.
	MaxBackupRunRecords = 100
)

// Kinds, states and verification results of a backup run.
const (
	BackupRunScheduled = "scheduled"
	BackupRunManual    = "manual"

	BackupRunning     = "running"
	BackupSucceeded   = "succeeded"
	BackupFailed      = "failed"
	BackupInterrupted = "interrupted"

	BackupVerifyNotRun = "not_run"
	BackupVerifyPassed = "passed"
	BackupVerifyFailed = "failed"
)

// ErrBackupRunning refuses a backup run while another one runs.
var ErrBackupRunning = errors.New("a backup is already running")

// ErrBackupRunEnded reports a run that is not running, so its outcome can
// no longer be recorded.
var ErrBackupRunEnded = errors.New("the backup run is not running")

// BackupRun is one backup that OwnGit started while serving.
type BackupRun struct {
	ID   string
	// Plan names the plan that started the run. It stays with the run
	// after the plan is deleted, so the run stays attributable and its
	// backup stays inspectable.
	Plan        string
	Kind        string
	Status      string
	Destination string
	// BackupName is the folder in Destination that the run writes.
	BackupName   string
	Verification string
	Message      string
	StartedAt    time.Time
	// FinishedAt is zero while the run runs.
	FinishedAt time.Time
	// HoldKnown says that the capture finished, so LongestHold and
	// LongestHoldRepository (empty without repositories) say how long
	// Git writes waited for it.
	HoldKnown             bool
	LongestHold           time.Duration
	LongestHoldRepository string
	// ManifestSHA256 is the SHA-256 of the manifest the run wrote, empty
	// when it wrote none: only the folder holding that manifest is the
	// run's backup.
	ManifestSHA256 string
}

// ValidateBackupPlanName checks name against the bounds the schema keeps:
// one to MaxBackupPlanName bytes, made of ASCII letters, digits,
// underscores and dashes, so a name is safe on every disk, in a URL and on
// a command line.
func ValidateBackupPlanName(name string) error {
	if len(name) < 1 || len(name) > MaxBackupPlanName {
		return fmt.Errorf("the backup plan name must be between 1 and %d characters", MaxBackupPlanName)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("the backup plan name %q may use only letters, digits, underscores and dashes", name)
	}
	return nil
}

// ValidateBackupSchedule checks a schedule against the bounds the schema
// keeps.
func ValidateBackupSchedule(schedule BackupSchedule) error {
	switch {
	case schedule.Interval < MinBackupInterval || schedule.Interval > MaxBackupInterval || schedule.Interval%time.Second != 0:
		return fmt.Errorf("the backup interval must be between %s and %s", MinBackupInterval, MaxBackupInterval)
	case schedule.Keep < 1 || schedule.Keep > MaxBackupKeep:
		return fmt.Errorf("the number of backups to keep must be between 1 and %d", MaxBackupKeep)
	case schedule.Destination == "" || len(schedule.Destination) > 4096 || !filepath.IsAbs(schedule.Destination):
		return errors.New("the backup folder must be an absolute path")
	case !utf8.ValidString(schedule.Destination):
		return errors.New("the backup folder must be valid UTF-8")
	}
	return nil
}

// ValidateBackupPlan checks the plan against the bounds the schema keeps.
func ValidateBackupPlan(plan BackupPlan) error {
	if err := ValidateBackupPlanName(plan.Name); err != nil {
		return err
	}
	return ValidateBackupSchedule(plan.BackupSchedule)
}

// scanBackupPlan scans one backup_plans row.
func scanBackupPlan(scanner interface {
	Scan(dest ...any) error
}) (BackupPlan, error) {
	var plan BackupPlan
	var enabled, verify int
	var interval, created, updated int64
	if err := scanner.Scan(&plan.Name, &enabled, &interval, &plan.Destination, &plan.Keep, &verify, &created, &updated); err != nil {
		return BackupPlan{}, err
	}
	plan.Enabled, plan.Verify = enabled == 1, verify == 1
	plan.Interval = time.Duration(interval) * time.Second
	plan.CreatedAt, plan.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return plan, nil
}

const backupPlanColumns = `name,enabled,interval_seconds,destination,keep,verify,created_at,updated_at`

// BackupPlans returns every plan in the stable order: when it was created,
// then its name.
func (s *Store) BackupPlans(ctx context.Context) ([]BackupPlan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupPlanColumns+` FROM backup_plans ORDER BY created_at,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []BackupPlan{}
	for rows.Next() {
		plan, err := scanBackupPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// BackupPlan returns the plan of name, and false when there is none.
func (s *Store) BackupPlan(ctx context.Context, name string) (BackupPlan, bool, error) {
	plan, err := scanBackupPlan(s.db.QueryRowContext(ctx, `SELECT `+backupPlanColumns+` FROM backup_plans WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return BackupPlan{}, false, nil
	}
	if err != nil {
		return BackupPlan{}, false, err
	}
	return plan, true, nil
}

// SaveBackupPlan inserts or replaces the plan of its name. CreatedAt is
// kept when the plan already exists; a new plan takes plan.CreatedAt.
func (s *Store) SaveBackupPlan(ctx context.Context, plan BackupPlan) error {
	if err := ValidateBackupPlan(plan); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO backup_plans(name,enabled,interval_seconds,destination,keep,verify,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET enabled=excluded.enabled,interval_seconds=excluded.interval_seconds,destination=excluded.destination,
		keep=excluded.keep,verify=excluded.verify,updated_at=excluded.updated_at`,
		plan.Name, boolInt(plan.Enabled), int64(plan.Interval/time.Second), plan.Destination, plan.Keep, boolInt(plan.Verify),
		plan.CreatedAt.Unix(), plan.UpdatedAt.Unix())
	return err
}

// DeleteBackupPlan removes the plan of name. Its runs stay: they name the
// plan as a plain string, so they and their backups stay inspectable.
func (s *Store) DeleteBackupPlan(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backup_plans WHERE name=?`, name)
	return err
}

// BackupSchedule returns the saved plan named default as a schedule, and
// false when there is no such plan. It keeps the calls that predate named
// plans working.
func (s *Store) BackupSchedule(ctx context.Context) (BackupSchedule, bool, error) {
	plan, configured, err := s.BackupPlan(ctx, DefaultBackupPlanName)
	if err != nil || !configured {
		return BackupSchedule{}, configured, err
	}
	return plan.BackupSchedule, true, nil
}

// SaveBackupSchedule replaces the plan named default, creating it when
// there is none. CreatedAt is set once.
func (s *Store) SaveBackupSchedule(ctx context.Context, schedule BackupSchedule) error {
	if err := ValidateBackupSchedule(schedule); err != nil {
		return err
	}
	created := schedule.UpdatedAt
	if existing, configured, err := s.BackupPlan(ctx, DefaultBackupPlanName); err != nil {
		return err
	} else if configured {
		created = existing.CreatedAt
	}
	return s.SaveBackupPlan(ctx, BackupPlan{Name: DefaultBackupPlanName, BackupSchedule: schedule, CreatedAt: created})
}

// StartBackupRun records run as running, or returns ErrBackupRunning when
// another run is running.
func (s *Store) StartBackupRun(ctx context.Context, run BackupRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_runs WHERE status='running'`).Scan(&running); err != nil {
		return err
	}
	if running != 0 {
		return ErrBackupRunning
	}
	plan := run.Plan
	if plan == "" {
		plan = DefaultBackupPlanName
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO backup_runs(id,plan,kind,status,destination,backup_name,started_at) VALUES(?,?,?,'running',?,?,?)`,
		run.ID, plan, run.Kind, run.Destination, run.BackupName, run.StartedAt.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishBackupRun records how a running run ended: its status,
// verification, message (cut to MaxBackupRunMessage bytes), finish time,
// hold, and its backup name, empty when the run published no backup. It
// also removes the finished records beyond the newest
// MaxBackupRunRecords, so the history stays bounded however the runs
// ended.
func (s *Store) FinishBackupRun(ctx context.Context, run BackupRun) error {
	var holdMS, holdRepository, manifest any
	if run.ManifestSHA256 != "" {
		manifest = run.ManifestSHA256
	}
	if run.HoldKnown {
		holdMS = run.LongestHold.Milliseconds()
		if run.LongestHoldRepository != "" {
			holdRepository = run.LongestHoldRepository
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE backup_runs SET status=?,verification=?,message=?,finished_at=?,longest_hold_ms=?,longest_hold_repository=?,backup_name=?,manifest_sha256=?
		WHERE id=? AND status='running'`,
		run.Status, run.Verification, cutText(run.Message, MaxBackupRunMessage), run.FinishedAt.Unix(), holdMS, holdRepository, run.BackupName, manifest, run.ID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("%w: %s", ErrBackupRunEnded, run.ID)
	}
	if err := forgetOldBackupRuns(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// InterruptBackupRuns marks every running run interrupted, with message,
// and applies the record limit as every recorded outcome does. Only a
// starting server calls it, before it starts a run: a run recorded as
// running then belongs to a process that ended.
func (s *Store) InterruptBackupRuns(ctx context.Context, message string, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE backup_runs SET status='interrupted',message=?,finished_at=? WHERE status='running'`,
		cutText(message, MaxBackupRunMessage), now.Unix())
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := forgetOldBackupRuns(ctx, tx); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// planHistory bounds one plan's records while they are read newest first.
type planHistory struct {
	index         int
	scheduledSeen bool
}

// forgetOldBackupRuns removes, within each plan, the finished records
// beyond the newest MaxBackupRunRecords that hold no backup, except the
// plan's newest scheduled run, whose start decides when its next scheduled
// backup is due. The record of a backup OwnGit still keeps stays. Plans
// never trim each other's history.
func forgetOldBackupRuns(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,plan,kind,status,backup_name FROM backup_runs ORDER BY rowid DESC`)
	if err != nil {
		return err
	}
	var forget []string
	buckets := map[string]*planHistory{}
	for rows.Next() {
		var id, plan, kind, status, backupName string
		if err := rows.Scan(&id, &plan, &kind, &status, &backupName); err != nil {
			rows.Close()
			return err
		}
		bucket := buckets[plan]
		if bucket == nil {
			bucket = &planHistory{index: -1}
			buckets[plan] = bucket
		}
		bucket.index++
		newestScheduled := kind == BackupRunScheduled && !bucket.scheduledSeen
		if kind == BackupRunScheduled {
			bucket.scheduledSeen = true
		}
		if bucket.index >= MaxBackupRunRecords && status != BackupRunning && backupName == "" && !newestScheduled {
			forget = append(forget, id)
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	for _, id := range forget {
		if _, err := tx.ExecContext(ctx, `DELETE FROM backup_runs WHERE id=? AND status<>'running'`, id); err != nil {
			return err
		}
	}
	return nil
}

const backupRunSelectColumns = `id,plan,kind,status,destination,backup_name,verification,message,started_at,finished_at,longest_hold_ms,longest_hold_repository,manifest_sha256`

// scanBackupRun scans one backup_runs row.
func scanBackupRun(scanner interface {
	Scan(dest ...any) error
}) (BackupRun, error) {
	var run BackupRun
	var started int64
	var finished, holdMS sql.NullInt64
	var holdRepository, manifest sql.NullString
	if err := scanner.Scan(&run.ID, &run.Plan, &run.Kind, &run.Status, &run.Destination, &run.BackupName, &run.Verification, &run.Message,
		&started, &finished, &holdMS, &holdRepository, &manifest); err != nil {
		return BackupRun{}, err
	}
	run.StartedAt, run.ManifestSHA256 = time.Unix(started, 0), manifest.String
	if finished.Valid {
		run.FinishedAt = time.Unix(finished.Int64, 0)
	}
	if holdMS.Valid {
		run.HoldKnown, run.LongestHold, run.LongestHoldRepository = true, time.Duration(holdMS.Int64)*time.Millisecond, holdRepository.String
	}
	return run, nil
}

// BackupRuns returns every recorded run, the newest first: in the order
// they were started, which one at a time keeps, whatever the clock said.
func (s *Store) BackupRuns(ctx context.Context) ([]BackupRun, error) {
	return s.backupRuns(ctx, ``)
}

// BackupRunsOfPlan returns every recorded run of plan, the newest first.
func (s *Store) BackupRunsOfPlan(ctx context.Context, plan string) ([]BackupRun, error) {
	return s.backupRuns(ctx, ` WHERE plan=?`, plan)
}

func (s *Store) backupRuns(ctx context.Context, where string, args ...any) ([]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupRunSelectColumns+` FROM backup_runs`+where+` ORDER BY rowid DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []BackupRun{}
	for rows.Next() {
		run, err := scanBackupRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// LastScheduledBackupRun returns the newest run of plan that started from
// the schedule, whatever it became: its start decides when the plan's next
// scheduled backup is due. False means the plan has never run on its
// schedule.
func (s *Store) LastScheduledBackupRun(ctx context.Context, plan string) (BackupRun, bool, error) {
	run, err := scanBackupRun(s.db.QueryRowContext(ctx, `SELECT `+backupRunSelectColumns+`
		FROM backup_runs WHERE plan=? AND kind=? ORDER BY rowid DESC LIMIT 1`, plan, BackupRunScheduled))
	if errors.Is(err, sql.ErrNoRows) {
		return BackupRun{}, false, nil
	}
	if err != nil {
		return BackupRun{}, false, err
	}
	return run, true, nil
}

// BackupPlanRunning reports whether a run of plan currently runs. A plan
// with such a run cannot be deleted.
func (s *Store) BackupPlanRunning(ctx context.Context, plan string) (bool, error) {
	var running int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_runs WHERE plan=? AND status='running'`, plan).Scan(&running); err != nil {
		return false, err
	}
	return running > 0, nil
}

// ForgetBackup records that the backup of the finished run id is gone:
// OwnGit removed it, or its folder no longer holds it.
func (s *Store) ForgetBackup(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backup_runs SET backup_name='' WHERE id=? AND status<>'running'`, id)
	return err
}

// CutBackupRunMessage cuts text to what a run's stored message holds at
// most, or to limit when that is smaller, without splitting a character: a
// message OwnGit shows without a record obeys the same bound as one it
// recorded. A limit below zero means no room.
func CutBackupRunMessage(text string, limit int) string {
	return cutText(text, max(0, min(limit, MaxBackupRunMessage)))
}

// cutText cuts text to at most limit bytes without splitting a character.
func cutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// SetBackupRunMessage replaces the message of the finished run id, cut to
// MaxBackupRunMessage bytes, so a pass that reads the backup folder after
// the run was recorded can add what it found there. A run whose record is
// still running keeps no message that would say it ended.
func (s *Store) SetBackupRunMessage(ctx context.Context, id, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE backup_runs SET message=? WHERE id=? AND status<>'running'`,
		cutText(message, MaxBackupRunMessage), id)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("backup run %s has no finished record", id)
	}
	return nil
}

// RecordBackupVerification records the result of verifying the backup of
// the finished run id again: passed or failed.
func (s *Store) RecordBackupVerification(ctx context.Context, id, verification string) error {
	if verification != BackupVerifyPassed && verification != BackupVerifyFailed {
		return fmt.Errorf("invalid verification result %q", verification)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE backup_runs SET verification=? WHERE id=? AND status<>'running' AND backup_name<>''`, verification, id)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("backup run %s has no finished backup", id)
	}
	return nil
}
