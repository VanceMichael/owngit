package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"owngit/internal/backups"
	"owngit/internal/state"
	"owngit/internal/webui"
)

// backupRunsQueryAllowed accepts the plan filter of GET backup runs, once.
func backupRunsQueryAllowed(request *http.Request) bool {
	if request.Method != http.MethodGet || request.URL.Path != "/api/v1/backups/runs" {
		return false
	}
	for key, values := range request.URL.Query() {
		if key != "plan" || len(values) != 1 || values[0] == "" {
			return false
		}
	}
	return true
}

// backupScheduleJSON is a schedule or plan change, and the body of a plan
// create: every field is optional for a change, while a new plan needs a
// destination. Scheduled is on the API, verify is on the schedule.
type backupScheduleJSON struct {
	Destination *string `json:"destination,omitempty"`
	// Scheduled is "on" or "off".
	Scheduled *string `json:"scheduled,omitempty"`
	// Interval is one of backups.Intervals.
	Interval *string `json:"interval,omitempty"`
	Keep     *int    `json:"keep,omitempty"`
	// Verify is "on" or "off".
	Verify *string `json:"verify,omitempty"`
	// Plan names the plan a manual backup starts; the calls that predate
	// named plans omit it and start the default plan.
	Plan *string `json:"plan,omitempty"`
}

// handleBackupsAPI serves, with the administrator password,
// /api/v1/backups (status), /backups/plans (named plans),
// /backups/plans/{name} (one plan), /backups/plans/{name}/runs (back up
// that plan now), /backups/runs (list, and POST to back up the default
// plan), /backups/runs/ID/verify (POST: verify that backup again),
// /backups/schedule (GET, PATCH: the default plan), and
// /backups/upload (PUT: upload a backup archive to restore) and
// /backups/summary, which names neither, is read with general access so
// coding agents can check that backups work, too. Folders and messages are
// administrator information.
func (app *App) handleBackupsAPI(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	switch {
	case request.URL.Path == "/api/v1/backups", request.URL.Path == "/api/v1/backups/summary":
		summary := request.URL.Path == "/api/v1/backups/summary"
		username, _, hasBasic := request.BasicAuth()
		if summary && !(hasBasic && username == "admin") {
			if !app.authorizeAPI(writer, request, settings) {
				return
			}
		} else if !app.authorizeAdminAPI(writer, request) {
			return
		}
		status, err := app.Backups.Status(request.Context())
		if err != nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "state_unavailable", "The backup status could not be read. Try again later.", nil)
			return
		}
		if summary {
			writeAPIJSON(writer, http.StatusOK, struct {
				OK bool `json:"ok"`
				backups.Summary
			}{true, backups.Summarize(status)})
			return
		}
		var restore *webui.BackupRestore
		upload := status.Upload
		if upload != nil && upload.Status == backups.UploadPassed && upload.Path != "" {
			restore = app.restoreGuide(upload.Path, settings)
		}
		writeAPIJSON(writer, http.StatusOK, struct {
			OK bool `json:"ok"`
			backups.Status
			UploadRestoreCommand string `json:"upload_restore_command,omitempty"`
			UploadRestoreProblem string `json:"upload_restore_problem,omitempty"`
		}{true, status, command(restore), problem(restore)})

	case request.URL.Path == "/api/v1/backups/plans":
		if request.Method != http.MethodGet {
			writeAPIMethodError(writer, http.MethodGet)
			return
		}
		if !app.authorizeAdminAPI(writer, request) {
			return
		}
		app.backupPlansList(writer, request)

	case strings.HasPrefix(request.URL.Path, "/api/v1/backups/plans/"):
		if !app.authorizeAdminAPI(writer, request) {
			return
		}
		app.backupPlanAPI(writer, request)

	case request.URL.Path == "/api/v1/backups/runs":
		if !app.authorizeAdminAPI(writer, request) {
			return
		}
		if request.Method == http.MethodGet {
			plan := request.URL.Query().Get("plan")
			var (
				runs []backups.RunView
				err  error
			)
			if plan != "" {
				runs, err = app.Backups.RunsOfPlan(request.Context(), plan)
			} else {
				runs, err = app.Backups.Runs(request.Context())
			}
			if err != nil {
				writeAPIError(writer, http.StatusServiceUnavailable, "state_unavailable", "The backup runs could not be read. Try again later.", nil)
				return
			}
			writeAPIJSON(writer, http.StatusOK, struct {
				OK   bool              `json:"ok"`
				Runs []backups.RunView `json:"runs"`
			}{true, runs})
			return
		}
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, "GET, POST")
			return
		}
		app.startBackup(writer, request)

	case strings.HasPrefix(request.URL.Path, "/api/v1/backups/runs/"):
		id, operation, found := strings.Cut(strings.TrimPrefix(request.URL.Path, "/api/v1/backups/runs/"), "/")
		if !found || id == "" || (operation != "verify" && operation != "archive") {
			writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
			return
		}
		method := map[string]string{"verify": http.MethodPost, "archive": http.MethodGet}[operation]
		if request.Method != method {
			writeAPIMethodError(writer, method)
			return
		}
		if !app.authorizeAdminAPI(writer, request) {
			return
		}
		if operation == "verify" {
			app.checkBackup(writer, request, id)
			return
		}
		download, err := app.Backups.OpenDownload(request.Context(), id)
		if err != nil {
			writeBackupAPIError(writer, request, err, "backup download")
			return
		}
		app.writeBackupArchive(writer, request, download)

	case request.URL.Path == "/api/v1/backups/schedule":
		if request.Method != http.MethodGet && request.Method != http.MethodPatch {
			writeAPIMethodError(writer, "GET, PATCH")
			return
		}
		if !app.authorizeAdminAPI(writer, request) {
			return
		}
		app.backupSchedule(writer, request)

	case request.URL.Path == "/api/v1/backups/upload":
		if request.Method != http.MethodPut {
			writeAPIMethodError(writer, http.MethodPut)
			return
		}
		if !app.authorizeAdminAPI(writer, request) {
			return
		}
		app.uploadBackup(writer, request)

	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
	}
}

func (app *App) backupPlansList(writer http.ResponseWriter, request *http.Request) {
	status, err := app.Backups.Status(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "state_unavailable", "The backup plans could not be read. Try again later.", nil)
		return
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		OK    bool                     `json:"ok"`
		Plans []backups.PlanStatusView `json:"plans"`
	}{true, status.Plans})
}

// backupPlanAPI serves one plan: PUT creates it, PATCH changes it, DELETE
// removes it when it is idle, GET reads it, and POST .../runs starts a
// backup of it.
func (app *App) backupPlanAPI(writer http.ResponseWriter, request *http.Request) {
	suffix := strings.TrimPrefix(request.URL.Path, "/api/v1/backups/plans/")
	name, suffix, subroute := strings.Cut(suffix, "/")
	if name == "" || (subroute && suffix != "runs") || (!subroute && suffix != "") {
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
		return
	}
	switch {
	case subroute && suffix == "runs":
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		run, err := app.Backups.StartPlanNow(name)
		if err != nil {
			writeBackupPlanError(writer, request, "start", err)
			return
		}
		writeAPIJSON(writer, http.StatusAccepted, struct {
			OK  bool             `json:"ok"`
			Run *backups.RunView `json:"run"`
		}{true, backups.ViewRun(run)})
	default:
		switch request.Method {
		case http.MethodGet:
			app.backupPlanRead(writer, request, name)
		case http.MethodPut:
			app.backupPlanSave(writer, request, name, true)
		case http.MethodPatch:
			app.backupPlanSave(writer, request, name, false)
		case http.MethodDelete:
			if err := app.Backups.DeletePlan(request.Context(), name); err != nil {
				writeBackupPlanError(writer, request, "delete", err)
				return
			}
			writeAPIJSON(writer, http.StatusOK, struct {
				OK   bool   `json:"ok"`
				Plan string `json:"plan"`
			}{true, name})
		default:
			writeAPIMethodError(writer, "GET, PUT, PATCH, DELETE")
		}
	}
}

func (app *App) backupPlanRead(writer http.ResponseWriter, request *http.Request, name string) {
	status, err := app.Backups.Status(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "state_unavailable", "The backup plan could not be read. Try again later.", nil)
		return
	}
	for _, plan := range status.Plans {
		if plan.Plan == name {
			writeAPIJSON(writer, http.StatusOK, struct {
				OK bool `json:"ok"`
				backups.PlanStatusView
			}{true, plan})
			return
		}
	}
	writeAPIError(writer, http.StatusNotFound, "backup_plan_not_found", "The backup plan does not exist.", nil)
}

func (app *App) backupPlanSave(writer http.ResponseWriter, request *http.Request, name string, create bool) {
	var input backupScheduleJSON
	if !decodeAPIJSON(writer, request, &input) {
		return
	}
	change, problem := backupChange(input)
	if problem != "" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_backup_plan", problem, nil)
		return
	}
	var (
		plan state.BackupPlan
		err  error
	)
	if create {
		plan, err = app.Backups.CreatePlan(request.Context(), name, change)
	} else {
		plan, err = app.Backups.ChangePlan(request.Context(), name, change)
	}
	if err != nil {
		writeBackupPlanError(writer, request, "save", err)
		return
	}
	var warnings []string
	if create {
		if !plan.Enabled {
			warnings = append(warnings, webui.Text(webui.LangEN, webui.MsgBackupScheduleOffWarning))
		}
		if !plan.Verify {
			warnings = append(warnings, webui.Text(webui.LangEN, webui.MsgBackupVerifyOffWarning))
		}
	}
	code := http.StatusOK
	if create {
		code = http.StatusCreated
	}
	writeAPIJSON(writer, code, struct {
		OK       bool                 `json:"ok"`
		Plan     string               `json:"plan"`
		Schedule backups.ScheduleView `json:"schedule"`
		Warnings []string             `json:"warnings,omitempty"`
	}{true, plan.Name, backups.ViewSchedule(plan.BackupSchedule, true), warnings})
}

// writeBackupAPIError answers a backup operation that did not happen.
func writeBackupAPIError(writer http.ResponseWriter, request *http.Request, err error, step string) {
	if refusal, ok := backupRefused(err); ok {
		writeAPIError(writer, refusal.status, refusal.code, refusal.apiMessage(), nil)
		return
	}
	writeAPIError(writer, unavailable(request, step, err), "state_unavailable", "The backup operation could not be carried out. Try again later.", nil)
}

// writeBackupPlanError maps the plan service's refusals to API answers.
func writeBackupPlanError(writer http.ResponseWriter, request *http.Request, step string, err error) {
	var refused *backups.ChangeError
	if errors.As(err, &refused) {
		writeAPIError(writer, http.StatusBadRequest, "invalid_backup_plan", refused.Message, nil)
		return
	}
	switch {
	case errors.Is(err, backups.ErrPlanNotFound):
		writeAPIError(writer, http.StatusNotFound, "backup_plan_not_found", "The backup plan does not exist.", nil)
	case errors.Is(err, backups.ErrPlanExists):
		writeAPIError(writer, http.StatusConflict, "backup_plan_exists", "A backup plan with this name already exists.", nil)
	case errors.Is(err, backups.ErrPlanActive):
		writeAPIError(writer, http.StatusConflict, "backup_plan_active", "The backup plan is still making a backup. Wait for it to finish, then delete it.", nil)
	case errors.Is(err, backups.ErrPlanLimit):
		writeAPIError(writer, http.StatusBadRequest, "backup_plan_limit", "There are already as many backup plans as OwnGit keeps.", nil)
	case errors.Is(err, backups.ErrNotConfigured):
		writeAPIError(writer, http.StatusConflict, "backup_not_configured", "Choose a backup folder first.", nil)
	default:
		writeBackupAPIError(writer, request, err, step+" backup plan")
	}
}

func command(restore *webui.BackupRestore) string {
	if restore != nil {
		return restore.Command
	}
	return ""
}

func problem(restore *webui.BackupRestore) string {
	if restore == nil || restore.Unchecked == "" {
		return ""
	}
	return "OwnGit could not check where " + restore.Unchecked + " will be once the state folder is renamed, so it gives no restore command: " + restore.Problem
}

func (app *App) checkBackup(writer http.ResponseWriter, request *http.Request, id string) {
	check, err := app.Backups.StartCheck(request.Context(), id)
	if err != nil {
		writeBackupAPIError(writer, request, err, "backup verification start")
		return
	}
	writeAPIJSON(writer, http.StatusAccepted, struct {
		OK bool `json:"ok"`
		backups.Check
	}{true, check})
}

func (app *App) uploadBackup(writer http.ResponseWriter, request *http.Request) {
	if request.ContentLength <= 0 {
		writeAPIError(writer, http.StatusLengthRequired, "length_required", webui.Text(webui.LangEN, webui.MsgBackupUploadNoSize), nil)
		return
	}
	request = app.beginTransfer(request)
	upload, err := app.Backups.ReceiveUpload(request.Context(), idleReader{request.Body, http.NewResponseController(writer)}, request.ContentLength)
	if err != nil {
		writeBackupAPIError(writer, request, err, "backup upload")
		return
	}
	writeAPIJSON(writer, http.StatusAccepted, struct {
		OK     bool           `json:"ok"`
		Upload backups.Upload `json:"upload"`
	}{true, upload})
}

// startBackup answers POST /runs with the backup of the named plan, or the
// default plan when the body names none.
func (app *App) startBackup(writer http.ResponseWriter, request *http.Request) {
	var input backupScheduleJSON
	if request.Body != nil && request.ContentLength != 0 {
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
	}
	var (
		run state.BackupRun
		err error
	)
	if input.Plan != nil && *input.Plan != "" {
		run, err = app.Backups.StartPlanNow(*input.Plan)
	} else {
		run, err = app.Backups.StartNow()
	}
	if err != nil {
		writeBackupPlanError(writer, request, "start", err)
		return
	}
	writeAPIJSON(writer, http.StatusAccepted, struct {
		OK  bool             `json:"ok"`
		Run *backups.RunView `json:"run"`
	}{true, backups.ViewRun(run)})
}

func (app *App) backupSchedule(writer http.ResponseWriter, request *http.Request) {
	var warnings []string
	if request.Method == http.MethodPatch {
		var input backupScheduleJSON
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		change, problem := backupChange(input)
		if problem != "" {
			writeAPIError(writer, http.StatusBadRequest, "invalid_backup_schedule", problem, nil)
			return
		}
		before, configured, after, err := app.Backups.ChangeSchedule(request.Context(), change)
		var refused *backups.ChangeError
		switch {
		case errors.As(err, &refused):
			writeAPIError(writer, http.StatusBadRequest, "invalid_backup_schedule", refused.Message, nil)
			return
		case err != nil:
			writeAPIError(writer, unavailable(request, "backup schedule save", err), "state_unavailable", "The backup schedule could not be saved. Try again later.", nil)
			return
		}
		if !after.Enabled && (!configured || before.Enabled) {
			warnings = append(warnings, webui.Text(webui.LangEN, webui.MsgBackupScheduleOffWarning))
		}
		if !after.Verify && (!configured || before.Verify) {
			warnings = append(warnings, webui.Text(webui.LangEN, webui.MsgBackupVerifyOffWarning))
		}
	}
	schedule, configured, err := app.Store.BackupSchedule(request.Context())
	if err != nil {
		writeAPIError(writer, unavailable(request, "backup schedule read", err), "state_unavailable", "The backup schedule could not be read. Try again later.", nil)
		return
	}
	writeAPIJSON(writer, http.StatusOK, struct {
		OK       bool                 `json:"ok"`
		Schedule backups.ScheduleView `json:"schedule"`
		Warnings []string             `json:"warnings,omitempty"`
	}{true, backups.ViewSchedule(schedule, configured), warnings})
}

// backupChange reads a schedule/plan change, or says what is wrong with it.
func backupChange(input backupScheduleJSON) (backups.ScheduleChange, string) {
	var change backups.ScheduleChange
	if input.Scheduled != nil {
		on, valid := parseOnOff(*input.Scheduled)
		if !valid {
			return change, "scheduled must be on or off."
		}
		change.Enabled = &on
	}
	if input.Interval != nil {
		var interval *time.Duration
		names := make([]string, 0, len(backups.Intervals))
		for _, choice := range backups.Intervals {
			names = append(names, choice.Name)
			if choice.Name == *input.Interval {
				interval = &choice.Interval
			}
		}
		if interval == nil {
			return change, "interval must be one of " + strings.Join(names, ", ") + "."
		}
		change.Interval = interval
	}
	change.Destination = input.Destination
	change.Keep = input.Keep
	if input.Verify != nil {
		on, valid := parseOnOff(*input.Verify)
		if !valid {
			return change, "verify must be on or off."
		}
		change.Verify = &on
	}
	return change, ""
}
