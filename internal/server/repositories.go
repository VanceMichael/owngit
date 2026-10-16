package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"owngit/internal/gitexec"
	"owngit/internal/logtext"
	"owngit/internal/markdown"
	"owngit/internal/repository"
	"owngit/internal/state"
	"owngit/internal/webui"
)

const maximumActivityCommits = 200_000

func (app *App) handleOverview(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	session, ok := app.requireGeneral(writer, request, settings)
	if !ok {
		return
	}
	// The first dashboard view after setup in this process shows "Setup
	// finished" once: after the terminal setup, or after the sign-in that
	// shared access asked for. The notice travels in the notice cookie.
	if notice := app.setupResult.Swap(nil); notice != nil {
		app.noticeRedirect(writer, request, "/?notice="+*notice)
		return
	}
	chrome, err := app.chrome(writer, request, webui.SectionOverview, "", session.CSRF)
	if err != nil {
		app.answerUnavailable(writer, request, "page frame read", err)
		return
	}
	if app.resultNotice(writer, request) == removedNotice {
		chrome.Notices = app.removedNotices(writer, request, chrome.Viewer.AdminConfirmed)
	}
	repositories, err := app.visibleRepositories(request)
	if err != nil {
		app.renderError(writer, request, unavailable(request, "repository list read", err), webui.MsgErrUnavailable, "")
		return
	}
	ctx := request.Context()
	snapshots, snapshotErrs := app.Repositories.RefSnapshotsWithin(ctx, repositories, repositoryListWait)
	query := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("q")))
	var summaries []webui.RepositorySummary
	// updated holds the time each listed row shows, for the sidebar below.
	updated := make(map[string]time.Time, len(repositories))
	listed := 0
	for index, stored := range repositories {
		// One repository that cannot be read never takes the page down. A
		// repository whose storage is unavailable is locked and prepared
		// again, as at startup; one whose Git data alone cannot be read is
		// marked here and stays available to Git. A repository deleted while
		// the page was built is left out.
		err := snapshotErrs[index]
		snapshot := snapshots[index]
		// A repository that another Git operation holds was not read for
		// this page: it shows its last listing (Stale), or, without one, is
		// listed as in use instead of holding up the page. It is listed only
		// if it still exists, and one found unreadable when it was last read
		// stays marked unreadable.
		busy := errors.Is(err, repository.ErrRepositoryInUse) && ctx.Err() == nil
		if busy || snapshot.Stale {
			if _, exists, lookupErr := app.Store.Repository(ctx, stored.ID); lookupErr == nil && !exists {
				continue
			}
		}
		if err != nil && !busy && !errors.Is(err, repository.ErrRepositoryPreparing) {
			if ctx.Err() != nil {
				app.renderError(writer, request, unavailable(request, "repository read", err), webui.MsgRepoUnreadable, stored.Name)
				return
			}
			if checked := app.Repositories.PrepareUnavailable(ctx, stored.ID); errors.Is(checked, repository.ErrRepositoryNotFound) {
				continue
			} else if checked != nil {
				err = checked
			}
		}
		unreadable := false
		switch {
		case busy:
			unreadable = app.unreadable.reported(stored.ID)
		case snapshot.Stale:
			// Not read now; the listing is from the last successful read.
		case err == nil || errors.Is(err, repository.ErrRepositoryPreparing):
			app.unreadable.recovered(stored.ID)
		default:
			app.unreadable.report(stored.ID, err)
			unreadable = true
		}
		repositories[listed], snapshots[listed], snapshotErrs[listed] = stored, snapshot, err
		listed++
		summary := app.repositorySummary(request, stored, snapshot)
		if err != nil {
			preparing := errors.Is(err, repository.ErrRepositoryPreparing)
			summary = webui.RepositorySummary{
				ID: stored.ID, Name: stored.Name, Description: stored.Description,
				URL: summary.URL, CloneURL: summary.CloneURL, CreatedAt: stored.CreatedAt,
				Preparing: preparing, Unreadable: unreadable, Busy: busy && !unreadable,
			}
		}
		updated[stored.ID] = summary.Updated()
		if query == "" || strings.Contains(strings.ToLower(stored.Name), query) || strings.Contains(strings.ToLower(stored.Description), query) {
			summaries = append(summaries, summary)
		}
	}
	// The sidebar was built before this page read the repositories, from
	// the snapshots it found then. It takes the times this page shows, so
	// the two lists are in the same order.
	for index := range chrome.Nav.Repositories {
		item := &chrome.Nav.Repositories[index]
		item.LastActivity = updated[item.ID]
	}
	webui.OrderNav(chrome.Nav.Repositories, chrome.Nav.Order, chrome.Lang)
	webui.OrderRepositories(summaries, chrome.Nav.Order, chrome.Lang)
	repositories, snapshots, snapshotErrs = repositories[:listed], snapshots[:listed], snapshotErrs[:listed]
	app.activity.forget(repositories)
	present := make([]string, len(repositories))
	for index, stored := range repositories {
		present[index] = stored.ID
	}
	app.Repositories.ForgetRefSnapshots(present)
	app.unreadable.forget(present)
	observation := app.observeActivity(ctx, repositories, activityKeysFrom(snapshots, snapshotErrs), app.activityLimit())
	recent := observation.entries
	sortActivityEntries(recent)
	if len(recent) > 8 {
		recent = recent[:8]
	}
	graph := buildActivityGraph(observation.counts, selectedYear(request, app.now().Year()), app.now(), len(repositories))
	observation.describe(&graph)
	app.render(writer, request, http.StatusOK, webui.OverviewPage{
		Chrome: chrome, Activity: graph, Repositories: summaries, Recent: recent,
		RecentMoreURL: "/activity", TotalCount: len(repositories),
		Release: app.releaseNotice(request, settings, chrome.Viewer.AdminConfirmed),
	})
}

// unreadableLog logs the cause once when a repository is first shown as
// unreadable, and again only after it was read successfully in between. The
// cause goes only to the server log; pages show a fixed notice.
//
// This differs on purpose from unavailable and logFailure, which log
// every failed read behind an unavailable answer or panel. The dashboard
// lists every repository on each view, so one damaged repository would
// otherwise repeat the same line on every visit to the dashboard, while its
// own repository page is one read that one person asked for.
type unreadableLog struct {
	mu     sync.Mutex
	logged map[string]bool
}

func (l *unreadableLog) report(id string, cause error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logged[id] {
		return
	}
	if l.logged == nil {
		l.logged = make(map[string]bool)
	}
	l.logged[id] = true
	log.Printf("repository %q is shown as unreadable because its Git data could not be read: %s", id, logtext.Cause(cause))
}

// reported tells whether id was shown as unreadable when it was last read.
func (l *unreadableLog) reported(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logged[id]
}

func (l *unreadableLog) recovered(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.logged, id)
}

// forget drops repositories that no longer exist.
func (l *unreadableLog) forget(present []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.logged {
		if !slices.Contains(present, id) {
			delete(l.logged, id)
		}
	}
}

func (app *App) handleNewRepositoryGet(writer http.ResponseWriter, request *http.Request, settings state.Settings, name, description string, notices []webui.Notice) {
	session, ok := app.requireGeneral(writer, request, settings)
	if !ok {
		return
	}
	chrome, err := app.chrome(writer, request, webui.SectionOverview, "", session.CSRF)
	if err != nil {
		app.answerUnavailable(writer, request, "page frame read", err)
		return
	}
	chrome.Notices = notices
	status := http.StatusOK
	if len(notices) != 0 {
		status = http.StatusUnprocessableEntity
	}
	app.render(writer, request, status, webui.NewRepositoryPage{
		Chrome: chrome, SubmitURL: "/repositories", Name: name, Description: description, NameRules: webui.MsgRepoNameRules,
	})
}

func (app *App) handleCreateRepository(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	if _, ok := app.requireGeneral(writer, request, settings); !ok {
		return
	}
	if !app.parseForm(writer, request) {
		return
	}
	if !app.requireCSRF(writer, request) {
		return
	}
	name := strings.TrimSpace(postValue(request, "name"))
	description := strings.TrimSpace(postValue(request, "description"))
	created, err := app.Repositories.Create(request.Context(), name, description)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrFailedCreationLimit):
			app.renderError(writer, request, unavailable(request, "repository creation", err), webui.MsgRepoCreationKept, strings.ToLower(name)+".git")
		case errors.Is(err, repository.ErrStorageChanged):
			logFailure(request, "repository creation", err)
			app.renderError(writer, request, http.StatusConflict, webui.MsgStorageChanged, "")
		case errors.Is(err, repository.ErrStorageInUse):
			logFailure(request, "repository creation", err)
			app.renderError(writer, request, http.StatusConflict, webui.MsgSetupStorageInUse, "")
		case errors.Is(err, repository.ErrImportInProgress):
			app.handleNewRepositoryGet(writer, request, settings, name, description, []webui.Notice{webui.Error("name", webui.MsgRepoNameBusy)})
		case errors.Is(err, repository.ErrNameTaken):
			app.handleNewRepositoryGet(writer, request, settings, name, description, []webui.Notice{webui.Error("name", webui.MsgRepoNameTaken)})
		case errors.Is(err, repository.ErrReservedName):
			app.handleNewRepositoryGet(writer, request, settings, name, description, []webui.Notice{webui.Error("name", webui.MsgRepoNameReserved)})
		case errors.Is(err, repository.ErrInvalidName):
			app.handleNewRepositoryGet(writer, request, settings, name, description, []webui.Notice{webui.Error("name", webui.MsgRepoNameInvalid)})
		case errors.Is(err, repository.ErrInvalidDescription):
			app.handleNewRepositoryGet(writer, request, settings, name, description, []webui.Notice{webui.Error("description", webui.MsgRepoDescriptionTooLong)})
		case errors.As(err, new(*state.PolicyError)):
			logFailure(request, "repository creation", err)
			app.renderError(writer, request, http.StatusConflict, webui.MsgBranchUnreadableCreate, "")
		default:
			app.renderError(writer, request, unavailable(request, "repository creation", err), webui.MsgRepoCreateFail, "")
		}
		return
	}
	app.noticeRedirect(writer, request, "/repositories/"+url.PathEscape(created.ID)+"?notice=repository_created")
}

func (app *App) handleActivity(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	session, ok := app.requireGeneral(writer, request, settings)
	if !ok {
		return
	}
	chrome, err := app.chrome(writer, request, webui.SectionActivity, "", session.CSRF)
	if err != nil {
		app.answerUnavailable(writer, request, "page frame read", err)
		return
	}
	repositories, err := app.visibleRepositories(request)
	if err != nil {
		app.renderError(writer, request, unavailable(request, "repository list read", err), webui.MsgActivityUnavail, "")
		return
	}
	year := selectedYear(request, app.now().Year())
	// A day outside the chosen year, or not a day at all, lists the year.
	var day time.Time
	if parsed, err := time.ParseInLocation("2006-01-02", request.URL.Query().Get("date"), app.now().Location()); err == nil && parsed.Year() == year {
		day = parsed
	}
	listing := app.readActivity(request, repositories, year, day)
	app.render(writer, request, http.StatusOK, webui.ActivityPage{Chrome: chrome, Activity: listing.graph, Days: groupActivity(listing.entries), Truncated: listing.truncated, ListLimit: maximumActivityEntries})
}

func (app *App) handleRepositoryRoute(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	remainder := strings.TrimPrefix(request.URL.Path, "/repositories/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || parts[0] == "" {
		app.renderError(writer, request, http.StatusNotFound, webui.MsgRepoNotFound, "")
		return
	}
	var session state.Session
	var ok bool
	// The helper credential, execution policy, and runner token screens are
	// administrator only, and that is decided here by route rather than by
	// which controls a page would draw. Hiding a button is presentation; this
	// is the authorization.
	if len(parts) >= 2 && administratorRepositoryScreen(parts[1]) {
		session, ok = app.requireAdminPage(writer, request)
	} else {
		session, ok = app.requireGeneral(writer, request, settings)
	}
	if !ok {
		return
	}
	id, ok := app.answerRepositoryAddressPage(writer, request)
	if !ok {
		return
	}
	stored, exists, err := app.Store.Repository(request.Context(), id)
	if err != nil {
		// A record that could not be read says nothing about whether the
		// repository exists.
		app.renderError(writer, request, unavailable(request, "repository record read", err), webui.MsgErrUnavailable, "")
		return
	}
	if !exists {
		app.renderError(writer, request, http.StatusNotFound, webui.MsgRepoNotFound, id)
		return
	}
	chrome, err := app.chrome(writer, request, webui.SectionRepository, id, session.CSRF)
	if err != nil {
		app.answerUnavailable(writer, request, "page frame read", err)
		return
	}
	// Deleting does not need the Git data, so it is routed before that read:
	// a repository whose data cannot be read can still be removed.
	if len(parts) == 2 && parts[1] == "delete" {
		app.handleRepositoryDelete(writer, request, stored, chrome, session)
		return
	}
	// Share links are state records alone, so they are managed without a
	// Git read too.
	if len(parts) == 2 && parts[1] == "share-links" {
		app.handleShareLinks(writer, request, stored, chrome, session)
		return
	}
	// The credential screens read and change only the state database. While
	// the repository is being prepared they are routed before the Git read,
	// with an empty summary, so an administrator can still revoke a
	// credential, as the API allows.
	if len(parts) == 2 && app.Repositories.Preparing(id) {
		switch parts[1] {
		case "helper-credentials":
			app.handleHelperCredentials(writer, request, stored, repository.Summary{})
			return
		case "runner-tokens":
			app.handleRunnerTokens(writer, request, stored, repository.Summary{})
			return
		}
	}
	var address pageAddress
	if request.Method == http.MethodGet && (len(parts) == 1 || parts[1] == "code" || parts[1] == "commits") {
		address, err = parsePageAddress(request, parts[1:])
		if err != nil {
			app.renderError(writer, request, http.StatusBadRequest, webui.MsgErrNotFound, "")
			return
		}
	}
	// One ref listing gives the summary every tab needs and the activity key
	// the overview's graph needs, where Summary alone would start two Git
	// processes and the graph a third.
	snapshot, err := app.Repositories.RefSnapshot(request.Context(), id)
	summary := snapshot.Summary
	if err != nil {
		app.renderRepositoryReadFailure(writer, request, app.baseRepositoryPage(request, chrome, stored, repository.Summary{}), err)
		return
	}
	if len(parts) == 2 && parts[1] == "pull-requests" {
		switch request.Method {
		case http.MethodGet:
			app.handlePullRequestsGet(writer, request, stored, summary, chrome)
		case http.MethodPost:
			app.handleCreatePullRequest(writer, request, stored, summary, chrome)
		}
		return
	}
	if len(parts) == 3 && parts[1] == "pull-requests" && parts[2] == "new" && request.Method == http.MethodGet {
		app.handleNewPullRequestGet(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) >= 3 && parts[1] == "pull-requests" {
		if number, valid := parsePullRequestNumber(parts[2]); valid {
			switch {
			case len(parts) == 3 && request.Method == http.MethodGet:
				app.handlePullRequestGet(writer, request, stored, summary, chrome, number)
				return
			case len(parts) == 5 && parts[3] == "review" && parts[4] == "request" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "review_request")
				return
			case len(parts) == 5 && parts[3] == "review" && parts[4] == "skip" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "review_skip")
				return
			case len(parts) == 5 && parts[3] == "review" && parts[4] == "submit" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "review_submit")
				return
			case len(parts) == 4 && parts[3] == "edit" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "edit")
				return
			case len(parts) == 4 && parts[3] == "merge" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "merge")
				return
			case len(parts) == 4 && parts[3] == "mergeability" && request.Method == http.MethodPost:
				app.handlePullRequestMergeability(writer, request, stored, summary, chrome, number)
				return
			case len(parts) == 4 && (parts[3] == "close" || parts[3] == "reopen") && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, parts[3])
				return
			case len(parts) == 4 && parts[3] == "auto-merge" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "auto_merge_enable")
				return
			case len(parts) == 5 && parts[3] == "auto-merge" && parts[4] == "cancel" && request.Method == http.MethodPost:
				app.handlePullRequestAction(writer, request, stored, summary, chrome, number, "auto_merge_cancel")
				return
			}
		}
	}
	if len(parts) == 2 && parts[1] == "tasks" && request.Method == http.MethodGet {
		app.handleTasksGet(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) == 2 && parts[1] == "helper-credentials" {
		app.handleHelperCredentials(writer, request, stored, summary)
		return
	}
	if len(parts) == 2 && parts[1] == "configured-checks" {
		app.handleConfiguredChecks(writer, request, stored, summary)
		return
	}
	if len(parts) == 2 && parts[1] == "runner-tokens" {
		app.handleRunnerTokens(writer, request, stored, summary)
		return
	}
	if len(parts) == 2 && parts[1] == "settings" && request.Method == http.MethodGet {
		app.handleRepositorySettingsGet(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) == 3 && parts[1] == "settings" && parts[2] == "default-branch" && request.Method == http.MethodPost {
		app.handleSetDefaultBranch(writer, request, stored, summary, chrome, session)
		return
	}
	if len(parts) == 3 && parts[1] == "settings" && parts[2] == "rename" && request.Method == http.MethodPost {
		app.handleRenameRepository(writer, request, stored, summary, chrome, session)
		return
	}
	if len(parts) == 3 && parts[1] == "settings" && parts[2] == "history" && request.Method == http.MethodPost {
		app.handleSaveHistory(writer, request, stored, summary, chrome, session)
		return
	}
	if len(parts) == 3 && parts[1] == "settings" && parts[2] == "ref-namespaces" && request.Method == http.MethodPost {
		app.handleSaveNamespaces(writer, request, stored, summary, chrome, session)
		return
	}
	if len(parts) == 2 && parts[1] == "import" {
		app.handleImportPage(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) == 2 && parts[1] == "restore" && request.Method == http.MethodGet {
		app.handleRestoreGet(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) == 3 && parts[1] == "restore" && parts[2] == "preview" && request.Method == http.MethodPost {
		app.handleRestorePreview(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) == 2 && parts[1] == "restore" && request.Method == http.MethodPost {
		app.handleRestoreApply(writer, request, stored, summary, chrome)
		return
	}
	if len(parts) == 2 && parts[1] == "archive" && request.Method == http.MethodGet {
		app.handleArchive(writer, request, stored)
		return
	}
	if len(parts) == 2 && parts[1] == "raw" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		app.handleRaw(writer, request, stored)
		return
	}
	if request.Method != http.MethodGet {
		app.renderError(writer, request, http.StatusNotFound, webui.MsgErrNotFound, request.URL.Path)
		return
	}
	app.serveCodePage(writer, request, app.baseRepositoryPage(request, chrome, stored, summary), snapshot, parts[1:], address)
}

// serveCodePage answers a GET of a repository's overview (tab empty), Code
// and Commits pages from snapshot, the repository's ref snapshot. base is
// the page before it is filled: a dashboard page (baseRepositoryPage) or a
// share link's (sharedRepositoryPage).
func (app *App) serveCodePage(writer http.ResponseWriter, request *http.Request, base webui.RepositoryPage, snapshot repository.RefSnapshot, tab []string, address pageAddress) {
	page := base
	requestedRef := address.Ref
	status := http.StatusOK
	var err error
	switch {
	case len(tab) == 0:
		page.Tab = webui.RepoTabOverview
		err = app.fillRepositoryOverview(request, &page, snapshot, requestedRef)
		if requestedRef != "" && page.Ref.Missing {
			status = http.StatusNotFound
		}
	case len(tab) == 1 && tab[0] == "code":
		page.Tab = webui.RepoTabCode
		err = app.fillCode(request, &page, snapshot, address)
		// A path or a requested branch or tag that does not exist is not
		// found. The page keeps the repository and links back to it. A
		// default branch that has gone is the repository's state, not a bad
		// address, so it stays a normal page with its warning.
		if page.Code.NotFound || (requestedRef != "" && page.Ref.Missing) {
			status = http.StatusNotFound
		}
	case len(tab) == 1 && tab[0] == "commits":
		page.Tab = webui.RepoTabCommits
		err = app.fillCommits(request, &page, snapshot, address, "")
		if page.Commits.NotFound || (requestedRef != "" && page.Ref.Missing) {
			status = http.StatusNotFound
		}
	case len(tab) == 2 && tab[0] == "commits":
		page.Tab = webui.RepoTabCommits
		// A commit this repository does not have, including one that only
		// another repository has, is not found. A commit that exists opens
		// even when the address names a missing branch: the page then shows
		// it as a bare revision.
		err = app.fillCommits(request, &page, snapshot, address, tab[1])
		if page.Commits.NotFound {
			status = http.StatusNotFound
		}
	default:
		app.renderError(writer, request, http.StatusNotFound, webui.MsgErrNotFound, request.URL.Path)
		return
	}
	var missingLine *lineNotFoundError
	if errors.As(err, &missingLine) {
		app.render(writer, request, http.StatusNotFound, webui.ErrorPage{Chrome: base.Chrome, Status: http.StatusNotFound,
			Code: webui.MsgCodeLineNotFound, RetryURL: missingLine.firstURL, RetryLabel: webui.MsgCodeFirstPage})
		return
	}
	if errors.Is(err, errPageAddress) {
		app.renderError(writer, request, http.StatusBadRequest, webui.MsgErrNotFound, "")
		return
	}
	// A read that failed is explained instead of the page, before any 404:
	// it is never reported as a missing ref, commit or path. So is one that
	// ran out of time, since it can leave a side panel unread too.
	if err == nil {
		err = request.Context().Err()
	}
	if err != nil {
		if request.Context().Err() != nil && app.Repositories.InUse(page.Repo.ID) {
			err = repository.ErrRepositoryInUse
		}
		app.renderRepositoryReadFailure(writer, request, base, err)
		return
	}
	page.NotFound = status == http.StatusNotFound
	app.render(writer, request, status, page)
}

// renderRepositoryReadFailure answers a repository page whose Git data could
// not be read: not found when the repository was deleted meanwhile, and
// otherwise unavailable, with the reason when it is known.
func (app *App) renderRepositoryReadFailure(writer http.ResponseWriter, request *http.Request, page webui.RepositoryPage, cause error) {
	if errors.Is(cause, repository.ErrRepositoryNotFound) {
		app.renderError(writer, request, http.StatusNotFound, webui.MsgRepoNotFound, page.Repo.ID)
		return
	}
	page.Repo.Unreadable, page.Repo.Empty = true, false
	switch {
	case errors.Is(cause, repository.ErrRepositoryPreparing):
		// The notice is fixed; the cause is only in the server log.
		page.Repo.Preparing = true
		writer.Header().Set("Retry-After", "30")
	case errors.Is(cause, repository.ErrRepositoryInUse):
		page.Repo.UnreadableReason = webui.MsgRepoBusyInUse
		writer.Header().Set("Retry-After", "10")
	case errors.Is(cause, gitexec.ErrReadMemoryBusy):
		// The memory this computer gives Git is given to other Git work right
		// now, as the transfer gate allows; the same read succeeds shortly.
		page.Repo.UnreadableReason = webui.MsgRepoBusyMemory
		writer.Header().Set("Retry-After", "10")
	case errors.As(cause, new(*state.PolicyError)):
		// A saved setting stops the read; the page names it.
		logFailure(request, "repository read", cause)
		page.Repo.UnreadableReason = webui.MsgBrowseUnreadable
		app.render(writer, request, http.StatusConflict, page)
		return
	}
	app.render(writer, request, unavailable(request, "repository read", cause), page)
}

// administratorRepositoryScreen names the repository screens that require an
// administrator session before anything is read. They all read or change
// execution authority, so a general session must not reach them at all.
// The Import tab is not one of them: its status is readable by anyone who
// can read the repository, and handleImportPage requires the administrator
// session for every change and keeps administrator data out of other views.
func administratorRepositoryScreen(segment string) bool {
	switch segment {
	case "helper-credentials", "configured-checks", "runner-tokens", "settings", "delete", "share-links":
		return true
	default:
		return false
	}
}

func (app *App) baseRepositoryPage(request *http.Request, chrome webui.Chrome, stored state.Repository, summary repository.Summary) webui.RepositoryPage {
	base := "/repositories/" + url.PathEscape(stored.Address)
	clone := app.cloneURL(request, stored.Address)
	page := webui.RepositoryPage{
		Chrome:      chrome,
		Repo:        webui.RepositoryHeader{ID: stored.ID, Address: stored.Address, Name: stored.Name, Description: stored.Description, URL: base, CloneURL: clone, Empty: summary.Empty},
		OverviewURL: base, CodeURL: base + "/code", CommitsURL: base + "/commits",
		RestoreURL:      restoreURL(stored.Address, summary.DefaultOID, summary.DefaultBranch, ""),
		PullRequestsURL: base + "/pull-requests",
		TasksURL:        base + "/tasks",
		ImportsURL:      base + "/import",
	}
	// The administrator sections are offered to every viewer. Their routes
	// send a viewer without an administrator session to the login, which
	// returns to the section afterwards.
	page.SettingsURL = base + "/settings"
	page.DeleteURL = base + "/delete"
	return page
}

// ownerRestoreURL is restoreURL for the repository of page, or "" on a
// shared page, which leads to nothing an owner does.
func ownerRestoreURL(page *webui.RepositoryPage, sourceOID, target, filePath string) string {
	if page.Shared {
		return ""
	}
	return restoreURL(page.Repo.Address, sourceOID, target, filePath)
}

// ownerArchiveLinks is archiveLinks for the repository of page, or none on
// a shared page, which offers what a share link gives: pages, raw files
// and, for a clone link, Git.
func ownerArchiveLinks(page *webui.RepositoryPage, ref string) []webui.ArchiveLink {
	if page.Shared {
		return nil
	}
	return archiveLinks(page.Repo.Address, ref)
}

// selectRef resolves the requested ref, or the default branch, for a page
// and fills its ref picker. It reports whether the ref resolved; one that
// does not exist is marked Missing. A lookup that failed is returned as an
// error, since it tells nothing about the ref.
func (app *App) selectRef(request *http.Request, page *webui.RepositoryPage, snapshot repository.RefSnapshot, requested string) (string, bool, error) {
	summary := snapshot.Summary
	selected := requested
	if selected == "" && summary.DefaultBranch != "" {
		selected = "refs/heads/" + summary.DefaultBranch
	}
	canonical, oid, resolved := "", "", false
	if selected != "" {
		var err error
		canonical, err = repository.BrowseRefName(summary, selected)
		if err == nil {
			oid, err = app.Repositories.RefCommitAt(request.Context(), page.Repo.ID, snapshot, canonical)
		} else if errors.Is(err, repository.ErrNotFound) {
			// A present snapshot identity remains selectable after a write.
			// Absent requests retain live validation and missing-ref behavior.
			canonical, oid, err = app.Repositories.ResolveRef(request.Context(), page.Repo.ID, selected)
		}
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return "", false, err
		}
		resolved = err == nil && oid != ""
	}
	if resolved {
		selected = canonical
	}
	page.Ref.Name = displayRef(selected)
	page.Ref.Kind = refKind(selected)
	page.Ref.IsDefault = selected != "" && selected == "refs/heads/"+summary.DefaultBranch
	for _, branch := range summary.Branches {
		full := "refs/heads/" + branch.Name
		option := webui.RefOption{Name: branch.Name, URL: withRef(request.URL.Path, full), Selected: selected == full, IsDefault: branch.Name == summary.DefaultBranch}
		page.Ref.Branches = append(page.Ref.Branches, option)
	}
	for _, tag := range summary.Tags {
		full := "refs/tags/" + tag.Name
		page.Ref.Tags = append(page.Ref.Tags, webui.RefOption{Name: tag.Name, URL: withRef(request.URL.Path, full), Selected: selected == full})
	}
	// The tabs keep only a ref that resolves. A missing one would send each
	// tab to a not-found page, so they then open the repository's
	// default addresses; the picker still shows the missing name.
	if resolved {
		page.OverviewURL = withRef(page.OverviewURL, selected)
		page.CodeURL = withRef(page.CodeURL, selected)
		page.CommitsURL = withRef(page.CommitsURL, selected)
	}
	if summary.Empty {
		return "", false, nil
	}
	if !resolved {
		page.Ref.Missing = true
		return "", false, nil
	}
	page.Ref.Revision = oid
	page.Ref.ShortRevision = shortOID(oid)
	return canonical, true, nil
}

func displayRef(ref string) string {
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ref
}

func refKind(ref string) string {
	if strings.HasPrefix(ref, "refs/tags/") {
		return "tag"
	}
	return "branch"
}

// Bounds for the overview. Every figure the overview shows comes from one
// bounded read, so a repository with thousands of refs, commits, or pull
// requests costs the same page work as a small one.
const (
	overviewRecentCommits = 7
	overviewOpenPRBound   = 99
	overviewBranchRows    = 6
	overviewTagRows       = 5
	overviewRetainedRows  = 5
	overviewAllRefsQuery  = "refs"
	overviewAllRefsValue  = "all"
	overviewRefsFragment  = "#refs"
)

// fillRepositoryOverview fills the overview. Its body, the recent commits
// and the top folder of the selected ref, is required: a failed read of it
// is returned, and the page then says the repository cannot be read. Each
// side panel (languages, activity, open pull requests, the default branch
// check, ref tips and kept history) is read on its own, and one that cannot
// be read says so on the page. It is never shown as empty, zero or absent.
func (app *App) fillRepositoryOverview(request *http.Request, page *webui.RepositoryPage, snapshot repository.RefSnapshot, requested string) error {
	summary := snapshot.Summary
	selectedRef, resolved, err := app.selectRef(request, page, snapshot, requested)
	if err != nil {
		return err
	}
	if page.Ref.Missing {
		code := webui.MsgRepoRefMissing
		if requested == "" {
			code = webui.MsgRepoDefaultGone
		}
		page.Chrome.Notices = append(page.Chrome.Notices, webui.Notice{Kind: webui.NoticeWarning, Code: code})
	}
	if summary.DefaultOID != "" {
		page.Overview.DefaultBranch = summary.DefaultBranch
	}
	page.Overview.BranchCount = len(summary.Branches)
	page.Overview.TagCount = len(summary.Tags)
	if page.Repo.Empty {
		// HEAD names the branch the repository started on, as Settings
		// chose it when the repository was created.
		branch := summary.DefaultBranch
		if branch == "" {
			branch = "HEAD"
		}
		if !page.Shared {
			page.Overview.PushCommands = []string{
				"git remote add origin " + page.Repo.CloneURL,
				"git push -u origin " + branch,
			}
			page.Overview.Activity = emptyActivityGraph(selectedYear(request, app.now().Year()), app.now(), page.Repo.Name)
		}
		return nil
	}
	page.Overview.Languages = app.overviewLanguages(request, page.Repo.ID, summary.DefaultOID)
	if resolved {
		// One extra commit says whether older history exists without counting it.
		commits, err := app.Repositories.CommitsAt(request.Context(), page.Repo.ID, page.Ref.Revision, overviewRecentCommits+1)
		if err := app.noteUnreadableCommits(request, page, selectedRef, err); err != nil {
			return err
		}
		if len(commits) > 0 {
			if len(commits) > overviewRecentCommits {
				page.Overview.RecentMore = true
				commits = commits[:overviewRecentCommits]
			}
			for _, commit := range commits {
				page.Overview.Recent = append(page.Overview.Recent, app.commitSummary(page.Repo.URL, selectedRef, commit))
			}
			// An unreadable tip leaves the latest commit unknown, not its
			// parent.
			if commits[0].OID == page.Ref.Revision {
				page.Overview.Head = page.Overview.Recent[0]
			}
		}
		// The README answers what the repository is. It is found and
		// rendered exactly as the code view does for the top folder.
		listing, err := app.Repositories.TreePageAt(request.Context(), page.Repo.ID, page.Ref.Revision, "", "")
		if err != nil {
			return err
		}
		page.Overview.Readme = app.folderReadme(request, page.Repo, selectedRef, "", listing.Readme)
	}
	// A shared page shows the code only: pull requests, checks, kept history
	// and the activity graph, which counts kept history, are the owner's.
	if !page.Shared {
		app.fillOverviewEvidence(request, page, summary)
	}

	showAll := request.URL.Query().Get(overviewAllRefsQuery) == overviewAllRefsValue
	branchTips, err := app.Repositories.RefTipsAt(request.Context(), page.Repo.ID, snapshot, false)
	page.Overview.BranchTipsKnown = err == nil
	if err != nil {
		logFailure(request, "branch tip read", err)
	}
	tagTips, err := app.Repositories.RefTipsAt(request.Context(), page.Repo.ID, snapshot, true)
	page.Overview.TagTipsKnown = err == nil
	if err != nil {
		logFailure(request, "tag tip read", err)
	}
	var branches, tags, retainedLines []webui.RefLine
	for _, branch := range summary.Branches {
		full := "refs/heads/" + branch.Name
		line := webui.RefLine{Name: branch.Name, Kind: "branch", IsDefault: branch.Name == summary.DefaultBranch, URL: withRef(page.Repo.URL, full)}
		if commit, ok := branchTips[branch.Name]; ok {
			line.Tip = app.commitSummary(page.Repo.URL, full, commit)
			line.RestoreURL = ownerRestoreURL(page, commit.OID, branch.Name, "")
		}
		branches = append(branches, line)
	}
	for _, tag := range summary.Tags {
		full := "refs/tags/" + tag.Name
		line := webui.RefLine{Name: tag.Name, Kind: "tag", URL: withRef(page.Repo.URL, full), Annotated: tag.Type == "tag"}
		if commit, ok := tagTips[tag.Name]; ok {
			line.Tip = app.commitSummary(page.Repo.URL, full, commit)
			line.RestoreURL = ownerRestoreURL(page, commit.OID, summary.DefaultBranch, "")
		}
		tags = append(tags, line)
	}
	var retained []repository.RetainedRef
	if !page.Shared {
		retained, err = app.Repositories.RetainedRefsAt(request.Context(), page.Repo.ID, snapshot)
		page.Overview.RetainedKnown = err == nil
		if err != nil {
			logFailure(request, "kept history read", err)
		}
	}
	for _, ref := range retained {
		line := webui.RefLine{Name: shortOID(ref.OID), Kind: ref.Kind, Retained: true}
		if ref.CommitOID != "" {
			line.Tip = app.commitSummary(page.Repo.URL, "", ref.Commit)
			line.URL = line.Tip.URL
			line.RestoreURL = ownerRestoreURL(page, ref.CommitOID, recoveredTarget(ref.OID, summary.Branches), "")
		}
		retainedLines = append(retainedLines, line)
	}
	// Newest first, so a repository with many releases shows its recent ones
	// rather than the first names in alphabetical order.
	sortRefLinesNewestFirst(branches)
	sortRefLinesNewestFirst(tags)
	sortRefLinesNewestFirst(retainedLines)
	page.Overview.RetainedCount = len(retainedLines)
	hidden := false
	if !showAll {
		var cut bool
		branches, cut = firstRefLines(branches, overviewBranchRows)
		hidden = hidden || cut
		tags, cut = firstRefLines(tags, overviewTagRows)
		hidden = hidden || cut
		retainedLines, cut = firstRefLines(retainedLines, overviewRetainedRows)
		hidden = hidden || cut
	}
	page.Overview.Branches, page.Overview.Tags, page.Overview.RetainedRefs = branches, tags, retainedLines
	switch {
	case showAll:
		page.Overview.FewerRefsURL = overviewRefsURL(request, false)
	case hidden:
		page.Overview.AllRefsURL = overviewRefsURL(request, true)
	}

	if !page.Shared {
		page.Overview.Activity = app.repositoryActivityGraph(request, page, snapshot.ActivityKey)
	}
	return nil
}

// fillOverviewEvidence reads the two summary figures that come from OwnGit's
// own records: how many pull requests are open, and the latest check on the
// default branch tip. Each is one bounded query. A failed read is reported as
// unreadable instead of as zero or as "no check".
func (app *App) fillOverviewEvidence(request *http.Request, page *webui.RepositoryPage, summary repository.Summary) {
	open, more, err := app.Store.OpenPullRequests(request.Context(), page.Repo.ID, overviewOpenPRBound)
	if err == nil {
		page.Overview.OpenPullRequests = len(open)
		page.Overview.OpenPullRequestsMore = more
		page.Overview.OpenPullRequestsKnown = true
	} else {
		logFailure(request, "open pull request count read", err)
	}
	if summary.DefaultOID == "" {
		return
	}
	page.Overview.DefaultCheckRev = summary.DefaultOID
	attempt, exists, err := app.Store.LatestCheckAttemptForRevision(request.Context(), page.Repo.ID, summary.DefaultOID)
	if err != nil {
		logFailure(request, "default branch check read", err)
		return
	}
	page.Overview.DefaultCheckKnown = true
	if exists {
		page.Overview.HasDefaultCheck = true
		page.Overview.DefaultCheck = app.browserAttemptRecord(request.Context(), attempt)
	}
}

// sortRefLinesNewestFirst orders rows by the tip date the row displays, so the
// list reads in the order it claims. A row without a tip goes last, and equal
// dates fall back to the name so the order is stable across reloads.
func sortRefLinesNewestFirst(lines []webui.RefLine) {
	sort.SliceStable(lines, func(i, j int) bool {
		left, right := lines[i], lines[j]
		if left.IsDefault != right.IsDefault {
			return left.IsDefault
		}
		if !left.Tip.AuthorDate.Equal(right.Tip.AuthorDate) {
			return left.Tip.AuthorDate.After(right.Tip.AuthorDate)
		}
		return left.Name < right.Name
	})
}

// firstRefLines keeps at most limit rows and reports whether any were cut.
func firstRefLines(lines []webui.RefLine, limit int) ([]webui.RefLine, bool) {
	if len(lines) <= limit {
		return lines, false
	}
	return lines[:limit], true
}

// overviewRefsURL is this overview with every ref listed, or with the short
// lists again. It keeps the selected ref and language, and lands on the ref
// column.
func overviewRefsURL(request *http.Request, all bool) string {
	values := request.URL.Query()
	values.Del(overviewAllRefsQuery)
	values.Del("notice")
	if all {
		values.Set(overviewAllRefsQuery, overviewAllRefsValue)
	}
	target := request.URL.Path
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target + overviewRefsFragment
}

func recoveredTarget(oid string, branches []repository.Ref) string {
	base := "recovered-" + shortOID(oid)
	used := make(map[string]bool, len(branches))
	for _, branch := range branches {
		used[strings.ToLower(branch.Name)] = true
	}
	if !used[strings.ToLower(base)] {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if !used[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

var errPageAddress = errors.New("invalid page address")

type pageAddress struct {
	Ref, Path, View, Revision, Blob, After string
	// Root and Skip place a later page of the list of commits: the page
	// shows the history of commit Root after its first Skip commits.
	Root  string
	Skip  int
	Lines linePageRequest
}

type linePageRequest struct {
	First, Line, From int
}

type lineNotFoundError struct{ firstURL string }

func (*lineNotFoundError) Error() string { return "line is not in the file" }

// parsePageAddress runs after authority checks but before any repository read.
// Original positions survive rounding for later content-range checks.
func parsePageAddress(request *http.Request, tab []string) (pageAddress, error) {
	address := pageAddress{Lines: linePageRequest{First: 1}}
	// Maximum-size paths and arbitrary-byte cursors fit when percent-encoded.
	if len(request.URL.RawQuery) > 64<<10 {
		return address, errPageAddress
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return address, errPageAddress
	}
	for _, key := range []string{"revision", "blob", "after", "root"} {
		values, present := query[key]
		if !present {
			continue
		}
		if len(values) != 1 || values[0] == "" {
			return address, errPageAddress
		}
		if key == "after" {
			if repository.ValidateTreeCursor(values[0]) != nil {
				return address, errPageAddress
			}
		} else if !validOID(values[0]) {
			return address, errPageAddress
		}
	}
	if len(tab) == 2 && tab[0] == "commits" && !validOID(tab[1]) {
		return address, errPageAddress
	}
	address.Ref, address.Path, address.View = query.Get("ref"), query.Get("path"), query.Get("view")
	address.Revision, address.Blob, address.After, address.Root = query.Get("revision"), query.Get("blob"), query.Get("after"), query.Get("root")
	if values, present := query["skip"]; present || address.Root != "" {
		skipped, err := strconv.Atoi(query.Get("skip"))
		if len(values) != 1 || err != nil || skipped < 1 || address.Root == "" {
			return address, errPageAddress
		}
		address.Skip = skipped
	}
	for _, key := range []string{"line", "from"} {
		values, present := query[key]
		if !present {
			continue
		}
		if len(values) != 1 {
			return address, errPageAddress
		}
		position, err := strconv.Atoi(values[0])
		if err != nil || position < 1 {
			return address, errPageAddress
		}
		if key == "line" {
			address.Lines.Line = position
		} else {
			address.Lines.From = position
		}
	}
	position := max(1, address.Lines.From)
	if address.Lines.Line != 0 {
		position = address.Lines.Line
	}
	address.Lines.First = (position-1)/maximumCommitDiffLines*maximumCommitDiffLines + 1
	return address, nil
}

func (page linePageRequest) check(total int, incomplete bool, firstURL string) error {
	if page.From > total {
		return errPageAddress
	}
	if page.Line > total {
		if incomplete {
			// A byte-limited prefix cannot prove this line is absent in the file.
			return errPageAddress
		}
		return &lineNotFoundError{firstURL: firstURL}
	}
	return nil
}

func sourceLinePage(content []byte, first int) ([]string, int) {
	var lines []string
	total := 0
	for line := range strings.SplitSeq(strings.TrimSuffix(string(content), "\n"), "\n") {
		total++
		if total >= first && len(lines) < maximumCommitDiffLines {
			lines = append(lines, strings.TrimSuffix(line, "\r"))
		}
	}
	return lines, total
}

func pinnedPageURL(address, revision, blob string) string {
	parsed, _ := url.Parse(address)
	query := parsed.Query()
	if revision != "" {
		query.Set("revision", revision)
	}
	if blob != "" {
		query.Set("blob", blob)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func lineContinuation(address string, first, shown, total int) webui.PageContinuation {
	continuation := webui.PageContinuation{First: first, Last: first + shown - 1, Total: total}
	if total <= maximumCommitDiffLines {
		return continuation
	}
	parsed, _ := url.Parse(address)
	query := parsed.Query()
	query.Del("line")
	query.Del("from")
	parsed.RawQuery = query.Encode()
	if first > 1 {
		continuation.FirstURL = parsed.String()
	}
	if continuation.Last < total {
		query.Set("from", strconv.Itoa(continuation.Last+1))
		parsed.RawQuery = query.Encode()
		continuation.MoreURL = parsed.String()
	}
	return continuation
}

func directoryContinuation(address, revision string, listing repository.TreePage) webui.PageContinuation {
	continuation := webui.PageContinuation{First: listing.Before + 1, Last: listing.Before + len(listing.Entries), Total: listing.Total}
	address = pinnedPageURL(address, revision, "")
	if listing.Before > 0 {
		continuation.FirstURL = address
	}
	if listing.After != "" {
		parsed, _ := url.Parse(address)
		query := parsed.Query()
		query.Set("after", listing.After)
		parsed.RawQuery = query.Encode()
		continuation.MoreURL = parsed.String()
	}
	return continuation
}

func (app *App) codePageRevision(request *http.Request, page *webui.RepositoryPage, revision string) (string, error) {
	if revision == "" || revision == page.Ref.Revision {
		return page.Ref.Revision, nil
	}
	_, pinned, err := app.Repositories.ResolveRevision(request.Context(), page.Repo.ID, revision)
	if err != nil {
		return "", err
	}
	if pinned != revision {
		return "", repository.ErrNotFound
	}
	if page.Shared {
		// A share never gains access to commits held only by kept history.
		reachable, err := app.Repositories.CommitReachableFrom(request.Context(), page.Repo.ID, page.Ref.Revision, pinned)
		if err != nil {
			return "", err
		}
		if !reachable {
			return "", repository.ErrNotFound
		}
	}
	if page.Ref.Revision == "" {
		page.Ref.Missing, page.Ref.Detached = false, true
		page.Ref.Kind, page.Ref.Name = "revision", shortOID(pinned)
	}
	page.Repo.Empty = false
	page.Ref.Revision, page.Ref.ShortRevision = pinned, shortOID(pinned)
	return pinned, nil
}

// fillCode fills the Code tab. A ref or path that does not exist is marked
// on the page; a read that failed is returned.
func (app *App) fillCode(request *http.Request, page *webui.RepositoryPage, snapshot repository.RefSnapshot, address pageAddress) error {
	summary := snapshot.Summary
	pagination, requested, requestedPath := address.Lines, address.Ref, address.Path
	selectedRef, resolved, err := app.selectRef(request, page, snapshot, requested)
	if err != nil {
		return err
	}
	if !resolved && (address.Revision == "" || page.Shared) {
		if requested == "" && page.Ref.Missing {
			page.Chrome.Notices = append(page.Chrome.Notices, webui.Notice{Kind: webui.NoticeWarning, Code: webui.MsgRepoDefaultGone})
		}
		return nil
	}
	// A continuation names the original commit, not the branch's new tip.
	currentRevision := page.Ref.Revision
	commitOID, err := app.codePageRevision(request, page, address.Revision)
	if errors.Is(err, repository.ErrNotFound) {
		page.Code.NotFound = true
		return nil
	}
	if err != nil {
		return err
	}
	lookup, listing, err := app.Repositories.PathPageAt(request.Context(), page.Repo.ID, commitOID, requestedPath, address.After)
	if errors.Is(err, repository.ErrNotFound) {
		page.Code = webui.CodeView{Path: requestedPath, NotFound: true, Crumbs: codeCrumbs(page.Repo, selectedRef, requestedPath)}
		return nil
	}
	if err != nil {
		return err
	}
	if !lookup.Folder {
		limits, err := app.Store.BrowseLimits(request.Context())
		if err != nil {
			return err
		}
		blob, err := app.Repositories.BlobAt(request.Context(), page.Repo.ID, lookup.File, limits.FileBytes)
		if err != nil {
			return err
		}
		binary := blob.Binary || !utf8.Valid(blob.Content)
		target := summary.DefaultBranch
		if strings.HasPrefix(selectedRef, "refs/heads/") {
			target = strings.TrimPrefix(selectedRef, "refs/heads/")
		}
		parent := path.Dir(requestedPath)
		if parent == "." {
			parent = ""
		}
		file := &webui.FileView{
			Path: requestedPath, Size: int64(len(blob.Content)), Binary: binary, Truncated: blob.Truncated,
			TooLarge: blob.TooLarge, TooLargeMemory: blob.TooLargeMemory,
			RawURL:     rawURL(page.Repo.URL, selectedRef, requestedPath),
			RestoreURL: ownerRestoreURL(page, commitOID, target, requestedPath),
		}
		file.RawCurrentRef = commitOID != currentRevision
		if !resolved {
			// The existing raw endpoint needs a public branch or tag.
			file.RawURL = ""
		}
		if lookup.File.Size >= 0 {
			file.Size = lookup.File.Size
		}
		if pinnedBlob := address.Blob; pinnedBlob != "" && pinnedBlob != blob.OID {
			page.Code.NotFound = true
			return nil
		}
		fileAddress := pinnedPageURL(codeURL(page.Repo.URL, selectedRef, requestedPath), commitOID, blob.OID)
		firstAddress := fileAddress
		if !binary && !blob.TooLarge {
			file.Lines, file.Continuation.Total = sourceLinePage(blob.Content, pagination.First)
			file.FirstLine = pagination.First
			file.LineURL = fileAddress
			if markdown.IsDocument(requestedPath) {
				file.LineURL += "&view=source"
			}
			firstAddress = file.LineURL
			file.Continuation = lineContinuation(file.LineURL, pagination.First, len(file.Lines), file.Continuation.Total)
			file.Continuation.Incomplete = blob.Truncated
		}
		// A refused read has no lines, so a line or continuation address adds
		// nothing and is answered with the same notice instead of an error.
		if !blob.TooLarge {
			if err := pagination.check(file.Continuation.Total, blob.Truncated, firstAddress); err != nil {
				return err
			}
		}
		if !binary && !blob.TooLarge && markdown.IsDocument(requestedPath) {
			file.Document = true
			file.ShowSource = address.View == "source" || pagination.Line != 0 || pagination.From != 0
			file.PreviewURL = fileAddress
			file.SourceURL = file.LineURL
			// A cut-off document would render a broken ending, so only a
			// whole file is rendered. The source view is always offered.
			if blob.Truncated {
				file.NotRendered = webui.MsgCodeNotShown
			} else if !file.ShowSource {
				file.Rendered, file.NotRendered = app.renderMarkdown(request.Context(), page.Repo.URL, selectedRef, parent, blob.Content)
			}
		}
		view := webui.CodeView{Path: requestedPath, Dir: parent, Crumbs: codeCrumbs(page.Repo, selectedRef, requestedPath), File: file, Entries: treeViewEntries(page.Repo.URL, selectedRef, commitOID, lookup.Entries), ListingUnavailable: listing.Unavailable}
		view.Continuation = directoryContinuation(codeURL(page.Repo.URL, selectedRef, requestedPath), commitOID, listing)
		// The drawer lists the file's folder, so "up" leaves that folder.
		if parent != "" {
			view.UpURL = codeURL(page.Repo.URL, selectedRef, path.Dir(parent))
		}
		// A download the raw endpoint would refuse is not offered: the saved
		// download limit and the server read bound both bound it. A refused
		// read is never served as a download, whatever the saved limit is.
		file.RawTooLarge = file.Size > limits.RawBytes || blob.TooLarge
		// A picture loads through the raw endpoint, so it is shown only
		// when that endpoint would serve it.
		if binary && !file.RawTooLarge && !file.RawCurrentRef && file.RawURL != "" {
			file.Image, file.ImageWidth, file.ImageHeight = inlineImage(requestedPath, blob.Content)
		}
		page.Code = view
		return nil
	}
	if pagination.Line != 0 || pagination.From != 0 {
		return errPageAddress
	}
	view := webui.CodeView{Path: requestedPath, Dir: requestedPath, Crumbs: codeCrumbs(page.Repo, selectedRef, requestedPath)}
	if requestedPath != "" {
		view.UpURL = codeURL(page.Repo.URL, selectedRef, path.Dir(requestedPath))
	}
	view.Entries = treeViewEntries(page.Repo.URL, selectedRef, commitOID, lookup.Entries)
	view.Continuation = directoryContinuation(codeURL(page.Repo.URL, selectedRef, requestedPath), commitOID, listing)
	view.Readme = app.folderReadme(request, page.Repo, selectedRef, requestedPath, listing.Readme)
	if view.Readme != nil {
		view.Readme.URL = pinnedPageURL(view.Readme.URL, commitOID, "")
	}
	page.Code = view
	// The whole branch or tag downloads from its top folder, where no folder
	// or file could be taken for what the archive holds.
	if requestedPath == "" {
		page.Downloads = ownerArchiveLinks(page, selectedRef)
	}
	return nil
}

// noteUnreadableCommits keeps a page that shows commits when some of them
// could not be read: the page shows the others, and one notice names each
// unreadable commit and links to its own page. Any other error is returned.
func (app *App) noteUnreadableCommits(request *http.Request, page *webui.RepositoryPage, ref string, err error) error {
	var unreadable *repository.UnreadableCommitError
	if !errors.As(err, &unreadable) {
		return err
	}
	// The repository is part of the cause, so the same commit in another
	// repository is logged on its own line rather than counted with this one.
	logFailure(request, "commit read", fmt.Errorf("repository %q: %w", page.Repo.ID, err))
	for _, oid := range unreadable.OIDs {
		named := func(notice webui.Notice) bool {
			return notice.Code == webui.MsgCommitUnreadable && notice.Detail == shortOID(oid)
		}
		if !slices.ContainsFunc(page.Chrome.Notices, named) {
			page.Chrome.Notices = append(page.Chrome.Notices, webui.Notice{
				Kind: webui.NoticeWarning, Code: webui.MsgCommitUnreadable, Detail: shortOID(oid), Link: commitURL(page.Repo.URL, ref, oid, ""),
			})
		}
	}
	return nil
}

// fillCommits fills the Commits tab: the list, or the commit openedOID. A
// ref, commit or path that does not exist is marked on the page, and so is a
// commit that could not be read; any other read that failed is returned.
func (app *App) fillCommits(request *http.Request, page *webui.RepositoryPage, snapshot repository.RefSnapshot, address pageAddress, openedOID string) error {
	summary := snapshot.Summary
	pagination, requested, requestedPath := address.Lines, address.Ref, address.Path
	if (pagination.Line != 0 || pagination.From != 0) && (openedOID == "" || requestedPath == "") {
		return errPageAddress
	}
	selectedRef, resolved, err := app.selectRef(request, page, snapshot, requested)
	if err != nil {
		return err
	}
	if !resolved && requested == "" && page.Ref.Missing {
		page.Chrome.Notices = append(page.Chrome.Notices, webui.Notice{Kind: webui.NoticeWarning, Code: webui.MsgRepoDefaultGone})
	}
	if openedOID == "" {
		if !resolved {
			return nil
		}
		// Every page continues one traversal: the history of a root commit
		// fixed when the first older page was linked, in Git's own order, so
		// merged branches keep their place. One more commit than the page
		// shows tells whether an older page exists.
		root, skip := page.Ref.Revision, address.Skip
		if address.Root != "" {
			reachable, err := app.Repositories.CommitReachableFrom(request.Context(), page.Repo.ID, page.Ref.Revision, address.Root)
			if errors.Is(err, repository.ErrNotFound) || (err == nil && !reachable) {
				return errPageAddress
			}
			if err != nil {
				return err
			}
			root = address.Root
		}
		commits, err := app.Repositories.CommitsPage(request.Context(), page.Repo.ID, root, skip, repository.CommitPageSize+1)
		if err := app.noteUnreadableCommits(request, page, selectedRef, err); err != nil {
			return err
		}
		page.Commits.Unreadable = err != nil
		commitsPage := func(skipped int) string {
			parsed, _ := url.Parse(page.CommitsURL)
			if skipped > 0 {
				query := parsed.Query()
				query.Set("root", root)
				query.Set("skip", strconv.Itoa(skipped))
				parsed.RawQuery = query.Encode()
			}
			return parsed.String()
		}
		if skip > 0 {
			page.Commits.NewerURL = commitsPage(max(0, skip-repository.CommitPageSize))
		}
		if len(commits) > repository.CommitPageSize {
			page.Commits.OlderURL = commitsPage(skip + repository.CommitPageSize)
			commits = commits[:repository.CommitPageSize]
		}
		for _, commit := range commits {
			page.Commits.List = append(page.Commits.List, app.commitSummary(page.Repo.URL, selectedRef, commit))
		}
		return nil
	}
	// One commit. The page names the selected branch or tag only when the
	// commit is part of its history. The list of commits is one link away
	// and is not read here.
	if resolved {
		reachable, err := app.Repositories.CommitReachableFrom(request.Context(), page.Repo.ID, page.Ref.Revision, openedOID)
		if errors.Is(err, repository.ErrNotFound) {
			page.Commits.NotFound = true
			return nil
		}
		if err != nil {
			return err
		}
		if !reachable {
			resolved = false
			selectedRef = ""
			page.OverviewURL = page.Repo.URL
			page.CodeURL = page.Repo.URL + "/code"
			page.CommitsURL = page.Repo.URL + "/commits"
		}
	}
	// A shared page reaches only commits in the history of its branch or
	// tag, never one that only kept history holds.
	if !resolved && page.Shared {
		page.Commits.NotFound = true
		return nil
	}
	if !resolved {
		page.Ref = webui.RefSelection{Name: shortOID(openedOID), Kind: "revision", Detached: true, Revision: openedOID, ShortRevision: shortOID(openedOID)}
	}
	commit, files, err := app.Repositories.CommitFiles(request.Context(), page.Repo.ID, openedOID)
	if errors.Is(err, repository.ErrNotFound) {
		page.Commits.NotFound = true
		return nil
	}
	if err != nil {
		if err := app.noteUnreadableCommits(request, page, selectedRef, err); err != nil {
			return err
		}
		page.Commits.Unreadable = true
		return nil
	}
	// A known commit stays browsable after its last public ref is removed.
	page.Repo.Empty = false
	// A commit opens with every file's diff. An address naming one file, as
	// the note on a file left out of a large commit does, loads that file's
	// diff alone.
	var selectedFile *repository.ChangedFile
	if requestedPath != "" {
		for index := range files {
			if files[index].Path == requestedPath {
				selectedFile = &files[index]
				break
			}
		}
		if selectedFile == nil {
			page.Commits.NotFound = true
			return nil
		}
	}
	target := summary.DefaultBranch
	if strings.HasPrefix(selectedRef, "refs/heads/") {
		target = strings.TrimPrefix(selectedRef, "refs/heads/")
	}
	view := webui.CommitDetail{
		Commit: app.commitSummary(page.Repo.URL, selectedRef, commit), Body: commit.Body,
		CommitterName: commit.CommitterName, CommitterDate: commit.CommittedAt,
		SelectedPath: requestedPath,
		RestoreURL:   ownerRestoreURL(page, commit.OID, target, requestedPath),
	}
	if requestedPath != "" {
		view.AllFilesURL = commitURL(page.Repo.URL, selectedRef, openedOID, "")
	}
	for _, parentOID := range commit.Parents {
		view.Parents = append(view.Parents, webui.CommitSummary{OID: parentOID, ShortOID: shortOID(parentOID), URL: commitURL(page.Repo.URL, selectedRef, parentOID, "")})
	}
	page.Downloads = ownerArchiveLinks(page, commit.OID)
	if len(commit.Parents) > 1 {
		if pagination.Line != 0 || pagination.From != 0 {
			return errPageAddress
		}
		view.Unavailable = true
		view.UnavailableReason = webui.MsgCommitDiffMerge
		page.Commits.Detail = &view
		return nil
	}
	fileURL := func(filePath string) string { return commitURL(page.Repo.URL, selectedRef, openedOID, filePath) }
	limits, err := app.Store.BrowseLimits(request.Context())
	if err != nil {
		return err
	}
	if requestedPath != "" {
		// Git can rebuild the whole object to write a patch even with a
		// large-file threshold.
		patch, truncated := "", false
		var err error
		if !selectedFile.TextDiffUnavailable {
			patch, truncated, err = app.Repositories.CommitPatch(request.Context(), page.Repo.ID, openedOID, requestedPath, nil, limits.FilePatchBytes)
		}
		if err != nil {
			return err
		}
		view.Truncated = truncated
		item := diffFileItem(*selectedFile, fileURL)
		item.Selected = true
		var total, shown int
		if !item.Binary {
			item.Hunks, total, shown = patchLinePage(patch, pagination.First, maximumCommitDiffLines)
			view.Continuation = lineContinuation(fileURL(requestedPath), pagination.First, shown, total)
			view.Continuation.Incomplete = truncated
		}
		if err := pagination.check(total, truncated, fileURL(requestedPath)); err != nil {
			return err
		}
		view.Files = append(view.Files, item)
		page.Commits.Detail = &view
		return nil
	}
	from, filePages := fileWindow(changeView(request).first, len(files))
	if filePages.Total > maximumDiffFiles {
		addFileLinks(request, &filePages)
		view.FilePages = filePages
	}
	files = files[from : from+max(0, filePages.Last-filePages.First+1)]
	excluded, deferred, readPatch := excludedFromDiff(files)
	var patch string
	var truncated bool
	if readPatch {
		patch, truncated, err = app.Repositories.CommitPatch(request.Context(), page.Repo.ID, openedOID, "", excluded, limits.CommitPatchBytes)
		if err != nil {
			return err
		}
	}
	view.Truncated = truncated
	view.Files, _ = diffFileItems(files, patch, truncated, deferred, fileURL, limits.CommitFileBytes)
	if !readPatch {
		// A read that could not leave the files above out could not be
		// bounded, so the page shows every file it did not compare instead.
		for index := range view.Files {
			if !view.Files[index].Binary {
				view.Files[index].NotLoaded = true
			}
		}
	}
	page.Commits.Detail = &view
	return nil
}

// Bounds for one commit or pull request diff. The sizes of the diff reads,
// and of one file's diff shown, are the owner's browsing limits
// (state.BrowseLimits). A page shows at most maximumCommitDiffLines diff
// lines, because each line becomes a table row and short lines would
// otherwise make a page many times larger than the diff. Files left out for
// any of these reasons are listed and marked as not loaded, with a link to
// their diff alone where the page has one. A commit leaves at most
// maximumExcludedFiles large files out of its diff read by name, which
// keeps the Git command line short; past that the diff read is left out and
// each file is offered on its own. The same line bound applies to one
// source or selected-file diff page, with navigation to its remaining lines.
const (
	maximumCommitDiffLines = 10000
	// maximumExcludedFiles is how many files one commit or comparison leaves
	// out of its diff read by name, which keeps the Git command line short.
	maximumExcludedFiles = 100
	// maximumDiffFiles is how many file sections one page shows. Each section
	// carries its own header markup, so many small files would otherwise make
	// a page far larger than the line limit suggests.
	maximumDiffFiles = 400
)

// changesView says which part of a long change list a request asks for: the
// page of files starting at file number first (1 for the first page), or the
// one file at path file with a link back to the whole list.
type changesView struct {
	first   int
	from    int // first line of the one file shown alone
	file    string
	allURL  string
	fileURL func(string) string
}

// changeView reads the "files" and "file" parameters of request. A missing
// or invalid "files" value means the first page, and one past the end means
// the last.
func changeView(request *http.Request) changesView {
	query := request.URL.Query()
	view := changesView{first: 1, from: 1, file: query.Get("file")}
	if number, err := strconv.Atoi(query.Get("from")); err == nil && number > 0 {
		view.from = (number-1)/maximumCommitDiffLines*maximumCommitDiffLines + 1
	}
	if number, err := strconv.Atoi(query.Get("files")); err == nil && number > 0 {
		view.first = number
	}
	address := func(change func(url.Values)) string {
		target := *request.URL
		values := target.Query()
		change(values)
		target.RawQuery = values.Encode()
		return target.RequestURI()
	}
	view.allURL = address(func(values url.Values) { values.Del("file") })
	view.fileURL = func(path string) string {
		return address(func(values url.Values) { values.Del("files"); values.Set("file", path) })
	}
	return view
}

// fileWindow returns where the page that contains file number first starts
// (as an index) and its place among total files. Nothing is paged when every
// file fits on one page.
func fileWindow(first, total int) (from int, page webui.PageContinuation) {
	if total <= maximumDiffFiles {
		return 0, webui.PageContinuation{First: 1, Last: total, Total: total}
	}
	first = (min(first, total)-1)/maximumDiffFiles*maximumDiffFiles + 1
	return first - 1, webui.PageContinuation{First: first, Last: min(first+maximumDiffFiles-1, total), Total: total}
}

// addFileLinks adds the links to the first and the next page of files to a
// window made by fileWindow.
func addFileLinks(request *http.Request, page *webui.PageContinuation) {
	if page.Total <= maximumDiffFiles {
		return
	}
	link := func(position int) string {
		address := *request.URL
		query := address.Query()
		query.Del("file")
		if position == 1 {
			query.Del("files")
		} else {
			query.Set("files", strconv.Itoa(position))
		}
		address.RawQuery = query.Encode()
		return address.RequestURI()
	}
	if page.First > 1 {
		page.FirstURL = link(1)
	}
	if page.Last < page.Total {
		page.MoreURL = link(page.Last + 1)
	}
}

// excludedFromDiff returns the paths of the files left out of the commit's
// patch read, the files whose diff is deferred for the page, and whether the
// read may run at all.
//
// Git can rebuild the whole object while writing its patch despite the
// large-file threshold. A file with more
// changed lines than the page shows is left out so that it cannot use up the
// size limit of the files after it. Every left out file is one more pathspec
// on the Git command line, so past the cap the patch is not read at all: the
// page offers each file on its own instead of a read that could not be
// bounded.
func excludedFromDiff(files []repository.ChangedFile) (excluded []string, deferred map[string]bool, readPatch bool) {
	deferred = map[string]bool{}
	largeFiles := 0
	for _, file := range files {
		if file.TextDiffUnavailable {
			largeFiles++
		}
	}
	if largeFiles > maximumExcludedFiles {
		return nil, deferred, false
	}
	for _, file := range files {
		if file.TextDiffUnavailable {
			excluded = append(excluded, file.Path)
			continue
		}
		if !file.Binary && file.Additions+file.Deletions > maximumCommitDiffLines && len(excluded) < maximumExcludedFiles {
			deferred[file.Path] = true
			excluded = append(excluded, file.Path)
		}
	}
	return excluded, deferred, len(excluded) < len(files)
}

// diffFileItem is the list row of one changed file, without its diff.
func diffFileItem(file repository.ChangedFile, fileURL func(string) string) webui.DiffFile {
	item := webui.DiffFile{
		Path: file.Path, OldPath: file.OldPath, Status: file.Status, Additions: file.Additions, Deletions: file.Deletions,
		Binary: file.Binary, TextDiffUnavailable: file.TextDiffUnavailable, CountsUnknown: !file.CountsRead,
	}
	if fileURL != nil {
		item.URL = fileURL(file.Path)
	}
	return item
}

// diffFileItems pairs changed files with their parts of patch, a diff read
// without rename detection, within the page's limits. A file in deferred was
// left out of patch on purpose. When truncated, the patch stopped early, so
// a file with changed lines and no complete part is not loaded, and so is a
// file whose line counts were never read (see ChangedFile.CountsRead) and
// whose part is not shown. notLoaded reports whether any file's changes are
// missing from the page.
func diffFileItems(files []repository.ChangedFile, patch string, truncated bool, deferred map[string]bool, fileURL func(string) string, fileBytes int64) (items []webui.DiffFile, notLoaded bool) {
	sections := splitPatchByFile(patch, truncated)
	shownLines := 0
	for _, file := range files {
		item := diffFileItem(file, fileURL)
		if !item.Binary {
			section, ok := sections[file.Path]
			lines := strings.Count(section, "\n")
			switch {
			case ok && int64(len(section)) <= fileBytes && shownLines+lines <= maximumCommitDiffLines:
				item.Hunks = parsePatch(section)
				shownLines += lines
			case ok || deferred[file.Path] || !file.CountsRead || truncated && file.Additions+file.Deletions > 0:
				item.NotLoaded = true
				notLoaded = true
			}
		}
		items = append(items, item)
	}
	return items, notLoaded
}

// splitPatchByFile splits a whole-commit patch read without rename detection
// into each file's part, keyed by the file's path. A path is read from the
// "diff --git" line itself, since only that line is present for every kind of
// change. A change of file type yields two parts with one path, which are
// joined. When truncated, the last part may be cut short, so it is left out.
func splitPatchByFile(patch string, truncated bool) map[string]string {
	sections := map[string]string{}
	var order []string
	var current string
	var body strings.Builder
	flush := func() {
		if current != "" {
			sections[current] += body.String()
		}
		body.Reset()
	}
	for _, line := range strings.SplitAfter(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			current = patchHeaderPath(strings.TrimSuffix(strings.TrimPrefix(line, "diff --git "), "\n"))
			if current != "" {
				order = append(order, current)
			}
			continue
		}
		body.WriteString(line)
	}
	flush()
	if truncated && len(order) != 0 {
		delete(sections, order[len(order)-1])
	}
	return sections
}

// patchHeaderPath reads the new path from the rest of a "diff --git" line
// written without rename detection, where both sides name the same path. Git
// quotes a path with unusual bytes in C style, on both sides alike, and
// strconv.Unquote reads that style. An unquoted pair is split at its middle,
// since both halves match.
func patchHeaderPath(rest string) string {
	if strings.HasPrefix(rest, "\"") {
		first, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return ""
		}
		second, err := strconv.Unquote(strings.TrimPrefix(rest[len(first):], " "))
		if err != nil {
			return ""
		}
		return strings.TrimPrefix(second, "b/")
	}
	if len(rest) < 5 || (len(rest)-5)%2 != 0 {
		return ""
	}
	half := (len(rest) - 5) / 2
	if rest[:2] != "a/" || rest[2+half:2+half+3] != " b/" || rest[2:2+half] != rest[5+half:] {
		return ""
	}
	return rest[5+half:]
}

func (app *App) repositorySummary(request *http.Request, stored state.Repository, snapshot repository.RefSnapshot) webui.RepositorySummary {
	summary := snapshot.Summary
	result := webui.RepositorySummary{
		ID: stored.ID, Name: stored.Name, Description: stored.Description,
		URL: repositoryPath(stored.Address), CloneURL: app.cloneURL(request, stored.Address),
		CreatedAt: stored.CreatedAt, Empty: summary.Empty, DefaultBranch: summary.DefaultBranch,
		// An empty repository has no branch at all yet, which is its normal
		// first state rather than a default branch that went missing.
		DefaultBranchMissing: !summary.Empty && summary.DefaultBranch != "" && summary.DefaultOID == "",
		BranchCount:          len(summary.Branches), TagCount: len(summary.Tags), Counted: true,
	}
	switch {
	case snapshot.HeadFound:
		result.Head = app.commitSummary(repositoryPath(stored.Address), "refs/heads/"+summary.DefaultBranch, snapshot.Head)
	case snapshot.HeadErr != nil:
		result.HeadUnreadable = true
		logFailure(request, "latest commit read", fmt.Errorf("repository %q: %w", stored.ID, snapshot.HeadErr))
	}
	return result
}

func (app *App) commitSummary(base, ref string, commit repository.Commit) webui.CommitSummary {
	return webui.CommitSummary{
		OID: commit.OID, ShortOID: shortOID(commit.OID), Subject: commit.Subject,
		AuthorName: commit.AuthorName, AuthorDate: commit.AuthoredAt, URL: commitURL(base, ref, commit.OID, ""),
	}
}

type activityObservation struct {
	entries  []webui.ActivityEntry
	counts   map[string]int
	complete bool
	// counting is true while some repository is still being counted.
	counting bool
	// preparing is true when some repository was skipped because it is
	// still being prepared after startup.
	preparing bool
	// unreadable names the repositories skipped because their refs or their
	// history could not be read. The server log has each cause.
	unreadable []string
}

// observeActivity gathers one bounded, current-history-first observation per
// repository and reuses it for both the recent list and the annual graph. The
// page-wide budget is shared, so a truncated observation is honestly
// incomplete for both outputs. Observations come from activityCache, keyed by
// the refs in keys.
func (app *App) observeActivity(ctx context.Context, repositories []state.Repository, keys []string, maximum int) activityObservation {
	observation := activityObservation{counts: make(map[string]int), complete: true}
	ids := make([]string, len(repositories))
	for index, stored := range repositories {
		ids[index] = stored.ID
	}
	parts := app.activity.observe(ctx, app.Repositories, ids, keys, maximum, activityWait)
	for index, part := range parts {
		if part.err != nil {
			// It was not counted: it is being prepared, by design, or its
			// refs or history could not be read. It is left out, the page
			// names it, and the others are still shown.
			observation.complete = false
			if errors.Is(part.err, errRefsUnlisted) && app.Repositories.Preparing(ids[index]) {
				observation.preparing = true
			} else {
				observation.unreadable = append(observation.unreadable, repositories[index].Name)
			}
			continue
		}
		if part.incomplete || part.pending {
			observation.complete = false
		}
		observation.counting = observation.counting || part.pending
		observation.entries = append(observation.entries, app.activityEntries(repositories[index], part.records)...)
		for _, record := range part.records {
			observation.counts[record.AuthoredAt.Format("2006-01-02")]++
		}
	}
	return observation
}

// describe sets the graph's completeness and availability, and the
// repositories it left out as unreadable, from the observation. Counting in progress takes
// precedence over the limit reason.
func (observation activityObservation) describe(graph *webui.ActivityGraph) {
	graph.Complete = observation.complete
	graph.Unreadable = observation.unreadable
	unreadable := len(observation.unreadable) != 0
	switch {
	case observation.counting:
		graph.IncompleteReason = webui.MsgActivityCounting
	case observation.preparing && unreadable:
		graph.IncompleteReason = webui.MsgActivitySkipped
	case observation.preparing:
		graph.IncompleteReason = webui.MsgActivityPreparing
	case unreadable:
		graph.IncompleteReason = webui.MsgActivityUnreadable
	case !observation.complete:
		graph.IncompleteReason = webui.MsgActivityLimit
	}
	// With no repository read, there is no count to show, not a count of zero.
	if unreadable && len(observation.unreadable) == graph.RepositoryCount {
		graph.Available = false
		graph.UnavailableReason = webui.MsgActivityScanFail
	}
}

// overviewLanguageRows is how many languages the Languages panel names
// before folding the rest into Other.
const overviewLanguageRows = 6

// overviewLanguages builds the Languages panel from the default branch's
// current commit. A count that hit its bounds or failed shows a short note,
// never a share that could be wrong.
func (app *App) overviewLanguages(request *http.Request, id, commitOID string) webui.LanguageSummary {
	if commitOID == "" {
		return webui.LanguageSummary{Note: webui.MsgRepoLanguagesNone}
	}
	stats, err := app.Repositories.Languages(request.Context(), id, commitOID)
	switch {
	case errors.Is(err, repository.ErrRepositoryInUse):
		// Only this panel waits for the operation; the rest of the page is
		// answered from what could be read.
		return webui.LanguageSummary{Note: webui.MsgRepoLanguagesBusy}
	case err != nil:
		logFailure(request, "language count", err)
		return webui.LanguageSummary{Note: webui.MsgRepoLanguagesUnavailable}
	case stats.TooLarge:
		return webui.LanguageSummary{Note: webui.MsgRepoLanguagesTooLarge}
	case stats.TimedOut:
		return webui.LanguageSummary{Note: webui.MsgRepoLanguagesSlow}
	case len(stats.Shares) == 0:
		return webui.LanguageSummary{Note: webui.MsgRepoLanguagesNone, AttributesNote: languageAttributesNote(stats.Attributes)}
	}
	return webui.LanguageSummary{Rows: languageRows(stats.Shares), AttributesNote: languageAttributesNote(stats.Attributes)}
}

// languageAttributesNote says why .gitattributes language settings were not
// applied, or nothing when they were.
func languageAttributesNote(state repository.AttributeState) webui.MessageCode {
	switch state {
	case repository.AttributesUnsupported:
		return webui.MsgRepoLanguagesNoAttrs
	case repository.AttributesUnreadable:
		return webui.MsgRepoLanguagesAttrsFailed
	}
	return ""
}

// languageRows turns sizes, largest first, into the panel rows: the first
// overviewLanguageRows languages and then Other for the rest.
func languageRows(shares []repository.LanguageShare) []webui.LanguageRow {
	var total int64
	for _, share := range shares {
		total += share.Bytes
	}
	if total <= 0 {
		return nil
	}
	percent := func(size int64) float64 { return float64(size) * 100 / float64(total) }
	var rows []webui.LanguageRow
	var rest int64
	for index, share := range shares {
		if index >= overviewLanguageRows {
			rest += share.Bytes
			continue
		}
		value := percent(share.Bytes)
		rows = append(rows, webui.LanguageRow{Name: share.Name, Color: repository.LanguageColor(share.Name), Share: value, Percent: languagePercent(value)})
	}
	if rest > 0 {
		value := percent(rest)
		rows = append(rows, webui.LanguageRow{Name: "Other", Share: value, Percent: languagePercent(value), Other: true})
	}
	return rows
}

// languagePercent shows a share with one decimal. A share too small for
// that shows "<0.1%" rather than a misleading 0.0%.
func languagePercent(value float64) string {
	if value > 0 && value < 0.05 {
		return "<0.1%"
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + "%"
}

// repositoryActivityGraph builds one repository's activity graph from the
// shared cache, with the repository's own full budget. key is the activity key
// of the ref snapshot the page was built from.
func (app *App) repositoryActivityGraph(request *http.Request, page *webui.RepositoryPage, key string) webui.ActivityGraph {
	year := selectedYear(request, app.now().Year())
	ctx := request.Context()
	name := page.Repo.Name
	part := app.activity.observe(ctx, app.Repositories, []string{page.Repo.ID}, []string{key}, app.activityLimit(), activityWait)[0]
	if part.err != nil {
		// A commit that could not be read is named at the top of the page.
		// The count logged any other cause.
		_ = app.noteUnreadableCommits(request, page, "", part.err)
		return webui.ActivityGraph{Year: year, Scope: name, UnavailableReason: webui.MsgActivityRepoFail}
	}
	counts := make(map[string]int)
	for _, record := range part.records {
		counts[record.AuthoredAt.Format("2006-01-02")]++
	}
	graph := buildActivityGraph(counts, year, app.now(), 1)
	graph.Scope = name
	graph.Complete = !part.incomplete && !part.pending
	switch {
	case part.pending:
		graph.IncompleteReason = webui.MsgActivityCountRepo
	case part.incomplete:
		graph.IncompleteReason = webui.MsgActivityLimit
	}
	return graph
}

func (app *App) activityLimit() int {
	if app.ActivityLimit > 0 {
		return app.ActivityLimit
	}
	return maximumActivityCommits
}

func (app *App) activityEntries(stored state.Repository, records []repository.ActivityRecord) []webui.ActivityEntry {
	entries := make([]webui.ActivityEntry, 0, len(records))
	for _, record := range records {
		ref := displayRef(record.Source)
		linkRef := record.Source
		if record.Retained || record.Uncertain {
			linkRef = ""
			if ref == "" {
				ref = "retained " + shortOID(record.OID)
			}
		}
		entries = append(entries, webui.ActivityEntry{
			RepositoryID: stored.ID, RepositoryName: stored.Name, RepositoryAddress: stored.Address, RepositoryURL: repositoryPath(stored.Address),
			Ref: ref, RefRetained: record.Retained,
			Commit: webui.CommitSummary{OID: record.OID, ShortOID: shortOID(record.OID), Subject: record.Subject, AuthorName: record.AuthorName, AuthorDate: record.AuthoredAt, URL: commitURL(repositoryPath(stored.Address), linkRef, record.OID, "")},
		})
	}
	return entries
}

func buildActivityGraph(counts map[string]int, year int, now time.Time, repositoryCount int) webui.ActivityGraph {
	graph := webui.ActivityGraph{Year: year, RepositoryCount: repositoryCount, Complete: true, Available: true}
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, now.Location())
	end := start.AddDate(1, 0, 0)
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		count := counts[key]
		graph.Days = append(graph.Days, webui.ActivityDay{Date: day, Count: count, Future: day.After(now), URL: "/activity?date=" + key + "&year=" + strconv.Itoa(year)})
		graph.Total += count
	}
	yearSet := map[int]struct{}{now.Year(): {}, year: {}}
	for day := range counts {
		if candidate, err := strconv.Atoi(strings.SplitN(day, "-", 2)[0]); err == nil {
			yearSet[candidate] = struct{}{}
		}
	}
	years := make([]int, 0, len(yearSet))
	for candidate := range yearSet {
		years = append(years, candidate)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	for _, candidate := range years {
		graph.Years = append(graph.Years, webui.ActivityYear{Year: candidate, URL: "/activity?year=" + strconv.Itoa(candidate)})
	}
	return graph
}

func emptyActivityGraph(year int, now time.Time, scope string) webui.ActivityGraph {
	graph := buildActivityGraph(nil, year, now, 1)
	graph.Scope = scope
	return graph
}

func selectedYear(request *http.Request, fallback int) int {
	year, err := strconv.Atoi(request.URL.Query().Get("year"))
	if err != nil || !activityYearInRange(year) {
		return fallback
	}
	return year
}

// activityYearInRange reports whether year is one an activity graph shows.
func activityYearInRange(year int) bool { return year >= 1970 && year <= 9999 }

func sortActivityEntries(entries []webui.ActivityEntry) {
	sort.SliceStable(entries, func(left, right int) bool {
		leftDay := entries[left].Commit.AuthorDate.Format("2006-01-02")
		rightDay := entries[right].Commit.AuthorDate.Format("2006-01-02")
		if leftDay != rightDay {
			return leftDay > rightDay
		}
		return entries[left].Commit.AuthorDate.After(entries[right].Commit.AuthorDate)
	})
}

func groupActivity(entries []webui.ActivityEntry) []webui.ActivityDayGroup {
	var groups []webui.ActivityDayGroup
	for _, entry := range entries {
		day := time.Date(entry.Commit.AuthorDate.Year(), entry.Commit.AuthorDate.Month(), entry.Commit.AuthorDate.Day(), 0, 0, 0, 0, entry.Commit.AuthorDate.Location())
		if len(groups) == 0 || groups[len(groups)-1].Date.Format("2006-01-02") != day.Format("2006-01-02") {
			groups = append(groups, webui.ActivityDayGroup{Date: day})
		}
		groups[len(groups)-1].Entries = append(groups[len(groups)-1].Entries, entry)
	}
	return groups
}

func withRef(base, ref string) string {
	return base + "?ref=" + url.QueryEscape(ref)
}

// The addresses of a repository's code pages start at base, the
// repository's own page: repositoryPath for the dashboard, or a share
// link's address.

func codeURL(base, ref, filePath string) string {
	values := url.Values{"ref": []string{ref}}
	if filePath != "" && filePath != "." {
		values.Set("path", filePath)
	}
	return base + "/code?" + values.Encode()
}

func commitURL(base, ref, oid, filePath string) string {
	values := make(url.Values)
	if ref != "" {
		values.Set("ref", ref)
	}
	if filePath != "" {
		values.Set("path", filePath)
	}
	result := base + "/commits/" + url.PathEscape(oid)
	if len(values) != 0 {
		result += "?" + values.Encode()
	}
	return result
}

func treeViewEntries(base, ref, revision string, entries []repository.TreeEntry) []webui.TreeEntry {
	views := make([]webui.TreeEntry, 0, len(entries))
	for _, entry := range entries {
		kind := "file"
		switch {
		case entry.Type == "tree":
			kind = "dir"
		case entry.Type == "commit":
			kind = "submodule"
		case entry.Mode == "120000":
			kind = "symlink"
		}
		views = append(views, webui.TreeEntry{Name: entry.Name, Path: entry.Path, URL: pinnedPageURL(codeURL(base, ref, entry.Path), revision, ""), Kind: kind, Size: entry.Size})
	}
	sort.SliceStable(views, func(left, right int) bool {
		leftDir := views[left].Kind == "dir"
		rightDir := views[right].Kind == "dir"
		if leftDir != rightDir {
			return leftDir
		}
		return strings.ToLower(views[left].Name) < strings.ToLower(views[right].Name)
	})
	return views
}

func codeCrumbs(repo webui.RepositoryHeader, ref, filePath string) []webui.Crumb {
	crumbs := []webui.Crumb{{Name: repo.Address, URL: codeURL(repo.URL, ref, ""), Current: filePath == ""}}
	if filePath == "" {
		return crumbs
	}
	parts := strings.Split(filePath, "/")
	for index, part := range parts {
		currentPath := strings.Join(parts[:index+1], "/")
		crumbs = append(crumbs, webui.Crumb{Name: part, URL: codeURL(repo.URL, ref, currentPath), Current: index == len(parts)-1})
	}
	return crumbs
}

func shortOID(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parsePatch(patch string) []webui.DiffHunk {
	hunks, _, _ := patchLinePage(patch, 1, int(^uint(0)>>1))
	return hunks
}

// patchLinePage counts patch rows while retaining only the requested page.
func patchLinePage(patch string, first, limit int) ([]webui.DiffHunk, int, int) {
	var hunks []webui.DiffHunk
	var current *webui.DiffHunk
	header := ""
	oldLine, newLine, total, shown := 0, 0, 0, 0
	for line := range strings.SplitSeq(patch, "\n") {
		line = strings.TrimSuffix(line, "\r")
		match := hunkHeader.FindStringSubmatch(line)
		if match != nil {
			oldLine, _ = strconv.Atoi(match[1])
			newLine, _ = strconv.Atoi(match[3])
			header, current = line, nil
			continue
		}
		if header == "" || line == "\\ No newline at end of file" || line == "" {
			continue
		}
		diffLine := webui.DiffLine{Kind: "context", Text: line}
		switch line[0] {
		case '+':
			diffLine.Kind, diffLine.NewLine, diffLine.Text = "add", newLine, line[1:]
			newLine++
		case '-':
			diffLine.Kind, diffLine.OldLine, diffLine.Text = "del", oldLine, line[1:]
			oldLine++
		case ' ':
			diffLine.OldLine, diffLine.NewLine, diffLine.Text = oldLine, newLine, line[1:]
			oldLine++
			newLine++
		default:
			continue
		}
		total++
		if total < first || shown >= limit {
			continue
		}
		if current == nil {
			hunks = append(hunks, webui.DiffHunk{Header: header})
			current = &hunks[len(hunks)-1]
		}
		current.Lines = append(current.Lines, diffLine)
		shown++
	}
	return hunks, total, shown
}
