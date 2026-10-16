package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"owngit/internal/backups"
	"owngit/internal/state"
)

// The named backup plan endpoints create, list, change and delete plans,
// start a plan's backup, and filter runs by plan; only the administrator
// uses them.
func TestBackupPlansAPI(t *testing.T) {
	fixture := newAPIFixture(t, true)
	server := serve(t, fixture.app.Handler())
	base := server.URL + "/api/v1/backups"

	// General access is refused like for the rest of backup management.
	if code, errorCode := checkStatus(t, apiRequest(t, http.MethodGet, base+"/plans", nil, "shared-password", "")); code != http.StatusUnauthorized || errorCode != "admin_authentication_required" {
		t.Fatalf("plans with general access: %d %s", code, errorCode)
	}

	// No plans at first.
	var list struct {
		Plans []backups.PlanStatusView `json:"plans"`
	}
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodGet, base+"/plans", nil, "admin-password"), &list); code != http.StatusOK || len(list.Plans) != 0 {
		t.Fatalf("plans before creating: %d %+v", code, list)
	}

	// Refusals before anything is saved.
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPut, base+"/plans/offline", map[string]any{}, "admin-password")); code != http.StatusBadRequest || errorCode != "invalid_backup_plan" {
		t.Fatalf("a plan without a destination: %d %s", code, errorCode)
	}
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPut, base+"/plans/bad name", map[string]any{"destination": t.TempDir()}, "admin-password")); code != http.StatusBadRequest || errorCode != "invalid_backup_plan" {
		t.Fatalf("an invalid plan name: %d %s", code, errorCode)
	}
	relative := "relative/backups"
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPut, base+"/plans/offline", map[string]any{"destination": relative}, "admin-password")); code != http.StatusBadRequest || errorCode != "invalid_backup_plan" {
		t.Fatalf("a relative destination: %d %s", code, errorCode)
	}

	// Create with the defaults apart from scheduling, which stays off so
	// the scheduler does not start between requests; the state defaults
	// (on) are covered by the service tests.
	destination := filepath.Join(t.TempDir(), "offline-backups")
	var created struct {
		Plan     string               `json:"plan"`
		Schedule backups.ScheduleView `json:"schedule"`
		Warnings []string             `json:"warnings"`
	}
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodPut, base+"/plans/offline", map[string]any{"destination": destination, "scheduled": "off"}, "admin-password"), &created); code != http.StatusCreated ||
		created.Plan != "offline" || created.Schedule.State != backups.ScheduleOff || created.Schedule.Keep != 7 || created.Schedule.Interval != "1d" || created.Schedule.Destination != destination {
		t.Fatalf("created plan: %d %+v", code, created)
	}
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPut, base+"/plans/offline", map[string]any{"destination": destination}, "admin-password")); code != http.StatusConflict || errorCode != "backup_plan_exists" {
		t.Fatalf("a duplicate plan: %d %s", code, errorCode)
	}

	// Two plans may use the same destination; both start with scheduling
	// off so the service's scheduler cannot start between the requests.
	second := filepath.Join(t.TempDir(), "second-backups")
	if code, _ := checkStatus(t, adminAPIRequest(t, http.MethodPut, base+"/plans/other", map[string]any{"destination": second, "keep": 3, "verify": "off", "scheduled": "off"}, "admin-password")); code != http.StatusCreated {
		t.Fatalf("the second plan: %d", code)
	}

	// Change the plan without enabling its schedule, so the scheduler
	// cannot start between the requests.
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodPatch, base+"/plans/offline", map[string]any{"keep": 4}, "admin-password"), &created); code != http.StatusOK || created.Schedule.State != backups.ScheduleOff || created.Schedule.Keep != 4 {
		t.Fatalf("changed plan: %d %+v", code, created)
	}
	off := "off"
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPatch, base+"/plans/missing", map[string]any{"scheduled": off}, "admin-password")); code != http.StatusNotFound || errorCode != "backup_plan_not_found" {
		t.Fatalf("changing a missing plan: %d %s", code, errorCode)
	}

	// The list and one-plan read; an off plan shows no next run.
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodGet, base+"/plans", nil, "admin-password"), &list); code != http.StatusOK || len(list.Plans) != 2 {
		t.Fatalf("plans after creating: %d %+v", code, list)
	}
	var one struct {
		backups.PlanStatusView
	}
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodGet, base+"/plans/other", nil, "admin-password"), &one); code != http.StatusOK || one.Plan != "other" || one.Schedule.Keep != 3 || one.NextRun != nil {
		t.Fatalf("one plan: %d %+v", code, one)
	}
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodGet, base+"/plans/missing", nil, "admin-password")); code != http.StatusNotFound || errorCode != "backup_plan_not_found" {
		t.Fatalf("reading a missing plan: %d %s", code, errorCode)
	}

	// Start a backup of the plan; it names the plan.
	var started struct {
		Run backups.RunView `json:"run"`
	}
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodPost, base+"/plans/offline/runs", nil, "admin-password"), &started); code != http.StatusAccepted || started.Run.Plan != "offline" {
		t.Fatalf("plan back up now: %d %+v", code, started)
	}
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPost, base+"/plans/missing/runs", nil, "admin-password")); code != http.StatusNotFound || errorCode != "backup_plan_not_found" {
		t.Fatalf("backing up a missing plan: %d %s", code, errorCode)
	}
	// The legacy start accepts an optional plan name.
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodPost, base+"/runs", map[string]any{"plan": "missing"}, "admin-password")); code != http.StatusNotFound || errorCode != "backup_plan_not_found" {
		t.Fatalf("the legacy start with a missing plan: %d %s", code, errorCode)
	}
	// Wait for the offline run to finish so the plan is idle for deletion.
	waitForPlanRun(t, fixture, "offline")

	// Runs filter by plan, and each names its plan.
	var runs struct {
		Runs []backups.RunView `json:"runs"`
	}
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodGet, base+"/runs?plan=offline", nil, "admin-password"), &runs); code != http.StatusOK {
		t.Fatalf("filtered runs: %d", code)
	}
	if len(runs.Runs) == 0 {
		t.Fatal("no offline runs")
	}
	for _, run := range runs.Runs {
		if run.Plan != "offline" {
			t.Fatalf("a run of another plan: %+v", run)
		}
	}

	// The status lists both plans.
	var fullStatus struct {
		Plans []backups.PlanStatusView `json:"plans"`
	}
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodGet, base, nil, "admin-password"), &fullStatus); code != http.StatusOK || len(fullStatus.Plans) != 2 {
		t.Fatalf("status plans: %d %+v", code, fullStatus.Plans)
	}

	// An active plan cannot be deleted; insert a running record of other.
	running := state.BackupRun{ID: strings.Repeat("7", 32), Plan: "other", Kind: state.BackupRunManual,
		Status: state.BackupRunning, Destination: second, Verification: state.BackupVerifyNotRun, StartedAt: time.Now()}
	if err := fixture.store.StartBackupRun(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodDelete, base+"/plans/other", nil, "admin-password")); code != http.StatusConflict || errorCode != "backup_plan_active" {
		t.Fatalf("deleting an active plan: %d %s", code, errorCode)
	}
	if err := fixture.store.FinishBackupRun(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	// Idle plans delete; missing ones 404.
	if code, _ := checkStatus(t, adminAPIRequest(t, http.MethodDelete, base+"/plans/offline", nil, "admin-password")); code != http.StatusOK {
		t.Fatalf("deleting an idle plan: %d", code)
	}
	if code, errorCode := checkStatus(t, adminAPIRequest(t, http.MethodDelete, base+"/plans/missing", nil, "admin-password")); code != http.StatusNotFound || errorCode != "backup_plan_not_found" {
		t.Fatalf("deleting a missing plan: %d %s", code, errorCode)
	}
	// The deleted plan's runs stay listed with its name.
	if code := decodeBackupJSON(t, adminAPIRequest(t, http.MethodGet, base+"/runs?plan=offline", nil, "admin-password"), &runs); code != http.StatusOK || len(runs.Runs) == 0 {
		t.Fatalf("the deleted plan's runs did not stay: %d %+v", code, runs)
	}
}

// waitForPlanRun waits until the newest run of plan is no longer running.
func waitForPlanRun(t *testing.T, fixture apiFixture, plan string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		runs, err := fixture.store.BackupRunsOfPlan(context.Background(), plan)
		noErr(t, err)
		if len(runs) > 0 && runs[0].Status != state.BackupRunning {
			if runs[0].Status == state.BackupFailed {
				t.Fatalf("the %s run failed: %s", plan, runs[0].Message)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the backup run did not finish")
}
