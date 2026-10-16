package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"owngit/internal/markdown"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
	"owngit/internal/webui"
)

// pullRequestListInput reads the state, limit and before parameters of a list
// request. An absent state lists every state. A value that is not a whole
// number is refused like an out-of-range one.
func pullRequestListInput(query url.Values) (pullrequest.ListInput, error) {
	input := pullrequest.ListInput{State: query.Get("state")}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 {
			return input, pullrequest.NewProblem("invalid_list_limit", "The page size must be a whole number of at least 1.")
		}
		input.Limit = limit
	}
	if value := query.Get("before"); value != "" {
		before, err := strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			return input, pullrequest.NewProblem("invalid_list_before", "The continuation must be a pull request number.")
		}
		input.Before = before
	}
	return input, nil
}

func (app *App) handlePullRequestsGet(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome) {
	page := app.pullRequestsPage(request, stored, summary, chrome)
	query := request.URL.Query()
	// The browser lists open pull requests unless another state is chosen.
	if !query.Has("state") {
		query.Set("state", state.PullRequestOpen)
	}
	input, err := pullRequestListInput(query)
	page.State = query.Get("state")
	status := http.StatusOK
	var result *pullrequest.ListResult
	if err == nil {
		result, err = app.PullRequests.List(request.Context(), stored.ID, input)
	}
	if err != nil {
		if status = apiStatus(request, "pull request list read", err); status == http.StatusBadRequest {
			app.renderError(writer, request, status, webui.MsgErrBadRequest, "")
			return
		}
		page.Unavailable = true
		page.UnavailableReason = failureText(status)
	} else {
		for _, view := range result.Items {
			page.Items = append(page.Items, app.pullRequestRow(stored.Address, view))
		}
		page.Complete = input.Before == 0 && result.Next == 0
		if result.Next != 0 {
			more := url.Values{"state": {page.State}, "before": {strconv.FormatInt(result.Next, 10)}}
			if input.Limit != 0 {
				more.Set("limit", strconv.Itoa(input.Limit))
			}
			page.MoreURL = page.Repo.URL + "/pull-requests?" + more.Encode()
		}
	}
	app.render(writer, request, status, page)
}

func (app *App) pullRequestsPage(request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome) webui.PullRequestsPage {
	basePage := app.baseRepositoryPage(request, chrome, stored, summary)
	base := basePage.Repo.URL + "/pull-requests"
	page := webui.PullRequestsPage{
		Chrome: chrome,
		Repo:   basePage.Repo,
		Tabs:   repositoryTabs(basePage, webui.RepoTabPullRequests),
	}
	if len(summary.Branches) >= 2 {
		page.NewURL = base + "/new"
	}
	return page
}

func (app *App) pullRequestRow(address string, view *pullrequest.View) webui.PullRequestRow {
	if view == nil {
		return webui.PullRequestRow{}
	}
	return webui.PullRequestRow{
		Number:    view.Number,
		Title:     view.Title,
		URL:       pullRequestURL(address, view.Number),
		State:     view.State,
		Source:    browserRevision(view.Source),
		Target:    browserRevision(view.Target),
		Checks:    browserCheckEvidence(address, view.Checks),
		Review:    browserReviewEvidence(view.Review, view.Source, view.Target),
		AutoMerge: view.AutoMerge != nil,
		CreatedAt: view.CreatedAt,
		UpdatedAt: view.UpdatedAt,
	}
}

// browserAutoMerge maps the API auto-merge projection to the page panel.
func browserAutoMerge(autoMerge *pullrequest.AutoMerge) *webui.AutoMergePanel {
	if autoMerge == nil || !autoMerge.Enabled {
		return nil
	}
	messages := map[string]webui.MessageCode{
		pullrequest.AutoMergeWaitingChecks:              webui.MsgAutoMergeWaitingChecks,
		pullrequest.AutoMergeChecksRunning:              webui.MsgAutoMergeChecksRunning,
		pullrequest.AutoMergeEvidenceUnreadable:         webui.MsgAutoMergeEvidenceUnreadable,
		pullrequest.AutoMergeMergeConflict:              webui.MsgAutoMergeMergeConflict,
		pullrequest.AutoMergeRepositoryUnavailable:      webui.MsgAutoMergeRepositoryUnavailable,
		pullrequest.AutoMergeReconciling:                webui.MsgAutoMergeReconciling,
		pullrequest.AutoMergeChecksFailed:               webui.MsgAutoMergeChecksFailed,
		pullrequest.AutoMergeChecksUntrustedWorktree:    webui.MsgAutoMergeChecksUntrustedWorktree,
		pullrequest.AutoMergeChecksConfigurationChanged: webui.MsgAutoMergeChecksConfigurationChanged,
	}
	message, ok := messages[autoMerge.Reason]
	if !ok {
		// Git/state blockers reuse the merge blocker sentences.
		message = autoMergeBlockerMessage(autoMerge.Reason)
	}
	return &webui.AutoMergePanel{Status: autoMerge.Status, Reason: autoMerge.Reason, Message: message}
}

// autoMergeBlockerMessage maps the Git/state blocker reasons the projection
// reuses to their localized merge blocker sentences.
func autoMergeBlockerMessage(code string) webui.MessageCode {
	switch code {
	case "source_branch_missing":
		return webui.MsgMergeBlockedSourceGone
	case "source_not_commit":
		return webui.MsgMergeBlockedSourceKind
	case "target_branch_missing":
		return webui.MsgMergeBlockedTargetGone
	case "target_not_commit":
		return webui.MsgMergeBlockedTargetKind
	case "already_merged":
		return webui.MsgPRMergedFixed
	default:
		return webui.MsgMergeUnexplained
	}
}

func (app *App) handleNewPullRequestGet(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome) {
	query := request.URL.Query()
	source, target := query.Get("source"), query.Get("target")
	var notices []webui.Notice
	status := http.StatusOK
	selected := query.Has("source") || query.Has("target") || query.Has("source_ref") || query.Has("target_ref")
	for _, choice := range []struct {
		key, field string
		branch     *string
	}{
		{"source_ref", "source_branch", &source},
		{"target_ref", "target_branch", &target},
	} {
		if query.Has(choice.key) {
			name, err := app.PullRequests.ValidateBranchInput(request.Context(), query.Get(choice.key), true)
			*choice.branch = name
			if err != nil {
				notices = append(notices, webui.Error(choice.field, webui.MsgPRInvalidBranch).WithDetail(query.Get(choice.key)))
				status = http.StatusUnprocessableEntity
				continue
			}
		}
		if selected && *choice.branch == "" {
			notices = append(notices, webui.Error(choice.field, webui.MsgPRInvalidBranch))
			status = http.StatusUnprocessableEntity
		}
	}
	app.renderNewPullRequest(writer, request, stored, summary, chrome,
		source, target, pullrequest.CreateInput{}, notices, status)
}

// renderNewPullRequest shows the creation screen for a branch pair, with the
// title, description and review choice of draft typed back in.
func (app *App) renderNewPullRequest(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome, sourceBranch, targetBranch string, draft pullrequest.CreateInput, notices []webui.Notice, status int) {
	basePage := app.baseRepositoryPage(request, chrome, stored, summary)
	base := basePage.Repo.URL + "/pull-requests"
	page := webui.NewPullRequestPage{
		Chrome:       chrome,
		Repo:         basePage.Repo,
		Tabs:         repositoryTabs(basePage, webui.RepoTabPullRequests),
		SelectURL:    base + "/new",
		SubmitURL:    base,
		CancelURL:    base,
		Title:        draft.Title,
		Body:         draft.Body,
		ReviewChoice: draft.ReviewChoice,
	}
	if notices != nil {
		page.Chrome.Notices = notices
	}

	if len(summary.Branches) >= 2 {
		for _, branch := range summary.Branches {
			page.Branches = append(page.Branches, webui.RefOption{Name: branch.Name})
		}
	}
	if sourceBranch == "" && targetBranch == "" && len(notices) == 0 {
		page.Source.Branch, page.Target.Branch = defaultPullRequestBranches(summary)
		app.render(writer, request, status, page)
		return
	}

	page.Observed = true
	page.Source = observedBranch(summary, sourceBranch)
	page.Target = observedBranch(summary, targetBranch)
	if sourceBranch == "" || targetBranch == "" {
		page.ChangesUnavailable = true
		page.ChangesReason = webui.MsgPRInvalidBranch
		if status == http.StatusOK {
			status = http.StatusUnprocessableEntity
		}
	} else if sourceBranch == targetBranch {
		page.Chrome.Notices = append(page.Chrome.Notices, webui.Error("source_branch", webui.MsgPRSameBranch))
		if status == http.StatusOK {
			status = http.StatusUnprocessableEntity
		}
	} else if !page.Source.Resolved() || !page.Target.Resolved() {
		page.ChangesUnavailable = true
	} else {
		changes, comparisonStatus := app.browserPullRequestChanges(request, stored.ID, page.Source.OID, page.Target.OID)
		if status == http.StatusOK {
			status = comparisonStatus
		}
		page.Changes, page.ChangesPages, page.ChangesAllURL, page.ChangesLines = changes.Files, changes.Pages, changes.AllURL, changes.Lines
		page.ChangesBase = changes.Base
		page.ChangesUnavailable = changes.Unavailable != ""
		page.ChangesReason = changes.Unavailable
		page.DiffTruncated = changes.PatchesIncomplete
		page.FilesTruncated = changes.FilesIncomplete
	}
	app.render(writer, request, status, page)
}

func (app *App) handleCreatePullRequest(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome) {
	if !app.parseForm(writer, request) {
		return
	}
	if !app.requireCSRF(writer, request) {
		return
	}

	input := pullrequest.CreateInput{
		Repository:   stored.ID,
		Title:        postValue(request, "title"),
		Body:         postValue(request, "body"),
		SourceBranch: postValue(request, "source_branch"),
		TargetBranch: postValue(request, "target_branch"),
		ReviewChoice: postValue(request, "review"),
		SourceOID:    postValue(request, "source_oid"),
		TargetOID:    postValue(request, "target_oid"),
		Actor:        generalAccessActor,
	}
	create := app.PullRequests.Create
	_, exactSource := request.PostForm["source_ref"]
	_, exactTarget := request.PostForm["target_ref"]
	if exactSource || exactTarget {
		input.SourceBranch, input.TargetBranch = postValue(request, "source_ref"), postValue(request, "target_ref")
		create = app.PullRequests.CreateExact
	}
	source, target := input.SourceBranch, input.TargetBranch
	if exactSource || exactTarget {
		source, target = strings.TrimPrefix(source, "refs/heads/"), strings.TrimPrefix(target, "refs/heads/")
	}
	if !validOID(input.SourceOID) || !validOID(input.TargetOID) {
		app.renderNewPullRequest(writer, request, stored, summary, chrome, source, target, input,
			[]webui.Notice{webui.Error("", webui.MsgPRStale)}, http.StatusUnprocessableEntity)
		return
	}
	created, err := create(request.Context(), input)
	if err != nil {
		notice, status := browserPullRequestProblem(request, "pull request creation", err, "")
		if existing, ok := pullrequest.AsProblem(err).Details.(pullrequest.ExistingPullRequest); ok {
			notice = notice.WithLink("#"+strconv.FormatInt(existing.Number, 10), pullRequestURL(stored.Address, existing.Number))
		}
		app.renderNewPullRequest(writer, request, stored, summary, chrome, source, target, input,
			[]webui.Notice{notice}, status)
		return
	}
	app.trayOrigins.note(request, originKey(state.NotifyPullRequest, pullRequestID(stored.ID, created.Number)))
	writer.Header().Set("Cache-Control", "no-store")
	app.noticeRedirect(writer, request, pullRequestURL(stored.Address, created.Number)+"?notice=pull_request_created")
}

func (app *App) handlePullRequestGet(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome, number int64) {
	app.renderPullRequest(writer, request, stored, summary, chrome, number, nil, nil, pullRequestDrafts{}, nil, http.StatusOK)
}

// handlePullRequestMergeability answers Check mergeability on the page where
// it was pressed. The answer is shown once and kept nowhere, so opening or
// reloading the pull request page never works out a merge.
func (app *App) handlePullRequestMergeability(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome, number int64) {
	if !app.parseForm(writer, request) {
		return
	}
	if !app.requireCSRF(writer, request) {
		return
	}
	expected := pullrequest.RevisionInput{SourceOID: postValue(request, "source_oid"), TargetOID: postValue(request, "target_oid")}
	answer, err := app.pullRequestMergeability(request, stored.ID, number, expected)
	writer.Header().Set("Cache-Control", "no-store")
	if err != nil {
		notice, status := browserPullRequestProblem(request, "pull request mergeability", err, "mergeability")
		app.renderPullRequest(writer, request, stored, summary, chrome, number, []webui.Notice{notice}, nil, pullRequestDrafts{}, nil, status)
		return
	}
	app.renderPullRequest(writer, request, stored, summary, chrome, number, []webui.Notice{}, nil, pullRequestDrafts{}, answer, http.StatusOK)
}

// pullRequestDrafts are the forms a refused change shows again, open and
// filled with what was typed, so nothing typed is lost.
type pullRequestDrafts struct {
	Edit   *webui.PullRequestEditForm
	Review *webui.ReviewDraft
}

func (app *App) handlePullRequestAction(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome, number int64, action string) {
	if !app.parseForm(writer, request) {
		return
	}
	if !app.requireCSRF(writer, request) {
		return
	}
	input := pullrequest.RevisionInput{SourceOID: postValue(request, "source_oid"), TargetOID: postValue(request, "target_oid"), Actor: generalAccessActor}
	var (
		view   *pullrequest.View
		err    error
		notice string
		drafts pullRequestDrafts
	)
	switch action {
	case "edit":
		title, body := postValue(request, "title"), postValue(request, "body")
		edit := pullrequest.EditInput{Title: &title, Body: &body, Actor: generalAccessActor}
		revision, parseErr := strconv.ParseInt(postValue(request, "edit_revision"), 10, 64)
		if parseErr == nil {
			edit.EditRevision = &revision
		}
		view, err = app.PullRequests.Edit(request.Context(), stored.ID, number, edit)
		notice = "pull_request_edited"
		drafts.Edit = &webui.PullRequestEditForm{Revision: revision, Title: title, Body: body, Open: true}
		// After a refused stale edit the page shows the newer text and keeps
		// this one in the form, so saving again is a choice made after seeing
		// both.
		if stale, ok := pullrequest.AsProblem(err).Details.(pullrequest.StaleEdit); ok {
			drafts.Edit.Revision = stale.CurrentEditRevision
		}
	case "review_submit":
		review := pullrequest.ReviewSubmitInput{
			SourceOID: input.SourceOID, TargetOID: input.TargetOID, Decision: postValue(request, "decision"),
			ReviewerLabel: postValue(request, "reviewer_label"), Note: postValue(request, "note"), Actor: generalAccessActor,
		}
		view, err = app.PullRequests.SubmitReview(request.Context(), stored.ID, number, review)
		notice = "review_recorded"
		drafts.Review = &webui.ReviewDraft{Decision: review.Decision, ReviewerLabel: review.ReviewerLabel, Note: review.Note, Open: true}
		// When a branch moved, the page shows the new commits and its form
		// is bound to them. The note and name stay, but the result is chosen
		// again, so a review of commits the reviewer has not seen is never
		// one click away.
		if movedRevision(err) {
			drafts.Review.Decision = ""
		}
	case "review_request":
		view, err = app.PullRequests.RequestReview(request.Context(), stored.ID, number, input)
		notice = "review_requested"
	case "review_skip":
		view, err = app.PullRequests.SkipReview(request.Context(), stored.ID, number, input)
		notice = "review_skipped"
	case "merge":
		view, err = app.PullRequests.Merge(request.Context(), stored.ID, number, input)
		notice = "pull_request_merged"
		if err == nil && view.Merge != nil && view.Merge.Mode == webui.MergeModeUpToDate {
			notice = "pull_request_up_to_date"
		}
	case "close":
		view, err = app.PullRequests.Close(request.Context(), stored.ID, number)
		notice = "pull_request_closed"
	case "reopen":
		view, err = app.PullRequests.Reopen(request.Context(), stored.ID, number)
		notice = "pull_request_reopened"
	case "auto_merge_enable":
		view, err = app.PullRequests.EnableAutoMerge(request.Context(), stored.ID, number, generalAccessActor)
		notice = "auto_merge_enabled"
	case "auto_merge_cancel":
		view, err = app.PullRequests.CancelAutoMerge(request.Context(), stored.ID, number)
		notice = "auto_merge_canceled"
	default:
		app.renderError(writer, request, http.StatusNotFound, webui.MsgErrNotFound, request.URL.Path)
		return
	}
	if err != nil {
		problemNotice, status := browserPullRequestProblem(request, "pull request "+strings.ReplaceAll(action, "_", " "), err, action)
		if existing, ok := pullrequest.AsProblem(err).Details.(pullrequest.ExistingPullRequest); ok {
			problemNotice = problemNotice.WithLink("#"+strconv.FormatInt(existing.Number, 10), pullRequestURL(stored.Address, existing.Number))
		}
		var blockers []webui.MergeBlocker
		if action == "merge" {
			blockers = browserMergeProblemBlockers(err)
		}
		app.renderPullRequest(writer, request, stored, summary, chrome, number, []webui.Notice{problemNotice}, blockers, drafts, nil, status)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	app.noticeRedirect(writer, request, pullRequestURL(stored.Address, view.Number)+"?notice="+url.QueryEscape(notice))
}

func (app *App) renderPullRequest(writer http.ResponseWriter, request *http.Request, stored state.Repository, summary repository.Summary, chrome webui.Chrome, number int64, notices []webui.Notice, extraBlockers []webui.MergeBlocker, drafts pullRequestDrafts, answer *pullrequest.Mergeability, status int) {
	view, err := app.PullRequests.Show(request.Context(), stored.ID, number)
	if err != nil {
		problem := pullrequest.AsProblem(err)
		if problem.Code == "pull_request_not_found" || problem.Code == "invalid_pull_request_number" {
			app.renderError(writer, request, http.StatusNotFound, webui.MsgPRNotFound, "")
			return
		}
		app.renderError(writer, request, apiStatus(request, "pull request read", err), webui.MsgPRFailed, "")
		return
	}
	if view.Repository != stored.ID {
		app.renderError(writer, request, http.StatusNotFound, webui.MsgPRNotFound, "")
		return
	}

	basePage := app.baseRepositoryPage(request, chrome, stored, summary)
	self := pullRequestURL(stored.Address, view.Number)
	page := webui.PullRequestPage{
		Chrome:    chrome,
		Repo:      basePage.Repo,
		Tabs:      repositoryTabs(basePage, webui.RepoTabPullRequests),
		Number:    view.Number,
		Title:     view.Title,
		State:     view.State,
		Source:    browserRevision(view.Source),
		Target:    browserRevision(view.Target),
		Checks:    browserCheckEvidence(stored.Address, view.Checks),
		Review:    browserReviewEvidence(view.Review, view.Source, view.Target),
		Merge:     browserMergeAvailability(view.MergeEligibility),
		SelfURL:   self,
		ListURL:   basePage.PullRequestsURL,
		TasksURL:  basePage.TasksURL,
		CreatedAt: view.CreatedAt,
		UpdatedAt: view.UpdatedAt,
		// A description can be edited in every state: it changes no branch
		// and no recorded result.
		EditURL: self + "/edit",
		Edit:    webui.PullRequestEditForm{Revision: view.EditRevision, Title: view.Title},
	}
	if view.Body != nil {
		page.Description = app.pullRequestText(request.Context(), stored.Address, view.Target.Branch, *view.Body)
		page.Edit.Body = *view.Body
	}
	if view.EditedAt != nil {
		page.EditedAt = *view.EditedAt
	}
	for _, note := range view.ReviewNotes {
		page.ReviewNotes = append(page.ReviewNotes, webui.ReviewNoteView{
			Decision: note.Decision, ReviewerLabel: note.ReviewerLabel,
			ShortSourceOID: shortOID(note.SourceOID), ShortTargetOID: shortOID(note.TargetOID),
			Current: note.Current, Note: app.pullRequestText(request.Context(), stored.Address, view.Target.Branch, note.Note),
			SubmittedAt: note.SubmittedAt,
		})
	}
	page.ReviewNotesTruncated = view.ReviewNotesTruncated
	if drafts.Edit != nil {
		page.Edit = *drafts.Edit
	}
	if drafts.Review != nil {
		page.ReviewDraft = *drafts.Review
	}
	if notices != nil {
		page.Chrome.Notices = notices
	} else {
		// A result is shown only after the action that produced it, once
		// (resultNotice), and only when the pull request's state confirms it.
		page.Chrome.Notices = pullRequestNotices(app.resultNotice(writer, request), view)
	}
	page.Merge.Blockers = append(page.Merge.Blockers, extraBlockers...)
	if len(extraBlockers) != 0 {
		page.Merge.Eligible = false
	}
	if view.Merge != nil {
		page.Merged = &webui.MergeRecord{
			Mode:       view.Merge.Mode,
			OID:        view.Merge.OID,
			ShortOID:   shortOID(view.Merge.OID),
			ReceiptRef: view.Merge.ReceiptRef,
			MergedAt:   view.Merge.MergedAt,
		}
	}
	if view.State == state.PullRequestOpen && page.Source.Resolved() && page.Target.Resolved() {
		page.ReviewRequestURL = self + "/review/request"
		page.ReviewSkipURL = self + "/review/skip"
		page.ReviewSubmitURL = self + "/review/submit"
		page.MergeURL = self + "/merge"
		page.MergeabilityURL = self + "/mergeability"
		if answer != nil {
			page.Mergeability = browserMergeability(answer, page.Source, page.Target)
		}
	}
	// Closing needs no branch, so it stays available after the source branch
	// was deleted, which is a common reason to close.
	switch view.State {
	case state.PullRequestOpen:
		page.CloseURL = self + "/close"
		page.AutoMerge = browserAutoMerge(view.AutoMerge)
		if view.AutoMerge != nil {
			page.AutoMergeCancelURL = self + "/auto-merge/cancel"
		} else {
			page.AutoMergeEnableURL = self + "/auto-merge"
		}
	case state.PullRequestClosed:
		page.ReopenURL = self + "/reopen"
	}
	if !page.Source.Resolved() || !page.Target.Resolved() {
		page.ChangesUnavailable = true
	} else {
		changes, comparisonStatus := app.browserPullRequestChanges(request, stored.ID, page.Source.OID, page.Target.OID)
		if status == http.StatusOK {
			status = comparisonStatus
		}
		page.Changes, page.ChangesPages, page.ChangesAllURL, page.ChangesLines = changes.Files, changes.Pages, changes.AllURL, changes.Lines
		page.ChangesBase = changes.Base
		page.ChangesUnavailable = changes.Unavailable != ""
		page.ChangesReason = changes.Unavailable
		page.DiffTruncated = changes.PatchesIncomplete
		page.FilesTruncated = changes.FilesIncomplete
	}
	app.render(writer, request, status, page)
}

// pullRequestNotices shows the result of an action this page redirected from,
// but only while the pull request's current state still confirms it. The
// notice key comes from the address, so anyone can write it into a link; a
// key the state contradicts, or one meant for another page, shows nothing.
func pullRequestNotices(key string, view *pullrequest.View) []webui.Notice {
	open := view.State == state.PullRequestOpen
	merged := view.State == state.PullRequestMerged && view.Merge != nil
	var confirmed bool
	var code webui.MessageCode
	switch key {
	case "pull_request_created":
		confirmed, code = open, webui.MsgPRCreated
	case "review_requested":
		confirmed, code = open && view.Review.Status == state.ReviewPending, webui.MsgPRReviewAsked
	case "review_skipped":
		confirmed, code = open && view.Review.Status == state.ReviewSkipped, webui.MsgPRReviewSkipped
	case "pull_request_merged":
		confirmed, code = merged && view.Merge.Mode != webui.MergeModeUpToDate, webui.MsgPRMerged
	case "pull_request_up_to_date":
		confirmed, code = merged && view.Merge.Mode == webui.MergeModeUpToDate, webui.MsgPRUpToDate
	case "pull_request_closed":
		confirmed, code = view.State == state.PullRequestClosed, webui.MsgPRClosedDone
	case "pull_request_reopened":
		confirmed, code = open, webui.MsgPRReopenedDone
	case "auto_merge_enabled":
		confirmed, code = open && view.AutoMerge != nil, webui.MsgAutoMergeEnabled
	case "auto_merge_canceled":
		confirmed, code = open && view.AutoMerge == nil, webui.MsgAutoMergeCanceled
	case "pull_request_edited":
		confirmed, code = view.EditedAt != nil, webui.MsgPREditSaved
	case "review_recorded":
		confirmed = open && (view.Review.Status == state.ReviewApproved || view.Review.Status == state.ReviewChangesRequested)
		code = webui.MsgPRReviewRecorded
	}
	if !confirmed {
		return nil
	}
	return []webui.Notice{webui.Success(code)}
}

// pullRequestText renders a description or review note. The Markdown
// renderer writes no raw HTML from the text, and no image: Raw is empty, so a
// picture in the repository is dropped, and one elsewhere becomes a link that
// the page never loads. Relative links open files on the target branch. Text
// that cannot be rendered now is shown as written.
func (app *App) pullRequestText(ctx context.Context, address, targetBranch, text string) webui.PullRequestText {
	result := webui.PullRequestText{Text: text}
	if text == "" {
		return result
	}
	rendered, err := markdown.Render(ctx, []byte(text), markdown.Links{
		File: "/repositories/" + url.PathEscape(address) + "/code?ref=" + url.QueryEscape(targetBranch) + "&path=",
	})
	if err != nil {
		result.NotRendered = markdownNotShown(err)
		return result
	}
	// markdown.Render writes no raw HTML from the text and resolves every
	// address itself, which is what makes this conversion safe.
	result.HTML = template.HTML(rendered)
	return result
}

// browserMergeability shows answer for the commits the page shows. The page
// reads the branches again after the answer, so an answer about other
// commits is shown as stale rather than as an answer about these.
func browserMergeability(answer *pullrequest.Mergeability, source, target webui.RevisionState) *webui.MergeabilityAnswer {
	shown := &webui.MergeabilityAnswer{
		Status: answer.Status, Method: answer.Method, Reason: answer.Reason,
		ConflictPaths: answer.ConflictPaths, ConflictPathsTruncated: answer.ConflictPathsTruncated,
		ShortSourceOID: source.ShortOID, ShortTargetOID: target.ShortOID,
	}
	if answer.Source.OID != source.OID || answer.Target.OID != target.OID {
		*shown = webui.MergeabilityAnswer{Status: webui.MergeabilityStale, ShortSourceOID: source.ShortOID, ShortTargetOID: target.ShortOID}
	}
	return shown
}

func browserRevision(revision pullrequest.Revision) webui.RevisionState {
	return webui.RevisionState{
		Branch:   revision.Branch,
		OID:      revision.OID,
		ShortOID: shortOID(revision.OID),
		Status:   revision.Status,
	}
}

func browserCheckEvidence(address string, checks pullrequest.Checks) webui.CheckEvidence {
	evidence := webui.CheckEvidence{
		Status:               checks.Status,
		Configured:           checks.Configured,
		Passed:               checks.Passed,
		TestedCommit:         checks.TestedCommit,
		Advisory:             checks.Advisory,
		Stale:                checks.Stale,
		WorktreeState:        checks.WorktreeState,
		RevisionOID:          checks.RevisionOID,
		RevisionShortOID:     shortOID(checks.RevisionOID),
		ConfigurationVersion: checks.ConfigurationVersion,
		AttemptID:            checks.AttemptID,
		AttemptShortID:       shortOpaqueID(checks.AttemptID),
		Summary:              checks.Summary,
		LogStatus:            webui.LogStatusOf(checks.LogStatus),
		OutputTruncated:      checks.LogTruncated,
		Protection:           browserProtection(checks.JobID, checks.Protection, checks.ExecutionScope),
		LogError:             checks.LogError,
		ReadFailure:          browserEvidenceReadFailure(checks.ReadFailure, webui.MsgCheckRecordUnreadable),
		CleanupFailed:        checks.CleanupFailed,
	}
	if checks.FinishedAt != nil {
		evidence.FinishedAt = *checks.FinishedAt
	}
	if checks.RegisteredAt != nil {
		evidence.RegisteredAt = *checks.RegisteredAt
	}
	if checks.LogExpiresAt != nil {
		evidence.LogExpiresAt = *checks.LogExpiresAt
	}
	evidence.CredentialProvenance = browserProvenance(checks.JobID, checks.CredentialID, checks.ExecutionScope)
	if checks.TaskID != "" {
		evidence.AttemptURL = tasksURL(address, checks.TaskID)
	}
	return evidence
}

func browserReviewEvidence(review pullrequest.Review, source, target pullrequest.Revision) webui.ReviewEvidence {
	bound := review.SourceOID == source.OID && review.TargetOID == target.OID && review.SourceOID != "" && review.TargetOID != ""
	evidence := webui.ReviewEvidence{
		Status:                 review.Status,
		Provenance:             review.Provenance,
		ReviewerLabel:          review.ReviewerLabel,
		Independent:            review.Independent,
		ExecutedChecks:         review.ExecutedChecks,
		SourceOID:              review.SourceOID,
		TargetOID:              review.TargetOID,
		ShortSourceOID:         shortOID(review.SourceOID),
		BoundToCurrentRevision: bound,
		ReadFailure:            browserEvidenceReadFailure(review.ReadFailure, webui.MsgReviewRecordUnreadable),
	}
	if review.SubmittedAt != nil {
		evidence.SubmittedAt = *review.SubmittedAt
	}
	return evidence
}

func browserEvidenceReadFailure(failure *pullrequest.ReadFailure, fallback webui.MessageCode) *webui.EvidenceReadFailure {
	if failure == nil {
		return nil
	}
	message := fallback
	switch failure.Code {
	case pullrequest.ReadFailureCheckConfiguration:
		message = webui.MsgCheckConfigurationUnreadable
	case pullrequest.ReadFailureCheckEvidence:
		message = webui.MsgCheckRecordUnreadable
	case pullrequest.ReadFailureReviewEvidence:
		message = webui.MsgReviewRecordUnreadable
	}
	return &webui.EvidenceReadFailure{Code: failure.Code, Message: message}
}

func browserMergeAvailability(eligibility pullrequest.Eligibility) webui.MergeAvailability {
	result := webui.MergeAvailability{Eligible: eligibility.Eligible}
	for _, blocker := range eligibility.Blockers {
		result.Blockers = append(result.Blockers, browserMergeBlocker(blocker.Code, blocker.Message))
	}
	return result
}

func browserMergeBlocker(code, detail string) webui.MergeBlocker {
	blocker := webui.MergeBlocker{Code: code}
	switch code {
	case "already_merged", "source_branch_missing", "target_branch_missing", "source_not_commit", "target_not_commit", "merge_conflict", "stale_revision", "pull_request_not_open", "git_update_failed":
		// The renderer has localized text for these stable codes. Repeating the
		// service's English sentence as detail would make a Korean page bilingual.
	default:
		blocker.Detail = detail
	}
	return blocker
}

func browserMergeProblemBlockers(err error) []webui.MergeBlocker {
	problem := pullrequest.AsProblem(err)
	if eligibility, ok := problem.Details.(pullrequest.Eligibility); ok {
		return browserMergeAvailability(eligibility).Blockers
	}
	switch problem.Code {
	case "merge_conflict", "stale_revision", "pull_request_not_open", "git_update_failed":
		return []webui.MergeBlocker{browserMergeBlocker(problem.Code, problem.Message)}
	default:
		return nil
	}
}

func browserPullRequestProblem(request *http.Request, step string, err error, action string) (webui.Notice, int) {
	problem := pullrequest.AsProblem(err)
	var ambiguous *repository.AmbiguousBranchError
	if errors.As(err, &ambiguous) {
		return webui.Error("", webui.MsgBranchAmbiguous).WithDetail(ambiguous.Detail()), apiStatus(request, step, err)
	}
	field := ""
	code := webui.MsgPRFailed
	switch problem.Code {
	case "invalid_title":
		field, code = "title", webui.MsgPRInvalidTitle
	case "invalid_body":
		field, code = "body", webui.MsgPRInvalidBody
	case "invalid_note":
		field, code = "note", webui.MsgPRInvalidNote
	case "invalid_reviewer_label":
		field, code = "reviewer_label", webui.MsgPRInvalidReviewer
	case "invalid_review_decision":
		field, code = "decision", webui.MsgPRInvalidDecision
	case "stale_edit":
		field, code = "edit", webui.MsgPREditStale
	case "invalid_branch", "reserved_ref":
		field, code = "source_branch", webui.MsgPRInvalidBranch
	case "source_branch_missing", "source_not_commit":
		field, code = "source_branch", webui.MsgPRInvalidBranch
		if action != "" {
			// The pull request page has no branch field, so the notice is page
			// level there and says what happened to the branch.
			field, code = "", webui.MsgMergeBlockedSourceGone
			if problem.Code == "source_not_commit" {
				code = webui.MsgMergeBlockedSourceKind
			}
		}
	case "target_branch_missing", "target_not_commit":
		field, code = "target_branch", webui.MsgPRInvalidBranch
		if action != "" {
			field, code = "", webui.MsgMergeBlockedTargetGone
			if problem.Code == "target_not_commit" {
				code = webui.MsgMergeBlockedTargetKind
			}
		}
	case "storage_changed":
		code = webui.MsgStorageChanged
	case "pull_request_exists":
		code = webui.MsgPRAlreadyOpen
	case "auto_merge_not_enabled":
		code = webui.MsgAutoMergeNotEnabled
	case "pull_request_merged":
		code = webui.MsgPRMergedFixed
	case "same_branch":
		field, code = "source_branch", webui.MsgPRSameBranch
	case "invalid_review_choice":
		field, code = "review", webui.MsgPRFailed
	case "invalid_revision", "stale_revision":
		switch action {
		case "merge":
			field, code = "merge", webui.MsgPRStale
		case "review_submit":
			field, code = "decision", webui.MsgPRReviewMoved
		default:
			code = webui.MsgPRStale
		}
	case "merge_conflict", "merge_blocked", "git_update_failed":
		field, code = "merge", webui.MsgPRMergeBlocked
	case "pull_request_not_found", "invalid_pull_request_number":
		code = webui.MsgPRNotFound
	case "pull_request_not_open":
		code = webui.MsgPRNotOpen
	case "merge_reconciliation_pending", "pull_request_creation_reconciliation_pending":
		code = webui.MsgPRReconciling
	}
	return webui.Error(field, code), apiStatus(request, step, err)
}

// movedRevision reports whether err refused a change because a branch is no
// longer at the commit the page showed.
func movedRevision(err error) bool {
	if err == nil {
		return false
	}
	code := pullrequest.AsProblem(err).Code
	return code == "stale_revision" || code == "invalid_revision"
}

func observedBranch(summary repository.Summary, name string) webui.RevisionState {
	revision := webui.RevisionState{Branch: name, Status: webui.RevisionMissing}
	for _, branch := range summary.Branches {
		if branch.Name != name {
			continue
		}
		revision.OID = branch.OID
		revision.ShortOID = shortOID(branch.OID)
		revision.Status = webui.RevisionNotCommit
		if branch.Type == "commit" && validOID(branch.OID) {
			revision.Status = webui.RevisionCommit
		}
		return revision
	}
	return revision
}

func defaultPullRequestBranches(summary repository.Summary) (string, string) {
	if len(summary.Branches) == 0 {
		return "", ""
	}
	target := summary.DefaultBranch
	if target == "" {
		target = summary.Branches[0].Name
	}
	source := summary.Branches[0].Name
	for _, branch := range summary.Branches {
		if branch.Name != target {
			source = branch.Name
			break
		}
	}
	return source, target
}

func repositoryTabs(page webui.RepositoryPage, active webui.RepoTab) webui.RepoTabs {
	return webui.RepoTabs{
		OverviewURL:     page.OverviewURL,
		CodeURL:         page.CodeURL,
		CommitsURL:      page.CommitsURL,
		PullRequestsURL: page.PullRequestsURL,
		TasksURL:        page.TasksURL,
		ImportsURL:      page.ImportsURL,
		SettingsURL:     page.SettingsURL,
		DeleteURL:       page.DeleteURL,
		AdminLocked:     page.Chrome.Viewer.AdminAsks,
		Active:          active,
	}
}

func pullRequestURL(address string, number int64) string {
	return "/repositories/" + url.PathEscape(address) + "/pull-requests/" + strconv.FormatInt(number, 10)
}

func tasksURL(address, taskID string) string {
	base := "/repositories/" + url.PathEscape(address) + "/tasks"
	if taskID == "" {
		return base
	}
	return base + "?task=" + url.QueryEscape(taskID)
}

func shortOpaqueID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// browserProtection names where a recorded attempt actually ran.
//
// The distinction that matters is manual against automatic. A manual helper
// run happens in the operator's own environment with their permissions; an
// automatic job is the server's own work under a saved policy, on the server
// host, in a container it started, or on an external runner. Those are
// different machines and different authorities, so they never share a
// sentence.
//
// The job link is the authority for that division, because it is the recorded
// fact rather than an inference: an attempt carries a job identifier only when
// OwnGit admitted and ran the work itself. The execution scope and protection
// are read afterwards, and only to choose which automatic wording applies.
// They cannot promote an unlinked attempt to automatic, and an automatic
// attempt whose protection was never established is reported as unknown rather
// than described as the operator's own run.
func browserProtection(jobID, protection, executionScope string) string {
	if jobID == "" {
		// No job admitted this run. The inherited scope with no established
		// protection is exactly what a manual helper submission records.
		// Anything else was not recorded as a manual run either, so it claims
		// nothing instead of borrowing an automatic sentence.
		if executionScope == state.ExecutionScopeInherited && protection == state.ProtectionUnknown {
			return webui.ProtectionInherited
		}
		if protection == state.ProtectionUnknown {
			return webui.ProtectionUnknown
		}
		return ""
	}
	// An admitted job ran on the server. Which of the three automatic sentences
	// applies is decided by the executor-derived scope together with the
	// protection the job actually established.
	switch executionScope {
	case state.ExecutionScopeContainer:
		if protection == state.ProtectionContainer {
			return webui.ProtectionAutomaticContainer
		}
	case state.ExecutionScopeExternalRunner:
		if protection == state.ProtectionRunnerReported {
			return webui.ProtectionRunnerReported
		}
	case state.ExecutionScopeInherited:
		if protection == state.ProtectionHost {
			return webui.ProtectionAutomaticHost
		}
	}
	// The store permits an automatic job to record no protection at all. That
	// is a missing fact about an automatic run, never evidence of a manual one.
	return webui.ProtectionUnknown
}

// browserProvenance names how an attempt reached OwnGit.
//
// A helper and a runner both authenticate, but they are not the same origin,
// so they never share a sentence. The job link decides whether OwnGit ran the
// work itself, and the executor-derived scope separates the server's own run
// from work an external runner claimed. An attempt with no credential states
// nothing.
func browserProvenance(jobID, credentialID, executionScope string) string {
	if credentialID == "" {
		return webui.ProvenanceUnstated
	}
	if jobID == "" {
		return webui.ProvenanceAuthenticatedHelper
	}
	if executionScope == state.ExecutionScopeExternalRunner {
		return webui.ProvenanceRunnerClaimed
	}
	return webui.ProvenanceAutomaticJob
}

// pullRequestChanges is what a pull request page shows as its changes.
type pullRequestChanges struct {
	Files []webui.DiffFile
	// Pages places Files among all changed files when they span several
	// pages; its links are added by the caller.
	Pages webui.PageContinuation
	// Lines places the lines of the one file shown alone.
	Lines webui.PageContinuation
	// AllURL leaves a view of one file for the whole list.
	AllURL string
	// Base is the merge base the changes are counted from.
	Base string
	// Unavailable names why there is no change list: the branches share no
	// history, or they have several merge bases.
	Unavailable webui.MessageCode
	// PatchesIncomplete is set when some file's changes are not shown.
	PatchesIncomplete bool
	// FilesIncomplete is set when the list of changed files was cut off.
	FilesIncomplete bool
}

func (app *App) browserPullRequestChanges(request *http.Request, repositoryID, sourceOID, targetOID string) (pullRequestChanges, int) {
	changes, err := app.comparePullRequestRevisions(request.Context(), repositoryID, sourceOID, targetOID, changeView(request))
	if err != nil {
		status, reason := comparisonFailure(request, err)
		return pullRequestChanges{Unavailable: reason}, status
	}
	addFileLinks(request, &changes.Pages)
	return changes, http.StatusOK
}

// comparisonFailure logs a comparison that failed and returns the status
// and the reason the page gives: the browsing limits when they cannot be
// read, and otherwise that the changes are unavailable.
func comparisonFailure(request *http.Request, err error) (int, webui.MessageCode) {
	if errors.As(err, new(*state.PolicyError)) {
		logFailure(request, "pull request comparison", err)
		return http.StatusConflict, webui.MsgBrowseUnreadable
	}
	return unavailable(request, "pull request comparison", err), webui.MsgErrUnavailable
}

// comparePullRequestRevisions reads what the source adds since it branched
// from the target: the changes from the merge base of the two recorded
// revisions to the source, as a three-dot diff shows them. Without a merge
// base, or with several, it shows no comparison rather than one against the
// target tip or an arbitrary base. Merge and review use their own exact
// revision checks and never this reading.
func (app *App) comparePullRequestRevisions(ctx context.Context, repositoryID, sourceOID, targetOID string, view changesView) (pullRequestChanges, error) {
	if !validOID(sourceOID) || !validOID(targetOID) {
		return pullRequestChanges{}, errors.New("invalid pull request revision")
	}
	limits, err := app.Store.BrowseLimits(ctx)
	if err != nil {
		return pullRequestChanges{}, err
	}
	if view.file != "" {
		if changes, found, err := app.pullRequestFile(ctx, repositoryID, sourceOID, targetOID, view, limits); err != nil || found {
			return changes, err
		}
	}
	comparison, err := app.Repositories.Compare(ctx, repositoryID, targetOID, sourceOID, limits.CompareBytes, limits.CompareTime)
	if err != nil {
		return pullRequestChanges{}, fmt.Errorf("read pull request changes: %w", err)
	}
	switch {
	case comparison.Bases == 0:
		return pullRequestChanges{Unavailable: webui.MsgPRChangesNoBase}, nil
	case comparison.Bases > 1:
		return pullRequestChanges{Unavailable: webui.MsgPRChangesManyBases}, nil
	}
	// The window of files comes first, so the page's line budget and patch
	// are spent on its own files and not on those before them.
	from, pages := fileWindow(view.first, len(comparison.Files))
	window := comparison.Files[from : from+max(0, pages.Last-pages.First+1)]
	if pages.Total <= maximumDiffFiles {
		pages = webui.PageContinuation{}
	}
	patch, patchTruncated := comparison.Patch, comparison.PatchTruncated
	if len(window) < len(comparison.Files) && !comparison.FilesTruncated {
		// A file this computer cannot compare as text is left out of the read
		// of one window as well: Git reads such a file whole while it writes
		// its patch, and the page says why it has no lines.
		paths := make([]string, 0, len(window))
		for _, file := range window {
			if !file.TextDiffUnavailable {
				paths = append(paths, file.Path)
			}
		}
		patch, patchTruncated = "", false
		if len(paths) > 0 {
			read, cut, err := app.Repositories.ComparePatch(ctx, repositoryID, comparison.Base, sourceOID, paths, limits.CompareBytes, limits.CompareTime)
			if err != nil {
				return pullRequestChanges{}, fmt.Errorf("read pull request changes: %w", err)
			}
			patch, patchTruncated = read, cut
		}
	}
	files, notLoaded := diffFileItems(window, patch, patchTruncated, nil, view.fileURL, limits.CommitFileBytes)
	return pullRequestChanges{
		Files: files, Pages: pages, Base: comparison.Base,
		PatchesIncomplete: notLoaded || patchTruncated,
		FilesIncomplete:   comparison.FilesTruncated,
	}, nil
}

// pullRequestFile reads one changed path alone, for the view of one file: its
// change and its diff, within the limit of one file's diff and paged by
// lines like a single-file commit diff. It does not read the list of the
// other changed files, so a long list never decides what this view shows.
// found is false when the path has no change in this comparison, and the
// caller then shows the list. A read that fails is returned as an error,
// never shown as an empty diff.
func (app *App) pullRequestFile(ctx context.Context, repositoryID, sourceOID, targetOID string, view changesView, limits state.BrowseLimits) (changes pullRequestChanges, found bool, err error) {
	bases, err := app.Repositories.MergeBases(ctx, repositoryID, targetOID, sourceOID)
	if err != nil {
		return pullRequestChanges{}, false, fmt.Errorf("read pull request changes: %w", err)
	}
	switch len(bases) {
	case 0:
		return pullRequestChanges{Unavailable: webui.MsgPRChangesNoBase}, true, nil
	case 1:
	default:
		return pullRequestChanges{Unavailable: webui.MsgPRChangesManyBases}, true, nil
	}
	file, patch, truncated, found, err := app.Repositories.CompareFile(ctx, repositoryID, bases[0], sourceOID, view.file, limits.FilePatchBytes, limits.CompareTime)
	if err != nil {
		return pullRequestChanges{}, false, fmt.Errorf("read pull request changes: %w", err)
	}
	if !found {
		return pullRequestChanges{}, false, nil
	}
	item := diffFileItem(file, view.fileURL)
	patch = splitPatchByFile(patch, false)[file.Path]
	var lines webui.PageContinuation
	if !item.Binary {
		first := view.from
		var total, shown int
		item.Hunks, total, shown = patchLinePage(patch, first, maximumCommitDiffLines)
		if total > 0 && shown == 0 {
			first = 1
			item.Hunks, total, shown = patchLinePage(patch, first, maximumCommitDiffLines)
		}
		lines = lineContinuation(view.fileURL(item.Path), first, shown, total)
		lines.Incomplete = truncated
		// Changed lines that were counted but not read are not an empty diff.
		item.NotLoaded = len(item.Hunks) == 0 && (file.Additions+file.Deletions > 0 || !file.CountsRead)
	}
	return pullRequestChanges{
		Files: []webui.DiffFile{item}, Lines: lines, AllURL: view.allURL, Base: bases[0],
		PatchesIncomplete: truncated || item.NotLoaded,
	}, true, nil
}

func parsePullRequestNumber(value string) (int64, bool) {
	number, err := strconv.ParseInt(value, 10, 64)
	return number, err == nil && number > 0 && strings.TrimSpace(value) == value
}
