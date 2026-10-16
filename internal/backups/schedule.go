package backups

import (
	"context"
	"time"

	"owngit/internal/state"
)

// ScheduleChange names the parts of one plan to change; nil leaves a part
// as it is.
type ScheduleChange struct {
	Destination *string
	Enabled     *bool
	Interval    *time.Duration
	Keep        *int
	Verify      *bool
}

// ChangeError is a plan change that cannot be saved as asked.
type ChangeError struct {
	// Message is the whole refusal in English, for the API and logs.
	Message string
	// Reason names a refusal that the dashboard words itself, and Detail is
	// the cause that goes with it; both are empty for any other refusal.
	Reason, Detail string
}

// The Reason of a ChangeError that the dashboard words itself.
const (
	ReasonChooseFolder   = "choose_folder"
	ReasonNotAbsolute    = "not_absolute"
	ReasonFolderUnusable = "folder_unusable"
	ReasonFolderOverlaps = "folder_overlaps"
)

func (e *ChangeError) Error() string { return e.Message }

// ChangeSchedule saves change to the plan named default and returns the
// plan before it (configured false when there was none) and after it. It
// keeps the schedule, status, runs and "back up now" calls that predate
// named plans working: their plan is default, created with the old
// defaults on the first change, which must set a backup folder.
func (s *Service) ChangeSchedule(ctx context.Context, change ScheduleChange) (before state.BackupSchedule, configured bool, after state.BackupSchedule, err error) {
	existing, found, err := s.Store.BackupPlan(ctx, state.DefaultBackupPlanName)
	if err != nil {
		return before, false, after, err
	}
	if found {
		before, configured = existing.BackupSchedule, true
	}
	if !found {
		if change.Destination == nil {
			return before, configured, after, &ChangeError{Message: "Choose a backup folder first.", Reason: ReasonChooseFolder}
		}
		now := s.now()
		existing = state.BackupPlan{
			Name: state.DefaultBackupPlanName,
			BackupSchedule: state.BackupSchedule{
				Enabled: true, Interval: DefaultInterval, Keep: DefaultKeep, Verify: true, UpdatedAt: now,
			},
			CreatedAt: now,
		}
	} else {
		existing.UpdatedAt = s.now()
	}
	updated, cerr := applyPlanChange(existing, change)
	if cerr != nil {
		return before, configured, after, cerr
	}
	if err := s.checkPlanFolder(ctx, updated.BackupSchedule); err != nil {
		return before, configured, after, err
	}
	if err := s.Store.SaveBackupPlan(ctx, updated); err != nil {
		return before, configured, after, err
	}
	s.Wake()
	if change.Destination != nil {
		s.noteRestoreLimit(updated.Destination)
	}
	return before, configured, updated.BackupSchedule, nil
}
