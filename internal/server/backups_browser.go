package server

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"owngit/internal/backups"
	"owngit/internal/recovery"
	"owngit/internal/state"
	"owngit/internal/webui"
)

// The Backups groups of Settings, Storage & recovery (templates/backups.html):
// the schedule, Back up now, verifying and downloading a listed backup, and
// uploading a backup to restore. Each is an administrator change, behind
// the same CSRF check and administrator confirmation as every Settings
// form, and uses the backups.Service operations the owner API uses.

// backupRefusal is how a refused backup operation is answered, in the
// dashboard and the API alike.
type backupRefusal struct {
	status  int
	code    string
	message webui.MessageCode
	// detail is the cause in English, shown beside the message.
	detail string
	// cause is the error behind the refusal, which the dashboard words
	// itself when OwnGit knows the reason.
	cause error
}

// backupRefused describes err when it is a refusal the owner can act on,
// and false when it is a failure to answer as unavailable.
func backupRefused(err error) (backupRefusal, bool) {
	switch {
	case errors.Is(err, state.ErrBackupRunning):
		return backupRefusal{http.StatusConflict, "backup_running", webui.MsgBackupRunningRefused, "", nil}, true
	case errors.Is(err, backups.ErrBusy):
		return backupRefusal{http.StatusConflict, "backup_busy", webui.MsgBackupBusy, "", nil}, true
	case errors.Is(err, backups.ErrNotConfigured):
		return backupRefusal{http.StatusConflict, "backup_not_configured", webui.MsgBackupChooseFolder, "", nil}, true
	case errors.Is(err, backups.ErrNoBackup):
		return backupRefusal{http.StatusNotFound, "backup_not_found", webui.MsgBackupNoBackup, "", nil}, true
	case errors.Is(err, backups.ErrBackupGone):
		return backupRefusal{http.StatusConflict, "backup_gone", webui.MsgBackupGone, err.Error(), nil}, true
	case errors.Is(err, backups.ErrUploadRefused):
		return backupRefusal{http.StatusBadRequest, "invalid_backup_archive", webui.MsgBackupUploadRefused, err.Error(), err}, true
	}
	return backupRefusal{}, false
}

// apiMessage is the refusal in English, for the API.
func (r backupRefusal) apiMessage() string {
	message := webui.Text(webui.LangEN, r.message)
	if r.detail != "" {
		message += " " + r.detail
	}
	return message
}

// notice is the refusal for the dashboard. A refused upload says its
// reason in the catalog; any other detail drops the error's own English lead.
func (r backupRefusal) notice() webui.Notice {
	if errors.Is(r.cause, backups.ErrUploadRefused) {
		return uploadRefusal(r.cause)
	}
	notice := webui.Error("", r.message)
	notice.Detail = strings.TrimPrefix(r.detail, backups.ErrBackupGone.Error()+": ")
	return notice
}

// uploadRefusal words why an upload was refused: the reason OwnGit knows in
// the catalog, with the entry or name it is about as the detail, and the
// reader's or system's own text for any other.
func uploadRefusal(err error) webui.Notice {
	var archive *recovery.ArchiveError
	code, detail := webui.MsgBackupUploadRefused, strings.TrimPrefix(err.Error(), backups.ErrUploadRefused.Error()+": ")
	switch {
	case errors.Is(err, backups.ErrUploadNoSize):
		code, detail = webui.MsgBackupUploadDeclaredNoSize, ""
	case errors.Is(err, backups.ErrUploadStopped):
		code, detail = webui.MsgBackupUploadStopped, ""
	case errors.As(err, &archive):
		known := map[recovery.ArchiveReason]webui.MessageCode{
			recovery.ArchiveEndsEarly: webui.MsgBackupUploadEndsEarly, recovery.ArchiveNoManifest: webui.MsgBackupUploadNoManifest,
			recovery.ArchiveUnsafePath: webui.MsgBackupUploadUnsafePath, recovery.ArchiveFolderName: webui.MsgBackupUploadFolderName,
			recovery.ArchiveManyFolders: webui.MsgBackupUploadManyFolders, recovery.ArchiveDuplicate: webui.MsgBackupUploadDuplicate,
			recovery.ArchiveUnknownEntry: webui.MsgBackupUploadUnknownEntry, recovery.ArchiveBeforeFolder: webui.MsgBackupUploadBeforeFolder,
		}
		if reason, ok := known[archive.Reason]; ok {
			code, detail = reason, archive.Value
		} else {
			detail = archive.Value
		}
	}
	return webui.Error("", code).WithDetail(detail)
}

// backupsInfo reads the Backups groups for the Settings page. visible is
// false for a visitor who is not a confirmed administrator, who sees none
// of it.
func (app *App) backupsInfo(request *http.Request, settings state.Settings, visible bool) (webui.BackupsInfo, error) {
	info := webui.BackupsInfo{Visible: visible}
	if !visible || app.Backups == nil {
		return info, nil
	}
	status, err := app.Backups.Status(request.Context())
	if err != nil {
		return info, err
	}
	selected := selectedBackupPlan(request, status)
	info.SelectedPlan = selected
	for _, plan := range status.Plans {
		info.Plans = append(info.Plans, webui.BackupPlanInfo{Name: plan.Plan, State: plan.Schedule.State, Selected: plan.Plan == selected})
	}
	info.ChoosePlans = len(status.Plans) > 1
	for _, choice := range backups.Intervals {
		info.Intervals = append(info.Intervals, choice.Name)
	}
	var selectedView *backups.PlanStatusView
	for index := range status.Plans {
		if status.Plans[index].Plan == selected {
			selectedView = &status.Plans[index]
			break
		}
	}
	if selectedView != nil {
		schedule := selectedView.Schedule
		info.Configured = true
		info.Scheduled, info.Destination, info.Interval, info.Keep, info.Verify = schedule.State == backups.ScheduleOn, schedule.Destination, schedule.Interval, schedule.Keep, *schedule.Verify
		info.Running, info.LastRun, info.LastVerified = app.backupRun(selectedView.Running, settings), app.backupRun(selectedView.LastRun, settings), app.backupRun(selectedView.LastVerified, settings)
		if next := selectedView.NextRun; next != nil {
			info.NextRun = *next
			info.NextRunNow = selectedView.Running != nil && !next.After(app.now())
		}
		if limit := selectedView.RestoreLimit; limit != nil {
			info.RestoreLimit = limit.FileSystem
		}
		runs, err := app.Backups.RunsOfPlan(request.Context(), selected)
		if err != nil {
			return info, err
		}
		for index := range runs {
			info.Runs = append(info.Runs, *app.backupRun(&runs[index], settings))
		}
	}
	if check := status.Check; check != nil {
		info.Check = &webui.BackupCheckInfo{Name: check.BackupName, Status: check.Status, Message: check.Message, MessageKO: backupMessageKO(check.Message)}
	}
	if upload := status.Upload; upload != nil {
		info.Upload = &webui.BackupUploadInfo{Name: upload.Name, Size: upload.Size, Status: upload.Status, Message: upload.Message, MessageKO: backupMessageKO(upload.Message)}
		if upload.RemovesAt != nil {
			info.Upload.RemovesAt = *upload.RemovesAt
		}
		if upload.Status == backups.UploadPassed && upload.Path != "" {
			info.Upload.Restore = app.restoreGuide(upload.Path, settings)
		}
	}
	info.Busy = globalBackupBusy(status)
	return info, nil
}

// globalBackupBusy says whether anything occupies the one backup slot.
func globalBackupBusy(status backups.Status) bool {
	for _, plan := range status.Plans {
		if plan.Running != nil {
			return true
		}
	}
	return status.Check != nil && status.Check.Status == backups.CheckRunning ||
		status.Upload != nil && status.Upload.Status == backups.UploadVerifying
}

// selectedBackupPlan is the plan the storage page shows: the one its query
// names when it still exists, otherwise the default plan.
func selectedBackupPlan(request *http.Request, status backups.Status) string {
	if name := strings.TrimSpace(request.URL.Query().Get("backup_plan")); name != "" {
		for _, plan := range status.Plans {
			if plan.Plan == name {
				return name
			}
		}
	}
	return state.DefaultBackupPlanName
}

// backupRun describes a run for the dashboard, with how to restore its
// backup when it has one.
func (app *App) backupRun(run *backups.RunView, settings state.Settings) *webui.BackupRunInfo {
	if run == nil {
		return nil
	}
	info := &webui.BackupRunInfo{
		ID: run.ID, Plan: run.Plan, Kind: run.Kind, Status: run.Status, Verification: run.Verification, Name: run.BackupName, Path: run.Path,
		Message: run.Message, StartedAt: run.StartedAt, HoldRepository: run.LongestHoldRepository,
	}
	if ko := backupMessageKO(info.Message); ko != info.Message {
		info.MessageEN, info.MessageKO = info.Message, ko
	}
	if run.FinishedAt != nil {
		info.FinishedAt = *run.FinishedAt
	}
	if run.LongestHoldMS != nil {
		info.HoldKnown, info.HoldMS = true, *run.LongestHoldMS
	}
	if run.Path != "" && run.Status != state.BackupRunning {
		info.Restore = app.restoreGuide(run.Path, settings)
	}
	return info
}

// backupMessageKO is a message of a backup, a verification or an upload in
// Korean: its fixed sentences come from the catalog, and the technical
// cause that follows each stays as the program wrote it. It is message
// itself when no fixed sentence is in it.
func backupMessageKO(message string) string {
	text := func(code webui.MessageCode) string { return webui.Text(webui.LangKO, code) }
	tail := message[strings.LastIndexByte(message, '\n')+1:]
	countText, _, _ := strings.Cut(tail, " ")
	omitted, parseErr := strconv.Atoi(countText)
	hasOmitted := parseErr == nil && omitted > 0 && tail == strings.Replace(recovery.OmittedAliasNotice, "%d", countText, 1)
	if hasOmitted {
		message = strings.TrimSuffix(message, tail)
	}
	message = strings.NewReplacer(
		recovery.AliasBranchNotice, text(webui.MsgBackupAliasBranches),
		recovery.MissingAliasBranchNotice, text(webui.MsgBackupMissingAliasBranches),
		recovery.UnresolvedAliasBranchNotice, text(webui.MsgBackupUnresolvedAliasBranches),
		backups.InterruptedMessage, text(webui.MsgBackupTextInterrupted),
		backups.NotVerifiedLead, text(webui.MsgBackupTextNotVerified),
		backups.CompleteButLead, text(webui.MsgBackupTextComplete),
		backups.AlsoLead, " "+text(webui.MsgBackupTextAlso),
		backups.UploadFailedLead, text(webui.MsgBackupTextUploadFailed),
		backups.NotRemovedLead, " "+text(webui.MsgBackupTextNotRemoved),
		backups.VerifyStopped, text(webui.MsgBackupTextVerifyStopped),
		backups.FolderChanged, text(webui.MsgBackupTextFolderChanged),
		backups.NotRecordedLead, text(webui.MsgBackupTextNotRecorded),
	).Replace(message)
	if hasOmitted {
		message += strings.Replace(text(webui.MsgBackupOmittedAliases), "%d", countText, 1)
	}
	return message
}

// restoreGuide is how to restore the backup at input on this computer, or
// nil when this server was not told how OwnGit runs here.
func (app *App) restoreGuide(input string, settings state.Settings) *webui.BackupRestore {
	if app.RestoreGuide == nil {
		return nil
	}
	return app.RestoreGuide(input, settings.RepositoryRoot)
}

// backupNotices names the result of each saved backup change, shown in its
// group (settingsNoticeGroups): the schedule's with the warnings of what
// the save turned off.
var backupNotices = map[string][]webui.Notice{
	"backup_saved":                {webui.Success(webui.MsgBackupSaved)},
	"backup_saved_off":            {webui.Success(webui.MsgBackupSaved), {Kind: webui.NoticeWarning, Code: webui.MsgBackupScheduleOffWarning}},
	"backup_saved_unverified":     {webui.Success(webui.MsgBackupSaved), {Kind: webui.NoticeWarning, Code: webui.MsgBackupVerifyOffWarning}},
	"backup_saved_off_unverified": {webui.Success(webui.MsgBackupSaved), {Kind: webui.NoticeWarning, Code: webui.MsgBackupScheduleOffWarning}, {Kind: webui.NoticeWarning, Code: webui.MsgBackupVerifyOffWarning}},
	"backup_started":              {webui.Success(webui.MsgBackupStarted)},
	"backup_check_started":        {webui.Success(webui.MsgBackupCheckStarted)},
	"backup_upload_received":      {webui.Success(webui.MsgBackupUploadReceived)},
	"backup_plan_created":         {webui.Success(webui.MsgBackupPlanCreated)},
	"backup_plan_deleted":         {webui.Success(webui.MsgBackupPlanDeleted)},
}

// backupNoticeGroup is the group that shows a backup result notice, or "".
func backupNoticeGroup(notice string) string {
	switch {
	case backupNotices[notice] == nil:
		return ""
	case strings.HasPrefix(notice, "backup_saved"), notice == "backup_plan_created", notice == "backup_plan_deleted":
		return webui.GroupBackups
	}
	return webui.GroupBackupRuns
}

// backupAction carries out a backup form of the Settings page, which
// passed the CSRF check and the administrator confirmation.
func (app *App) backupAction(writer http.ResponseWriter, request *http.Request, settings state.Settings, csrf, action string) {
	refuse := func(status int, notices ...webui.Notice) {
		app.renderSettingsPage(writer, request, settings, csrf, action, notices, status, settingsView{AdminVerified: true})
	}
	answer := func(err error, step string) {
		if refusal, ok := backupRefused(err); ok {
			refuse(refusal.status, refusal.notice())
			return
		}
		refuse(unavailable(request, step, err), webui.Error("", webui.MsgBackupFailed))
	}
	switch action {
	case webui.ActionCreateBackupPlan:
		app.backupCreatePlan(writer, request, settings, refuse, action)
	case webui.ActionDeleteBackupPlan:
		name := strings.TrimSpace(postValue(request, "backup_plan"))
		if err := app.Backups.DeletePlan(request.Context(), name); err != nil {
			answerBackupPlanError(err, name, "", refuse, request, "backup plan delete")
			return
		}
		app.settingsSaved(writer, request, backupPlanResultURL(action, "backup_plan_deleted", ""))
	case webui.ActionSaveBackupPlan:
		name := strings.TrimSpace(postValue(request, "backup_plan"))
		if name == "" {
			name = state.DefaultBackupPlanName
		}
		change, notices := backupScheduleForm(request)
		if len(notices) > 0 {
			refuse(http.StatusUnprocessableEntity, notices...)
			return
		}
		plan, err := app.Backups.ChangePlan(request.Context(), name, change)
		var refused *backups.ChangeError
		if errors.As(err, &refused) {
			refuse(http.StatusUnprocessableEntity, scheduleRefusal(refused))
			return
		}
		if err != nil {
			answerBackupPlanError(err, name, "", refuse, request, "backup plan save")
			return
		}
		app.settingsSaved(writer, request, backupPlanResultURL(action, "backup_saved", plan.Name))
	case webui.ActionSaveBackupSchedule:
		change, notices := backupScheduleForm(request)
		if len(notices) > 0 {
			refuse(http.StatusUnprocessableEntity, notices...)
			return
		}
		before, configured, after, err := app.Backups.ChangeSchedule(request.Context(), change)
		var refused *backups.ChangeError
		if errors.As(err, &refused) {
			refuse(http.StatusUnprocessableEntity, scheduleRefusal(refused))
			return
		}
		if err != nil {
			answer(err, "backup schedule save")
			return
		}
		notice := "backup_saved"
		if !after.Enabled && (!configured || before.Enabled) {
			notice += "_off"
		}
		if !after.Verify && (!configured || before.Verify) {
			notice += "_unverified"
		}
		app.settingsSaved(writer, request, settingsResultURL(action, notice))
	case webui.ActionBackupNow:
		name := strings.TrimSpace(postValue(request, "backup_plan"))
		var err error
		if name != "" {
			_, err = app.Backups.StartPlanNow(name)
		} else {
			_, err = app.Backups.StartNow()
		}
		if err != nil {
			answer(err, "backup start")
			return
		}
		app.settingsSaved(writer, request, backupPlanResultURL(action, "backup_started", name))
	case webui.ActionBackupVerify:
		if _, err := app.Backups.StartCheck(request.Context(), request.URL.Query().Get("run")); err != nil {
			answer(err, "backup verification start")
			return
		}
		app.settingsSaved(writer, request, settingsResultURL(action, "backup_check_started"))
	case webui.ActionBackupDownload:
		download, err := app.Backups.OpenDownload(request.Context(), request.URL.Query().Get("run"))
		if err != nil {
			answer(err, "backup download")
			return
		}
		app.writeBackupArchive(writer, request, download)
	}
}

// backupCreatePlan handles the new-plan form: a name and a folder, both
// required, with the service applying the defaults.
func (app *App) backupCreatePlan(writer http.ResponseWriter, request *http.Request, settings state.Settings, refuse func(int, ...webui.Notice), action string) {
	name := strings.TrimSpace(postValue(request, "backup_new_plan"))
	destination := strings.TrimSpace(postValue(request, "backup_new_destination"))
	if name == "" {
		refuse(http.StatusUnprocessableEntity, webui.Error("backup_new_plan", webui.MsgBackupPlanName).WithDetail("The plan name is required."))
		return
	}
	if err := state.ValidateBackupPlanName(name); err != nil {
		refuse(http.StatusUnprocessableEntity, webui.Error("backup_new_plan", webui.MsgBackupPlanName).WithDetail(err.Error()))
		return
	}
	if destination == "" {
		refuse(http.StatusUnprocessableEntity, webui.Error("backup_new_destination", webui.MsgBackupChooseFolder))
		return
	}
	plan, err := app.Backups.CreatePlan(request.Context(), name, backups.ScheduleChange{Destination: &destination})
	var refused *backups.ChangeError
	if errors.As(err, &refused) {
		refuse(http.StatusUnprocessableEntity, planCreationRefusal(refused))
		return
	}
	if err != nil {
		answerBackupPlanError(err, name, "backup_new_plan", refuse, request, "backup plan create")
		return
	}
	app.settingsSaved(writer, request, backupPlanResultURL(action, "backup_plan_created", plan.Name))
}

// planCreationRefusal words a refusal while creating a plan at the right
// field: the folder errors belong to the new destination field.
func planCreationRefusal(refused *backups.ChangeError) webui.Notice {
	switch refused.Reason {
	case "", backups.ReasonChooseFolder:
		return webui.Error("backup_new_destination", webui.MsgBackupChooseFolder)
	case backups.ReasonNotAbsolute:
		return webui.Error("backup_new_destination", webui.MsgBackupFolderNotAbsolute)
	case backups.ReasonFolderOverlaps:
		return webui.Error("backup_new_destination", webui.MsgBackupFolderOverlaps)
	case backups.ReasonFolderUnusable:
		return webui.Error("backup_new_destination", webui.MsgBackupFolderUnusable).WithDetail(refused.Detail)
	}
	return webui.Error("backup_new_plan", webui.MsgBackupScheduleRefused).WithDetail(refused.Message)
}

// answerBackupPlanError maps the plan service's sentinel refusals to
// dashboard notices; nameField names a name field for a name error.
func answerBackupPlanError(err error, name, nameField string, refuse func(int, ...webui.Notice), request *http.Request, step string) {
	var notice webui.Notice
	switch {
	case errors.Is(err, backups.ErrPlanNotFound):
		notice = webui.Error(nameField, webui.MsgBackupPlanMissingRefused)
	case errors.Is(err, backups.ErrPlanExists):
		notice = webui.Error("backup_new_plan", webui.MsgBackupPlanExistsRefused)
	case errors.Is(err, backups.ErrPlanActive):
		notice = webui.Error("", webui.MsgBackupActiveRefused)
	case errors.Is(err, backups.ErrPlanLimit):
		notice = webui.Error("", webui.MsgBackupPlanLimitRefused)
	default:
		if refusal, ok := backupRefused(err); ok {
			refuse(refusal.status, refusal.notice())
			return
		}
		refuse(unavailable(request, step, err), webui.Error("", webui.MsgBackupFailed))
		return
	}
	refuse(http.StatusConflict, notice)
}

// backupPlanResultURL is the result URL for a plan action, keeping the
// selected plan in view when one is named.
func backupPlanResultURL(action, notice, plan string) string {
	target := settingsResultURL(action, notice)
	if plan != "" {
		target += "&backup_plan=" + url.QueryEscape(plan)
	}
	return target
}

// scheduleRefusal is a refused schedule change for the dashboard: worded
// from the catalog when the refusal has a reason, and with the English
// cause as its detail otherwise.
func scheduleRefusal(refused *backups.ChangeError) webui.Notice {
	switch refused.Reason {
	case backups.ReasonChooseFolder:
		return webui.Error("backup_destination", webui.MsgBackupChooseFolder)
	case backups.ReasonNotAbsolute:
		return webui.Error("backup_destination", webui.MsgBackupFolderNotAbsolute)
	case backups.ReasonFolderOverlaps:
		return webui.Error("backup_destination", webui.MsgBackupFolderOverlaps)
	case backups.ReasonFolderUnusable:
		return webui.Error("backup_destination", webui.MsgBackupFolderUnusable).WithDetail(refused.Detail)
	}
	return webui.Error("", webui.MsgBackupScheduleRefused).WithDetail(refused.Message)
}

// backupScheduleForm reads the schedule form, which sends every part.
func backupScheduleForm(request *http.Request) (backups.ScheduleChange, []webui.Notice) {
	destination := strings.TrimSpace(postValue(request, "backup_destination"))
	enabled, verify := formChecked(postValue(request, "backup_scheduled")), formChecked(postValue(request, "backup_verify"))
	change := backups.ScheduleChange{Destination: &destination, Enabled: &enabled, Verify: &verify}
	var notices []webui.Notice
	if destination == "" {
		notices = append(notices, webui.Error("backup_destination", webui.MsgBackupChooseFolder))
	}
	for _, choice := range backups.Intervals {
		if choice.Name == postValue(request, "backup_interval") {
			interval := choice.Interval
			change.Interval = &interval
		}
	}
	if change.Interval == nil {
		notices = append(notices, webui.Error("backup_interval", webui.MsgSettingsUnknownAct))
	}
	keep, err := strconv.Atoi(strings.TrimSpace(postValue(request, "backup_keep")))
	if err != nil || keep < 1 || keep > state.MaxBackupKeep {
		notices = append(notices, webui.Error("backup_keep", webui.MsgBackupKeepInvalid))
	}
	change.Keep = &keep
	return change, notices
}

// writeBackupArchive sends download as a tar file and closes it. Once the
// file has started, a failure can only cut the connection, so the client
// never takes a shortened archive for a whole one.
func (app *App) writeBackupArchive(writer http.ResponseWriter, request *http.Request, download *backups.Download) {
	defer download.Close()
	request = app.beginTransfer(request)
	header := writer.Header()
	header.Set("Content-Type", "application/x-tar")
	header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": download.Name + ".tar"}))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	if err := download.WriteArchive(request.Context(), idleWriter{writer, http.NewResponseController(writer)}); err != nil {
		logFailure(request, "backup download of "+download.Name, err)
		panic(http.ErrAbortHandler)
	}
}

// backupFieldLimit bounds each form field that comes before the file in an
// upload: the CSRF token, the action and the administrator password.
const backupFieldLimit = 16 << 10

// isMultipart reports whether request sends a multipart form, as the
// upload of a backup does.
func isMultipart(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	return err == nil && mediaType == "multipart/form-data"
}

// receiveBackupUpload answers the upload form of the Backups groups. The
// fields before the file are read under the page deadline, and the file
// only once the CSRF check and the administrator confirmation passed.
func (app *App) receiveBackupUpload(writer http.ResponseWriter, request *http.Request, settings state.Settings, csrf string) {
	if !requireFormOrigin(request) {
		http.Error(writer, "request origin is required", http.StatusForbidden)
		return
	}
	reader, err := request.MultipartReader()
	if err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	fields := url.Values{}
	var file *multipart.Part
	for file == nil {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(fields) > 8 {
			http.Error(writer, "invalid form", http.StatusBadRequest)
			return
		}
		if part.FormName() == "backup_file" {
			file = part
			break
		}
		value, err := io.ReadAll(io.LimitReader(part, backupFieldLimit+1))
		if err != nil || len(value) > backupFieldLimit {
			http.Error(writer, "invalid form", http.StatusBadRequest)
			return
		}
		fields.Add(part.FormName(), string(value))
	}
	request.PostForm = fields
	if !app.requireCSRF(writer, request) {
		return
	}
	action := postValue(request, "action")
	if action != webui.ActionBackupUpload {
		app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("action", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest)
		return
	}
	if _, err := app.confirmAdmin(writer, request, nil, false); err != nil {
		notice, status := adminPasswordNotice(request, err, "admin_password")
		app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{notice}, status)
		return
	}
	refuse := func(status int, notice webui.Notice) {
		app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{notice}, status, settingsView{AdminVerified: true})
	}
	switch {
	case file == nil || file.FileName() == "":
		refuse(http.StatusUnprocessableEntity, webui.Error("", webui.MsgBackupUploadMissing))
		return
	case request.ContentLength <= 0:
		refuse(http.StatusLengthRequired, webui.Error("", webui.MsgBackupUploadNoSize))
		return
	}
	// A long upload outlives the page deadline, so the answer uses the
	// transfer's request.
	request = app.beginTransfer(request)
	_, err = app.Backups.ReceiveUpload(request.Context(), idleReader{file, http.NewResponseController(writer)}, request.ContentLength)
	if err != nil {
		if refusal, ok := backupRefused(err); ok {
			refuse(refusal.status, refusal.notice())
			return
		}
		refuse(unavailable(request, "backup upload", err), webui.Error("", webui.MsgBackupFailed))
		return
	}
	app.settingsSaved(writer, request, settingsResultURL(action, "backup_upload_received"))
}
