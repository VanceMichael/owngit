package backups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"owngit/internal/recovery"
	"owngit/internal/state"
)

// Besides making backups, the service verifies a listed backup again, lets
// one be downloaded and receives an uploaded one (upload.go). One backup,
// verification or upload runs at a time, so a verification never reads a
// backup that is being written, and a backup never waits behind a
// verification for folder space it would share. A download runs beside
// them: the backup it reads is not removed until it ends.

// ErrBusy refuses a backup, a verification or an upload while OwnGit
// verifies or receives a backup.
var ErrBusy = errors.New("OwnGit is verifying or receiving a backup")

// FolderChanged and NotRecordedLead are fixed sentences of a verification's
// message.
const (
	FolderChanged   = "The backup's folder no longer holds this backup (it was moved, removed, changed or replaced by another backup), so nothing was recorded for it."
	NotRecordedLead = "The result could not be recorded:"
)

// ErrNoBackup refuses a run that is unknown, still running, or whose
// backup OwnGit removed or found gone.
var ErrNoBackup = errors.New("this run has no backup")

// ErrBackupGone refuses a run whose folder no longer holds the backup it
// wrote; the error that wraps it says why.
var ErrBackupGone = errors.New("the backup is no longer in its folder")

// Check states.
const (
	CheckRunning = "running"
	CheckPassed  = "passed"
	CheckFailed  = "failed"
)

// Check is the last verification of a listed backup that an owner asked
// for, kept until OwnGit stops; its result is also recorded with the run.
type Check struct {
	RunID      string     `json:"run_id"`
	BackupName string     `json:"backup_name"`
	Status     string     `json:"status"`
	Message    string     `json:"message,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// begin takes the one slot for a verification or an upload, refusing
// while a backup runs, while a finished backup holds the one slot to write
// its record or to remove its older backups, or while another one holds it.
// The caller holds s.mu.
func (s *Service) begin(ctx context.Context, task string) error {
	if s.ctx == nil || s.ctx.Err() != nil {
		return errors.New("OwnGit is stopping, so it starts nothing new")
	}
	if s.task != "" {
		return ErrBusy
	}
	if s.removing > 0 {
		return state.ErrBackupRunning
	}
	runs, err := s.Store.BackupRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == state.BackupRunning {
			return state.ErrBackupRunning
		}
	}
	s.task = task
	return nil
}

// end frees the slot and lets a scheduled backup that waited start.
func (s *Service) end() {
	s.mu.Lock()
	s.task = ""
	s.mu.Unlock()
	s.Wake()
}

// findRun returns the finished run id that still names its backup.
func (s *Service) findRun(ctx context.Context, id string) (state.BackupRun, error) {
	runs, err := s.Store.BackupRuns(ctx)
	if err != nil {
		return state.BackupRun{}, err
	}
	for _, run := range runs {
		if run.ID == id && run.Status != state.BackupRunning && run.BackupName != "" {
			return run, nil
		}
	}
	return state.BackupRun{}, ErrNoBackup
}

// openRun opens the backup of run in its folder, as the run owns it.
func openRun(run state.BackupRun) (*recovery.BackupFolder, *recovery.BackupCopy, error) {
	folder, err := recovery.OpenBackupFolder(run.Destination)
	if err != nil {
		return nil, nil, err
	}
	backup, err := openOwned(folder, run)
	if err != nil {
		folder.Close()
		if errors.Is(err, errMissing) || errors.Is(err, errNotOwned) {
			return nil, nil, fmt.Errorf("%w: %v", ErrBackupGone, err)
		}
		return nil, nil, err
	}
	return folder, backup, nil
}

// StartCheck verifies the backup of run id again, as a new backup is
// verified, and returns at once. The backup stays held from the check that
// the run owns it until the verification ends, and is verified only as the
// backup with the run's manifest (recovery.BackupCopy.Verify), so a folder
// that another backup took the name of meanwhile is never recorded as
// this run's. The result is recorded with the run and
// kept as LastCheck. It fails with ErrNoBackup or ErrBackupGone when there
// is nothing to verify, with state.ErrBackupRunning while a backup runs or
// finishes, and with ErrBusy while another verification or an upload runs.
func (s *Service) StartCheck(ctx context.Context, id string) (Check, error) {
	run, err := s.findRun(ctx, id)
	if err != nil {
		return Check{}, err
	}
	folder, backup, err := openRun(run)
	if err != nil {
		return Check{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "check"); err != nil {
		backup.Close()
		folder.Close()
		return Check{}, err
	}
	check := &Check{RunID: run.ID, BackupName: run.BackupName, Status: CheckRunning, StartedAt: s.now()}
	s.check = check
	s.work.Add(1)
	go func(ctx context.Context) {
		defer s.work.Done()
		defer s.end()
		defer folder.Close()
		defer backup.Close()
		s.finishCheck(ctx, run, backup, check)
	}(s.ctx)
	return *check, nil
}

// finishCheck verifies backup, the held backup of run, and records the
// result only while it is provably that backup: its manifest is still the
// run's, and its path still leads to it. Otherwise nothing is recorded for
// run, whatever the path holds now.
func (s *Service) finishCheck(ctx context.Context, run state.BackupRun, backup *recovery.BackupCopy, check *Check) {
	status, message, verification := CheckPassed, "", state.BackupVerifyPassed
	err := s.verifyWith(ctx, func(ctx context.Context, gitPath string) (recovery.Verification, error) {
		return backup.Verify(ctx, "", gitPath)
	})
	there, thereErr := backup.StillThere()
	switch {
	case ctx.Err() != nil:
		status, message, verification = CheckFailed, VerifyStopped+".", ""
	case thereErr != nil:
		status, message, verification = CheckFailed, joinSentences(FolderChanged, thereErr.Error()), ""
	case errors.Is(err, recovery.ErrReplaced) || !there:
		status, message, verification = CheckFailed, FolderChanged, ""
	case err != nil:
		status, message, verification = CheckFailed, err.Error(), state.BackupVerifyFailed
	}
	if verification != "" {
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.Store.RecordBackupVerification(record, run.ID, verification); err != nil {
			message = joinSentences(message, NotRecordedLead+" "+err.Error())
			s.logf("the verification of backup %s could not be recorded: %v", runPath(run), err)
		}
	}
	s.logf("verification of backup %s %s%s", runPath(run), status, prefixed(": ", message))
	finished := s.now()
	s.mu.Lock()
	check.Status, check.Message, check.FinishedAt = status, message, &finished
	s.mu.Unlock()
}

// Download is a backup held open for downloading. Close it once written.
type Download struct {
	// Name is the backup's folder name.
	Name    string
	service *Service
	runID   string
	folder  *recovery.BackupFolder
	backup  *recovery.BackupCopy
}

// OpenDownload opens the backup of run id to be written as an archive.
// Until Close, removing older backups leaves it in place. It fails with
// ErrNoBackup or ErrBackupGone when there is nothing to download.
func (s *Service) OpenDownload(ctx context.Context, id string) (*Download, error) {
	run, err := s.findRun(ctx, id)
	if err != nil {
		return nil, err
	}
	folder, backup, err := openRun(run)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse[run.ID] < 0 {
		backup.Close()
		folder.Close()
		return nil, fmt.Errorf("%w: OwnGit is removing it", ErrBackupGone)
	}
	if s.inUse == nil {
		s.inUse = map[string]int{}
	}
	s.inUse[run.ID]++
	return &Download{Name: run.BackupName, service: s, runID: run.ID, folder: folder, backup: backup}, nil
}

// WriteArchive writes the backup as a tar stream (recovery.WriteArchive).
func (d *Download) WriteArchive(ctx context.Context, writer io.Writer) error {
	return d.backup.WriteArchive(ctx, writer)
}

// Close releases the backup.
func (d *Download) Close() {
	d.backup.Close()
	d.folder.Close()
	d.service.mu.Lock()
	defer d.service.mu.Unlock()
	if d.service.inUse[d.runID]--; d.service.inUse[d.runID] <= 0 {
		delete(d.service.inUse, d.runID)
	}
}

// claimRemoval marks the backup of run id as being removed, so no
// download opens it, unless a download holds it: then it says false.
// releaseRemoval ends the mark.
func (s *Service) claimRemoval(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inUse[id] != 0 {
		return false
	}
	if s.inUse == nil {
		s.inUse = map[string]int{}
	}
	s.inUse[id] = -1
	return true
}

func (s *Service) releaseRemoval(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inUse, id)
}

// runPath is the folder of run's backup.
func runPath(run state.BackupRun) string {
	return filepath.Join(run.Destination, run.BackupName)
}

// RestoreLimit is a backup folder on a file system that a restore cannot
// write into (recovery.RestoreLimit). Backups there work.
type RestoreLimit struct {
	Destination string `json:"destination"`
	FileSystem  string `json:"file_system"`
}

// noteRestoreLimit finds out whether a restore could write into
// destination and keeps the answer for that destination's plans. Plans
// with different folders keep their own answers. A folder that cannot be
// tested keeps its last answer.
func (s *Service) noteRestoreLimit(destination string) {
	fileSystem, err := recovery.RestoreLimit(destination)
	if err != nil {
		s.logf("could not test whether a restore can write into %s: %v", destination, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits == nil {
		s.limits = map[string]*RestoreLimit{}
	}
	if fileSystem == "" {
		delete(s.limits, destination)
		return
	}
	s.limits[destination] = &RestoreLimit{Destination: destination, FileSystem: fileSystem}
}

// restoreLimit is the kept answer for destination, or nil.
func (s *Service) restoreLimit(destination string) *RestoreLimit {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.limits[destination]
	if limit == nil {
		return nil
	}
	result := *limit
	return &result
}

// joinSentences joins two messages, either of which may be empty.
func joinSentences(first, second string) string {
	if first == "" {
		return second
	}
	return first + " " + second
}

// prefixed is prefix and text, or "" without text.
func prefixed(prefix, text string) string {
	if text == "" {
		return ""
	}
	return prefix + text
}
