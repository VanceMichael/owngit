package backups

import (
	"context"
	"errors"
	"path/filepath"

	"owngit/internal/recovery"
	"owngit/internal/state"
)

// Named backup plans let an owner keep separate schedules, destinations,
// retention and verification choices, for example one on this computer and
// one on an offline mount. Each run names its plan and freezes the plan as
// it was when the run started.

// A plan that cannot be used or changed is refused with one of these, so
// the API, the dashboard and the command line can word their own answers.
var (
	// ErrPlanNotFound refuses a plan name that no saved plan has.
	ErrPlanNotFound = errors.New("the backup plan does not exist")
	// ErrPlanExists refuses a second plan with a name that is in use.
	ErrPlanExists = errors.New("a backup plan with this name already exists")
	// ErrPlanActive refuses to delete a plan whose backup still occupies
	// the one slot: it runs, or it removes its older backups.
	ErrPlanActive = errors.New("the backup plan is still making a backup")
	// ErrPlanLimit refuses one plan beyond MaxBackupPlans.
	ErrPlanLimit = errors.New("there are already as many backup plans as OwnGit keeps")
)

// Plans returns the saved plans in their stable order.
func (s *Service) Plans(ctx context.Context) ([]state.BackupPlan, error) {
	return s.Store.BackupPlans(ctx)
}

// Plan returns the saved plan of name, and false when there is none.
func (s *Service) Plan(ctx context.Context, name string) (state.BackupPlan, bool, error) {
	return s.Store.BackupPlan(ctx, name)
}

// CreatePlan saves a new plan. Its first change must set a backup folder;
// the rest starts at the defaults: on, daily, seven backups kept and each
// one verified. A new folder must be an absolute path that no backup would
// overlap OwnGit's storage in; it is created, private to this account,
// when missing. The name follows state's plan-name bounds, it must not be
// in use, and there may be at most MaxBackupPlans plans.
func (s *Service) CreatePlan(ctx context.Context, name string, change ScheduleChange) (state.BackupPlan, error) {
	if err := state.ValidateBackupPlanName(name); err != nil {
		return state.BackupPlan{}, &ChangeError{Message: err.Error() + "."}
	}
	plans, err := s.Store.BackupPlans(ctx)
	if err != nil {
		return state.BackupPlan{}, err
	}
	for _, plan := range plans {
		if plan.Name == name {
			return state.BackupPlan{}, ErrPlanExists
		}
	}
	if len(plans) >= state.MaxBackupPlans {
		return state.BackupPlan{}, ErrPlanLimit
	}
	if change.Destination == nil {
		return state.BackupPlan{}, &ChangeError{Message: "Choose a backup folder first.", Reason: ReasonChooseFolder}
	}
	now := s.now()
	plan := state.BackupPlan{
		Name: name,
		BackupSchedule: state.BackupSchedule{
			Enabled: true, Interval: DefaultInterval, Keep: DefaultKeep, Verify: true, UpdatedAt: now,
		},
		CreatedAt: now,
	}
	plan, cerr := applyPlanChange(plan, change)
	if cerr != nil {
		return state.BackupPlan{}, cerr
	}
	if err := s.checkPlanFolder(ctx, plan.BackupSchedule); err != nil {
		return state.BackupPlan{}, err
	}
	if err := s.Store.SaveBackupPlan(ctx, plan); err != nil {
		return state.BackupPlan{}, err
	}
	s.Wake()
	s.noteRestoreLimit(plan.Destination)
	return plan, nil
}

// ChangePlan saves change to the plan of name and returns the plan as it
// is saved. The folder rules are the same as for a new plan.
func (s *Service) ChangePlan(ctx context.Context, name string, change ScheduleChange) (state.BackupPlan, error) {
	plan, configured, err := s.Store.BackupPlan(ctx, name)
	if err != nil {
		return state.BackupPlan{}, err
	}
	if !configured {
		return state.BackupPlan{}, ErrPlanNotFound
	}
	after := plan
	after.UpdatedAt = s.now()
	after, cerr := applyPlanChange(after, change)
	if cerr != nil {
		return state.BackupPlan{}, cerr
	}
	if err := s.checkPlanFolder(ctx, after.BackupSchedule); err != nil {
		return state.BackupPlan{}, err
	}
	if err := s.Store.SaveBackupPlan(ctx, after); err != nil {
		return state.BackupPlan{}, err
	}
	s.Wake()
	if change.Destination != nil {
		s.noteRestoreLimit(after.Destination)
	}
	return after, nil
}

// DeletePlan removes the plan of name. It refuses while a backup of the
// plan runs or removes its older backups: the plan that started it must
// stay for the run's record and its cleanup. The plan's runs and the
// backups it wrote stay, still named with the plan.
func (s *Service) DeletePlan(ctx context.Context, name string) error {
	if _, configured, err := s.Store.BackupPlan(ctx, name); err != nil {
		return err
	} else if !configured {
		return ErrPlanNotFound
	}
	// Hold the slot lock across the active check and the deletion: start
	// inserts its run under the same lock, so a plan cannot disappear in
	// the gap between the count and the delete.
	s.mu.Lock()
	if s.removingRun != nil && s.removingRun.Plan == name {
		s.mu.Unlock()
		return ErrPlanActive
	}
	running, err := s.Store.BackupPlanRunning(ctx, name)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if running {
		s.mu.Unlock()
		return ErrPlanActive
	}
	err = s.Store.DeleteBackupPlan(ctx, name)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// Wake takes the same lock, so it comes after the unlock.
	s.Wake()
	return nil
}

// StartPlanNow starts a backup into the plan's folder and returns its run
// at once. The slot and refusal rules are StartNow's; a missing plan is
// ErrPlanNotFound.
func (s *Service) StartPlanNow(name string) (state.BackupRun, error) {
	return s.start(state.BackupRunManual, name)
}

// folderUnusable words a folder that a plan names but cannot use.
func folderUnusable(err error) *ChangeError {
	if errors.Is(err, recovery.ErrBackupOverlapsStorage) {
		return &ChangeError{Message: "The backup folder cannot be used: " + err.Error() + ".", Reason: ReasonFolderOverlaps}
	}
	return &ChangeError{Message: "The backup folder cannot be used: " + err.Error() + ".", Reason: ReasonFolderUnusable, Detail: err.Error()}
}

// checkPlanFolder validates the folder schedule names, staging a private
// folder there the way the single schedule did.
func (s *Service) checkPlanFolder(ctx context.Context, schedule state.BackupSchedule) error {
	if err := recovery.CheckBackupFolder(s.Store, s.Repositories, schedule.Destination); err != nil {
		return folderUnusable(err)
	}
	area, err := state.OpenStagingArea(schedule.Destination)
	if err != nil {
		return folderUnusable(err)
	}
	area.Close()
	return nil
}

// applyPlanChange returns plan with change applied, or a refusal. It does
// not touch the store or the disk: callers validate the folder and save.
func applyPlanChange(plan state.BackupPlan, change ScheduleChange) (state.BackupPlan, *ChangeError) {
	if change.Destination != nil {
		destination := *change.Destination
		if !filepath.IsAbs(destination) {
			return plan, &ChangeError{Message: "The backup folder must be an absolute path.", Reason: ReasonNotAbsolute}
		}
		plan.Destination = filepath.Clean(destination)
	}
	if change.Enabled != nil {
		plan.Enabled = *change.Enabled
	}
	if change.Interval != nil {
		plan.Interval = *change.Interval
	}
	if change.Keep != nil {
		plan.Keep = *change.Keep
	}
	if change.Verify != nil {
		plan.Verify = *change.Verify
	}
	if err := state.ValidateBackupSchedule(plan.BackupSchedule); err != nil {
		return plan, &ChangeError{Message: err.Error() + "."}
	}
	return plan, nil
}
