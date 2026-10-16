package backups

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"owngit/internal/auth"
	"owngit/internal/gitexec"
	"owngit/internal/recovery"
	"owngit/internal/repository"
	"owngit/internal/state"
	"owngit/internal/testfixture"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

type fixture struct {
	store       *state.Store
	manager     *repository.Manager
	service     *Service
	clock       *clock
	destination string
}

// newFixture is a configured OwnGit with a repository that has a commit and
// an empty one, and a backup service that no scheduler drives: the tests
// call tick themselves.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repositoriesRoot := filepath.Join(root, "repositories")
	noErr(t, os.Mkdir(repositoriesRoot, 0o700))
	store, err := state.Open(ctx, filepath.Join(root, "state"))
	noErr(t, err)
	t.Cleanup(func() { _ = store.Close() })
	adminHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	noErr(t, store.CompleteSetup(ctx, repositoriesRoot, "open", "", adminHash, false))
	runner, err := gitexec.New("", filepath.Join(root, "state", "runtime"))
	noErr(t, err)
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoriesRoot}
	for _, id := range []string{"project", "empty"} {
		if _, err := manager.Create(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := manager.Path("project")
	noErr(t, err)
	work := filepath.Join(root, "work")
	git(t, "", "init", "--initial-branch=main", work)
	git(t, work, "config", "user.name", "Backup Test")
	git(t, work, "config", "user.email", "backup@example.invalid")
	noErr(t, os.WriteFile(filepath.Join(work, "file"), []byte("data"), 0o600))
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "data")
	git(t, work, "push", remote, "HEAD:refs/heads/main")

	fake := &clock{now: time.Now().Truncate(time.Second)}
	service := &Service{Store: store, Repositories: manager, Now: fake.Now, Logf: t.Logf}
	service.open(ctx)
	t.Cleanup(func() { noErr(t, service.Stop(context.Background())) })
	return &fixture{store: store, manager: manager, service: service, clock: fake, destination: filepath.Join(root, "backups")}
}

func (f *fixture) configure(t *testing.T, change ScheduleChange) state.BackupSchedule {
	t.Helper()
	if change.Destination == nil {
		if _, configured, err := f.store.BackupSchedule(context.Background()); err != nil || !configured {
			change.Destination = &f.destination
		}
	}
	_, _, after, err := f.service.ChangeSchedule(context.Background(), change)
	noErr(t, err)
	return after
}

// plan returns the default plan as it is saved, the frozen configuration a
// run started now would take.
func (f *fixture) plan(t *testing.T) state.BackupPlan {
	t.Helper()
	plan, configured, err := f.store.BackupPlan(context.Background(), state.DefaultBackupPlanName)
	noErr(t, err)
	if !configured {
		t.Fatal("the default plan is not configured")
	}
	return plan
}

// backUpNow starts a backup, waits for it to end, and returns its record.
// Every backup starts an hour after the one before.
func (f *fixture) backUpNow(t *testing.T) state.BackupRun {
	t.Helper()
	f.clock.advance(time.Hour)
	run, err := f.service.StartNow()
	noErr(t, err)
	f.service.work.Wait()
	return f.run(t, run.ID)
}

func (f *fixture) run(t *testing.T, id string) state.BackupRun {
	t.Helper()
	runs, err := f.store.BackupRuns(context.Background())
	noErr(t, err)
	for _, run := range runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("no run %s", id)
	return state.BackupRun{}
}

// present says whether run's folder holds a backup.
func present(run state.BackupRun) bool {
	folder, err := recovery.OpenBackupFolder(run.Destination)
	if err != nil {
		return false
	}
	defer folder.Close()
	backup, err := folder.Open(run.BackupName)
	if err != nil {
		return false
	}
	backup.Close()
	return true
}

// waitPending waits until the result of the run id waits for the state
// store, whatever else was reported, and how that report is ordered.
func waitPending(t *testing.T, service *Service, id string) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Millisecond) {
		if service.pendingResult(id) != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the result of the run never waited for the state store")
		}
	}
}

// slotHeld says whether a run holds the one slot for removing older
// backups now.
func slotHeld(service *Service) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.removing > 0
}

// seam holds one log line until the test lets it go, so a test can ask
// questions while the product is inside that line. A seam always lets its
// line go when the test ends, so a failing check cannot hold the service.
// wait reports the line and waits; at waits for the line; let lets it go.
type seam struct {
	arrived  chan struct{}
	released chan struct{}
	signal   sync.Once
	letOnce  sync.Once
}

func newSeam() *seam {
	return &seam{arrived: make(chan struct{}), released: make(chan struct{})}
}

func (s *seam) wait() {
	s.signal.Do(func() {
		close(s.arrived)
		<-s.released
	})
}

func (s *seam) at(t *testing.T) {
	t.Helper()
	select {
	case <-s.arrived:
	case <-time.After(time.Minute):
		t.Fatal("the seam was never reached")
	}
}

func (s *seam) let() {
	s.letOnce.Do(func() { close(s.released) })
}

func TestBackUpNowWritesAndVerifiesABackup(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.StartNow(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("backup without a folder: %v", err)
	}
	schedule := f.configure(t, ScheduleChange{})
	if !schedule.Enabled || schedule.Interval != 24*time.Hour || schedule.Keep != 7 || !schedule.Verify {
		t.Fatalf("defaults: %+v", schedule)
	}
	run := f.backUpNow(t)
	if run.Kind != state.BackupRunManual || run.Status != state.BackupSucceeded || run.Verification != state.BackupVerifyPassed || run.Message != "" {
		t.Fatalf("run: %+v", run)
	}
	if !run.HoldKnown || run.LongestHoldRepository == "" || run.LongestHold < 0 {
		t.Fatalf("the run did not record how long Git writes waited: %+v", run)
	}
	if !present(run) || !strings.HasPrefix(run.BackupName, namePrefix) {
		t.Fatalf("backup %s is not there", run.BackupName)
	}
	status, err := f.service.Status(context.Background())
	noErr(t, err)
	if status.Schedule.State != ScheduleOn || status.Running != nil || status.LastRun == nil || status.LastRun.ID != run.ID ||
		status.LastVerified == nil || status.LastVerified.ID != run.ID || status.LastRun.Path == "" || status.NextRun == nil {
		t.Fatalf("status: %+v", status)
	}
}

func TestScheduledBackupsRunOncePerInterval(t *testing.T) {
	f := newFixture(t)
	if wait := f.service.tick(); wait != recheck {
		t.Fatalf("not configured: waits %s", wait)
	}
	f.configure(t, ScheduleChange{})
	// The first scheduled backup is due at once.
	f.service.tick()
	f.service.work.Wait()
	runs, err := f.store.BackupRuns(context.Background())
	noErr(t, err)
	if len(runs) != 1 || runs[0].Kind != state.BackupRunScheduled || runs[0].Status != state.BackupSucceeded {
		t.Fatalf("runs: %+v", runs)
	}
	if !runs[0].StartedAt.Equal(f.clock.Now()) {
		t.Fatalf("the run started at %s, not at the clock's %s", runs[0].StartedAt, f.clock.Now())
	}
	f.clock.advance(23 * time.Hour)
	if wait := f.service.tick(); wait != time.Hour {
		t.Fatalf("an hour before the next backup: waits %s", wait)
	}
	f.clock.advance(time.Hour)
	f.service.tick()
	f.service.work.Wait()
	// Another look at the same instant finds the slot used.
	f.service.tick()
	f.service.work.Wait()
	runs, err = f.store.BackupRuns(context.Background())
	noErr(t, err)
	if len(runs) != 2 || runs[0].Kind != state.BackupRunScheduled || !runs[0].StartedAt.Equal(f.clock.Now()) {
		t.Fatalf("runs: %+v", runs)
	}

	off := false
	f.configure(t, ScheduleChange{Enabled: &off})
	f.clock.advance(48 * time.Hour)
	f.service.tick()
	f.service.work.Wait()
	if runs, _ := f.store.BackupRuns(context.Background()); len(runs) != 2 {
		t.Fatalf("scheduled backups ran while off: %d runs", len(runs))
	}
	status, err := f.service.Status(context.Background())
	noErr(t, err)
	if status.Schedule.State != ScheduleOff || status.NextRun != nil {
		t.Fatalf("status while off: %+v", status)
	}
	// Back up now still works while scheduled backups are off.
	if run := f.backUpNow(t); run.Status != state.BackupSucceeded {
		t.Fatalf("run: %+v", run)
	}
}

func TestSecondBackupIsRefusedWhileOneRuns(t *testing.T) {
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	other := state.BackupRun{ID: strings.Repeat("a", 32), Kind: state.BackupRunManual, Destination: f.destination, BackupName: namePrefix + "other", StartedAt: f.clock.Now()}
	noErr(t, f.store.StartBackupRun(context.Background(), other))
	if _, err := f.service.StartNow(); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("second backup: %v", err)
	}
	f.service.tick()
	f.service.work.Wait()
	if runs, _ := f.store.BackupRuns(context.Background()); len(runs) != 1 {
		t.Fatalf("a scheduled backup started beside a running one: %+v", runs)
	}
}

// A run that a stopped process left running becomes interrupted, never a
// success, and its slot is used, so the schedule does not repeat it.
func TestRestartRecordsARunningBackupAsInterrupted(t *testing.T) {
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	left := state.BackupRun{ID: strings.Repeat("b", 32), Kind: state.BackupRunScheduled, Destination: f.destination, BackupName: namePrefix + "left", StartedAt: f.clock.Now()}
	noErr(t, f.store.StartBackupRun(context.Background(), left))

	restarted := &Service{Store: f.store, Repositories: f.manager, Now: f.clock.Now, Logf: t.Logf}
	noErr(t, restarted.Start(context.Background()))
	noErr(t, restarted.Stop(context.Background()))
	run := f.run(t, left.ID)
	if run.Status != state.BackupInterrupted || run.Verification != state.BackupVerifyNotRun || run.Message != InterruptedMessage || run.FinishedAt.IsZero() {
		t.Fatalf("left run: %+v", run)
	}
	timings, err := f.service.planTimings(context.Background())
	noErr(t, err)
	if len(timings) != 1 || !timings[0].due.Equal(left.StartedAt.Add(24 * time.Hour)) {
		t.Fatalf("next scheduled backup %+v, want one interval after the interrupted one", timings)
	}
}

// A backup that OwnGit stops while it runs ends interrupted, and nothing
// is published.
func TestStoppedBackupIsInterrupted(t *testing.T) {
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	run := state.BackupRun{ID: strings.Repeat("c", 32), Plan: state.DefaultBackupPlanName, Kind: state.BackupRunManual, Status: state.BackupRunning, Destination: f.destination,
		BackupName: namePrefix + "stopped", Verification: state.BackupVerifyNotRun, StartedAt: f.clock.Now()}
	noErr(t, f.store.StartBackupRun(context.Background(), run))
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	f.service.execute(stopped, run, f.plan(t))
	run = f.run(t, run.ID)
	if run.Status != state.BackupInterrupted || present(run) {
		t.Fatalf("stopped run: %+v, published %v", run, present(run))
	}
}

// The final record of a stopped backup is written by the stop's deadline and
// not after it; a record that missed the deadline is left to the next start.
func TestStoppedBackupRecordsOnlyBeforeTheStopDeadline(t *testing.T) {
	for _, test := range []struct {
		name   string
		stopBy time.Duration
		want   string
	}{
		{"before the deadline", time.Hour, state.BackupInterrupted},
		{"after the deadline", -time.Second, state.BackupRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.configure(t, ScheduleChange{})
			run := state.BackupRun{ID: strings.Repeat("d", 32), Plan: state.DefaultBackupPlanName, Kind: state.BackupRunManual, Status: state.BackupRunning, Destination: f.destination,
				BackupName: namePrefix + "deadline", Verification: state.BackupVerifyNotRun, StartedAt: f.clock.Now()}
			noErr(t, f.store.StartBackupRun(context.Background(), run))
			f.service.StopBy = func() time.Time { return time.Now().Add(test.stopBy) }
			stopped, cancel := context.WithCancel(context.Background())
			cancel()
			f.service.execute(stopped, run, f.plan(t))
			if got := f.run(t, run.ID).Status; got != test.want {
				t.Fatalf("status %s, want %s", got, test.want)
			}
		})
	}
}

func TestVerificationPastItsLimitFails(t *testing.T) {
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	f.service.VerifyLimit = time.Nanosecond
	run := f.backUpNow(t)
	if run.Status != state.BackupFailed || run.Verification != state.BackupVerifyFailed || !strings.Contains(run.Message, "did not finish within 1ns") {
		t.Fatalf("run: %+v", run)
	}
	status, err := f.service.Status(context.Background())
	noErr(t, err)
	if status.LastVerified != nil || status.LastRun.Status != state.BackupFailed {
		t.Fatalf("status: %+v", status)
	}
}

func TestRetentionRemovesOnlyOlderBackupsOwnGitWrote(t *testing.T) {
	f := newFixture(t)
	keep := 2
	f.configure(t, ScheduleChange{Keep: &keep})
	first := f.backUpNow(t)
	// A folder named like a backup and a file that OwnGit did not write.
	foreign := filepath.Join(f.destination, namePrefix+"foreign")
	noErr(t, os.CopyFS(foreign, os.DirFS(filepath.Join(f.destination, first.BackupName))))
	noErr(t, os.WriteFile(filepath.Join(f.destination, "notes.txt"), []byte("mine"), 0o600))
	second := f.backUpNow(t)
	// The owner put a file into the second backup, so OwnGit will not
	// remove it and says so.
	added := filepath.Join(f.destination, second.BackupName, "readme.txt")
	noErr(t, os.WriteFile(added, []byte("mine"), 0o600))
	third := f.backUpNow(t)
	if !present(third) || !present(second) || present(first) || third.Message != "" {
		t.Fatalf("after the third backup: first %v second %v third %v, %q", present(first), present(second), present(third), third.Message)
	}
	fourth := f.backUpNow(t)
	if fourth.Status != state.BackupSucceeded || !strings.Contains(fourth.Message, "could not remove the older backup") ||
		!strings.Contains(fourth.Message, "readme.txt") || !present(second) {
		t.Fatalf("fourth: %+v, second present %v", fourth, present(second))
	}
	if _, err := os.Stat(added); err != nil {
		t.Fatalf("the owner's file is gone: %v", err)
	}
	for _, path := range []string{foreign, filepath.Join(f.destination, "notes.txt")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s is gone: %v", path, err)
		}
	}
}

func TestRetentionKeepsTheLastVerifiedBackup(t *testing.T) {
	f := newFixture(t)
	keep := 1
	f.configure(t, ScheduleChange{Keep: &keep})
	verified := f.backUpNow(t)
	off := false
	f.configure(t, ScheduleChange{Verify: &off})
	second := f.backUpNow(t)
	third := f.backUpNow(t)
	if second.Verification != state.BackupVerifyNotRun || third.Status != state.BackupSucceeded {
		t.Fatalf("unverified runs: %+v %+v", second, third)
	}
	if !present(verified) || present(second) || !present(third) {
		t.Fatalf("verified %v second %v third %v", present(verified), present(second), present(third))
	}
	// A backup that fails verification does not count toward keep and
	// removes no verified backup.
	on := true
	f.configure(t, ScheduleChange{Verify: &on})
	f.service.VerifyLimit = time.Nanosecond
	failed := f.backUpNow(t)
	if failed.Status != state.BackupFailed || !present(verified) || !present(third) || !present(failed) {
		t.Fatalf("after a failed verification: %+v, verified %v third %v", failed, present(verified), present(third))
	}
	status, err := f.service.Status(context.Background())
	noErr(t, err)
	if status.LastVerified == nil || status.LastVerified.ID != verified.ID {
		t.Fatalf("last verified: %+v", status.LastVerified)
	}
	// Only the newest failed backup stays. One that cannot be removed is
	// reported in a short notice that leaves the failure reason whole.
	added := filepath.Join(f.destination, failed.BackupName, "readme.txt")
	noErr(t, os.WriteFile(added, []byte("mine"), 0o600))
	failed2 := f.backUpNow(t)
	if !present(failed) || !present(failed2) || !strings.HasPrefix(failed2.Message, failed.Message) || !strings.Contains(failed2.Message, "left in place") {
		t.Fatalf("failed2 failure: failed %v failed2 %v: %q", present(failed), present(failed2), failed2.Message)
	}
	noErr(t, os.Remove(added))
	failed3 := f.backUpNow(t)
	if present(failed) || present(failed2) || !present(failed3) || !present(verified) || f.run(t, failed.ID).BackupName != "" {
		t.Fatalf("failed3 failure: failed %v failed2 %v failed3 %v verified %v", present(failed), present(failed2), present(failed3), present(verified))
	}
	// A verified backup supersedes the earlier failure.
	f.service.VerifyLimit = 0
	last := f.backUpNow(t)
	if last.Status != state.BackupSucceeded || present(failed3) || f.run(t, failed3.ID).BackupName != "" || !present(last) {
		t.Fatalf("after a verified backup: %+v, failed3 %v", last, present(failed3))
	}
}

func TestScheduleRefusesAFolderInsideOwnGitStorage(t *testing.T) {
	f := newFixture(t)
	inside := filepath.Join(f.manager.RepositoryRoot(), "backups")
	for _, destination := range []string{"relative/backups", inside, filepath.Join(f.store.Dir(), "backups")} {
		_, _, _, err := f.service.ChangeSchedule(context.Background(), ScheduleChange{Destination: &destination})
		var refused *ChangeError
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v", destination, err)
		}
	}
	if _, configured, _ := f.store.BackupSchedule(context.Background()); configured {
		t.Fatal("a refused folder was saved")
	}
	keep := 3
	if _, _, _, err := f.service.ChangeSchedule(context.Background(), ScheduleChange{Keep: &keep}); err == nil {
		t.Fatal("a schedule without a folder was saved")
	}
}

func git(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = testfixture.GitEnvironment(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The scheduler of a started service makes the first backup as soon as a
// folder is set, and a stop ends it.
func TestSchedulerStartsTheFirstBackupWhenAFolderIsSet(t *testing.T) {
	if testing.Short() {
		t.Skip("creates and verifies a real scheduled backup")
	}
	f := newFixture(t)
	ended := make(chan struct{}, 1)
	running := &Service{Store: f.store, Repositories: f.manager, Now: f.clock.Now, Logf: func(string, ...any) {
		select {
		case ended <- struct{}{}:
		default:
		}
	}}
	noErr(t, running.Start(context.Background()))
	defer func() { noErr(t, running.Stop(context.Background())) }()
	_, _, _, err := running.ChangeSchedule(context.Background(), ScheduleChange{Destination: &f.destination})
	noErr(t, err)
	select {
	case <-ended:
	case <-time.After(2 * time.Minute):
		t.Fatal("no scheduled backup ended")
	}
	runs, err := f.store.BackupRuns(context.Background())
	noErr(t, err)
	if len(runs) != 1 || runs[0].Kind != state.BackupRunScheduled || runs[0].Status != state.BackupSucceeded || runs[0].Verification != state.BackupVerifyPassed {
		t.Fatalf("runs: %+v", runs)
	}
}

// runAt starts and executes a manual backup with the given ID, as start
// does, and returns its record.
func (f *fixture) runAt(t *testing.T, id string, started time.Time) state.BackupRun {
	t.Helper()
	plan, configured, err := f.store.BackupPlan(context.Background(), state.DefaultBackupPlanName)
	noErr(t, err)
	if !configured {
		t.Fatal("the default plan is not configured")
	}
	run := state.BackupRun{ID: id, Plan: plan.Name, Kind: state.BackupRunManual, Status: state.BackupRunning, Destination: plan.Destination,
		BackupName: namePrefix + started.UTC().Format("20060102-150405") + "-" + id[:8], Verification: state.BackupVerifyNotRun, StartedAt: started}
	noErr(t, f.store.StartBackupRun(context.Background(), run))
	f.service.execute(context.Background(), run, plan)
	return f.run(t, id)
}

// Two backups started in the same second keep the newer one: the order is
// the order the runs started, not their random IDs.
func TestRetentionKeepsTheNewerOfTwoBackupsInTheSameSecond(t *testing.T) {
	f := newFixture(t)
	keep, off := 1, false
	f.configure(t, ScheduleChange{Keep: &keep, Verify: &off})
	started := time.Now().Truncate(time.Second)
	older := f.runAt(t, strings.Repeat("f", 32), started)
	newer := f.runAt(t, strings.Repeat("0", 32), started)
	if newer.Status != state.BackupSucceeded || !present(newer) || present(state.BackupRun{Destination: f.destination, BackupName: namePrefix + started.UTC().Format("20060102-150405") + "-ffffffff"}) {
		t.Fatalf("newer %+v present %v; older %+v", newer, present(newer), older)
	}
	status, err := f.service.Status(context.Background())
	noErr(t, err)
	if status.LastRun == nil || status.LastRun.ID != newer.ID {
		t.Fatalf("last run: %+v", status.LastRun)
	}
}

// A backup folder replaced by a link is not followed: the backup the link
// leads to stays, and the run says what it left.
func TestRetentionDoesNotFollowALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need a privilege on Windows")
	}
	f := newFixture(t)
	keep, off := 1, false
	f.configure(t, ScheduleChange{Keep: &keep, Verify: &off})
	first := f.backUpNow(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	noErr(t, os.Rename(filepath.Join(f.destination, first.BackupName), elsewhere))
	noErr(t, os.Symlink(elsewhere, filepath.Join(f.destination, first.BackupName)))
	second := f.backUpNow(t)
	if second.Status != state.BackupSucceeded || !strings.Contains(second.Message, first.BackupName) || !strings.Contains(second.Message, "left in place") {
		t.Fatalf("second: %+v", second)
	}
	for _, name := range []string{"manifest.json", filepath.Join("repositories", "project.bundle")} {
		if _, err := os.Stat(filepath.Join(elsewhere, name)); err != nil {
			t.Fatalf("the backup behind the link lost %s: %v", name, err)
		}
	}
	if run := f.run(t, first.ID); run.BackupName != "" {
		t.Fatalf("the run still claims the linked folder: %+v", run)
	}
}

// A file added to a backup is never removed, even one named like a bundle.
func TestRetentionLeavesAnAddedBundleFile(t *testing.T) {
	f := newFixture(t)
	keep, off := 1, false
	f.configure(t, ScheduleChange{Keep: &keep, Verify: &off})
	first := f.backUpNow(t)
	added := filepath.Join(f.destination, first.BackupName, "repositories", "foreign.bundle")
	noErr(t, os.WriteFile(added, []byte("mine"), 0o600))
	second := f.backUpNow(t)
	if !strings.Contains(second.Message, "foreign.bundle") || !present(first) {
		t.Fatalf("second: %+v, first present %v", second, present(first))
	}
	if content, err := os.ReadFile(added); err != nil || string(content) != "mine" {
		t.Fatalf("the added file: %q %v", content, err)
	}
}

// A backup whose manifest cannot be read is reported and left, never taken
// for absent, and the free space goes unchecked with that reason.
func TestRetentionReportsAnUnreadableBackup(t *testing.T) {
	f := newFixture(t)
	keep := 1
	f.configure(t, ScheduleChange{Keep: &keep})
	first := f.backUpNow(t)
	noErr(t, os.WriteFile(filepath.Join(f.destination, first.BackupName, "manifest.json"), []byte("{\"format\":"), 0o600))
	off := false
	f.configure(t, ScheduleChange{Verify: &off})
	second := f.backUpNow(t)
	if second.Status != state.BackupSucceeded || !strings.Contains(second.Message, "could not read the older backup "+first.BackupName) ||
		!strings.Contains(second.Message, "free space was not checked") {
		t.Fatalf("second: %+v", second)
	}
	if _, err := os.Stat(filepath.Join(f.destination, first.BackupName, "repositories", "project.bundle")); err != nil {
		t.Fatalf("the unreadable backup lost its bundle: %v", err)
	}
	if run := f.run(t, first.ID); run.BackupName == "" {
		t.Fatal("the unreadable backup was forgotten")
	}
}

// A backup that finished while the state store refused its final record
// says so where the page names the running backup: it stands in as the
// running one, which keeps another backup from starting, its message gives
// the result and says the record is not saved yet, the last runs stay the
// ones OwnGit has recorded, and the result is recorded when the state
// store takes writes again, so the backup it wrote stays OwnGit's and the
// next backup removes it like any other.
func TestFinishedBackupRecordsItsResultWhenTheStateStoreRefusesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("waits a second for the retried final record")
	}
	f := newFixture(t)
	off, keep := false, 1
	f.configure(t, ScheduleChange{Verify: &off, Keep: &keep})
	ctx := context.Background()
	// A state store that refuses the final record, as a full or read-only
	// one does, made through the seam for breaking one write on purpose.
	noErr(t, f.store.Exec(ctx, `CREATE TRIGGER refuse_backup_record BEFORE UPDATE ON backup_runs BEGIN SELECT RAISE(ABORT,'recording refused'); END`))
	refused := make(chan string, 8)
	f.service.Logf = func(format string, arguments ...any) {
		line := fmt.Sprintf(format, arguments...)
		if strings.Contains(line, "could not be recorded") {
			select {
			case refused <- line:
			default:
			}
		}
	}
	run, err := f.service.StartNow()
	noErr(t, err)
	// The result is held, whatever order holding it and reporting it happen
	// in.
	waitPending(t, f.service, run.ID)
	select {
	case <-refused:
	case <-time.After(time.Minute):
		t.Fatal("the state store was refused the result without a report")
	}
	// The run has finished and its record is not saved, so it stands in as
	// the running backup, and the last runs are still the ones recorded.
	status, err := f.service.Status(ctx)
	noErr(t, err)
	if status.Running == nil || status.Running.ID != run.ID || status.Running.Status != state.BackupRunning ||
		status.Running.FinishedAt != nil || status.LastRun != nil || status.LastVerified != nil ||
		!strings.Contains(status.Running.Message, "could not be saved yet") || !strings.Contains(status.Running.Message, state.BackupSucceeded) {
		t.Fatalf("status while the result waits: %+v", status)
	}
	if views, err := f.service.Runs(ctx); err != nil || len(views) != 1 || views[0].Status != state.BackupRunning || views[0].Message != status.Running.Message {
		t.Fatalf("runs while the result waits: %+v, %v", views, err)
	}
	if summary := Summarize(status); summary.LastRun != nil || summary.LastVerifiedAt != nil {
		t.Fatalf("summary while the result waits: %+v", summary)
	}
	if _, err := f.service.StartNow(); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a backup started beside the finished run: %v", err)
	}
	// The state store takes records again: the result is recorded without a
	// restart, and the run still owns the folder it wrote.
	noErr(t, f.store.Exec(ctx, `DROP TRIGGER refuse_backup_record`))
	f.service.work.Wait()
	settled := f.run(t, run.ID)
	if settled.Status != state.BackupSucceeded || settled.Verification != state.BackupVerifyNotRun ||
		settled.BackupName != run.BackupName || settled.ManifestSHA256 == "" || settled.FinishedAt.IsZero() {
		t.Fatalf("settled run: %+v", settled)
	}
	folder, err := recovery.OpenBackupFolder(f.destination)
	noErr(t, err)
	defer folder.Close()
	backup, err := openOwned(folder, settled)
	if err != nil {
		t.Fatalf("the settled run does not own its backup: %v", err)
	}
	backup.Close()
	// The scheduler picks up again, and the backup whose record was delayed
	// is removed like any other.
	f.service.tick()
	f.service.work.Wait()
	runs, err := f.store.BackupRuns(ctx)
	noErr(t, err)
	if len(runs) != 2 || runs[0].Kind != state.BackupRunScheduled || runs[0].Status != state.BackupSucceeded || runs[0].Message != "" {
		t.Fatalf("runs after the result was recorded: %+v", runs)
	}
	if present(settled) {
		t.Fatalf("the backup %s whose record was delayed was left behind", settled.BackupName)
	}
}

// Removing an older backup waits for the record that owns the new one: a
// stop while the new result waits for the state store leaves the older
// backup, its folder and the record that owns it as they were, and the
// removal happens once the new record is saved.
func TestRemovingAnOlderBackupWaitsForTheNewRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("waits a second for the retried final record")
	}
	ctx := context.Background()
	// A state store that refuses the final record, as a full or read-only
	// one does, made through the seam for breaking one write on purpose.
	refuse := func(t *testing.T, f *fixture) {
		t.Helper()
		noErr(t, f.store.Exec(ctx, `CREATE TRIGGER refuse_backup_record BEFORE UPDATE ON backup_runs BEGIN SELECT RAISE(ABORT,'recording refused'); END`))
	}
	configured := func(t *testing.T) *fixture {
		t.Helper()
		f := newFixture(t)
		enabled, off, keep := false, false, 1
		f.configure(t, ScheduleChange{Enabled: &enabled, Verify: &off, Keep: &keep})
		return f
	}

	t.Run("the result waits and OwnGit stops", func(t *testing.T) {
		f := configured(t)
		first := f.backUpNow(t)
		refuse(t, f)
		second, err := f.service.StartNow()
		noErr(t, err)
		waitPending(t, f.service, second.ID)
		// The new backup wrote its folder, but nothing says OwnGit made it,
		// so the older backup is still the one OwnGit keeps.
		if !present(first) {
			t.Fatalf("the backup %s was removed before the new record was saved", first.BackupName)
		}
		noErr(t, f.service.Stop(ctx))
		// The next process records the waiting run as interrupted, and the
		// older backup is still its own: folder, record and manifest.
		noErr(t, f.store.Exec(ctx, `DROP TRIGGER refuse_backup_record`))
		restarted := &Service{Store: f.store, Repositories: f.manager, Now: f.clock.Now, Logf: t.Logf}
		noErr(t, restarted.Start(ctx))
		noErr(t, restarted.Stop(ctx))
		recorded := f.run(t, first.ID)
		if recorded.BackupName != first.BackupName || recorded.ManifestSHA256 == "" || !present(first) {
			t.Fatalf("the older backup after a restart: %+v, present %v", recorded, present(first))
		}
		folder, err := recovery.OpenBackupFolder(f.destination)
		noErr(t, err)
		defer folder.Close()
		backup, err := openOwned(folder, recorded)
		if err != nil {
			t.Fatalf("the older backup lost its owner: %v", err)
		}
		backup.Close()
		if waited := f.run(t, second.ID); waited.Status != state.BackupInterrupted || waited.ManifestSHA256 != "" {
			t.Fatalf("the run whose record waited: %+v", waited)
		}
	})

	t.Run("the result is saved", func(t *testing.T) {
		f := configured(t)
		first := f.backUpNow(t)
		refuse(t, f)
		second, err := f.service.StartNow()
		noErr(t, err)
		waitPending(t, f.service, second.ID)
		if !present(first) {
			t.Fatalf("the backup %s was removed before the new record was saved", first.BackupName)
		}
		// The state store takes records again: the new record is saved, and
		// only then is the older backup removed.
		noErr(t, f.store.Exec(ctx, `DROP TRIGGER refuse_backup_record`))
		f.service.work.Wait()
		if present(first) {
			t.Fatalf("the backup %s was kept although the record of its replacement gives it up", first.BackupName)
		}
		settled := f.run(t, second.ID)
		if settled.Status != state.BackupSucceeded || settled.ManifestSHA256 == "" {
			t.Fatalf("the run whose record waited: %+v", settled)
		}
		if run := f.run(t, first.ID); run.BackupName != "" {
			t.Fatalf("the run of the removed backup still claims it: %+v", run)
		}
	})
}

// A stop that finds a result waiting for the state store makes one last
// attempt to record it, so a state store that takes writes again just as
// OwnGit stops does not leave a finished backup recorded as one that never
// ended.
func TestStoppingRecordsAWaitingResultOnceMore(t *testing.T) {
	f := newFixture(t)
	off, keep := false, 1
	f.configure(t, ScheduleChange{Verify: &off, Keep: &keep})
	ctx := context.Background()
	noErr(t, f.store.Exec(ctx, `CREATE TRIGGER refuse_backup_record BEFORE UPDATE ON backup_runs BEGIN SELECT RAISE(ABORT,'recording refused'); END`))
	run, err := f.service.StartNow()
	noErr(t, err)
	waitPending(t, f.service, run.ID)
	// The state store takes writes again, and OwnGit stops before another
	// try is due.
	noErr(t, f.store.Exec(ctx, `DROP TRIGGER refuse_backup_record`))
	noErr(t, f.service.Stop(ctx))
	settled := f.run(t, run.ID)
	if settled.Status != state.BackupSucceeded || settled.BackupName != run.BackupName || settled.ManifestSHA256 == "" || settled.FinishedAt.IsZero() {
		t.Fatalf("the result a stop recorded: %+v", settled)
	}
}

// The waiting message keeps valid text inside the bytes a stored record
// holds, even when the cut falls inside a multi-byte character of the run's
// own message, because the cut walks back to a character boundary.
func TestPendingMessageKeepsValidTextAtItsBound(t *testing.T) {
	notice := pendingMessage(state.BackupSucceeded, "")
	if len(notice) > state.MaxBackupRunMessage/2 {
		t.Fatalf("the notice takes %d bytes of %d", len(notice), state.MaxBackupRunMessage)
	}
	// One byte more than the room the message has, so the cut falls on the
	// second byte of a three-byte character.
	room := state.MaxBackupRunMessage - len(notice) - 1
	message := pendingMessage(state.BackupSucceeded, strings.Repeat("a", room-1)+"한글")
	if !utf8.ValidString(message) {
		t.Fatalf("the waiting message is not valid text: %q", message)
	}
	if len(message) > state.MaxBackupRunMessage {
		t.Fatalf("the waiting message is %d bytes", len(message))
	}
	if !strings.HasPrefix(message, strings.Repeat("a", room-1)) || !strings.HasSuffix(message, notice) {
		t.Fatalf("the waiting message lost its text or its notice: %q", message)
	}
}

// Removing an older backup holds the one slot from before the record of the
// run that removes it until every folder and the message are done: a
// backup, a verification or an upload starts nothing in that time, and the
// run stands in as the running one, so the page and the refusal agree.
func TestRemovingOlderBackupsHoldsTheOneSlot(t *testing.T) {
	f := newFixture(t)
	off, keep := false, 1
	f.configure(t, ScheduleChange{Verify: &off, Keep: &keep})
	ctx := context.Background()
	first := f.backUpNow(t)
	// A state store that refuses the final record first, as one that is full
	// does, so the test can ask while the record is being written.
	noErr(t, f.store.Exec(ctx, `CREATE TRIGGER refuse_backup_record BEFORE UPDATE ON backup_runs WHEN NEW.status <> 'running' AND NEW.backup_name <> '' BEGIN SELECT RAISE(ABORT,'recording refused'); END`))
	// A state store that refuses to forget the removed run, as one that
	// cannot write does, so the removal reports it from the removal pass,
	// where the test holds it.
	noErr(t, f.store.Exec(ctx, `CREATE TRIGGER keep_backup_name BEFORE UPDATE OF backup_name ON backup_runs WHEN OLD.backup_name <> '' AND NEW.backup_name = '' BEGIN SELECT RAISE(ABORT,'keep the name'); END`))
	recorded, removed := newSeam(), newSeam()
	defer recorded.let()
	defer removed.let()
	f.service.Logf = func(format string, arguments ...any) {
		t.Logf(format, arguments...)
		line := fmt.Sprintf(format, arguments...)
		if strings.Contains(line, "could not be recorded") {
			recorded.wait()
		}
		if strings.Contains(line, "could not record that backup") {
			removed.wait()
		}
	}
	second, err := f.service.StartNow()
	noErr(t, err)
	// While the record is written, the slot is held already: nothing can
	// start between the saved record and the removal that follows it.
	recorded.at(t)
	if !slotHeld(f.service) {
		t.Fatal("the one slot was free while the final record was written")
	}
	if _, err := f.service.StartNow(); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a backup started while the record was written: %v", err)
	}
	if _, err := f.service.StartCheck(ctx, first.ID); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a verification started while the record was written: %v", err)
	}
	// The state store takes records again: the retry saves the record, and
	// the removal follows it.
	noErr(t, f.store.Exec(ctx, `DROP TRIGGER refuse_backup_record`))
	recorded.let()
	removed.at(t)
	// The record of the second run is saved and the older backup is being
	// removed: nothing else may look at those folders yet, and both views
	// name the run that holds the slot.
	if _, err := f.service.StartNow(); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a backup started while older backups were removed: %v", err)
	}
	if _, err := f.service.StartCheck(ctx, second.ID); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a verification started while older backups were removed: %v", err)
	}
	status, err := f.service.Status(ctx)
	noErr(t, err)
	if status.Running == nil || status.Running.ID != second.ID || status.Running.Status != state.BackupRunning ||
		!strings.Contains(status.Running.Message, "removing the older backups") {
		t.Fatalf("status while older backups are removed: %+v", status)
	}
	if views, err := f.service.Runs(ctx); err != nil || len(views) != 2 || views[0].ID != second.ID || views[0].Status != state.BackupRunning {
		t.Fatalf("runs while older backups are removed: %+v, %v", views, err)
	}
	removed.let()
	f.service.work.Wait()
	if present(first) {
		t.Fatalf("the older backup %s was not removed", first.BackupName)
	}
	settled := f.run(t, second.ID)
	if settled.Status != state.BackupSucceeded || settled.ManifestSHA256 == "" || !present(settled) {
		t.Fatalf("the run that removed it: %+v", settled)
	}
	// The slot is free again, and the run is finished in both views.
	if slotHeld(f.service) {
		t.Fatal("the one slot was still held after the removal")
	}
	after, err := f.service.Status(ctx)
	noErr(t, err)
	if after.Running != nil || after.LastRun == nil || after.LastRun.ID != second.ID || after.LastRun.Status != state.BackupSucceeded {
		t.Fatalf("status after the removal: %+v", after)
	}
}

// The one slot counts its holders: a run that finishes removing its older
// backups cannot release the slot of one that still holds it.
func TestTheRemovalSlotCountsEveryHolder(t *testing.T) {
	f := newFixture(t)
	off, keep := false, 1
	f.configure(t, ScheduleChange{Verify: &off, Keep: &keep})
	ctx := context.Background()
	first := f.backUpNow(t)
	second := f.backUpNow(t)
	another, other := f.run(t, first.ID), f.run(t, second.ID)
	f.service.holdRemoval(&another)
	f.service.holdRemoval(&other)
	f.service.dropRemoval()
	if _, err := f.service.StartNow(); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a backup started after the first holder let go: %v", err)
	}
	if _, err := f.service.StartCheck(ctx, second.ID); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("a verification started after the first holder let go: %v", err)
	}
	f.service.dropRemoval()
	if _, err := f.service.StartNow(); err != nil {
		t.Fatalf("the slot was not free after every holder let go: %v", err)
	}
	f.service.work.Wait()
}

// A run takes the one slot before its final record is written. Until that
// record is saved, the stored run is still running: both views show it as
// it is stored, never as a last run without a finish time, and the summary
// general access reads still answers.
func TestARunHoldingTheSlotBeforeItsRecordIsSavedStaysRunning(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	run := state.BackupRun{ID: fmt.Sprintf("%032x", 7), Kind: state.BackupRunManual, Status: state.BackupRunning, Destination: f.destination,
		BackupName: "owngit-backup-x", Verification: state.BackupVerifyNotRun, StartedAt: f.clock.Now()}
	noErr(t, f.store.StartBackupRun(ctx, run))
	finished := run
	finished.Status, finished.FinishedAt = state.BackupSucceeded, f.clock.Now()
	f.service.holdRemoval(&finished)
	defer f.service.dropRemoval()
	status, err := f.service.Status(ctx)
	noErr(t, err)
	if status.LastRun != nil || status.Running == nil || status.Running.ID != run.ID || strings.Contains(status.Running.Message, "removing") {
		t.Fatalf("status before the record is saved: %+v", status)
	}
	if summary := Summarize(status); summary.LastRun != nil {
		t.Fatalf("summary before the record is saved: %+v", summary)
	}
	if views, err := f.service.Runs(ctx); err != nil || len(views) != 1 || views[0].Status != state.BackupRunning || views[0].FinishedAt != nil ||
		strings.Contains(views[0].Message, "removing") {
		t.Fatalf("runs before the record is saved: %+v, %v", views, err)
	}
}

// What removing older backups could not do keeps its place in a recorded
// run's message, also when the run's own message fills the bytes a record
// holds.
func TestTheMessageOfWhatRemovalLeftInPlaceKeepsItsPlace(t *testing.T) {
	f := newFixture(t)
	off := false
	f.configure(t, ScheduleChange{Verify: &off})
	run := f.backUpNow(t)
	run.Message = strings.Repeat("alias notice ", 40)
	if len(run.Message) < state.MaxBackupRunMessage {
		t.Fatalf("the run's own message is only %d bytes", len(run.Message))
	}
	left := []string{"could not remove the older backup owngit-backup-old: it was left in place"}
	f.service.noteLeftInPlace(context.Background(), &run, left)
	settled := f.run(t, run.ID)
	if !strings.Contains(settled.Message, "left in place") {
		t.Fatalf("the record lost what removal left in place: %q", settled.Message)
	}
	if len(settled.Message) > state.MaxBackupRunMessage {
		t.Fatalf("the recorded message is %d bytes", len(settled.Message))
	}
	if !strings.HasPrefix(settled.Message, "alias notice ") {
		t.Fatalf("the record lost the message of the run: %q", settled.Message)
	}
}

// A stop that finds a record attempt running ends it, and leaves one
// attempt the stop does not end, so the record work OwnGit owns while it
// stops stays inside the budget its caller gives Stop. The control where
// that last attempt succeeds is TestStoppingRecordsAWaitingResultOnceMore.
func TestStoppingIsBoundedWhileARecordAttemptIsBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the bound of the one attempt a stop allows")
	}
	f := newFixture(t)
	off, keep := false, 1
	f.configure(t, ScheduleChange{Verify: &off, Keep: &keep})
	ctx := context.Background()
	// A state store whose record write does not finish on its own, as a
	// stalled one does, made through the seam for breaking one write on
	// purpose.
	noErr(t, f.store.Exec(ctx, `CREATE TRIGGER stall_backup_record BEFORE UPDATE ON backup_runs BEGIN SELECT (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT sum(x) FROM c); END`))
	// The clock holds the run just before it records its result, so the test
	// can order the stop after the attempt started.
	var calls atomic.Int64
	atResult, resume := make(chan struct{}), make(chan struct{})
	f.service.Now = func() time.Time {
		if calls.Add(1) == 2 {
			close(atResult)
			<-resume
		}
		return f.clock.Now()
	}
	run, err := f.service.StartNow()
	noErr(t, err)
	select {
	case <-atResult:
	case <-time.After(time.Minute):
		t.Fatal("the backup never came to record its result")
	}
	close(resume)
	// Wait until the attempt holds the state store's one connection, so the
	// stop meets an attempt that is running.
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Millisecond) {
		probe, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		_, busyErr := f.store.TableRowCount(probe, "backup_runs")
		cancel()
		if busyErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the record attempt never took the state store")
		}
	}
	stop, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	stopErr := f.service.Stop(stop)
	elapsed := time.Since(started)
	if stopErr != nil {
		t.Fatalf("OwnGit did not stop inside the caller's budget: %v after %s", stopErr, elapsed)
	}
	if elapsed > 14*time.Second {
		t.Fatalf("the stop held the record work for %s, more than the one attempt it allows", elapsed)
	}
	if elapsed < 9*time.Second {
		t.Fatalf("the stop ended after %s, so the one attempt did not run to its bound", elapsed)
	}
	settled := f.run(t, run.ID)
	if settled.Status != state.BackupRunning {
		t.Fatalf("the blocked state store recorded the run: %+v", settled)
	}
}

// Status and its summary come from OwnGit's records alone, so a backup
// folder that is gone, or would hang, does not hold them up.
func TestStatusReadsNoBackupFolder(t *testing.T) {
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	verified := f.backUpNow(t)
	noErr(t, os.Rename(f.destination, f.destination+"-away"))
	noErr(t, os.WriteFile(f.destination, []byte("not a folder"), 0o600))
	status, err := f.service.Status(context.Background())
	noErr(t, err)
	summary := Summarize(status)
	if status.LastVerified == nil || status.LastVerified.ID != verified.ID || summary.LastVerifiedAt == nil || summary.LastRun == nil {
		t.Fatalf("status %+v summary %+v", status, summary)
	}
}

// A run owns its folder only while it holds the very manifest the run
// wrote.
func TestARunOwnsOnlyTheManifestItWrote(t *testing.T) {
	f := newFixture(t)
	f.configure(t, ScheduleChange{})
	run := f.backUpNow(t)
	folder, err := recovery.OpenBackupFolder(f.destination)
	noErr(t, err)
	defer folder.Close()
	backup, err := openOwned(folder, run)
	noErr(t, err)
	backup.Close()
	for _, digest := range []string{"", strings.Repeat("0", 64)} {
		other := run
		other.ManifestSHA256 = digest
		if _, err := openOwned(folder, other); !errors.Is(err, errNotOwned) {
			t.Errorf("a run with manifest digest %q owns the backup: %v", digest, err)
		}
	}
	missing := run
	missing.BackupName = namePrefix + "missing"
	if _, err := openOwned(folder, missing); !errors.Is(err, errMissing) {
		t.Errorf("missing: %v", err)
	}
}

// Two verified backups made in the same second whose folders were
// exchanged are both left: each folder holds the other run's manifest.
func TestRetentionLeavesExchangedBackupsOfTheSameSecond(t *testing.T) {
	f := newFixture(t)
	keep := 2
	f.configure(t, ScheduleChange{Keep: &keep})
	started := f.clock.Now()
	older := f.runAt(t, strings.Repeat("f", 32), started)
	newer := f.runAt(t, strings.Repeat("e", 32), started)
	if older.Verification != state.BackupVerifyPassed || newer.Verification != state.BackupVerifyPassed {
		t.Fatalf("runs: %+v %+v", older, newer)
	}
	olderPath, newerPath := filepath.Join(f.destination, older.BackupName), filepath.Join(f.destination, newer.BackupName)
	noErr(t, os.Rename(olderPath, olderPath+"-swap"))
	noErr(t, os.Rename(newerPath, olderPath))
	noErr(t, os.Rename(olderPath+"-swap", newerPath))
	third := f.runAt(t, strings.Repeat("d", 32), started)
	if third.Status != state.BackupSucceeded || !strings.Contains(third.Message, "holds another backup") {
		t.Fatalf("third: %+v", third)
	}
	for _, path := range []string{olderPath, newerPath} {
		if _, err := os.Stat(filepath.Join(path, "manifest.json")); err != nil {
			t.Fatalf("%s was removed: %v", path, err)
		}
	}
}
