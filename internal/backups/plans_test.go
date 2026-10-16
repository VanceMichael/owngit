package backups

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"owngit/internal/state"
)

// createPlan saves a plan through the service, with the given changes over
// the defaults.
func (f *fixture) createPlan(t *testing.T, name, destination string, change ...ScheduleChange) state.BackupPlan {
	t.Helper()
	c := ScheduleChange{Destination: &destination}
	if len(change) > 0 {
		c = change[0]
		if c.Destination == nil {
			c.Destination = &destination
		}
	}
	plan, err := f.service.CreatePlan(context.Background(), name, c)
	noErr(t, err)
	return plan
}

// backUpPlan starts a manual backup of a plan, waits for it, and returns
// its record.
func (f *fixture) backUpPlan(t *testing.T, name string) state.BackupRun {
	t.Helper()
	f.clock.advance(time.Hour)
	run, err := f.service.StartPlanNow(name)
	noErr(t, err)
	f.service.work.Wait()
	return f.run(t, run.ID)
}

// Plans are created, listed in stable order, changed and deleted with the
// same folder rules as the default schedule, and the name and count
// bounds are refused.
func TestPlansAreCreatedListedChangedAndRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, configured, err := f.service.Plan(ctx, "missing"); err != nil || configured {
		t.Fatalf("missing plan configured=%v err=%v", configured, err)
	}
	first := filepath.Join(f.destination, "first")
	second := filepath.Join(f.destination, "second")
	alpha := f.createPlan(t, "alpha", first)
	if !alpha.Enabled || alpha.Keep != DefaultKeep || alpha.Verify == false || alpha.Interval != DefaultInterval || !alpha.CreatedAt.Equal(alpha.UpdatedAt) {
		t.Fatalf("new plan defaults=%+v", alpha)
	}
	// A second plan may use the same destination.
	f.createPlan(t, "bravo", second, ScheduleChange{Destination: &second, Keep: ptr(3), Verify: ptr(false)})
	plans, err := f.service.Plans(ctx)
	noErr(t, err)
	if len(plans) != 2 || plans[0].Name != "alpha" || plans[1].Name != "bravo" {
		t.Fatalf("plans=%+v", plans)
	}
	off := false
	f.clock.advance(time.Second)
	updated, err := f.service.ChangePlan(ctx, "alpha", ScheduleChange{Enabled: &off})
	noErr(t, err)
	if updated.Enabled || !updated.CreatedAt.Equal(alpha.CreatedAt) || !updated.UpdatedAt.After(alpha.UpdatedAt) {
		t.Fatalf("changed plan=%+v", updated)
	}
	// Refusals.
	if _, err := f.service.CreatePlan(ctx, "alpha", ScheduleChange{Destination: &first}); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	for _, name := range []string{"has space", "a/b", strings.Repeat("x", state.MaxBackupPlanName+1)} {
		if _, err := f.service.CreatePlan(ctx, name, ScheduleChange{Destination: &first}); err == nil {
			t.Fatalf("bad plan name %q was accepted", name)
		}
	}
	if _, err := f.service.CreatePlan(ctx, "gamma", ScheduleChange{}); err == nil {
		t.Fatal("a plan without a destination was accepted")
	}
	relative := "relative/backups"
	var refused *ChangeError
	if _, err := f.service.CreatePlan(ctx, "gamma", ScheduleChange{Destination: &relative}); !errors.As(err, &refused) {
		t.Fatalf("relative destination: %v", err)
	}
	// The plan count bound is enforced.
	for index := 0; index < state.MaxBackupPlans-2; index++ {
		destination := filepath.Join(f.destination, "more", "p"+itoa(index))
		f.createPlan(t, "p"+itoa(index), destination)
	}
	if _, err := f.service.CreatePlan(ctx, "one-too-many", ScheduleChange{Destination: &first}); !errors.Is(err, ErrPlanLimit) {
		t.Fatalf("plan limit: %v", err)
	}
}

// A plan with a running backup, or one whose finished backup still holds
// the removal slot, cannot be deleted; an idle plan can, and its runs
// stay.
func TestDeletePlanRefusesAnActivePlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createPlan(t, "alpha", f.destination)
	run := state.BackupRun{ID: strings.Repeat("1", 32), Plan: "alpha", Kind: state.BackupRunManual, Status: state.BackupRunning,
		Destination: f.destination, BackupName: namePrefix + "active", Verification: state.BackupVerifyNotRun, StartedAt: f.clock.Now()}
	noErr(t, f.store.StartBackupRun(ctx, run))
	if err := f.service.DeletePlan(ctx, "alpha"); !errors.Is(err, ErrPlanActive) {
		t.Fatalf("deleting a running plan: %v", err)
	}
	if _, configured, err := f.service.Plan(ctx, "alpha"); err != nil || !configured {
		t.Fatal("the refused deletion removed the plan")
	}
	run.Status, run.FinishedAt = state.BackupSucceeded, f.clock.Now()
	noErr(t, f.store.FinishBackupRun(ctx, run))
	// The removal slot held by the plan's run refuses too.
	f.service.mu.Lock()
	finished := run
	f.service.removingRun = &finished
	f.service.mu.Unlock()
	if err := f.service.DeletePlan(ctx, "alpha"); !errors.Is(err, ErrPlanActive) {
		t.Fatalf("deleting a plan while it removes backups: %v", err)
	}
	f.service.mu.Lock()
	f.service.removingRun = nil
	f.service.mu.Unlock()
	noErr(t, f.service.DeletePlan(ctx, "alpha"))
	if _, configured, err := f.service.Plan(ctx, "alpha"); err != nil || configured {
		t.Fatalf("deleted plan configured=%v err=%v", configured, err)
	}
	runs, err := f.service.RunsOfPlan(ctx, "alpha")
	noErr(t, err)
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Plan != "alpha" {
		t.Fatalf("the deleted plan's runs did not stay: %+v", runs)
	}
	if err := f.service.DeletePlan(ctx, "missing"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("deleting a missing plan: %v", err)
	}
}

// A run freezes the plan as it was when it started: changing the folder,
// verification and retention while it runs changes only later runs.
func TestRunFreezesThePlanItStartedWith(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := filepath.Join(f.destination, "first")
	second := filepath.Join(f.destination, "second")
	plan := f.createPlan(t, "alpha", first, ScheduleChange{Destination: &first, Verify: ptr(false), Keep: ptr(1)})
	// A previous backup in the first folder, so the frozen keep and the
	// room check have something to work with.
	older := f.backUpPlan(t, "alpha")
	// Start the run with the frozen plan, then change everything before
	// execute runs.
	now := f.clock.Now()
	started := state.BackupRun{ID: strings.Repeat("2", 32), Plan: "alpha", Kind: state.BackupRunManual, Status: state.BackupRunning,
		Destination: plan.Destination, BackupName: namePrefix + now.UTC().Format("20060102-150405") + "-22222222",
		Verification: state.BackupVerifyNotRun, StartedAt: now}
	noErr(t, f.store.StartBackupRun(ctx, started))
	_, err := f.service.ChangePlan(ctx, "alpha", ScheduleChange{Destination: &second, Verify: ptr(true), Keep: ptr(9)})
	noErr(t, err)
	f.service.execute(ctx, started, plan)
	finished := f.run(t, started.ID)
	if finished.Verification != state.BackupVerifyNotRun {
		t.Fatalf("the frozen plan's verify setting changed mid-run: %+v", finished)
	}
	if !present(finished) {
		t.Fatal("the frozen plan's folder holds no backup")
	}
	if entries, _ := os.ReadDir(second); len(entries) != 0 {
		t.Fatalf("the changed folder was used by the running backup: %v", entries)
	}
	// The frozen keep removed the older backup of the first folder; the
	// new keep would have kept it.
	if present(older) {
		t.Fatalf("cleanup used the changed keep; the older backup stayed")
	}
}

// Deleting a plan leaves its backups inspectable: runs still list, and a
// listed backup verifies and downloads by its run ID.
func TestDeletedPlanBackupsStayInspectable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createPlan(t, "alpha", f.destination, ScheduleChange{Destination: &f.destination, Verify: ptr(false)})
	run := f.backUpPlan(t, "alpha")
	noErr(t, f.service.DeletePlan(ctx, "alpha"))
	runs, err := f.service.RunsOfPlan(ctx, "alpha")
	noErr(t, err)
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Plan != "alpha" || runs[0].Path == "" {
		t.Fatalf("runs after deletion=%+v", runs)
	}
	download, err := f.service.OpenDownload(ctx, run.ID)
	noErr(t, err)
	download.Close()
	if !present(run) {
		t.Fatal("the backup folder left with its deleted plan")
	}
}

// Two plans that write the same folder remove only their own backups:
// each applies its own keep, and neither forgets nor deletes the other's.
func TestPlansSharingAFolderKeepRetentionSeparate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createPlan(t, "alpha", f.destination, ScheduleChange{Destination: &f.destination, Verify: ptr(false), Keep: ptr(2)})
	f.createPlan(t, "bravo", f.destination, ScheduleChange{Destination: &f.destination, Verify: ptr(false), Keep: ptr(3)})
	var alpha, bravo []state.BackupRun
	for index := 0; index < 4; index++ {
		f.clock.advance(time.Hour)
		alpha = append(alpha, f.backUpPlan(t, "alpha"))
		f.clock.advance(time.Minute)
		bravo = append(bravo, f.backUpPlan(t, "bravo"))
	}
	entries, err := os.ReadDir(f.destination)
	noErr(t, err)
	onDisk := map[string]bool{}
	for _, entry := range entries {
		onDisk[entry.Name()] = true
	}
	expectKept := func(runs []state.BackupRun, keep int, name string) {
		t.Helper()
		kept := 0
		for index, run := range runs {
			exists := onDisk[run.BackupName]
			if index >= len(runs)-keep {
				if !exists {
					t.Fatalf("%s backup %s was removed by another plan", name, run.BackupName)
				}
				kept++
			} else if exists {
				t.Fatalf("%s kept %d backups; an older one %s stayed", name, keep, run.BackupName)
			}
		}
		if kept != keep {
			t.Fatalf("%s kept %d backups", name, kept)
		}
	}
	expectKept(alpha, 2, "alpha")
	expectKept(bravo, 3, "bravo")
	// Cleanup forgot only the removed runs of each plan.
	alphaRuns, err := f.store.BackupRunsOfPlan(ctx, "alpha")
	noErr(t, err)
	bravoRuns, err := f.store.BackupRunsOfPlan(ctx, "bravo")
	noErr(t, err)
	forgotten := func(runs []state.BackupRun, keep int) int {
		count := 0
		for _, run := range runs {
			if run.BackupName == "" {
				count++
			}
		}
		return count
	}
	if forgotten(alphaRuns, 2) != 2 || forgotten(bravoRuns, 3) != 1 {
		t.Fatalf("forgotten alpha=%d bravo=%d", forgotten(alphaRuns, 2), forgotten(bravoRuns, 3))
	}
}

// Another plan's unreadable backup in the same folder is not this plan's
// room estimate: the backup proceeds without a "free space was not
// checked" note about the other plan's folder.
func TestRoomEstimateUsesOnlyTheSamePlan(t *testing.T) {
	f := newFixture(t)
	f.createPlan(t, "alpha", f.destination, ScheduleChange{Destination: &f.destination, Verify: ptr(false), Keep: ptr(5)})
	f.createPlan(t, "bravo", f.destination, ScheduleChange{Destination: &f.destination, Verify: ptr(false), Keep: ptr(5)})
	other := f.backUpPlan(t, "bravo")
	noErr(t, os.WriteFile(filepath.Join(f.destination, other.BackupName, "manifest.json"), []byte("{\"format\":"), 0o600))
	run := f.backUpPlan(t, "alpha")
	if strings.Contains(run.Message, other.BackupName) || strings.Contains(run.Message, "free space was not checked") {
		t.Fatalf("alpha's room estimate read bravo's backup: %s", run.Message)
	}
}

// The scheduler starts due plans by due time, then stable order, and keeps
// going after a plan whose target fails: the failure is that plan's run
// alone, the others still run, and the failed plan waits for its interval.
func TestSchedulerStartsDuePlansInOrderAndContinuesAfterFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	goodA := filepath.Join(f.destination, "good-a")
	goodB := filepath.Join(f.destination, "good-b")
	goodC := filepath.Join(f.destination, "good-c")
	// Names fix the stable order with equal creation times: broken plan
	// first, then three good ones.
	broken := filepath.Join(f.destination, "blocked")
	f.createPlan(t, "aaa-broken", broken, ScheduleChange{Destination: &broken, Verify: ptr(false)})
	f.createPlan(t, "bbb-one", goodA, ScheduleChange{Destination: &goodA, Verify: ptr(false)})
	f.createPlan(t, "ccc-two", goodB, ScheduleChange{Destination: &goodB, Verify: ptr(false)})
	f.createPlan(t, "ddd-three", goodC, ScheduleChange{Destination: &goodC, Verify: ptr(false)})
	// Saving the plan created its folder privately; replace it with a file,
	// so opening that target for a backup fails like an unmounted mount.
	noErr(t, os.RemoveAll(broken))
	noErr(t, os.WriteFile(broken, []byte("not a folder"), 0o600))

	for attempt := 0; attempt < 6; attempt++ {
		f.service.tick()
		f.service.work.Wait()
	}
	runs, err := f.store.BackupRuns(ctx)
	noErr(t, err)
	// Newest first: reverse to start order.
	order := make([]string, 0, len(runs))
	for index := len(runs) - 1; index >= 0; index-- {
		order = append(order, runs[index].Plan)
	}
	wantOrder := []string{"aaa-broken", "bbb-one", "ccc-two", "ddd-three"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("start order=%v, want %v", order, wantOrder)
	}
	byPlan := map[string][]state.BackupRun{}
	for _, run := range runs {
		byPlan[run.Plan] = append(byPlan[run.Plan], run)
	}
	failed := byPlan["aaa-broken"]
	if len(failed) != 1 || failed[0].Status != state.BackupFailed || failed[0].Kind != state.BackupRunScheduled {
		t.Fatalf("broken plan runs=%+v", failed)
	}
	for _, name := range []string{"bbb-one", "ccc-two", "ddd-three"} {
		planRuns := byPlan[name]
		if len(planRuns) != 1 || planRuns[0].Status != state.BackupSucceeded {
			t.Fatalf("%s runs=%+v", name, planRuns)
		}
	}
	// Nothing is due once each plan used its turn; the failed plan's next
	// run is an interval away, so it cannot hot-loop.
	if due := dueFrom(mustTimings(t, f), f.clock.Now()); len(due) != 0 {
		t.Fatalf("plans still due after their turn: %+v", due)
	}
	// Repair the target and retry that plan by hand.
	noErr(t, os.Remove(broken))
	manual := func() state.BackupRun {
		run, err := f.service.StartPlanNow("aaa-broken")
		noErr(t, err)
		f.service.work.Wait()
		return f.run(t, run.ID)
	}()
	if manual.Status != state.BackupSucceeded || manual.Kind != state.BackupRunManual {
		t.Fatalf("manual retry after repair: %+v", manual)
	}
}

// While one backup uses the one slot, no other plan's backup starts; the
// other plans start once it ends.
func TestOtherPlansWaitForTheOneSlot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.configure(t, ScheduleChange{Verify: ptr(false)})
	other := filepath.Join(f.destination, "other")
	f.createPlan(t, "other", other, ScheduleChange{Destination: &other, Verify: ptr(false)})
	running := state.BackupRun{ID: strings.Repeat("9", 32), Plan: state.DefaultBackupPlanName, Kind: state.BackupRunManual,
		Status: state.BackupRunning, Destination: f.destination, Verification: state.BackupVerifyNotRun, StartedAt: f.clock.Now()}
	noErr(t, f.store.StartBackupRun(ctx, running))
	if _, err := f.service.StartPlanNow("other"); !errors.Is(err, state.ErrBackupRunning) {
		t.Fatalf("another plan started while one ran: %v", err)
	}
	f.service.tick()
	f.service.work.Wait()
	if runs, _ := f.store.BackupRunsOfPlan(ctx, "other"); len(runs) != 0 {
		t.Fatalf("the scheduler started another plan while one ran: %+v", runs)
	}
	running.Status, running.FinishedAt = state.BackupSucceeded, f.clock.Now()
	noErr(t, f.store.FinishBackupRun(ctx, running))
	// The scheduler starts the plans that waited in stable order: default
	// first, then the other plan.
	f.service.tick()
	f.service.work.Wait()
	f.service.tick()
	f.service.work.Wait()
	runs, err := f.store.BackupRunsOfPlan(ctx, "other")
	noErr(t, err)
	if len(runs) != 1 || runs[0].Status != state.BackupSucceeded || runs[0].Kind != state.BackupRunScheduled {
		t.Fatalf("the other plan did not start after the slot freed: %+v", runs)
	}
}

// On restart, the interrupted run stays with its plan, no other plan is
// counted as run, and each plan is due only from its own scheduled runs.
func TestRestartAttributesTheInterruptedRunToItsPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.createPlan(t, "alpha", f.destination, ScheduleChange{Destination: &f.destination, Verify: ptr(false)})
	bravoDir := filepath.Join(f.destination, "bravo")
	f.createPlan(t, "bravo", bravoDir, ScheduleChange{Destination: &bravoDir, Verify: ptr(false)})
	start := f.clock.Now()
	interrupted := state.BackupRun{ID: strings.Repeat("8", 32), Plan: "alpha", Kind: state.BackupRunScheduled, Status: state.BackupRunning,
		Destination: f.destination, StartedAt: start}
	noErr(t, f.store.StartBackupRun(ctx, interrupted))
	// A restart marks only that run interrupted, with its plan.
	count, err := f.store.InterruptBackupRuns(ctx, InterruptedMessage, f.clock.Now())
	noErr(t, err)
	if count != 1 {
		t.Fatalf("interrupted %d runs", count)
	}
	alphaRuns, err := f.store.BackupRunsOfPlan(ctx, "alpha")
	noErr(t, err)
	bravoRunsBefore, err := f.store.BackupRunsOfPlan(ctx, "bravo")
	noErr(t, err)
	if len(alphaRuns) != 1 || alphaRuns[0].Status != state.BackupInterrupted || alphaRuns[0].Plan != "alpha" {
		t.Fatalf("alpha runs=%+v", alphaRuns)
	}
	if len(bravoRunsBefore) != 0 {
		t.Fatalf("bravo was counted as run: %+v", bravoRunsBefore)
	}
	// Alpha used its interval; bravo has never run and is due. One tick
	// starts only bravo: exactly one catch-up.
	timings := mustTimings(t, f)
	due := dueFrom(timings, f.clock.Now())
	if len(due) != 1 || due[0].plan.Name != "bravo" {
		t.Fatalf("due plans=%+v, only bravo must catch up", due)
	}
	f.service.tick()
	f.service.work.Wait()
	bravoRuns, err := f.store.BackupRunsOfPlan(ctx, "bravo")
	noErr(t, err)
	if len(bravoRuns) != 1 || bravoRuns[0].Kind != state.BackupRunScheduled || bravoRuns[0].Status != state.BackupSucceeded {
		t.Fatalf("bravo catch-up=%+v", bravoRuns)
	}
	// Alpha still needs a manual retry after its target is fixed.
	f.clock.advance(time.Minute)
	manual := f.backUpPlan(t, "alpha")
	if manual.Kind != state.BackupRunManual || manual.Status != state.BackupSucceeded {
		t.Fatalf("alpha manual retry=%+v", manual)
	}
}

// Status carries one view per plan with the top-level fields on default,
// and the summary aggregates every plan; runs carry their plan and filter
// by it.
func TestStatusAndSummaryCoverEveryPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.configure(t, ScheduleChange{Verify: ptr(false)})
	other := filepath.Join(f.destination, "other")
	f.createPlan(t, "other", other, ScheduleChange{Destination: &other, Verify: ptr(false), Enabled: ptr(false)})
	_ = f.backUpNow(t)
	_ = f.backUpPlan(t, "other") // manual runs while scheduled are off.
	status, err := f.service.Status(ctx)
	noErr(t, err)
	if len(status.Plans) != 2 {
		t.Fatalf("plans in status=%d", len(status.Plans))
	}
	byName := map[string]PlanStatusView{}
	for _, plan := range status.Plans {
		byName[plan.Plan] = plan
	}
	defaultView := byName[state.DefaultBackupPlanName]
	otherView := byName["other"]
	if status.Schedule.State != defaultView.Schedule.State || status.LastRun.ID != defaultView.LastRun.ID ||
		status.NextRun == nil || *status.NextRun != *defaultView.NextRun {
		t.Fatalf("top-level status is not the default plan: %+v", status)
	}
	if otherView.Schedule.State != ScheduleOff || otherView.LastRun == nil || otherView.NextRun != nil {
		t.Fatalf("other plan view=%+v", otherView)
	}
	allRuns, err := f.service.Runs(ctx)
	noErr(t, err)
	if len(allRuns) != 2 {
		t.Fatalf("runs=%d", len(allRuns))
	}
	for _, run := range allRuns {
		if run.Plan == "" {
			t.Fatalf("run without a plan: %+v", run)
		}
	}
	otherRuns, err := f.service.RunsOfPlan(ctx, "other")
	noErr(t, err)
	if len(otherRuns) != 1 || otherRuns[0].Plan != "other" {
		t.Fatalf("filtered runs=%+v", otherRuns)
	}
	summary := Summarize(status)
	if summary.Schedule != ScheduleOn {
		t.Fatalf("summary with one plan on=%q", summary.Schedule)
	}
	if summary.LastRun == nil || summary.NextRun == nil || summary.LastRun.Plan == "" {
		t.Fatalf("summary aggregate=%+v", summary)
	}
	// With both plans off, the summary says off; with no plans,
	// not_configured.
	off := false
	_, _, _, err = f.service.ChangeSchedule(ctx, ScheduleChange{Enabled: &off})
	noErr(t, err)
	status, err = f.service.Status(ctx)
	noErr(t, err)
	if Summarize(status).Schedule != ScheduleOff {
		t.Fatal("summary is not off with every plan off")
	}
	noErr(t, f.service.DeletePlan(ctx, "other"))
	noErr(t, f.service.DeletePlan(ctx, state.DefaultBackupPlanName))
	status, err = f.service.Status(ctx)
	noErr(t, err)
	if len(status.Plans) != 0 || status.Schedule.State != ScheduleNotConfigured {
		t.Fatalf("status after deleting every plan=%+v", status)
	}
	if Summarize(status).Schedule != ScheduleNotConfigured {
		t.Fatal("summary is not not_configured without plans")
	}
}

func mustTimings(t *testing.T, f *fixture) []planTiming {
	t.Helper()
	timings, err := f.service.planTimings(context.Background())
	noErr(t, err)
	return timings
}

func ptr[T any](value T) *T { return &value }

// itoa names the throwaway extra plans without pulling in strconv in the
// assertions.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
