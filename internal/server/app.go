package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"owngit/internal/auth"
	"owngit/internal/backups"
	"owngit/internal/checkrun"
	"owngit/internal/githttp"
	"owngit/internal/importsync"
	"owngit/internal/logtext"
	"owngit/internal/pullrequest"
	"owngit/internal/releasecheck"
	"owngit/internal/repository"
	"owngit/internal/requestctx"
	"owngit/internal/state"
	"owngit/internal/webui"
)

const (
	// The cookies that carry a session or a form token have one name per
	// scheme: the name below over plain HTTP, and the same name with the
	// __Host- prefix over HTTPS (cookieNameForScheme). A browser that uses
	// both this server's HTTPS address and a plain address then holds a
	// session for each, and signing in at one never has to replace the
	// other's cookie. Language, appearance and list order also have separate
	// names, with a legacy read fallback (upgradeCookie).
	generalCookie  = "owngit_general"
	adminCookie    = "owngit_admin"
	setupCookie    = "owngit_setup"
	preauthCookie  = "owngit_preauth"
	languageCookie = "owngit_lang"
	// appearanceCookie is a preference the page script also reads and writes.
	appearanceCookie = "owngit_appearance"
	// orderCookie holds the repository list order, a preference the page
	// script also reads and writes.
	orderCookie = "owngit_order"
	// releaseDismissCookie holds the release version whose dashboard notice
	// this browser dismissed. It is a preference, not a credential.
	releaseDismissCookie = "owngit_release_dismissed"
	// noticeCookie carries the result notice of the action that redirected
	// here. A page shows a result notice from its address only when this
	// cookie names the same notice, so an address alone cannot show one.
	noticeCookie       = "owngit_notice"
	noticeCookieMaxAge = 2 * time.Minute
)

type App struct {
	// Store through Tailscale are required. The serving process always sets
	// them, so no handler treats one as absent.
	Store        *state.Store
	Auth         *auth.Manager
	Repositories *repository.Manager
	PullRequests *pullrequest.Service
	Imports      *importsync.Service
	// Backups makes scheduled backups and backups asked for now.
	Backups  *backups.Service
	GitHTTP  *githttp.Handler
	Renderer *webui.Renderer
	Hosts    *HostPolicy
	// Network holds the base URL, trusted proxies and accepted Host names
	// the running server uses now, which Tailscale sharing can change
	// without a restart. It derives each request's scheme, Host and client
	// address, and the addresses shown to people. See serverOrigin.
	Network *LiveNetwork
	// PublicShareURL is the address visitors use for the public share
	// address this server started with (PublicShareHandler), or "" when it
	// is off.
	PublicShareURL string
	// PublicShareAddress is the address the public share address listens
	// on, host:port, or "" when it is off.
	PublicShareAddress string
	// Tailscale shares this OwnGit on the tailnet with Tailscale Serve.
	// Whether Tailscale is installed is what its Find reports.
	Tailscale               *Tailscale
	SuggestedRepositoryRoot string
	GitVersion              string
	HTTPBackendFound        bool
	// RunningRecordLive is true when this process holds the running-record
	// lock, so the stored running network record is its own and the Settings
	// page may show it as what the server uses. See
	// state.OwnRunningNetwork.
	RunningRecordLive bool
	// Version is the running application version. It comes from the single
	// version source and is never read from storage or a remote value.
	Version string
	// Releases is the new-release check. Nil means the server started with
	// --no-update-check, which overrides the saved setting.
	Releases *releasecheck.Checker
	// UpdateCommand returns the command that updates this installation to
	// a version, as its install route does, or "", and what the owner
	// starts afterwards when no service does: a program in a new place
	// (start), or OwnGit where it runs (restart). Nil shows none.
	UpdateCommand func(version string) (command, start string, restart bool)
	// RestoreGuide is how to restore the backup at input into this
	// OwnGit's state folder and repositoryRoot on this computer, as the
	// Backups group shows it. Nil shows none.
	RestoreGuide func(input, repositoryRoot string) *webui.BackupRestore
	// Diagnose runs the checkup of this computer that "owngit doctor"
	// runs, from this server's own facts. Nil shows no checkup.
	Diagnose func(ctx context.Context) []webui.Finding
	// TrayToken is the token of state.TrayAccessFile that TrayStatusPath
	// answers to, and TrayProof the secret with which it proves its
	// answers (state.TrayProof). Either empty leaves that path unanswered.
	TrayToken, TrayProof string
	// HealthKey is the key of state.HealthRunFile, with which the health
	// check proves its answer. Empty leaves the answer without a proof.
	HealthKey string
	// TrayAvailable is true when this install offers the OwnGit icon: it
	// runs as the account that signs in at the desktop, not as a dedicated
	// service account.
	TrayAvailable bool
	// TrayDesktop reports whether this computer has a desktop where the
	// icon can show now (service.Environment.Desktop).
	TrayDesktop func() bool
	// trayCheckup is the checkup the tray status reuses.
	trayCheckup trayCheckup
	// trayOrigins remembers which records the event feed reports came
	// from this computer.
	trayOrigins trayOrigins
	HTTPTimeout time.Duration
	// ImportRunTimeout is the deadline of an import run started by this
	// server. The request itself keeps ImportResponseMargin more, so a run
	// that reaches its deadline still returns its result. Zero uses the import
	// service default. It is not a second hard-coded limit.
	ImportRunTimeout time.Duration
	ActivityLimit    int
	Now              func() time.Time
	// Sleep, when set, replaces waiting on the real clock; tests that set
	// Now move their clock with it.
	Sleep func(time.Duration)
	// requestObserver runs after a per-request deadline is installed: when
	// the request starts and when an operation begins. Tests use it to
	// observe that deadline. Production leaves it nil.
	requestObserver func(*http.Request)
	// OnSetupComplete runs once after first-run setup succeeds in this
	// process, so work that an initialized startup begins can begin now.
	OnSetupComplete func()
	// HeadlessListen is the every-address listen address that this start
	// saved or kept for a computer without a screen before setup, or ""
	// otherwise. On such a computer the setup form keeps the address it was
	// opened by unless the owner unticks it, and setup that keeps no address
	// saves DefaultListenAddress, so the listener matches what setup says.
	HeadlessListen string
	// listenReturned records that setup saved DefaultListenAddress.
	listenReturned atomic.Bool
	// setupResult is set when first-run setup is saved in this process, by
	// the web page or the terminal, to its result notice (see
	// setupResultNotice). The next dashboard view, after sign-in when shared
	// access needs one, takes it and shows the notice once through the
	// notice cookie.
	setupResult atomic.Pointer[string]
	// setupHosts binds a setup session redeemed from an unknown Host to that
	// Host. See setup_host.go.
	setupHosts setupHostBinding
	// folderBusy bounds filesystem work that may outlive its response.
	folderBusy atomic.Bool
	// OnHostAccepted runs after setup kept the Host it was reached by, so the
	// serving process can record that it now accepts that Host.
	OnHostAccepted func()
	// Approvals is set while first-run setup runs in the terminal that
	// started OwnGit. A browser then asks that terminal for approval instead
	// of redeeming a setup file. Nil keeps the setup file flow.
	Approvals *SetupApprovals
	// WakeChecks is an advisory nonblocking reconciliation signal.
	WakeChecks func(repositoryID string)
	// OnCheckCompletion wakes the auto-merge convergence after a helper or a
	// runner wrote a terminal check attempt. It must not block or run Git.
	OnCheckCompletion func(repositoryID string)
	// CheckRuntimeUnavailableCode and CheckRuntimeUnavailableReason expose a
	// sanitized owner-visible startup status. Empty code means available.
	CheckRuntimeUnavailableCode   string
	CheckRuntimeUnavailableReason string
	// activity caches activity observations by ref key. See activityCache.
	activity activityCache
	// unreadable remembers the repositories already logged as unreadable.
	unreadable unreadableLog
}

func (app *App) Handler() http.Handler {
	next := app.Hosts.MiddlewareAdmitting(app.admitUnknownHost, app.answerUnavailable, http.HandlerFunc(app.serveHTTP))
	return refuseFunnel(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// The proxies trusted now, which Tailscale sharing can change.
		app.Network.Resolver().Middleware(next).ServeHTTP(writer, withoutShareSecret(request))
	}))
}

// admissionWithdrawn reports whether request came in under the container
// image's loopback admission (HostPolicy.InContainer) that settings no
// longer grant: access no longer needs the password it needed then.
func admissionWithdrawn(request *http.Request, settings state.Settings) bool {
	version, admitted := containerAdmissionVersion(request.Context())
	return admitted && (settings.AccessMode != "password" || settings.AccessSessionVersion != version)
}

// AuthorizeGit reports whether a Git request may proceed. auth.ErrRateLimited
// means the client's address is locked out after wrong passwords; any other
// error means that could not be decided.
func (app *App) AuthorizeGit(request *http.Request) (bool, error) {
	settings, err := app.Store.Settings(request.Context())
	if err != nil {
		logFailure(request, "Git access check", err)
		return false, err
	}
	if admissionWithdrawn(request, settings) {
		return false, nil
	}
	if !settings.Initialized {
		return false, nil
	}
	if settings.AccessMode == "open" {
		return true, nil
	}
	_, password, ok := request.BasicAuth()
	if !ok {
		return false, nil
	}
	_, err = app.Auth.VerifyCredential(request.Context(), "general", password, requestctx.Of(request).ClientAddress)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, auth.ErrInvalidCredentials):
		return false, nil
	case errors.Is(err, auth.ErrRateLimited):
		return false, err
	}
	logFailure(request, "Git password check", err)
	return false, err
}

// ImportResponseMargin is how long an import run request outlives the run's
// own deadline. A run that reaches its deadline still records its outcome and
// delivers the result to the client inside this margin.
const ImportResponseMargin = time.Minute

// ImportRunRequestTimeout is the request and response deadline for an import
// run whose own deadline is runTimeout.
func ImportRunRequestTimeout(runTimeout time.Duration) time.Duration {
	return runTimeout + ImportResponseMargin
}

// importRunTimeout is the run deadline the service applies to runs started by
// this server.
func (app *App) importRunTimeout() time.Duration {
	if app.ImportRunTimeout > 0 {
		return app.ImportRunTimeout
	}
	return importsync.DefaultLimits().RunTimeout
}

// importRunLimits pins the run deadline so the run ends before its request.
func (app *App) importRunLimits() importsync.Limits {
	return importsync.Limits{RunTimeout: app.importRunTimeout()}
}

// replyReserve is the most time an ordinary request keeps after its work
// deadline for writing the response.
const replyReserve = 5 * time.Second

// requestTimeout returns how long a request may take and how much of that
// time is reserved after the handler's work deadline for writing the
// response. Work that runs out of time, such as Git on slow storage, then
// still produces an error page instead of an empty reply. An import run keeps
// ImportResponseMargin beyond its own run deadline instead. The longer limits
// of import runs and archives apply only once the handler begins the
// operation; see beginOperation.
func (app *App) requestTimeout(request *http.Request) (time.Duration, time.Duration) {
	if importRunRequest(request) {
		return ImportRunRequestTimeout(app.importRunTimeout()), 0
	}
	// An archive download is a Git transfer. Its own operation deadline ends
	// it first; the request keeps the reply reserve beyond that, so the limit
	// that fires is the one the log names. When the limits cannot be read
	// here, the request gets the longest a transfer may take, and the
	// transfer, which reads them again, refuses to start.
	if archiveRoute(request) {
		operation := state.MaximumTransferOperation
		if limits, err := app.GitHTTP.Limits(request.Context()); err == nil && limits.Operation > 0 {
			operation = limits.Operation
		}
		return operation + 2*replyReserve, replyReserve
	}
	return app.pageTimeout()
}

// pageTimeout returns the time limit and reply reserve of an ordinary
// request. Every request reads its body within this limit.
func (app *App) pageTimeout() (time.Duration, time.Duration) {
	timeout := 30 * time.Second
	if app.HTTPTimeout > 0 {
		timeout = app.HTTPTimeout
	}
	return timeout, min(replyReserve, timeout/4)
}

func importRunRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodPost {
		return false
	}
	path := request.URL.Path
	if path == "/repositories/new-import" {
		return true
	}
	const apiPrefix = "/api/v1/repositories/"
	if strings.HasPrefix(path, apiPrefix) && strings.HasSuffix(path, "/import/run") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, apiPrefix), "/import/run")
		return id != "" && !strings.Contains(id, "/")
	}
	const repoPrefix = "/repositories/"
	if strings.HasPrefix(path, repoPrefix) && strings.HasSuffix(path, "/import") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, repoPrefix), "/import")
		if id == "" || strings.Contains(id, "/") {
			return false
		}
		return peekFormAction(request) == webui.ActionImportRefresh
	}
	return false
}

// maxRefreshFormBytes bounds what peekFormAction reads before authentication.
// The refresh form carries csrf, action and admin_password. A password is at
// most 1024 characters of up to four bytes, which URL-encodes to at most
// 12288 bytes, so every valid refresh form fits. A larger body is handled as
// an ordinary request.
const maxRefreshFormBytes = 16 << 10

// peekFormAction reads at most maxRefreshFormBytes of a URL-encoded form to
// find its action. The handler still receives the complete body, including
// any unread remainder and any read error.
func peekFormAction(request *http.Request) string {
	if request.Body == nil || request.ContentLength > maxRefreshFormBytes {
		return ""
	}
	if mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type")); err != nil || mediaType != "application/x-www-form-urlencoded" {
		return ""
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, maxRefreshFormBytes+1))
	var rest io.Reader = request.Body
	if err != nil {
		rest = failedReader{err}
	}
	request.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(content), rest), request.Body}
	if err != nil || len(content) > maxRefreshFormBytes {
		return ""
	}
	values, err := url.ParseQuery(string(content))
	if err != nil {
		return ""
	}
	return values.Get("action")
}

// failedReader repeats a body read error for the handler.
type failedReader struct{ err error }

func (reader failedReader) Read([]byte) (int, error) { return 0, reader.err }

func (app *App) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	app.noteHTTPS(request)
	// A browser that signed in here with OwnGit 1.1.4 or earlier keeps the
	// cookie names of that version, which block a plain address until the
	// first secure page removes them (forgetLegacyCookies). Only a page does
	// it: a browser holds the marker that stops the cleanup from running
	// again, and a Git or API client, which keeps no cookies, would get the
	// same expiries on every request.
	if dashboardPage(request.URL.Path) || strings.HasPrefix(request.URL.Path, "/setup") {
		app.forgetLegacyCookies(writer, request)
	}
	if request.URL.Path == HealthPath {
		app.handleHealth(writer, request)
		return
	}
	// The Git handler resolves its own repository address.
	if strings.HasPrefix(request.URL.Path, "/git/") {
		app.GitHTTP.ServeHTTP(writer, request)
		return
	}
	if id, suffix, ok := shareGitRoute(request.URL.Path); ok {
		app.serveShareGit(writer, request, id, suffix)
		return
	}
	// A request for a repository postpones its maintenance, from the start
	// until the end of the request.
	request = app.resolveRepositoryAddress(request)
	if address, _ := repositoryAddressOf(request); address.id != "" {
		app.Repositories.NoteRepositoryUse(address.id)
		defer app.Repositories.NoteRepositoryUse(address.id)
	}

	pageTimeout, pageReserve := app.pageTimeout()
	operationTimeout, operationReserve := app.requestTimeout(request)
	request, deadlines, cancel := startDeadlines(writer, request, pageTimeout, pageReserve, operationTimeout, operationReserve)
	defer cancel()
	defer deadlines.finish()
	if app.requestObserver != nil {
		app.requestObserver(request)
	}
	if strings.HasPrefix(request.URL.Path, "/assets/") {
		app.Renderer.Assets().ServeHTTP(writer, cloneWithPath(request, strings.TrimPrefix(request.URL.Path, "/assets")))
		return
	}
	switch request.URL.Path {
	case TrayStatusPath:
		app.handleTrayStatus(writer, request)
		return
	case TrayEventsPath:
		app.handleTrayEvents(writer, request)
		return
	}

	settings, err := app.Store.Settings(request.Context())
	if err != nil {
		app.answerUnavailable(writer, request, "settings read", err)
		return
	}
	if admissionWithdrawn(request, settings) {
		refuseHost(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, sharePrefix) {
		app.serveSharePage(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/") {
		app.handleAPI(writer, request, settings)
		return
	}
	if !settings.Initialized && request.URL.Path != "/setup" && request.URL.Path != "/setup/redeem" && request.URL.Path != "/setup/approval" && !isSetupFolderPath(request.URL.Path) {
		http.Redirect(writer, request, "/setup", http.StatusSeeOther)
		return
	}
	if settings.Initialized && (request.URL.Path == "/setup/redeem" || ((request.URL.Path == "/setup" || request.URL.Path == "/setup/approval") && request.Method != http.MethodGet)) {
		app.renderError(writer, request, http.StatusConflict, webui.MsgSetupAlreadyDone, "")
		return
	}
	if app.redirectToHTTPS(writer, request) {
		return
	}

	switch {
	case request.URL.Path == "/" && request.Method == http.MethodGet:
		app.handleOverview(writer, request, settings)
	case request.URL.Path == "/setup" && request.Method == http.MethodGet:
		app.handleSetupGet(writer, request, settings)
	case isSetupFolderPath(request.URL.Path) && request.Method == http.MethodPost:
		app.handleSetupFolders(writer, request, settings)
	case request.URL.Path == "/setup" && request.Method == http.MethodPost:
		app.handleSetupPost(writer, request)
	case request.URL.Path == "/setup/redeem" && request.Method == http.MethodPost:
		app.handleSetupRedeem(writer, request)
	case request.URL.Path == "/setup/approval" && request.Method == http.MethodPost:
		app.handleSetupApprovalRequest(writer, request)
	case request.URL.Path == "/setup/approval" && request.Method == http.MethodGet:
		app.handleSetupApprovalStatus(writer, request, settings)
	case request.URL.Path == "/login" && request.Method == http.MethodGet:
		app.handleLoginGet(writer, request, settings, webui.AuthGeneral)
	case request.URL.Path == "/login" && request.Method == http.MethodPost:
		app.handleLoginPost(writer, request, settings, webui.AuthGeneral)
	case request.URL.Path == "/logout" && request.Method == http.MethodPost:
		app.handleLogout(writer, request, webui.AuthGeneral)
	case request.URL.Path == "/admin/login" && request.Method == http.MethodGet:
		app.handleLoginGet(writer, request, settings, webui.AuthAdmin)
	case request.URL.Path == "/admin/login" && request.Method == http.MethodPost:
		app.handleLoginPost(writer, request, settings, webui.AuthAdmin)
	case request.URL.Path == "/admin/logout" && request.Method == http.MethodPost:
		app.handleLogout(writer, request, webui.AuthAdmin)
	case isSettingsPath(request.URL.Path) && request.Method == http.MethodGet:
		app.handleSettingsGet(writer, request, settings)
	case isSettingsPath(request.URL.Path) && request.Method == http.MethodPost:
		app.handleSettingsPost(writer, request, settings)
	case request.URL.Path == "/repositories/new" && request.Method == http.MethodGet:
		app.handleNewRepositoryGet(writer, request, settings, "", "", nil)
	case request.URL.Path == "/repositories/new-import" && (request.Method == http.MethodGet || request.Method == http.MethodPost):
		app.handleNewImport(writer, request)
	case request.URL.Path == "/repositories" && request.Method == http.MethodPost:
		app.handleCreateRepository(writer, request, settings)
	case request.URL.Path == releaseDismissPath && request.Method == http.MethodPost:
		app.handleReleaseDismiss(writer, request, settings)
	case request.URL.Path == "/activity" && request.Method == http.MethodGet:
		app.handleActivity(writer, request, settings)
	case request.URL.Path == codingToolsPath && request.Method == http.MethodGet:
		app.handleCodingTools(writer, request, settings)
	case strings.HasPrefix(request.URL.Path, "/repositories/") && (request.Method == http.MethodGet || request.Method == http.MethodPost):
		app.handleRepositoryRoute(writer, request, settings)
	case strings.HasPrefix(request.URL.Path, "/repositories/") && strings.HasSuffix(request.URL.Path, "/raw") && request.Method == http.MethodHead:
		// Download tools ask for a file's headers first. Only the raw route
		// answers HEAD; the route still applies the same access checks.
		app.handleRepositoryRoute(writer, request, settings)
	default:
		app.renderError(writer, request, http.StatusNotFound, webui.MsgErrNotFound, request.URL.Path)
	}
}

// render answers request with page. The page is rendered in full before
// anything is written, so a page that cannot be rendered is answered as a
// failure instead of a partial page.
func (app *App) render(writer http.ResponseWriter, request *http.Request, status int, page webui.Page) {
	var output bytes.Buffer
	if err := app.Renderer.Render(&output, page); err != nil {
		app.writePlainError(writer, internalError(request, "page render", err))
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	// An incomplete reply must not have valid HTTP framing for a whole page.
	writer.Header().Set("Content-Length", strconv.Itoa(output.Len()))
	if err := writePage(writer, request, status, output.Bytes()); err != nil {
		logFailure(request, "page transfer", err)
		panic(http.ErrAbortHandler)
	}
}

// renderError answers with an error page. A page frame that cannot be read
// is left out and logged, and the page is shown without it.
func (app *App) renderError(writer http.ResponseWriter, request *http.Request, status int, code webui.MessageCode, detail string) {
	if strings.HasPrefix(request.URL.Path, sharePrefix) {
		app.renderShareError(writer, request, status, code, detail)
		return
	}
	chrome, err := app.chrome(writer, request, webui.SectionNone, "", "")
	if err != nil {
		logFailure(request, "page frame read", err)
	}
	app.render(writer, request, status, webui.ErrorPage{Chrome: chrome, Status: status, Code: code, Detail: detail, RetryURL: "/"})
}

// unavailable returns 503 and logs the cause unless it is an intended state
// such as a repository being prepared (see intendedCause).
func unavailable(request *http.Request, step string, err error) int {
	logFailure(request, step, err)
	return http.StatusServiceUnavailable
}

// answerUnavailable answers a request that step could not complete now,
// through unavailable: an API request with its JSON error, any other with
// plain text. A page answers this way when its frame, or a read that decides
// the request, such as its session, fails; a failed read that may have
// allowed the request is neither refused nor sent to sign in.
func (app *App) answerUnavailable(writer http.ResponseWriter, request *http.Request, step string, err error) {
	status := unavailable(request, step, err)
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writeAPIError(writer, status, "state_unavailable", "OwnGit state is unavailable.", nil)
		return
	}
	app.writePlainError(writer, status)
}

// internalError logs why step of request failed and returns 500 Internal
// Server Error, the status that answers it: OwnGit found a fault in itself
// or its data, such as a page it cannot render or a pull request whose
// records disagree, which retrying does not fix.
func internalError(request *http.Request, step string, err error) int {
	logFailure(request, step, err)
	return http.StatusInternalServerError
}

// failureText is the page text for a failure answered with status 500 or
// 503, the statuses internalError and unavailable return: a fault in OwnGit
// for 500, which waiting does not fix, and work that could not be done now
// for 503. A page answering another status says what that status means.
func failureText(status int) webui.MessageCode {
	if status == http.StatusInternalServerError {
		return webui.MsgErrInternal
	}
	return webui.MsgErrUnavailable
}

// logFailure logs why step of request could not be completed. An answer
// logs through unavailable or internalError. What logs here directly is not
// answered in this package: a part of a page shown as unavailable, such as
// a side panel, a choice that is right whatever the read would have given,
// and a Git request, which githttp answers. The line holds only the method
// and escaped path, cut when long, never the request's password, cookie or
// token, and the cause is quoted and bounded (see logtext). The same step
// and cause met again soon after is counted rather than logged again (see
// failureLog).
func logFailure(request *http.Request, step string, err error) {
	if intendedCause(request.Context(), err) {
		return
	}
	failures.write(logtext.Request(request), step, logtext.Chain(err))
}

// intendedCause reports whether err, from work done under ctx, holds only
// states that work as intended, which the log leaves out; one real failure
// beside them is logged. A server that is stopping (importsync's
// ErrShuttingDown) leaves nothing to fix. A repository being prepared had
// its cause logged when preparation locked it, and an unavailable check
// runtime when OwnGit started. A cancellation is intended only when ctx
// itself was cancelled: the client went away or the work was dropped. One
// from another context left this request waiting for an answer. Then a busy
// repository is intended too: it names only why the work was waiting when
// it ended. A busy repository while the client still waits, or when the
// wait ran out of time, is logged.
func intendedCause(ctx context.Context, err error) bool {
	states := []error{importsync.ErrShuttingDown, repository.ErrRepositoryPreparing, checkrun.ErrRuntimeUnavailable}
	if errors.Is(ctx.Err(), context.Canceled) {
		states = append(states, context.Canceled, repository.ErrRepositoryInUse)
	}
	return logtext.Intended(err, states...)
}

func (app *App) writePlainError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(http.StatusText(status) + "\n"))
}

func (app *App) wakeChecks(repositoryID string) {
	if app.WakeChecks != nil {
		app.WakeChecks(repositoryID)
	}
}

func (app *App) now() time.Time {
	if app.Now != nil {
		return app.Now()
	}
	return time.Now()
}

func cloneWithPath(request *http.Request, path string) *http.Request {
	clone := request.Clone(request.Context())
	urlCopy := *request.URL
	urlCopy.Path = path
	clone.URL = &urlCopy
	return clone
}

func localNext(value, fallback string) string {
	if value == "" {
		return fallback
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") {
		return fallback
	}
	// Browsers drop tab, CR and LF inside a URL, so "/\t/host" would become
	// "//host" and leave this server. Every control character is refused.
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fallback
		}
	}
	return value
}

// maximumForm bounds a form sent from a page. The longest pull request text,
// 64 KiB, is well inside it even when every byte is percent-encoded.
const maximumForm = 1 << 20

// parseForm reads a form sent from one of this server's pages, and answers
// the request itself when it cannot. A form over maximumForm is answered with
// a page that says so: nothing was read, so what was entered cannot be shown
// again, and the page says that too.
func (app *App) parseForm(writer http.ResponseWriter, request *http.Request) bool {
	if !requireFormOrigin(request) {
		http.Error(writer, "request origin is required", http.StatusForbidden)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumForm)
	if err := request.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			app.renderError(writer, request, http.StatusRequestEntityTooLarge, webui.MsgErrFormTooLarge, "")
			return false
		}
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

func postValue(request *http.Request, name string) string {
	return request.PostForm.Get(name)
}

func formChecked(value string) bool {
	return value == "on" || value == "1"
}

func constantEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}
