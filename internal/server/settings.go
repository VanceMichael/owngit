package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"owngit/internal/auth"
	"owngit/internal/bidi"
	"owngit/internal/requestctx"
	"owngit/internal/state"
	"owngit/internal/webui"
)

// Settings is split into tabs, each its own address (webui.SettingsTabs),
// and each tab into groups: one form per group, with its own Save. A group
// posts only its own fields, to its tab's address, so saving it never saves
// or changes another group's settings.
//
// The page's script can save a group without leaving the page, for a form
// without a password field (groupSave in owngit.js): every group while this
// browser's administrator confirmation is remembered or the check is off
// (see confirmAdmin), except those that always ask for it. A form that
// asks for the administrator password is always posted by the browser.
// The script sends the same form with the
// settingsGroupHeader header, which selects only the form of the answer;
// the change and its checks are the same:
//
//   - saved: 200 with JSON {"location": URL}, the address a browser
//     without the script is redirected to. The result notice is kept for
//     that address as for the redirect. When it is this tab, the script
//     reads it and puts the group's new state, with its saved notice, in
//     place; otherwise it goes there.
//   - refused: the tab itself, as without the script, with the refused
//     group showing its notices and the non-secret values it sent. The
//     script takes that group from it.
//
// Anything else, such as a sign-in page after the session ended, is not a
// saved change.
//
// A page submission may also carry settingsLeaveField, the page the owner
// was leaving for when the script asked to save first. Once the change is
// saved, the redirect goes there instead of back to the tab; a refused
// change still shows the tab, so the owner stays with the error.
const settingsGroupHeader = "X-OwnGit-Group"

const settingsLeaveField = "leave_to"

// settingsLeaveTarget is the address a page submission asked to continue to
// once saved: a path on this site, or "".
func settingsLeaveTarget(request *http.Request) string {
	if request.Header.Get(settingsGroupHeader) != "" {
		return ""
	}
	return localNext(postValue(request, settingsLeaveField), "")
}

// isSettingsPath reports whether path is the address of a Settings tab.
func isSettingsPath(path string) bool {
	_, ok := webui.SettingsTabOfPath(path)
	return ok
}

func (app *App) handleSettingsGet(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	csrf, ok := app.allowSettingsViewer(writer, request, settings)
	if !ok {
		return
	}
	app.renderSettings(writer, request, settings, csrf, "", nil, http.StatusOK)
}

func (app *App) handleSettingsPost(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	csrf, ok := app.allowSettingsViewer(writer, request, settings)
	if !ok {
		return
	}
	if isMultipart(request) {
		app.receiveBackupUpload(writer, request, settings, csrf)
		return
	}
	if !app.parseForm(writer, request) {
		return
	}
	if !app.requireCSRF(writer, request) {
		return
	}
	// The group actions map to the actions of the 1.1.2 forms, which are
	// still accepted, behind the same checks, so a page opened before an
	// upgrade still saves.
	action := postValue(request, "action")
	// Changing the administrator password asks for the current one, and
	// turning Do not ask on asks one last time, even when this browser is
	// remembered or the check is off.
	always := action == webui.ActionChangeAdminPassword
	var choice state.AdminConfirmation
	if action == webui.ActionSaveConfirmation {
		var valid bool
		if choice, valid = state.ParseAdminConfirmation(postValue(request, "admin_confirmation")); !valid {
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("admin_confirmation", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest)
			return
		}
		// A saved value this build does not know is replaced by any
		// choice, Every time included.
		savedChoice, known, err := app.Store.AdminConfirmation(request.Context())
		if err != nil {
			app.renderNotSaved(writer, request, settings, csrf, action, "administrator confirmation read", err)
			return
		}
		if known && choice == savedChoice {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Info(webui.MsgSettingsNothing)}, http.StatusOK, settingsView{Unchanged: true})
			return
		}
		if choice == state.ConfirmNever {
			if !formChecked(postValue(request, "no_ask_ack")) {
				app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("no_ask_ack", webui.MsgConfirmAckNeeded)}, http.StatusUnprocessableEntity)
				return
			}
			always = true
		}
	}
	// verified holds the administrator password and its version when this
	// request typed it, and is empty when the change needed none.
	verified, err := app.confirmAdmin(writer, request, nil, always)
	if err != nil {
		notice, status := adminPasswordNotice(request, err, "admin_password")
		app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{notice}, status)
		return
	}
	if action == webui.ActionSaveAccess {
		var changes bool
		action, changes = accessAction(request, settings)
		if action == "" {
			app.renderSettings(writer, request, settings, csrf, webui.ActionSaveAccess, []webui.Notice{webui.Error("action", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest)
			return
		}
		if !changes {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Info(webui.MsgSettingsNothing)}, http.StatusOK, settingsView{Unchanged: true})
			return
		}
	}

	// ends is the session cookie a saved change ends in this browser. It is
	// cleared only once the change is saved; a change that was not saved
	// changes nothing, this browser's session included.
	var ends string
	var accessSession auth.NewSession
	var accessLinks state.CrossSiteLinks
	// notice is the result the page shows in the saved group.
	notice := "settings_saved"
	switch action {
	case webui.ActionEnableAccessPassword, webui.ActionChangeAccessPassword:
		authority, readErr := app.adminAuthority(writer, request)
		if readErr != nil {
			if verified.password == "" {
				app.renderNotSaved(writer, request, settings, csrf, action, "administrator confirmation read", readErr)
				return
			}
			logFailure(request, "administrator confirmation read", readErr)
		}
		if verified.password != "" || authority.confirmed {
			accessSession, accessLinks, err = app.accessChangeSession(request)
			if err != nil {
				app.renderNotSaved(writer, request, settings, csrf, action, "general session read", err)
				return
			}
		}
		err = app.setAccessPassword(request.Context(), requestctx.Of(request).ClientAddress, verified.password, postValue(request, "access_password"))
		var policyErr *state.PolicyError
		switch {
		case errors.Is(err, auth.ErrRateLimited), errors.As(err, &policyErr):
			// The comparison with the administrator password was refused.
			if seconds := auth.RetryAfter(err); seconds > 0 {
				writer.Header().Set("Retry-After", strconv.Itoa(seconds))
			}
			notice, status := adminPasswordNotice(request, err, "access_password")
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{notice}, status)
			return
		case passwordRuleError(err):
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("access_password", passwordRuleMessage(err, webui.MsgSetupAccessPassShort))}, http.StatusUnprocessableEntity)
			return
		case errors.Is(err, errSharedSameAsAdmin):
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("access_password", webui.MsgSetupGenSameAsAdmin)}, http.StatusUnprocessableEntity)
			return
		}
		ends, notice = generalCookie, "access_changed"
		if action == webui.ActionEnableAccessPassword {
			notice = "access_enabled"
		}
	case webui.ActionDisableAccessPassword:
		err = app.Store.DisableAccessPassword(request.Context())
		ends, notice = generalCookie, "access_disabled"
	case webui.ActionChangeAdminPassword:
		err = app.changeAdminPassword(request.Context(), verified, postValue(request, "new_admin_password"))
		var refused webui.MessageCode
		switch {
		case passwordRuleError(err):
			refused = passwordRuleMessage(err, webui.MsgSetupAdminShort)
		case errors.Is(err, errAdminPasswordSame):
			refused = webui.MsgSettingsAdminSame
		case errors.Is(err, errAdminSameAsShared):
			refused = webui.MsgSetupAdminSameAsGen
		case errors.Is(err, state.ErrAccessChanged):
			// A password changed since the confirmation is no longer the
			// current one, so it cannot replace its replacement.
			notice, status := adminPasswordNotice(request, auth.ErrInvalidCredentials, "admin_password")
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{notice}, status)
			return
		}
		if refused != "" {
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("new_admin_password", refused)}, http.StatusUnprocessableEntity)
			return
		}
		ends, notice = adminCookie, "admin_password_changed"
	case webui.ActionAcknowledgeInsecure:
		if !formChecked(postValue(request, "insecure_ack")) {
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("insecure_ack", webui.MsgSetupInsecureNeed)}, http.StatusUnprocessableEntity)
			return
		}
		err = app.Store.AcknowledgeInsecureHTTP(request.Context())
		notice = "insecure_acknowledged"
	case webui.ActionSetUpdateCheck:
		// An unticked switch sends nothing, which means off.
		value := postValue(request, "update_check")
		if value != "on" && value != "off" && value != "" {
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("action", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest)
			return
		}
		err = app.savePolicies(request.Context(), state.PolicyChange{UpdateCheck: pointer(value == "on")})
	case webui.ActionSetTrayIcon:
		if !app.TrayAvailable {
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("tray_icon", webui.MsgSettingsTrayUnavailable)}, http.StatusConflict)
			return
		}
		// An unticked switch sends nothing, which means off.
		value := postValue(request, "tray_icon")
		if value != "on" && value != "off" && value != "" {
			app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("action", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest)
			return
		}
		err = app.setTrayHidden(value != "on")
		notice = "tray_saved"
	case webui.ActionSaveConfirmation:
		err = app.Auth.SetAdminConfirmation(request.Context(), choice)
		// A password typed for the new choice starts its window now, in
		// place of the session this browser held under the old one.
		if err == nil && verified.password != "" && choice.Window() > 0 {
			if rememberErr := app.rememberAdmin(writer, request, nil, verified.version); rememberErr != nil {
				logFailure(request, "administrator confirmation start", rememberErr)
			}
		}
		notice = "confirmation_saved"
		if choice == state.ConfirmNever {
			notice = "confirmation_off"
		}
	case webui.ActionSaveSession:
		choice, valid := state.ParseGeneralSession(postValue(request, "general_session"))
		if !valid {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Error("general_session", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{Session: &choice})
		notice = "session_saved"
	case webui.ActionSaveInitialBranch:
		branch := postValue(request, "initial_branch")
		if state.ValidateInitialBranch(branch) != nil {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Error("initial_branch", webui.MsgInitialBranchInvalid)}, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{InitialBranch: &branch})
		notice = "initial_branch_saved"
	case webui.ActionSaveTransfers:
		limits, notices := transferLimitsForm(request)
		if len(notices) > 0 {
			app.renderSettingsPage(writer, request, settings, csrf, action, notices, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{GitTransfer: &limits})
		notice = "transfer_saved"
		if limits.Looser() {
			notice = "transfer_looser"
		}
	case webui.ActionSaveBrowseLimits:
		limits, notices := browseLimitsForm(request)
		if len(notices) > 0 {
			app.renderSettingsPage(writer, request, settings, csrf, action, notices, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{Browse: &limits})
		notice = "browse_saved"
		if limits.Looser() {
			notice = "browse_looser"
		}
	case webui.ActionSaveCheckCeilings:
		ceilings, notices := checkCeilingsForm(request)
		if len(notices) > 0 {
			app.renderSettingsPage(writer, request, settings, csrf, action, notices, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{CheckCeilings: &ceilings})
		notice = "ceilings_saved"
		if ceilings.Looser() {
			notice = "ceilings_looser"
		}
	case webui.ActionSaveMaintenance:
		choices, notices := maintenanceForm(request)
		if len(notices) > 0 {
			app.renderSettingsPage(writer, request, settings, csrf, action, notices, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{Maintenance: &choices})
		if err == nil {
			app.Repositories.WakeMaintenance()
		}
		notice = "maintenance_saved"
		if choices.Looser() {
			notice = "maintenance_looser"
		}
	case webui.ActionSaveCleanup:
		cleanup, notices := cleanupForm(request)
		if len(notices) > 0 {
			app.renderSettingsPage(writer, request, settings, csrf, action, notices, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{Cleanup: &cleanup})
		if err == nil {
			app.Repositories.WakeMaintenance()
		}
		notice = "cleanup_off"
		if cleanup.Enabled {
			notice = "cleanup_on"
		}
	case webui.ActionSaveCheckLogs:
		retention, valid := state.ParseCheckLogRetention(postValue(request, "check_logs"))
		if !valid {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Error("check_logs", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{CheckLogs: &retention})
		notice = "check_logs_saved"
	case webui.ActionSaveKeptHistory:
		keep, valid := parseOnOff(postValue(request, "kept_history"))
		if !valid {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Error("kept_history", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{KeptHistory: &keep})
		notice = "kept_history_on"
		if !keep {
			notice = "kept_history_off"
		}
	case webui.ActionSaveDeleteName:
		ask, valid := parseOnOff(postValue(request, "delete_requires_name"))
		if !valid {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Error("delete_requires_name", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{DeleteRequiresName: &ask})
		notice = "delete_name_on"
		if !ask {
			notice = "delete_name_off"
		}
	case webui.ActionSaveLoginLimits:
		limits, notices := loginLimitsForm(request)
		if len(notices) > 0 {
			app.renderSettingsPage(writer, request, settings, csrf, action, notices, http.StatusUnprocessableEntity, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{LoginLimits: &limits})
		notice = "login_limits_saved"
		if limits.Looser() {
			notice = "login_limits_looser"
		}
	case webui.ActionSaveCrossSite:
		links, valid := state.ParseCrossSiteLinks(postValue(request, "cross_site_links"))
		if !valid {
			app.renderSettingsPage(writer, request, settings, csrf, action, []webui.Notice{webui.Error("cross_site_links", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest, settingsView{AdminVerified: true})
			return
		}
		err = app.Store.SavePolicies(request.Context(), state.PolicyChange{CrossSiteLinks: &links})
		if err == nil {
			app.renewGeneralCookie(writer, request, links)
		}
		notice = "cross_site_strict"
		if links == state.CrossSiteLax {
			notice = "cross_site_lax"
		}
	case webui.ActionSaveNetwork:
		app.saveNetwork(writer, request, settings, csrf)
		return
	case webui.ActionSaveTailscale, webui.ActionTailscaleOn, webui.ActionTailscaleOff:
		app.changeTailscale(writer, request, settings, csrf, action)
		return
	case webui.ActionSaveBackupSchedule, webui.ActionSaveBackupPlan, webui.ActionCreateBackupPlan, webui.ActionDeleteBackupPlan,
		webui.ActionBackupNow, webui.ActionBackupVerify, webui.ActionBackupDownload:
		app.backupAction(writer, request, settings, csrf, action)
		return
	default:
		app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("action", webui.MsgSettingsUnknownAct)}, http.StatusBadRequest)
		return
	}
	if err != nil {
		app.renderNotSaved(writer, request, settings, csrf, action, "settings save", err)
		return
	}
	if accessSession.Token != "" {
		if err := app.Store.StartSession(request.Context(), "", accessSession.Token, "general", accessSession.CSRF, settings.AccessSessionVersion+1, accessSession.Expires); err != nil {
			logFailure(request, "general session start", err)
		} else {
			app.setGeneralCookie(writer, request, accessSession.Token, accessSession.Expires, accessLinks)
			ends = ""
		}
	}
	if ends != "" {
		app.clearCookie(writer, request, cookieNameForScheme(request, ends))
	}
	if ends == generalCookie && (action == webui.ActionEnableAccessPassword || action == webui.ActionChangeAccessPassword) {
		next := settingsLeaveTarget(request)
		if next == "" {
			next = webui.SettingsTabURL(webui.SettingsAccess)
		}
		app.settingsAnswer(writer, request, "/login?notice=access_password_saved&next="+url.QueryEscape(next))
		return
	}
	app.settingsSaved(writer, request, settingsResultURL(action, notice))
}

// accessAction turns a save of the Access group into the change it asks
// for: turning the shared password on, changing it or turning it off.
// changes is false when the form asks for what is already saved. The action
// is "" for a mode that does not exist.
func accessAction(request *http.Request, settings state.Settings) (action string, changes bool) {
	passwordNow := settings.AccessMode == "password"
	switch postValue(request, "access_mode") {
	case "open":
		return webui.ActionDisableAccessPassword, passwordNow
	case "password":
		if !passwordNow {
			return webui.ActionEnableAccessPassword, true
		}
		return webui.ActionChangeAccessPassword, postValue(request, "access_password") != ""
	}
	return "", false
}

// settingsResultURL is where a saved change of action shows notice: its
// group on its tab.
func settingsResultURL(action, notice string) string {
	group := webui.SettingsActionGroup(action)
	return webui.SettingsTabURL(webui.SettingsGroupTab(group)) + "?notice=" + url.QueryEscape(notice) + "#grp-" + group
}

// settingsNoticeGroups names the group that shows each result notice of a
// saved Settings change.
var settingsNoticeGroups = map[string]string{
	"settings_saved":         webui.GroupUpdate,
	"tray_saved":             webui.GroupTray,
	"access_enabled":         webui.GroupAccess,
	"access_changed":         webui.GroupAccess,
	"access_disabled":        webui.GroupAccess,
	"admin_password_changed": webui.GroupAdmin,
	"confirmation_saved":     webui.GroupConfirm,
	"confirmation_off":       webui.GroupConfirm,
	"session_saved":          webui.GroupSession,
	"initial_branch_saved":   webui.GroupBranch,
	"transfer_saved":         webui.GroupTransfer,
	"transfer_looser":        webui.GroupTransfer,
	"browse_saved":           webui.GroupBrowse,
	"browse_looser":          webui.GroupBrowse,
	"ceilings_saved":         webui.GroupCeilings,
	"ceilings_looser":        webui.GroupCeilings,
	"maintenance_saved":      webui.GroupMaintenance,
	"maintenance_looser":     webui.GroupMaintenance,
	"cleanup_off":            webui.GroupCleanup,
	"cleanup_on":             webui.GroupCleanup,
	"check_logs_saved":       webui.GroupLogs,
	"kept_history_on":        webui.GroupHistory,
	"kept_history_off":       webui.GroupHistory,
	"delete_name_on":         webui.GroupDeleteName,
	"delete_name_off":        webui.GroupDeleteName,
	"login_limits_saved":     webui.GroupLogin,
	"login_limits_looser":    webui.GroupLogin,
	"cross_site_strict":      webui.GroupCrossSite,
	"cross_site_lax":         webui.GroupCrossSite,
	"insecure_acknowledged":  webui.GroupConnection,
	"network_saved":          webui.GroupNetwork,
	"tailscale_on":           webui.GroupTailscale,
	"tailscale_on_kept":      webui.GroupTailscale,
	"tailscale_off":          webui.GroupTailscale,
	"tailscale_moved":        webui.GroupTailscale,
}

// settingsSaved ends a saved Settings change: a redirect to target, or to
// the page the owner was leaving for (settingsLeaveTarget), or for the
// page's script the same target as JSON (see settingsGroupHeader).
func (app *App) settingsSaved(writer http.ResponseWriter, request *http.Request, target string) {
	if leave := settingsLeaveTarget(request); leave != "" {
		http.Redirect(writer, request, leave, http.StatusSeeOther)
		return
	}
	app.settingsAnswer(writer, request, target)
}

// settingsAnswer answers a saved change with target, as a redirect or,
// for the page's script, as JSON.
func (app *App) settingsAnswer(writer http.ResponseWriter, request *http.Request, target string) {
	if request.Header.Get(settingsGroupHeader) == "" {
		app.noticeRedirect(writer, request, target)
		return
	}
	if parsed, err := url.Parse(target); err == nil {
		if notice := parsed.Query().Get("notice"); notice != "" {
			app.setCookie(writer, request, noticeCookie, notice, app.now().Add(noticeCookieMaxAge), true)
		}
	}
	body, err := bidi.MarshalJSON(struct {
		Location string `json:"location"`
	}{target})
	if err != nil {
		app.writePlainError(writer, internalError(request, "settings answer", err))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

// passwordRuleMessage names the password rule a refused password broke.
func passwordRuleMessage(err error, tooShort webui.MessageCode) webui.MessageCode {
	if errors.Is(err, auth.ErrPasswordTooLong) {
		return webui.MsgPasswordTooLong
	}
	return tooShort
}

func (app *App) allowSettingsViewer(writer http.ResponseWriter, request *http.Request, settings state.Settings) (string, bool) {
	if settings.AccessMode == "open" {
		session, ok := app.requireGeneral(writer, request, settings)
		return session.CSRF, ok
	}
	// Either session shows Settings, so one that was read decides even when
	// the other could not be read.
	general, generalOK, generalErr := app.cookieSession(request, "general", generalCookie)
	if generalOK {
		return general.CSRF, true
	}
	admin, adminOK, adminErr := app.cookieSession(request, "admin", adminCookie)
	switch {
	case adminOK:
		return admin.CSRF, true
	case generalErr != nil || adminErr != nil:
		app.answerUnavailable(writer, request, "session read", errors.Join(generalErr, adminErr))
	default:
		http.Redirect(writer, request, "/login?next="+url.QueryEscape(loginNext(request)), http.StatusSeeOther)
	}
	return "", false
}

// renderNotSaved answers a settings change that failed before anything was
// changed: the form says the change was not saved, and step and its cause
// are logged once.
func (app *App) renderNotSaved(writer http.ResponseWriter, request *http.Request, settings state.Settings, csrf, action, step string, err error) {
	app.renderSettings(writer, request, settings, csrf, action, []webui.Notice{webui.Error("", webui.MsgSettingsNotSaved)}, unavailable(request, step, err))
}

func (app *App) renderSettings(writer http.ResponseWriter, request *http.Request, settings state.Settings, csrf, action string, notices []webui.Notice, status int) {
	app.renderSettingsPage(writer, request, settings, csrf, action, notices, status, settingsView{})
}

// settingsView is what a refused form brings to the Settings page that
// shows it again.
type settingsView struct {
	// Network is what a refused Network save submitted. Without it the
	// form shows what the request sent, unless NetworkStale: the form was
	// opened before another change, so it shows the values saved now.
	Network      *webui.NetworkForm
	NetworkStale bool
	// AdminVerified is true when this request passed confirmAdmin, so the
	// page may show what only an administrator sees.
	AdminVerified bool
	// TailscaleRefused is the problem of a refused Tailscale change shown
	// on the page, which the Tailscale block then does not repeat, and
	// TailscaleOccupied what it found on the port the owner named.
	TailscaleRefused  string
	TailscaleOccupied *TailscaleOccupied
	// Unchanged is true when the form asked for nothing that could be
	// saved: the group then shows the saved values, not what was sent.
	Unchanged bool
}

// settingsDraftFields are the non-secret fields a refused form shows again.
// A switch or checkbox is written "on" or "off", since an unticked one
// sends nothing.
var settingsDraftFields = map[string]bool{
	"access_mode": false, "admin_confirmation": false, "no_ask_ack": true, "general_session": false, "initial_branch": false,
	"transfer_size": false, "transfer_size_unit": false, "transfer_time": false, "transfer_time_unit": false, "check_logs": false,
	"delete_requires_name": false, "login_attempts": false, "login_window": false, "login_window_unit": false, "login_pause": false, "login_pause_unit": false, "cross_site_links": false,
	"update_check": true, "tray_icon": true, "tailscale": true, "home_network": true, "tailscale_port": false, "tailscale_https_port": false, "insecure_ack": true,
	"transfer_per_repository": false, "transfer_extra_slots": false, "transfer_idle": false, "transfer_idle_unit": false, "transfer_queue": false,
	"transfer_queue_unit": false, "browse_raw": false, "browse_raw_unit": false, "browse_file": false, "browse_file_unit": false,
	"browse_commit_patch": false, "browse_commit_patch_unit": false, "browse_file_patch": false, "browse_file_patch_unit": false,
	"browse_commit_file": false, "browse_commit_file_unit": false, "browse_compare": false, "browse_compare_unit": false, "browse_compare_time": false,
	"browse_compare_time_unit": false, "maintenance_idle": false, "maintenance_idle_unit": false, "maintenance_command": false,
	"maintenance_command_unit": false, "maintenance_full_repack": false, "maintenance_full_repack_unit": false, "maintenance_enabled": false,
	"maintenance_window_start": false, "maintenance_window_end": false, "maintenance_pack_threshold": false, "cleanup_enabled": false,
	"cleanup_grace":      false,
	"backup_destination": false, "backup_interval": false, "backup_keep": false, "backup_scheduled": true, "backup_verify": true,
	"backup_new_plan": false, "backup_new_destination": false,
	"ceiling_timeout": false, "ceiling_timeout_unit": false, "ceiling_output": false, "ceiling_output_unit": false,
	"ceiling_queue": false, "ceiling_active": false, "ceiling_cpu": false, "ceiling_cpu_unit": false, "ceiling_memory": false,
	"ceiling_memory_unit": false, "ceiling_pids": false, "ceiling_scratch": false, "ceiling_scratch_unit": false, "ceiling_source": false,
	"ceiling_source_unit": false,
}

// settingsDraft collects what a refused form sent, for settingsDraftFields.
func settingsDraft(request *http.Request) map[string]string {
	draft := map[string]string{}
	for field, checkbox := range settingsDraftFields {
		value := postValue(request, field)
		switch {
		case checkbox:
			draft[field] = "off"
			if formChecked(value) {
				draft[field] = "on"
			}
		case value != "":
			draft[field] = value
		}
	}
	return draft
}

// renderSettingsPage renders a Settings tab. action is the form that was
// sent, or "": its group shows notices and the values it sent, on its tab.
// Otherwise the tab is the one the address names, and a result notice in
// the address is shown in the group it belongs to.
func (app *App) renderSettingsPage(writer http.ResponseWriter, request *http.Request, settings state.Settings, csrf, action string, notices []webui.Notice, status int, view settingsView) {
	chrome, err := app.chrome(writer, request, webui.SectionSettings, "", csrf)
	if err != nil {
		app.answerUnavailable(writer, request, "page frame read", err)
		return
	}
	tab, _ := webui.SettingsTabOfPath(request.URL.Path)
	page := webui.SettingsPage{Chrome: chrome, AccessMode: webui.AccessOpen}
	if settings.AccessMode == "password" {
		page.AccessMode = webui.AccessPassword
	}
	switch group := webui.SettingsActionGroup(action); {
	case group != "":
		tab, page.Group, page.Notices, page.Chrome.Notices = webui.SettingsGroupTab(group), group, notices, nil
		if request.Method == http.MethodPost && !view.Unchanged {
			page.Draft = settingsDraft(request)
		}
	case notices != nil:
		// A form this page does not know: its answer stands above the tab.
		page.Chrome.Notices = notices
	default:
		// The result of a saved change, kept by its redirect, is shown in
		// its group when that group is on this tab.
		notice := request.URL.Query().Get("notice")
		group := settingsNoticeGroups[notice]
		if group == "" {
			group = backupNoticeGroup(notice)
		}
		if len(chrome.Notices) > 0 && group != "" && webui.SettingsGroupTab(group) == tab {
			page.Group, page.Notices, page.Chrome.Notices = group, chrome.Notices, nil
		}
	}
	page.Tab, page.SubmitURL = tab, webui.SettingsTabURL(tab)

	report, err := app.networkReport(request.Context())
	if err != nil {
		app.answerUnavailable(writer, request, "network settings read", err)
		return
	}
	page.Network = networkInfo(report)
	switch {
	case view.Network != nil:
		page.Network.Form = *view.Network
	case page.Group == webui.GroupNetwork && page.Draft != nil && !view.NetworkStale:
		page.Network.Form = networkForm(request)
	}
	if chrome.Viewer.AdminConfirmed {
		page.Storage = webui.StorageInfo{Visible: true, Path: settings.RepositoryRoot}
		page.CloneHint = app.serverOrigin(request) + "/git/"
	}
	page.UpdateCheck = webui.UpdateCheckInfo{Enabled: settings.UpdateCheck, ForcedOff: app.Releases == nil}
	if tab == webui.SettingsGeneral {
		page.Tray = app.trayInfo(request)
	}
	// The checkup reads this computer's firewall and folder owners, which
	// can take a moment, so it runs only for the administrator, who alone
	// sees it, and only on its tab.
	if tab == webui.SettingsGeneral && chrome.Viewer.AdminConfirmed && app.Diagnose != nil {
		page.Checkup = webui.CheckupInfo{Visible: true, Findings: app.Diagnose(request.Context())}
	}
	if page.Policies, err = app.tabPolicies(request, tab, chrome.Viewer.AdminConfirmed || view.AdminVerified); err != nil {
		app.answerUnavailable(writer, request, "settings read", err)
		return
	}
	if tab == webui.SettingsStorage {
		if page.Backups, err = app.backupsInfo(request, settings, chrome.Viewer.AdminConfirmed || view.AdminVerified); err != nil {
			app.answerUnavailable(writer, request, "backup status read", err)
			return
		}
	}
	// Only the Network tab shows sharing on the tailnet, and reading it
	// asks Tailscale, so the other tabs do not.
	if tab == webui.SettingsNetwork {
		page.Tailscale = app.tailscaleBlock(request, chrome.Viewer.AdminConfirmed || view.AdminVerified, view.TailscaleRefused, view.TailscaleOccupied)
	}
	app.render(writer, request, status, page)
}
