package webui

import (
	"fmt"
	"html/template"
	"time"
)

// The Backups group of Storage & recovery: the schedule, Back up now, the
// state of backups, the recorded runs with what can be done with each
// backup, and the upload of a backup to restore.

// Backup actions of the Settings form.
const (
	// ActionSaveBackupSchedule saves the schedule. Fields:
	// backup_destination, backup_scheduled and backup_verify (switches),
	// backup_interval (one of BackupsInfo.Intervals), backup_keep.
	ActionSaveBackupSchedule = "save_backup_schedule"
	// ActionSaveBackupPlan saves the selected plan. Fields: backup_plan
	// plus the destination, switch, interval and keep fields.
	ActionSaveBackupPlan = "save_backup_plan"
	// ActionCreateBackupPlan creates a plan: backup_new_plan and
	// backup_new_destination.
	ActionCreateBackupPlan = "create_backup_plan"
	// ActionDeleteBackupPlan deletes the plan named backup_plan while it is
	// idle.
	ActionDeleteBackupPlan = "delete_backup_plan"
	// ActionBackupNow starts a backup.
	ActionBackupNow = "backup_now"
	// ActionBackupVerify verifies a listed backup again, and
	// ActionBackupDownload downloads it; the run is the run field.
	ActionBackupVerify   = "backup_verify"
	ActionBackupDownload = "backup_download"
	// ActionBackupUpload receives a backup archive, the backup_file part of
	// a multipart form.
	ActionBackupUpload = "backup_upload"

	// GroupBackups is the schedule; GroupBackupRuns shows the result of
	// the other actions.
	GroupBackups    = "backups"
	GroupBackupRuns = "backup_runs"
)

// BackupPlanInfo is one named plan in the plan chooser.
type BackupPlanInfo struct {
	// Name is the plan's stable name.
	Name string
	// State is the plan's state, on or off.
	State string
	// Selected marks the plan the page shows.
	Selected bool
}

// BackupsInfo is the Backups group.
type BackupsInfo struct {
	// Visible is true for a confirmed administrator, who alone sees and
	// changes backups: the rest is empty otherwise.
	Visible bool
	// Configured is false when the plan the page shows has no folder yet.
	Configured bool
	// SelectedPlan is the plan the page shows; it is default when the URL
	// names none.
	SelectedPlan string
	// Plans are the saved plans, in their stable order.
	Plans []BackupPlanInfo
	// ChoosePlans is true when more than one plan exists and the chooser is
	// shown.
	ChoosePlans bool
	// Scheduled, Destination, Interval, Keep and Verify are the selected
	// plan's schedule; Interval is one of Intervals.
	Scheduled   bool
	Destination string
	Interval    string
	Keep        int
	Verify      bool
	Intervals   []string
	// Running is the selected plan's backup that runs, if any.
	Running *BackupRunInfo
	// LastRun is the last backup that ended, and LastVerified the newest
	// verified one that OwnGit keeps, of the selected plan.
	LastRun, LastVerified *BackupRunInfo
	// NextRun is when the selected plan's next scheduled backup is due;
	// zero when none is. NextRunNow says it is due as soon as the running
	// backup ends.
	NextRun    time.Time
	NextRunNow bool
	// Busy is true while a backup, a verification or an upload runs, when
	// no other can start.
	Busy bool
	// RestoreLimit names the file system of the backup folder when a
	// restore cannot write into a folder on it.
	RestoreLimit string
	// Runs are the recorded backups of the plan the page shows, newest
	// first.
	Runs []BackupRunInfo
	// Check is the last verification an owner asked for.
	Check *BackupCheckInfo
	// Upload is the uploaded backup.
	Upload *BackupUploadInfo
}

// BackupRunInfo is one recorded backup.
type BackupRunInfo struct {
	ID string
	// Plan names the plan that started the run.
	Plan string
	// Kind is scheduled or manual; Status running, succeeded, failed or
	// interrupted; Verification passed, failed or not_run.
	Kind, Status, Verification string
	// Name and Path are the backup's folder, empty when OwnGit keeps no
	// backup of this run.
	Name, Path string
	// Message is the run's own text, in English.
	Message              string
	MessageEN, MessageKO string
	StartedAt            time.Time
	FinishedAt           time.Time
	// HoldKnown says that Hold is how long Git writes to one repository,
	// HoldRepository, waited for the backup at most.
	HoldKnown      bool
	HoldMS         int64
	HoldRepository string
	// Restore is how to restore this backup, when it has one.
	Restore *BackupRestore
}

// BackupCheckInfo is a verification an owner asked for.
type BackupCheckInfo struct {
	Name string
	// Status is running, passed or failed; Message says why it failed.
	Status  string
	Message string
	// MessageKO is Message in Korean.
	MessageKO string
}

// BackupUploadInfo is the uploaded backup.
type BackupUploadInfo struct {
	Name string
	Size int64
	// Status is verifying, passed or failed; Message says why it failed.
	Status    string
	Message   string
	MessageKO string
	RemovesAt time.Time
	Restore   *BackupRestore
}

// BackupRestore is how to restore one backup on this computer, while
// OwnGit is stopped: every command is for its shell.
type BackupRestore struct {
	// Stop and Start are the commands that stop and start the OwnGit
	// service, empty when OwnGit does not run as a service.
	Stop, Start string
	// Network is the command that saves the base URL and trusted proxy, which
	// a restore resets; they apply at the next start.
	Network string
	// StateDir and RepositoryRoot are the folders the restore writes, which
	// must not exist, so the current ones are renamed first to MovedState
	// and MovedRepositories.
	StateDir, RepositoryRoot      string
	MovedState, MovedRepositories string
	// Command is the restore command, and Shell the shell it is written
	// for when that is not the usual POSIX shell (PowerShell on Windows).
	Command string
	Shell   string
	// Unchecked is the backup when where it will be after the move could
	// not be checked, for Problem; there is then no Command.
	Unchecked, Problem string
}

// backupIntervalLabels names each schedule interval.
var backupIntervalLabels = map[string]MessageCode{"12h": MsgBackupEvery12h, "1d": MsgBackupEveryDay, "7d": MsgBackupEvery7d}

// backupInterval is the label of a schedule interval.
func backupInterval(lang Lang, interval string) template.HTML {
	if code, known := backupIntervalLabels[interval]; known {
		return bi(lang, code)
	}
	return biText(lang, interval, interval)
}

// backupWords names the recorded words of a backup: its kind, status and
// verification, and the states of a verification and an upload.
var backupWords = map[string]MessageCode{
	"kind.scheduled": MsgBackupKindScheduled, "kind.manual": MsgBackupKindManual,
	"status.running": MsgBackupStatusRunning, "status.succeeded": MsgBackupStatusSucceeded,
	"status.failed": MsgBackupStatusFailed, "status.interrupted": MsgBackupStatusInterrupted,
	"verification.passed": MsgBackupVerifyPassed, "verification.failed": MsgBackupVerifyFailed,
	"verification.not_run": MsgBackupVerifyNotRun,
	"check.running":        MsgBackupCheckRunning, "check.passed": MsgBackupCheckPassed, "check.failed": MsgBackupCheckFailed,
	"upload.verifying": MsgBackupUploadVerifying, "upload.passed": MsgBackupUploadPassed, "upload.failed": MsgBackupUploadFailed,
}

// backupWord is the word for value of kind (kind, status, verification,
// check or upload). A value this build does not know is shown as it is.
func backupWord(lang Lang, kind, value string) template.HTML {
	if code, known := backupWords[kind+"."+value]; known {
		return bi(lang, code)
	}
	return biText(lang, value, value)
}

// backupState is the status mark of a recorded word, so the word never
// relies on colour alone.
func backupState(value string) NoticeKind {
	switch value {
	case "succeeded", "passed":
		return NoticeSuccess
	case "failed", "interrupted":
		return NoticeError
	case "running", "verifying":
		return NoticeInfo
	}
	return NoticeWarning
}

// biWhen renders a sentence whose single %s is the date and time t, each
// language with its own date.
func biWhen(lang Lang, code MessageCode, t time.Time) template.HTML {
	return biText(lang, fmt.Sprintf(Text(LangEN, code), formatDateTime(LangEN, t)), fmt.Sprintf(Text(LangKO, code), formatDateTime(LangKO, t)))
}
