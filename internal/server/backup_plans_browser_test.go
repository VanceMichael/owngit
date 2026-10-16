package server

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"owngit/internal/state"
	"owngit/internal/webui"
)

// The Storage & recovery dashboard manages named plans with native forms:
// create, choose, change, back up now and delete, each carrying its plan,
// with the same administrator confirmation and bilingual wording as the
// rest of the page.
func TestBackupPlansInTheDashboard(t *testing.T) {
	fixture := newAPIFixture(t, false)
	server, client, jar := openBrowser(t, fixture)
	storage := server.URL + webui.SettingsTabURL(webui.SettingsStorage)
	csrf := browserAdminSessionFor(t, fixture, server.URL, jar, "backups")
	askEveryTime(t, fixture.app)

	defaultDir := filepath.Join(t.TempDir(), "default-backups")
	offlineDir := filepath.Join(t.TempDir(), "offline-backups")
	post := func(values url.Values) browserHTTPResult {
		values.Set("csrf", csrf)
		values.Set("admin_password", "admin-password")
		return browserForm(t, client, storage, values, server.URL)
	}

	// The default plan is created through the old schedule form, which
	// keeps working.
	save := url.Values{
		"action":             {webui.ActionSaveBackupSchedule},
		"backup_destination": {defaultDir},
		"backup_interval":    {"1d"},
		"backup_keep":        {"7"},
		"backup_scheduled":   {"off"},
		"backup_verify":      {"off"},
	}
	if result := post(save); result.status != http.StatusSeeOther || !strings.Contains(result.header.Get("Location"), "notice=backup_saved") {
		t.Fatalf("default schedule: %d %s", result.status, result.header.Get("Location"))
	}

	// Before a second plan exists there is no chooser.
	page := browserGET(t, client, storage)
	if strings.Contains(page.body, `id="backup-plan-choose"`) {
		t.Fatal("the plan chooser is shown with one plan")
	}

	// A bad name and a duplicate are refused on the page.
	create := url.Values{
		"action":                 {webui.ActionCreateBackupPlan},
		"backup_new_plan":        {"bad name"},
		"backup_new_destination": {offlineDir},
	}
	if result := post(create); result.status != http.StatusUnprocessableEntity || !strings.Contains(result.body, webui.Text(webui.LangEN, webui.MsgBackupPlanName)) {
		t.Fatalf("a bad plan name: %d", result.status)
	}
	create.Set("backup_new_plan", "offline")
	if result := post(create); result.status != http.StatusSeeOther || !strings.Contains(result.header.Get("Location"), "notice=backup_plan_created") || !strings.Contains(result.header.Get("Location"), "backup_plan=offline") {
		t.Fatalf("create plan: %d %s", result.status, result.header.Get("Location"))
	}
	if result := post(create); result.status != http.StatusConflict || !strings.Contains(result.body, webui.Text(webui.LangEN, webui.MsgBackupPlanExistsRefused)) {
		t.Fatalf("a duplicate plan: %d", result.status)
	}

	// The chooser now exists, and the offline page shows its own settings.
	page = browserGET(t, client, storage+"?backup_plan=offline")
	if page.status != http.StatusOK || !strings.Contains(page.body, `id="backup-plan-choose"`) ||
		!strings.Contains(page.body, `value="offline" selected`) || !strings.Contains(page.body, `value="`+offlineDir+`"`) {
		t.Fatalf("the offline plan page is missing the chooser or its values: %d", page.status)
	}

	// The Korean page words the plan controls in Korean too.
	ko := browserGET(t, client, storage+"?backup_plan=offline&lang=ko")
	if !strings.Contains(ko.body, webui.Text(webui.LangKO, webui.MsgBackupCreatePlan)) ||
		!strings.Contains(ko.body, webui.Text(webui.LangKO, webui.MsgBackupPlansTitle)) {
		t.Fatal("the plan controls are not worded in Korean")
	}

	// Save changes the selected plan.
	change := url.Values{
		"action":             {webui.ActionSaveBackupPlan},
		"backup_plan":        {"offline"},
		"backup_destination": {offlineDir},
		"backup_interval":    {"7d"},
		"backup_keep":        {"5"},
		"backup_scheduled":   {"off"},
		"backup_verify":      {"off"},
	}
	if result := post(change); result.status != http.StatusSeeOther || !strings.Contains(result.header.Get("Location"), "backup_plan=offline") {
		t.Fatalf("save plan: %d %s", result.status, result.header.Get("Location"))
	}
	page = browserGET(t, client, storage+"?backup_plan=offline")
	if !strings.Contains(page.body, `value="5" data-saved="5"`) {
		t.Fatal("the saved keep is not shown")
	}

	// Creating the plan enabled it, so wait for the scheduled backup it
	// started before using the one slot by hand.
	waitUntilNoPlanRuns(t, fixture)
	// Back up now starts the selected plan, and the run list for that plan
	// names it.
	now := url.Values{"action": {webui.ActionBackupNow}, "backup_plan": {"offline"}}
	if result := post(now); result.status != http.StatusSeeOther || !strings.Contains(result.header.Get("Location"), "backup_plan=offline") {
		t.Fatalf("back up offline: %d %s", result.status, result.header.Get("Location"))
	}
	waitForPlanRunsInDashboard(t, fixture, "offline")
	runsPage := browserGET(t, client, storage+"?backup_plan=offline")
	if !strings.Contains(runsPage.body, ">offline<") {
		t.Fatal("the offline run does not name its plan")
	}

	// The default plan's page shows none of the offline runs.
	defaultPage := browserGET(t, client, storage)
	if strings.Contains(defaultPage.body, ">offline<") {
		t.Fatal("the default page shows another plan's runs")
	}

	// Deleting the idle plan removes it from the chooser and keeps its
	// history out of the page list.
	del := url.Values{"action": {webui.ActionDeleteBackupPlan}, "backup_plan": {"offline"}}
	if result := post(del); result.status != http.StatusSeeOther || !strings.Contains(result.header.Get("Location"), "notice=backup_plan_deleted") {
		t.Fatalf("delete plan: %d", result.status)
	}
	page = browserGET(t, client, storage)
	if strings.Contains(page.body, `value="offline"`) {
		t.Fatal("the deleted plan is still offered")
	}
	runs, err := fixture.app.Backups.RunsOfPlan(context.Background(), "offline")
	noErr(t, err)
	if len(runs) != 2 {
		t.Fatalf("the deleted plan lost its run history: %d", len(runs))
	}
}

// waitForPlanRunsInDashboard polls until the newest run of plan is no
// longer running.
func waitForPlanRunsInDashboard(t *testing.T, fixture apiFixture, plan string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		runs, err := fixture.app.Backups.RunsOfPlan(context.Background(), plan)
		noErr(t, err)
		if len(runs) > 0 && runs[0].Status != state.BackupRunning {
			if runs[0].Status == state.BackupFailed {
				t.Fatalf("the %s run failed: %s", plan, runs[0].Message)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the plan run did not finish")
}

// waitUntilNoPlanRuns waits until no plan has a running backup.
func waitUntilNoPlanRuns(t *testing.T, fixture apiFixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		status, err := fixture.app.Backups.Status(context.Background())
		noErr(t, err)
		busy := false
		for _, plan := range status.Plans {
			if plan.Running != nil {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("a plan backup stayed running")
}
