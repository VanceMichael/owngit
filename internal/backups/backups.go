// Package backups makes the backups that OwnGit takes while it serves: on a
// schedule and when an owner asks, each one recorded as a run, verified when
// the schedule says so, and followed by the removal of older backups that
// OwnGit made in the same folder.
package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"owngit/internal/recovery"
	"owngit/internal/repository"
	"owngit/internal/state"
)

const (
	// DefaultVerifyLimit bounds the verification of one backup. A
	// verification that has not finished by then fails.
	DefaultVerifyLimit = 2 * time.Hour
	// recheck bounds how long the scheduler sleeps before it reads the
	// schedule again, so a changed clock delays a backup at most this long.
	recheck = time.Hour
	// recordWait and recordWaitMax bound how long a run's final result
	// waits before OwnGit records it again after the state store refused
	// it: the wait doubles from the first value up to the second, so a
	// state store that becomes writable again is used within recordWaitMax.
	recordWait    = time.Second
	recordWaitMax = time.Minute
	// recordWrite bounds one attempt to write a run's final record. The
	// attempt also ends with the service, so a stop ends one that is
	// running.
	recordWrite = 30 * time.Second
	// recordEndWrite bounds the one attempt a stop allows, on a clock the
	// stop does not end: short enough to fit the stop's own budget, and
	// longer than the state store's busy wait.
	recordEndWrite = 10 * time.Second
	// namePrefix starts the folder name of every backup a run writes.
	namePrefix = "owngit-backup-"
)

// Default schedule once a backup folder is set.
const (
	DefaultInterval = 24 * time.Hour
	DefaultKeep     = 7
)

// Intervals are the schedule's choices, shortest first.
var Intervals = []struct {
	Name     string
	Interval time.Duration
}{{"12h", 12 * time.Hour}, {"1d", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}}

// IntervalName is the choice name of interval, or "" when it is none.
func IntervalName(interval time.Duration) string {
	for _, choice := range Intervals {
		if choice.Interval == interval {
			return choice.Name
		}
	}
	return ""
}

// ErrNotConfigured refuses a backup before a backup folder is set.
var ErrNotConfigured = errors.New("no backup folder is set")

// The fixed sentences of a run's message. The dashboard shows them in the
// owner's language; what follows each is the technical detail of the cause.
const (
	// InterruptedMessage is the message of a run that OwnGit stopped, or that
	// a process which ended left running.
	InterruptedMessage = "OwnGit stopped before this backup finished, so it is not a finished backup."
	// NotVerifiedLead starts the reason of a backup that was written but failed
	// its verification.
	NotVerifiedLead = "the backup was written but did not pass verification:"
	// CompleteButLead and AlsoLead start the problems found after a backup.
	CompleteButLead = "The backup is complete, but"
	AlsoLead        = " Also,"
	// VerifyStopped is the reason of a verification that OwnGit's stop ended.
	VerifyStopped = "OwnGit stopped before the verification finished"
)

// Service starts scheduled backups and backups asked for now. One runs at a
// time, which the store enforces across restarts too.
type Service struct {
	Store        *state.Store
	Repositories *repository.Manager
	Logf         func(format string, arguments ...any)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// VerifyLimit bounds each verification; zero is DefaultVerifyLimit.
	VerifyLimit time.Duration
	// StopBy returns the moment a stop must be over, or nil. The final
	// record of an interrupted backup is written by then, not after it.
	StopBy func() time.Time

	mu   sync.Mutex
	ctx  context.Context
	stop context.CancelFunc
	// work counts the scheduler and the running backup.
	work sync.WaitGroup
	wake chan struct{}
	// task is the verification or upload that runs, or "" (check.go).
	task  string
	check *Check
	// upload is the uploaded backup (upload.go), and uploadTimer removes
	// it when its time is up.
	upload      *Upload
	uploadTimer *time.Timer
	// inUse counts the downloads of each run's backup; -1 marks one that
	// is being removed.
	inUse map[string]int
	// limits is what the last test of each backup folder found, keyed by
	// the destination it concerns.
	limits map[string]*RestoreLimit
	// pending is the finished run whose record the state store refused,
	// held until the state takes it, so Status and Runs show it as the
	// running backup, saying how it ended and that its record is not saved
	// yet. Its stored record still stands as running, which keeps any other
	// backup from starting.
	pending *state.BackupRun
	// removing counts the runs that hold the one slot for removing their
	// older backups, and removingRun is the one that took it first: while it
	// is held, no backup, verification or upload starts, since they all look
	// at the same folders and a verification could record a false failure
	// while one is removed. A run takes the slot before its final record is
	// written, so nothing can start between a saved record and the removal
	// that follows it. It counts, so a run that finishes its removal cannot
	// release the slot of one that still holds it.
	removing    int
	removingRun *state.BackupRun
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logf(format string, arguments ...any) {
	if s.Logf != nil {
		s.Logf(format, arguments...)
	}
}

// Start records every run that a previous process left running as
// interrupted, then starts the scheduler. Stop ends it.
func (s *Service) Start(ctx context.Context) error {
	if count, err := s.Store.InterruptBackupRuns(ctx, InterruptedMessage, s.now()); err != nil {
		return fmt.Errorf("record unfinished backups as interrupted: %w", err)
	} else if count > 0 {
		s.logf("a backup that OwnGit was making when it stopped is recorded as interrupted")
	}
	if err := s.removeUploads(); err != nil {
		return fmt.Errorf("remove the uploaded backup left from before: %w", err)
	}
	s.open(ctx)
	s.work.Add(1)
	go s.schedule()
	if plans, err := s.Store.BackupPlans(ctx); err == nil {
		for _, plan := range plans {
			go s.noteRestoreLimit(plan.Destination)
		}
	}
	return nil
}

// open lets backups start under ctx until Stop.
func (s *Service) open(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx, s.stop = context.WithCancel(ctx)
	s.wake = make(chan struct{}, 1)
}

// Stop cancels the scheduler and a running backup, which records itself
// as interrupted, and waits for both until ctx ends.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
	}
	if s.uploadTimer != nil {
		s.uploadTimer.Stop()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("a backup was still stopping: %w", ctx.Err())
	}
}

// Wake makes the scheduler read the schedule again, as after a change.
func (s *Service) Wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) schedule() {
	defer s.work.Done()
	for {
		timer := time.NewTimer(s.tick())
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// planTiming is a plan with when its next scheduled backup is due: zero
// when the plan is off.
type planTiming struct {
	plan state.BackupPlan
	due  time.Time
}

// planTimings reads every plan with its due time, in the plans' stable
// order.
func (s *Service) planTimings(ctx context.Context) ([]planTiming, error) {
	plans, err := s.Store.BackupPlans(ctx)
	if err != nil {
		return nil, err
	}
	timings := make([]planTiming, 0, len(plans))
	for _, plan := range plans {
		timing := planTiming{plan: plan}
		if plan.Enabled {
			last, found, err := s.Store.LastScheduledBackupRun(ctx, plan.Name)
			if err != nil {
				return nil, err
			}
			if found {
				timing.due = last.StartedAt.Add(plan.Interval)
			} else {
				timing.due = plan.CreatedAt
			}
		}
		timings = append(timings, timing)
	}
	return timings, nil
}

// dueFrom returns the timings whose time has come, the soonest due first.
// Equal due times keep the plans' stable order: a plan that is due first
// starts first, and ties never depend on map iteration.
func dueFrom(timings []planTiming, now time.Time) []planTiming {
	var due []planTiming
	for _, timing := range timings {
		if !timing.due.IsZero() && !timing.due.After(now) {
			due = append(due, timing)
		}
	}
	// timings already hold the stable order; a stable sort by due time
	// keeps it between equal due times.
	sort.SliceStable(due, func(i, j int) bool { return due[i].due.Before(due[j].due) })
	return due
}

// tick starts the scheduled backup of the plan that is due first and
// returns how long to wait before looking again. The end of a backup, a
// verification or an upload wakes the scheduler, so a plan that was due
// while the one slot was taken starts when it frees, and then the next
// plan that is still due starts: a plan that failed used its interval, so
// it cannot take the turn a plan that is still due waits for.
func (s *Service) tick() time.Duration {
	now := s.now()
	timings, err := s.planTimings(s.ctx)
	if err != nil {
		s.logf("scheduled backups could not read their plans: %v", err)
		return recheck
	}
	if due := dueFrom(timings, now); len(due) > 0 {
		if _, err := s.start(state.BackupRunScheduled, due[0].plan.Name); err != nil && !errors.Is(err, state.ErrBackupRunning) && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrPlanNotFound) && s.ctx.Err() == nil {
			s.logf("a scheduled backup of plan %s did not start: %v", due[0].plan.Name, err)
		}
		return recheck
	}
	wait := time.Duration(0)
	for _, timing := range timings {
		if timing.due.IsZero() {
			continue
		}
		until := timing.due.Sub(now)
		if until <= 0 {
			return recheck
		}
		if wait == 0 || until < wait {
			wait = until
		}
	}
	if wait == 0 {
		return recheck
	}
	return min(wait, recheck)
}

// StartNow starts a backup into the default plan's folder and returns its
// run at once. It fails with state.ErrBackupRunning while another one runs
// or finishes (it is writing its record or removing its older backups),
// with ErrBusy while OwnGit verifies or receives a backup, and with
// ErrNotConfigured before the default plan is set.
func (s *Service) StartNow() (state.BackupRun, error) {
	return s.start(state.BackupRunManual, state.DefaultBackupPlanName)
}

func (s *Service) start(kind, planName string) (state.BackupRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return state.BackupRun{}, errors.New("OwnGit is stopping, so it starts no backup")
	}
	if s.task != "" {
		return state.BackupRun{}, ErrBusy
	}
	if s.removing > 0 {
		return state.BackupRun{}, state.ErrBackupRunning
	}
	plan, configured, err := s.Store.BackupPlan(s.ctx, planName)
	if err != nil {
		return state.BackupRun{}, err
	}
	if !configured {
		// The calls that predate named plans name the default plan, and
		// keep reading "no folder is set" when it is absent.
		if planName == state.DefaultBackupPlanName {
			return state.BackupRun{}, ErrNotConfigured
		}
		return state.BackupRun{}, ErrPlanNotFound
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return state.BackupRun{}, err
	}
	now := s.now()
	run := state.BackupRun{
		ID: hex.EncodeToString(id), Plan: plan.Name, Kind: kind, Status: state.BackupRunning,
		Destination: plan.Destination, Verification: state.BackupVerifyNotRun, StartedAt: now,
	}
	run.BackupName = namePrefix + now.UTC().Format("20060102-150405") + "-" + run.ID[:8]
	if err := s.Store.StartBackupRun(s.ctx, run); err != nil {
		return state.BackupRun{}, err
	}
	s.work.Add(1)
	go func(ctx context.Context) {
		defer s.work.Done()
		s.execute(ctx, run, plan)
		s.Wake()
	}(s.ctx)
	return run, nil
}

// execute makes the backup of run, verifies it when the plan as it was
// when the run started asks, records how it ended, and removes older
// backups of that plan once that record is saved.
func (s *Service) execute(ctx context.Context, run state.BackupRun, plan state.BackupPlan) {
	schedule := plan.BackupSchedule
	output := filepath.Join(run.Destination, run.BackupName)
	note, err := s.checkRoom(ctx, run)
	var report recovery.CaptureReport
	if err == nil {
		report, err = recovery.CreateWhileServing(ctx, s.Store, s.Repositories, output)
	}
	if report.Captured {
		run.HoldKnown, run.LongestHold, run.LongestHoldRepository = true, report.LongestHold, report.LongestHoldRepository
	}
	run.ManifestSHA256 = report.ManifestSHA256
	if err == nil && schedule.Verify {
		if err = s.verify(ctx, output); err != nil {
			run.Verification = state.BackupVerifyFailed
			err = fmt.Errorf(NotVerifiedLead+" %w", err)
		} else {
			run.Verification = state.BackupVerifyPassed
		}
	}
	var problems []string
	switch {
	case ctx.Err() != nil:
		run.Status, run.Message = state.BackupInterrupted, InterruptedMessage
		if run.Verification == state.BackupVerifyFailed {
			run.Verification = state.BackupVerifyNotRun
		}
	case err != nil:
		run.Status, run.Message = state.BackupFailed, err.Error()
	default:
		run.Status = state.BackupSucceeded
	}
	// A backup that failed after it was published, or was stopped then,
	// is still there, and is kept as OwnGit's; one never published is not.
	if err != nil || ctx.Err() != nil {
		if published, problem := s.published(run); !published {
			run.BackupName, run.ManifestSHA256 = "", ""
		} else if problem != "" {
			problems = append(problems, problem)
		}
	}
	if note != "" {
		problems = append([]string{note}, problems...)
	}
	if len(problems) > 0 {
		if run.Message == "" {
			run.Message = CompleteButLead + " " + strings.Join(problems, "; ")
		} else {
			run.Message += AlsoLead + " " + strings.Join(problems, "; ")
		}
	}
	if run.BackupName != "" {
		if run.Status == state.BackupSucceeded {
			s.noteAliases(&run, report)
		}
		s.noteRestoreLimit(run.Destination)
	}
	run.FinishedAt = s.now()
	// The one slot is taken before the final record is written: a backup, a
	// verification or an upload admitted between the saved record and the
	// removal that follows would look at the folders while an older backup is
	// removed. A stop that saved the result here removes nothing; the next
	// backup removes then.
	if retains(run) && ctx.Err() == nil {
		s.holdRemoval(&run)
		defer s.dropRemoval()
	}
	if !s.record(ctx, run) {
		return
	}
	if retains(run) && ctx.Err() == nil {
		if left := s.removeOld(ctx, run, schedule.Keep); len(left) > 0 {
			s.noteLeftInPlace(ctx, &run, left)
		}
	}
	line := fmt.Sprintf("backup %s %s", output, run.Status)
	if run.HoldKnown && run.LongestHoldRepository != "" {
		line += fmt.Sprintf("; Git writes waited at most %s (repository %s)", run.LongestHold.Round(time.Millisecond), run.LongestHoldRepository)
	}
	if run.Message != "" {
		line += ": " + run.Message
	}
	s.logf("%s", line)
}

// retains says whether run, once recorded, is followed by removing
// backups: a succeeded run applies the keep limit, and a run that failed
// verification removes only the older failed-verification backups.
func retains(run state.BackupRun) bool {
	return run.Status == state.BackupSucceeded || failedVerification(run)
}

// failedVerification says whether run wrote a backup that did not pass the
// verification right after it was written.
func failedVerification(run state.BackupRun) bool {
	return run.Status == state.BackupFailed && run.Verification == state.BackupVerifyFailed
}

// record writes the final result of run, and says whether it was saved.
// A state store that refuses the write is tried again with a bounded
// backoff, so a state store that was full or briefly unavailable does not
// leave a finished backup looking like one that is still copying: while
// the result waits, Status and Runs show it as the running backup and say
// how it ended and that its record is not saved yet. A run whose stored
// record is no longer running can never be recorded, so that refusal is
// reported once. An ordinary attempt takes its bound and its end from the
// service: a stop ends one it found running, and leaves one attempt the
// stop does not end, bounded by recordEndWrite, so recording a result
// delays a stop by at most that.
func (s *Service) record(ctx context.Context, run state.BackupRun) bool {
	wait := recordWait
	for {
		if ctx.Err() != nil {
			// OwnGit is stopping before this attempt: only the one bounded
			// attempt a stop allows is left.
			return s.recordEnd(ctx, run)
		}
		err := s.recordOnce(ctx, run, recordWrite)
		if err == nil {
			s.forgetPending(run.ID)
			return true
		}
		// The result is held before the refusal is reported, so a reader
		// that sees the report sees the result too.
		s.mu.Lock()
		s.pending = &run
		s.mu.Unlock()
		s.logf("backup %s ended %s but could not be recorded: %v", filepath.Join(run.Destination, run.BackupName), run.Status, err)
		if errors.Is(err, state.ErrBackupRunEnded) {
			s.forgetPending(run.ID)
			return false
		}
		select {
		case <-ctx.Done():
			return s.recordEnd(ctx, run)
		case <-time.After(wait):
		}
		wait = min(wait*2, recordWaitMax)
	}
}

// recordEnd makes the one attempt a stop allows: while OwnGit stops, the
// result it holds is offered to the state store once, within recordEndWrite
// on a clock the stop does not end.
func (s *Service) recordEnd(ctx context.Context, run state.BackupRun) bool {
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordEndWrite)
	defer cancel()
	if s.StopBy != nil {
		var bounded context.CancelFunc
		write, bounded = context.WithDeadline(write, s.StopBy())
		defer bounded()
	}
	if s.Store.FinishBackupRun(write, run) != nil {
		return false
	}
	s.forgetPending(run.ID)
	return true
}

// recordOnce makes one attempt to record the final result of run, within
// bound, ending with the service.
func (s *Service) recordOnce(ctx context.Context, run state.BackupRun, bound time.Duration) error {
	write, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return s.Store.FinishBackupRun(write, run)
}

// holdRemoval takes the one slot for the run whose result is about to be
// recorded: no backup, verification or upload starts until every holder has
// released it. run is the run the views name while the slot is held.
func (s *Service) holdRemoval(run *state.BackupRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removing == 0 {
		result := *run
		s.removingRun = &result
	}
	s.removing++
}

// dropRemoval releases one hold; the slot is free when the last holder
// releases it.
func (s *Service) dropRemoval() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removing = max(0, s.removing-1)
	if s.removing == 0 {
		s.removingRun = nil
	}
}

// noteLeftInPlace adds what removing older backups did not do to the
// message of the run that is recorded already, so the record says what
// OwnGit left where it is. The backup itself is saved by now, so a message
// that cannot be saved is logged and the run stands; the write ends with
// the service, since nothing depends on it. After a successful run the note
// keeps its room in the message, so a long message in front of it cannot cut
// it away. After a failed run the failure reason stays whole at the front and
// the note takes only the room left; the full list goes to the server log.
func (s *Service) noteLeftInPlace(ctx context.Context, run *state.BackupRun, left []string) {
	note := "Also, " + strings.Join(left, "; ")
	switch {
	case failedVerification(*run):
		// The failure reason stays whole and first; the notice takes the
		// room that is left, and the log keeps the full list.
		s.logf("backup %s: %s", filepath.Join(run.Destination, run.BackupName), note)
		kind := "older failed backup was"
		if len(left) > 1 {
			kind = "older failed backups were"
		}
		lead := fmt.Sprintf(" Also, %d %s left in place; the first: ", len(left), kind)
		run.Message += state.CutBackupRunMessage(lead+left[0], state.MaxBackupRunMessage-len(run.Message))
	case run.Message == "":
		run.Message = CompleteButLead + " " + strings.Join(left, "; ")
	default:
		run.Message = withNotice(run.Message, note)
	}
	write, cancel := context.WithTimeout(ctx, recordWrite)
	defer cancel()
	if err := s.Store.SetBackupRunMessage(write, run.ID, run.Message); err != nil {
		s.logf("backup %s could not record what removing older backups did: %v", filepath.Join(run.Destination, run.BackupName), err)
	}
}

// noteAliases keeps complete guidance within the run record's remaining space.
// Log the full notice before recording, even if recording later fails.
func (s *Service) noteAliases(run *state.BackupRun, report recovery.CaptureReport) {
	if len(report.AliasBranches) == 0 {
		return
	}
	budget := state.MaxBackupRunMessage - len(run.Message)
	if run.Message != "" {
		budget-- // Keep existing warnings first, separated by a newline.
	}
	var summary string
	if budget > 0 {
		summary = report.AliasNoticeWithin(budget)
	}
	output := filepath.Join(run.Destination, run.BackupName)
	if summary == "" {
		s.logf("backup %s alias notice did not fit in the backup run record; the full list follows", output)
	} else {
		if run.Message != "" {
			run.Message += "\n"
		}
		run.Message += summary
	}
	for _, entry := range strings.Split(report.AliasNotice(), "\n") {
		s.logf("backup %s alias notice: %s", output, entry)
	}
}

// published says whether the folder of run holds the backup it wrote, and
// what kept it from telling.
func (s *Service) published(run state.BackupRun) (bool, string) {
	folder, err := recovery.OpenBackupFolder(run.Destination)
	if err != nil {
		return true, fmt.Sprintf("could not check whether %s was written: %v", run.BackupName, err)
	}
	defer folder.Close()
	backup, err := openOwned(folder, run)
	switch {
	case errors.Is(err, errMissing), errors.Is(err, errNotOwned):
		return false, ""
	case err != nil:
		return true, fmt.Sprintf("could not check whether %s was written: %v", run.BackupName, err)
	}
	backup.Close()
	return true, ""
}

// checkRoom refuses the backup of run when its folder has less free space
// than the newest backup that OwnGit keeps there took, for the repositories
// that still exist. Without such a backup there is nothing to estimate from,
// and a full disk ends the backup while it writes. When that backup cannot
// be read, the backup goes ahead and note says why the room was not
// checked.
func (s *Service) checkRoom(ctx context.Context, run state.BackupRun) (note string, err error) {
	// Only this plan's backups estimate the room its backup needs: another
	// plan that writes the same folder keeps a separate estimate, and a
	// plan whose folder changed compares only backups in the new folder.
	runs, err := s.Store.BackupRunsOfPlan(ctx, run.Plan)
	if err != nil {
		return "", err
	}
	folder, err := recovery.OpenBackupFolder(run.Destination)
	if err != nil {
		return "", err
	}
	defer folder.Close()
	for _, record := range runs {
		if record.ID == run.ID || record.Status == state.BackupRunning || record.Destination != run.Destination || record.BackupName == "" {
			continue
		}
		backup, err := openOwned(folder, record)
		if errors.Is(err, errMissing) || errors.Is(err, errNotOwned) {
			continue
		}
		if err != nil {
			return fmt.Sprintf("the free space was not checked, because the last backup %s could not be read: %v", record.BackupName, err), nil
		}
		defer backup.Close()
		repositories, err := s.Store.Repositories(ctx)
		if err != nil {
			return "", err
		}
		exists := map[string]bool{}
		for _, item := range repositories {
			exists[item.ID] = true
		}
		needed, err := backup.Size(func(id string) bool { return exists[id] })
		if err != nil {
			return fmt.Sprintf("the free space was not checked, because the last backup %s could not be read: %v", record.BackupName, err), nil
		}
		return "", folder.CheckRoom(needed)
	}
	return "", nil
}

// verify rehearses a restore of the backup at output within the limit.
func (s *Service) verify(ctx context.Context, output string) error {
	return s.verifyWith(ctx, func(ctx context.Context, gitPath string) (recovery.Verification, error) {
		return recovery.Verify(ctx, output, "", gitPath)
	})
}

// verifyWith runs the verification rehearse within the limit.
func (s *Service) verifyWith(ctx context.Context, rehearse func(ctx context.Context, gitPath string) (recovery.Verification, error)) error {
	limit := s.VerifyLimit
	if limit == 0 {
		limit = DefaultVerifyLimit
	}
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	result, err := rehearse(bounded, s.Repositories.Git.GitPath)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case bounded.Err() != nil:
		return fmt.Errorf("the verification did not finish within %s", limit)
	case err != nil:
		return err
	case !result.Verified:
		return errors.New(result.Error)
	}
	return nil
}

// A run's folder that holds no backup of that run: errMissing when the
// folder is gone, errNotOwned when it holds another backup or something
// that is no backup.
var (
	errMissing  = errors.New("the backup is missing")
	errNotOwned = errors.New("the folder does not hold the backup that OwnGit wrote there")
)

// openOwned opens the backup that run wrote, held for its removal. The run
// owns its folder only while the folder, not a link, holds the very
// manifest the run wrote, as its SHA-256 recorded with the run shows; no
// other backup, even one made in the same second, has it. A folder that is
// missing gives errMissing, one that holds anything else errNotOwned
// (wrapped, saying why); a folder that cannot be read gives that error.
func openOwned(folder *recovery.BackupFolder, run state.BackupRun) (*recovery.BackupCopy, error) {
	if !validName(run.BackupName) {
		return nil, fmt.Errorf("%w: %q is not a backup name", errNotOwned, run.BackupName)
	}
	if run.ManifestSHA256 == "" {
		return nil, fmt.Errorf("%s: %w: the run recorded no manifest, so OwnGit cannot tell its backup", run.BackupName, errNotOwned)
	}
	backup, err := folder.Open(run.BackupName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errMissing
	case errors.Is(err, recovery.ErrNotABackup):
		return nil, fmt.Errorf("%s: %w: %v", run.BackupName, errNotOwned, err)
	case err != nil:
		return nil, err
	}
	if backup.ManifestSHA256 != run.ManifestSHA256 {
		backup.Close()
		return nil, fmt.Errorf("%s: %w: it holds another backup", run.BackupName, errNotOwned)
	}
	return backup, nil
}

// forget records that the backup of run is gone.
func (s *Service) forget(ctx context.Context, run state.BackupRun) {
	if err := s.Store.ForgetBackup(ctx, run.ID); err != nil {
		s.logf("could not record that backup %s is gone: %v", run.BackupName, err)
	}
}

// validName accepts a folder name that a run writes: a single name in its
// folder, never a path.
func validName(name string) bool {
	return strings.HasPrefix(name, namePrefix) && filepath.Base(name) == name && !strings.ContainsAny(name, `/\:`)
}

// removeOld removes the backups in run's folder beyond the newest keep that
// OwnGit's runs wrote, in the order the runs started, never run's own and
// never the newest verified one, and forgets old run records. A backup that
// failed verification does not count toward keep. When run is one, it is the
// only one that stays and nothing else is removed; when run passed
// verification, none stays; otherwise the newest stays. A backup is
// found by its run record and its manifest and removed through the folder
// that was opened, never through a link, and not when it holds anything
// OwnGit did not write. It returns what it could not do; nothing else is
// touched.
func (s *Service) removeOld(ctx context.Context, run state.BackupRun, keep int) []string {
	// Only runs of the same plan are removal candidates, so two plans that
	// write the same folder never remove each other's backups, even when
	// the folder, the manifest checks and the timestamps match.
	runs, err := s.Store.BackupRunsOfPlan(ctx, run.Plan)
	if err != nil {
		return []string{"older backups were not removed because the backup records could not be read: " + err.Error()}
	}
	folder, err := recovery.OpenBackupFolder(run.Destination)
	if err != nil {
		return []string{"older backups were not removed because the folder could not be opened: " + err.Error()}
	}
	defer folder.Close()
	var problems []string
	kept, verifiedKept, failedKept := 0, false, false
	onlyFailed := failedVerification(run)
	supersedes := run.Verification == state.BackupVerifyPassed
	for _, record := range runs {
		if record.ID == run.ID {
			record = run
		} else if record.Status == state.BackupRunning || record.Destination != run.Destination || record.BackupName == "" {
			continue
		}
		if onlyFailed && record.ID != run.ID && !failedVerification(record) {
			continue
		}
		backup, err := openOwned(folder, record)
		switch {
		case record.ID == run.ID && err != nil:
			problems = append(problems, fmt.Sprintf("the new backup %s could not be opened again, so no older backup was removed: %v", record.BackupName, err))
			return problems
		case errors.Is(err, errMissing):
			s.forget(ctx, record)
			continue
		case errors.Is(err, errNotOwned):
			s.forget(ctx, record)
			problems = append(problems, fmt.Sprintf("%v; it was left in place", err))
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("could not read the older backup %s, so it was left in place: %v", record.BackupName, err))
			continue
		}
		verified := record.Verification == state.BackupVerifyPassed
		if failedVerification(record) {
			if record.ID == run.ID || !onlyFailed && !supersedes && !failedKept {
				failedKept = true
				backup.Close()
				continue
			}
		} else if record.ID == run.ID || kept < keep || verified && !verifiedKept {
			kept++
			verifiedKept = verifiedKept || verified
			backup.Close()
			continue
		}
		if !s.claimRemoval(record.ID) {
			backup.Close()
			problems = append(problems, fmt.Sprintf("the older backup %s was being downloaded, so it was left in place until the next backup", record.BackupName))
			continue
		}
		err = backup.Remove()
		backup.Close()
		s.releaseRemoval(record.ID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("could not remove the older backup %s: %v", record.BackupName, err))
			continue
		}
		s.forget(ctx, record)
	}
	return problems
}
