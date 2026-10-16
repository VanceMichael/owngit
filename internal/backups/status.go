package backups

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"owngit/internal/state"
)

// Schedule states.
const (
	ScheduleNotConfigured = "not_configured"
	ScheduleOff           = "off"
	ScheduleOn            = "on"
)

// ScheduleView is one plan as owners see it.
type ScheduleView struct {
	// State is on or off for a saved plan; the default plan is
	// not_configured before it was ever saved.
	State           string     `json:"state"`
	Destination     string     `json:"destination,omitempty"`
	Interval        string     `json:"interval,omitempty"`
	IntervalSeconds int64      `json:"interval_seconds,omitempty"`
	Keep            int        `json:"keep,omitempty"`
	Verify          *bool      `json:"verify,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

// RunView is one run as owners see it.
type RunView struct {
	ID   string `json:"id"`
	// Plan names the plan that started the run; it stays with the run
	// after that plan is deleted.
	Plan        string `json:"plan"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Destination string `json:"destination"`
	// BackupName is the folder in Destination that holds the run's
	// backup, empty when it wrote none or OwnGit removed it or found it
	// gone. Path is the whole path, or empty.
	BackupName string `json:"backup_name"`
	Path       string `json:"path"`
	// Verification is passed only when a rehearsed restore of this backup
	// passed.
	Verification string     `json:"verification"`
	Message      string     `json:"message,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	// LongestHoldMS is how long Git writes to one repository waited for
	// the backup at most, and LongestHoldRepository names it; both are
	// absent until the backup read every repository.
	LongestHoldMS         *int64 `json:"longest_hold_ms,omitempty"`
	LongestHoldRepository string `json:"longest_hold_repository,omitempty"`
}

// PlanStatusView is one plan's state: its schedule, the backup of it that
// runs, the last one that ended, the newest verified one it keeps, and
// when its next scheduled one is due.
type PlanStatusView struct {
	Plan         string        `json:"plan"`
	Schedule     ScheduleView  `json:"schedule"`
	Running      *RunView      `json:"running"`
	LastRun      *RunView      `json:"last_run"`
	LastVerified *RunView      `json:"last_verified"`
	NextRun      *time.Time    `json:"next_run"`
	RestoreLimit *RestoreLimit `json:"restore_limit,omitempty"`
}

// Status is the state of backups as OwnGit's records hold it. The
// top-level fields describe the default plan, so calls that predate named
// plans keep their answers; Plans lists every saved plan with the same
// fields. It reads nothing in the backup folders, so a slow or unavailable
// folder never holds it up; each backup and its removal of older ones
// record what they found there. Check and Upload are global: the last
// verification an owner asked for and the uploaded backup, which no plan
// owns. A run that still holds the one slot stands in as the running one
// in its plan, so nothing else may start while the page says it runs.
type Status struct {
	Schedule     ScheduleView  `json:"schedule"`
	Running      *RunView      `json:"running"`
	LastRun      *RunView      `json:"last_run"`
	LastVerified *RunView      `json:"last_verified"`
	NextRun      *time.Time    `json:"next_run"`
	Plans        []PlanStatusView `json:"plans"`
	Check        *Check        `json:"check"`
	Upload       *Upload       `json:"upload"`
	RestoreLimit *RestoreLimit `json:"restore_limit,omitempty"`
}

// ViewSchedule describes a plan's schedule; configured is false only for
// the default plan before it was saved.
func ViewSchedule(schedule state.BackupSchedule, configured bool) ScheduleView {
	if !configured {
		return ScheduleView{State: ScheduleNotConfigured}
	}
	view := ScheduleView{
		State: ScheduleOff, Destination: schedule.Destination, Interval: IntervalName(schedule.Interval),
		IntervalSeconds: int64(schedule.Interval / time.Second), Keep: schedule.Keep, Verify: &schedule.Verify, UpdatedAt: &schedule.UpdatedAt,
	}
	if schedule.Enabled {
		view.State = ScheduleOn
	}
	return view
}

// ViewRun describes run.
func ViewRun(run state.BackupRun) *RunView {
	view := &RunView{
		ID: run.ID, Plan: run.Plan, Kind: run.Kind, Status: run.Status, Destination: run.Destination, BackupName: run.BackupName,
		Verification: run.Verification, Message: run.Message, StartedAt: run.StartedAt,
	}
	if run.BackupName != "" {
		view.Path = filepath.Join(run.Destination, run.BackupName)
	}
	if run.HoldKnown {
		milliseconds := run.LongestHold.Milliseconds()
		view.LongestHoldMS, view.LongestHoldRepository = &milliseconds, run.LongestHoldRepository
	}
	if run.Status != state.BackupRunning {
		finished := run.FinishedAt
		view.FinishedAt = &finished
	}
	return view
}

// pendingResult is how a run whose final record the state store refused
// appears: as the backup that still occupies the folder, since no other
// may start while its record stands as running, saying how it ended and
// that its record waits for the state store. It is nil for every other
// run.
func (s *Service) pendingResult(id string) *state.BackupRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || s.pending.ID != id {
		return nil
	}
	result := *s.pending
	result.Message = pendingMessage(result.Status, result.Message)
	result.Status = state.BackupRunning
	return &result
}

// removalResult is how the run that holds the one slot for removing its
// older backups appears: as the backup that still occupies the folder,
// since no other may start until the removal ends, saying what it ended
// with and what it is doing. Its record is saved, so it stays the run
// OwnGit recorded as well. It is nil for every other run.
func (s *Service) removalResult(id string) *state.BackupRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removingRun == nil || s.removingRun.ID != id {
		return nil
	}
	result := *s.removingRun
	result.Message = removalMessage(result.Status)
	result.Status = state.BackupRunning
	return &result
}

// forgetPending forgets the result held for the run id, if any.
func (s *Service) forgetPending(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil && s.pending.ID == id {
		s.pending = nil
	}
}

// pendingMessage is how a finished backup whose record waits for the
// state store is described: how it ended, and that its record is not saved
// yet.
func pendingMessage(status, message string) string {
	return withNotice(message, fmt.Sprintf("The backup finished: %s. Its record could not be saved yet; OwnGit keeps trying and saves it as soon as the state is writable again.", status))
}

// removalMessage is how the run that removes its older backups is
// described: what it ended with, and what it is doing with the backups it
// no longer keeps.
func removalMessage(status string) string {
	return fmt.Sprintf("The backup finished: %s. OwnGit is removing the older backups it no longer keeps.", status)
}

// withNotice returns the message a record holds for a run together with a
// notice about the run itself: the notice keeps its room, and the message
// in front of it is cut first, so a notice is never the text that is cut
// away.
func withNotice(message, notice string) string {
	if message == "" {
		return notice
	}
	return state.CutBackupRunMessage(message, state.MaxBackupRunMessage-len(notice)-1) + " " + notice
}

// runDisplay is one stored run after the pending and removal slots gave
// their run the running appearance for views.
type runDisplay struct {
	run       state.BackupRun
	stoodIn   bool
	savedToo  bool
}

// Status reads the state of backups.
func (s *Service) Status(ctx context.Context) (Status, error) {
	plans, err := s.Store.BackupPlans(ctx)
	if err != nil {
		return Status{}, err
	}
	runs, err := s.Store.BackupRuns(ctx)
	if err != nil {
		return Status{}, err
	}
	timings, err := s.planTimings(ctx)
	if err != nil {
		return Status{}, err
	}
	due := map[string]time.Time{}
	for _, timing := range timings {
		if timing.plan.Enabled {
			due[timing.plan.Name] = timing.due
		}
	}
	// Aggregate each plan from the newest runs down. The runs are read
	// newest first, which the one-at-a-time ordering keeps whatever the
	// clock said.
	views := map[string]*PlanStatusView{}
	order := make([]string, 0, len(plans))
	for _, plan := range plans {
		view := &PlanStatusView{Plan: plan.Name, Schedule: ViewSchedule(plan.BackupSchedule, true)}
		if at, ok := due[plan.Name]; ok {
			at := at
			view.NextRun = &at
		}
		view.RestoreLimit = s.restoreLimit(plan.Destination)
		views[plan.Name] = view
		order = append(order, plan.Name)
	}
	for _, run := range runs {
		view := views[run.Plan]
		display := runDisplay{run: run}
		if result := s.pendingResult(run.ID); result != nil {
			// The finished run whose record waits stands in as the
			// running one; the last runs stay the ones OwnGit recorded,
			// since its own record is not saved yet.
			display.run, display.stoodIn = *result, true
		} else if result := s.removalResult(run.ID); result != nil && run.Status != state.BackupRunning {
			display.run, display.stoodIn, display.savedToo = *result, true, true
		}
		if view != nil {
			if display.stoodIn && view.Running == nil {
				view.Running = ViewRun(display.run)
			}
			if !display.stoodIn && run.Status == state.BackupRunning && view.Running == nil {
				view.Running = ViewRun(run)
			}
			if (!display.stoodIn || display.savedToo) && run.Status != state.BackupRunning && view.LastRun == nil {
				view.LastRun = ViewRun(run)
			}
			if view.LastVerified == nil && run.Verification == state.BackupVerifyPassed && run.BackupName != "" {
				view.LastVerified = ViewRun(run)
			}
		}
	}
	status := Status{Plans: make([]PlanStatusView, 0, len(plans))}
	for _, name := range order {
		status.Plans = append(status.Plans, *views[name])
	}
	if defaultPlan, ok := views[state.DefaultBackupPlanName]; ok {
		status.Schedule = defaultPlan.Schedule
		status.Running = defaultPlan.Running
		status.LastRun = defaultPlan.LastRun
		status.LastVerified = defaultPlan.LastVerified
		status.NextRun = defaultPlan.NextRun
		status.RestoreLimit = defaultPlan.RestoreLimit
	} else {
		status.Schedule = ViewSchedule(state.BackupSchedule{}, false)
	}
	s.mu.Lock()
	if s.check != nil {
		check := *s.check
		status.Check = &check
	}
	if s.upload != nil {
		upload := *s.upload
		status.Upload = &upload
	}
	s.mu.Unlock()
	return status, nil
}

// Summary says whether backups work without naming a folder, a repository
// or an error: what general access may read, for coding agents. Every field
// is a fixed word or a time.
type Summary struct {
	// Schedule is not_configured, off or on across every plan.
	Schedule string      `json:"schedule"`
	LastRun  *SummaryRun `json:"last_run"`
	// LastVerifiedAt is when the newest verified backup that is still in
	// its folder finished, across every plan.
	LastVerifiedAt *time.Time `json:"last_verified_at"`
	NextRun        *time.Time `json:"next_run"`
}

// SummaryRun is the last backup that ended, without its folder or message.
type SummaryRun struct {
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	Verification string    `json:"verification"`
	Plan         string    `json:"plan"`
	FinishedAt   time.Time `json:"finished_at"`
}

// Summarize reduces status to its Summary over every plan.
func Summarize(status Status) Summary {
	summary := Summary{Schedule: status.Schedule.State, NextRun: status.NextRun}
	if len(status.Plans) > 0 {
		enabled := false
		var lastRun, lastVerified *RunView
		var nextRun *time.Time
		for index := range status.Plans {
			plan := &status.Plans[index]
			if plan.Schedule.State == ScheduleOn {
				enabled = true
			}
			if newerRun(plan.LastRun, lastRun) {
				lastRun = plan.LastRun
			}
			if newerRun(plan.LastVerified, lastVerified) {
				lastVerified = plan.LastVerified
			}
	if plan.NextRun != nil && (nextRun == nil || plan.NextRun.Before(*nextRun)) {
				nextRun = plan.NextRun
			}
		}
		if enabled {
			summary.Schedule = ScheduleOn
		} else {
			summary.Schedule = ScheduleOff
		}
		if lastRun != nil {
			summary.LastRun = &SummaryRun{Kind: lastRun.Kind, Status: lastRun.Status, Verification: lastRun.Verification, Plan: lastRun.Plan, FinishedAt: *lastRun.FinishedAt}
		}
		if lastVerified != nil {
			summary.LastVerifiedAt = lastVerified.FinishedAt
		}
		summary.NextRun = nextRun
	}
	return summary
}

// newerRun says whether candidate ends later than current.
func newerRun(candidate, current *RunView) bool {
	return candidate != nil && candidate.FinishedAt != nil && (current == nil || current.FinishedAt == nil || candidate.FinishedAt.After(*current.FinishedAt))
}

// Runs lists every recorded run, the newest first. A run that holds the
// one slot (its record waits for the state store, or it is removing its
// older backups) appears as the running one, as in Status.
func (s *Service) Runs(ctx context.Context) ([]RunView, error) {
	return s.runs(ctx, "")
}

// RunsOfPlan lists the recorded runs of one plan, the newest first. It
// also lists a deleted plan's runs: the plan's name stays with them.
func (s *Service) RunsOfPlan(ctx context.Context, plan string) ([]RunView, error) {
	return s.runs(ctx, plan)
}

func (s *Service) runs(ctx context.Context, plan string) ([]RunView, error) {
	var stored []state.BackupRun
	var err error
	if plan == "" {
		stored, err = s.Store.BackupRuns(ctx)
	} else {
		stored, err = s.Store.BackupRunsOfPlan(ctx, plan)
	}
	if err != nil {
		return nil, err
	}
	views := make([]RunView, 0, len(stored))
	for _, run := range stored {
		if result := s.pendingResult(run.ID); result != nil {
			run = *result
		} else if result := s.removalResult(run.ID); result != nil && run.Status != state.BackupRunning {
			run = *result
		}
		views = append(views, *ViewRun(run))
	}
	return views, nil
}
