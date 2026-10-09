package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"owngit/internal/auth"
	"owngit/internal/bidi"
	"owngit/internal/jsoninput"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/requestctx"
	"owngit/internal/state"
)

const (
	maximumAPIRequest  = 64 << 10
	maximumAPIResponse = 4 << 20
)

func (app *App) handleAPI(writer http.ResponseWriter, request *http.Request, settings state.Settings) {
	if !strings.HasPrefix(request.URL.Path, "/api/v1/") {
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
		return
	}
	if !settings.Initialized {
		writeAPIError(writer, http.StatusConflict, "setup_incomplete", "OwnGit setup is not complete.", nil)
		return
	}
	resource, remainder, repositoryRoute := parseRepositoryAPIRoute(request.URL.Path)
	if request.URL.RawQuery != "" {
		// URL.Query drops malformed pairs and the parse error, so an invalid
		// ref, year or commit pair would be read as an omitted one. Refuse
		// query text that cannot be parsed instead.
		if _, err := url.ParseQuery(request.URL.RawQuery); err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "The query string is malformed.", nil)
			return
		}
		if !importHistoryQueryAllowed(request, repositoryRoute, resource, remainder) &&
			!pullRequestDiffQueryAllowed(request) && !pullRequestListQueryAllowed(request) && !helperTaskListQueryAllowed(request) && !archiveQueryAllowed(request, repositoryRoute, resource, remainder) && !activityQueryAllowed(request) &&
			!taskViewQueryAllowed(request) {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "This API endpoint does not accept query parameters.", nil)
			return
		}
	}
	if request.URL.Path == "/api/v1/settings" {
		app.handleSettingsAPI(writer, request)
		return
	}
	if name, ok := ownerSettingName(request.URL.Path); ok {
		app.handleOwnerSettingAPI(writer, request, name)
		return
	}
	if request.URL.Path == "/api/v1/backups" || strings.HasPrefix(request.URL.Path, "/api/v1/backups/") {
		app.handleBackupsAPI(writer, request, settings)
		return
	}
	if request.URL.Path == activityAPIPath {
		app.handleActivityAPI(writer, request, settings)
		return
	}
	if request.URL.Path == taskViewAPIPath || strings.HasPrefix(request.URL.Path, taskViewAPIPath+"/") {
		app.handleTaskViewAPI(writer, request, settings)
		return
	}
	if request.URL.Path == allHelperCredentialsAPIPath {
		app.handleAllHelperCredentialsAPI(writer, request)
		return
	}
	if name, ok := repositoryAPIRoute(request.URL.Path); ok {
		app.handleRepositoryAPI(writer, request, settings, name != "")
		return
	}
	// The routes below use the ID the path's repository name reaches. The
	// authorization of each route answers a name that reaches no current
	// repository (see answerRepositoryAddressAPI).
	address, _ := repositoryAddressOf(request)
	repositoryID := address.id
	if repositoryRoute {
		switch resource {
		case "runner":
			app.handleRunnerAPI(writer, request, repositoryID, remainder)
			return
		case "check-policy", "check-jobs", "runner-credentials":
			app.handleConfiguredCheckOwnerAPI(writer, request, repositoryID, resource, remainder)
			return
		case "tasks", "check-configurations":
			app.handleCheckAPI(writer, request, repositoryID, resource, remainder)
			return
		case "check-attempts":
			app.handleCheckAttemptLog(writer, request, repositoryID, remainder)
			return
		case "helper-credentials":
			app.handleHelperCredentialAPI(writer, request, repositoryID, remainder)
			return
		case "import":
			request = app.firstImportDestination(request)
			address, _ := repositoryAddressOf(request)
			app.handleImportAPI(writer, request, address.id, remainder)
			return
		case "settings":
			app.handleRepositorySettingsAPI(writer, request, repositoryID, remainder)
			return
		case "share-links":
			app.handleShareLinksAPI(writer, request, repositoryID, remainder)
			return
		case "rename":
			app.handleRenameRepositoryAPI(writer, request, repositoryID, remainder)
			return
		case "archive":
			app.handleArchiveAPI(writer, request, settings, repositoryID, remainder)
			return
		case "default-branch", "delete":
			app.handleRepositoryAdminAPI(writer, request, repositoryID, resource, remainder)
			return
		case "kept-history", "restore":
			app.handleRestoreAPI(writer, request, settings, repositoryID, resource, remainder)
			return
		}
	}
	if !app.authorizeAPI(writer, request, settings) {
		return
	}

	number, operation, ok := parsePullRequestAPIRoute(request.URL.Path)
	if !ok {
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
		return
	}
	if operation == "collection" {
		switch request.Method {
		case http.MethodGet:
			operation = "list"
		case http.MethodPost:
			operation = "create"
		default:
			writeAPIMethodError(writer, http.MethodGet+", "+http.MethodPost)
			return
		}
	}

	var result any
	var err error
	switch operation {
	case "list":
		if request.Method != http.MethodGet {
			writeAPIMethodError(writer, http.MethodGet)
			return
		}
		input, parseErr := pullRequestListInput(request.URL.Query())
		if parseErr != nil {
			err = parseErr
			break
		}
		var page *pullrequest.ListResult
		page, err = app.PullRequests.List(request.Context(), repositoryID, input)
		if err == nil {
			result = struct {
				OK    bool                `json:"ok"`
				Items []*pullrequest.View `json:"pull_requests"`
				Next  int64               `json:"next,omitempty"`
			}{OK: true, Items: page.Items, Next: page.Next}
		}
	case "create":
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		var input pullrequest.CreateInput
		if !decodeAPIJSONLimit(writer, request, &input, pullrequest.MaximumTextRequestBytes) {
			return
		}
		if input.Repository != "" && input.Repository != repositoryID {
			writeAPIError(writer, http.StatusBadRequest, "invalid_repository", "The body repository does not match the API path.", nil)
			return
		}
		input.Repository, input.Actor = repositoryID, generalAccessActor
		var view *pullrequest.View
		view, err = app.PullRequests.Create(request.Context(), input)
		if err == nil {
			app.trayOrigins.note(request, originKey(state.NotifyPullRequest, pullRequestID(repositoryID, view.Number)))
			result = pullrequest.SuccessEnvelope{OK: true, PullRequest: view}
		}
	case "show":
		if request.Method != http.MethodGet {
			writeAPIMethodError(writer, http.MethodGet)
			return
		}
		var view *pullrequest.View
		view, err = app.PullRequests.Show(request.Context(), repositoryID, number)
		if err == nil {
			result = pullrequest.SuccessEnvelope{OK: true, PullRequest: view}
		}
	case "review_request", "review_skip", "merge":
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		var input pullrequest.RevisionInput
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		input.Actor = generalAccessActor
		var view *pullrequest.View
		switch operation {
		case "review_request":
			view, err = app.PullRequests.RequestReview(request.Context(), repositoryID, number, input)
		case "review_skip":
			view, err = app.PullRequests.SkipReview(request.Context(), repositoryID, number, input)
		case "merge":
			view, err = app.PullRequests.Merge(request.Context(), repositoryID, number, input)
		}
		if err == nil {
			result = pullrequest.SuccessEnvelope{OK: true, PullRequest: view}
		}
	case "diff":
		if request.Method != http.MethodGet {
			writeAPIMethodError(writer, http.MethodGet)
			return
		}
		query := request.URL.Query()
		pinned := pullrequest.RevisionInput{SourceOID: query.Get("source_oid"), TargetOID: query.Get("target_oid")}
		result, err = app.pullRequestDiff(request.Context(), repositoryID, number, pinned)
	case "mergeability":
		if request.Method != http.MethodGet {
			writeAPIMethodError(writer, http.MethodGet)
			return
		}
		query := request.URL.Query()
		expected := pullrequest.RevisionInput{SourceOID: query.Get("source_oid"), TargetOID: query.Get("target_oid")}
		result, err = app.pullRequestMergeability(request, repositoryID, number, expected)
	case "close", "reopen":
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		// The body is an empty JSON object. Requiring JSON keeps the same
		// cross-site protection as every other mutating call.
		var input struct{}
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		var view *pullrequest.View
		if operation == "close" {
			view, err = app.PullRequests.Close(request.Context(), repositoryID, number)
		} else {
			view, err = app.PullRequests.Reopen(request.Context(), repositoryID, number)
		}
		if err == nil {
			result = pullrequest.SuccessEnvelope{OK: true, PullRequest: view}
		}
	case "edit":
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		var input pullrequest.EditInput
		if !decodeAPIJSONLimit(writer, request, &input, pullrequest.MaximumTextRequestBytes) {
			return
		}
		input.Actor = generalAccessActor
		var view *pullrequest.View
		view, err = app.PullRequests.Edit(request.Context(), repositoryID, number, input)
		if err == nil {
			result = pullrequest.SuccessEnvelope{OK: true, PullRequest: view}
		}
	case "review_submit":
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		var input pullrequest.ReviewSubmitInput
		if !decodeAPIJSONLimit(writer, request, &input, pullrequest.MaximumTextRequestBytes) {
			return
		}
		input.Actor = generalAccessActor
		var view *pullrequest.View
		view, err = app.PullRequests.SubmitReview(request.Context(), repositoryID, number, input)
		if err == nil {
			result = pullrequest.SuccessEnvelope{OK: true, PullRequest: view}
		}
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
		return
	}
	if err != nil {
		problem := pullrequest.AsProblem(err)
		writeAPIError(writer, apiStatus(request, "pull request "+strings.ReplaceAll(operation, "_", " "), err), problem.Code, problem.Message, problem.Details)
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

// generalAccessActor is who a pull request change records. Every one is
// authorized by general access: open mode, the shared password, or a browser
// session that holds it.
var generalAccessActor = state.Actor{Kind: state.ActorAccess}

func (app *App) authorizeAPI(writer http.ResponseWriter, request *http.Request, settings state.Settings) bool {
	if settings.AccessMode == "open" {
		return answerRepositoryAddressAPI(writer, request)
	}
	_, password, ok := request.BasicAuth()
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Basic realm="OwnGit"`)
		writeAPIError(writer, http.StatusUnauthorized, "authentication_required", "A shared general-access password is required.", nil)
		return false
	}
	_, ok = app.checkAPIPassword(writer, request, "general", password)
	return ok && answerRepositoryAddressAPI(writer, request)
}

// checkAPIPassword verifies an API request's password of kind ("general" or
// "admin") and answers when it is not accepted: 401 with a challenge only
// for a wrong password, 429 with Retry-After for a rate limit, 409 when a
// wrong password cannot be counted because the saved login limits cannot
// be read, and 503 without a challenge when the check could not be
// completed, because the password may be right. An accepted password comes
// with the version it was verified at.
func (app *App) checkAPIPassword(writer http.ResponseWriter, request *http.Request, kind, password string) (version int64, ok bool) {
	version, err := app.Auth.VerifyCredential(request.Context(), kind, password, requestctx.Of(request).ClientAddress)
	switch {
	case err == nil:
		return version, true
	case errors.Is(err, auth.ErrRateLimited):
		if seconds := auth.RetryAfter(err); seconds > 0 {
			writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
		message := "Too many authentication attempts. Try again later."
		if auth.IsServerWide(err) {
			message = "Too many wrong administrator passwords were sent from all addresses, so every administrator password check is paused. Wait and try again, or run reset-admin on the computer that runs OwnGit."
		}
		writeAPIError(writer, http.StatusTooManyRequests, "authentication_rate_limited", message, nil)
	case errors.As(err, new(*state.PolicyError)):
		writeSettingUnreadable(writer, request, kind+" password check", err)
	case errors.Is(err, auth.ErrInvalidCredentials):
		realm, code, message := `Basic realm="OwnGit"`, "invalid_credentials", "The shared general-access password is invalid."
		if kind == "admin" {
			realm, code, message = `Basic realm="OwnGit admin"`, "invalid_admin_credentials", "The administrator password is invalid."
		}
		writer.Header().Set("WWW-Authenticate", realm)
		writeAPIError(writer, http.StatusUnauthorized, code, message, nil)
	default:
		writeAPIError(writer, unavailable(request, kind+" password check", err), "state_unavailable", "The password could not be verified. Try again later.", pullrequest.OperationErrorDetails{OperationStarted: new(bool)})
	}
	return 0, false
}

func parseRepositoryAPIRoute(requestPath string) (string, string, bool) {
	const prefix = "/api/v1/repositories/"
	if !strings.HasPrefix(requestPath, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(requestPath, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	resource, remainder := parts[1], ""
	if index := strings.Index(parts[1], "/"); index >= 0 {
		resource, remainder = parts[1][:index], parts[1][index+1:]
	}
	return resource, remainder, true
}

// pullRequestListQueryAllowed accepts the paging parameters of a pull request
// list, each given once.
func pullRequestListQueryAllowed(request *http.Request) bool {
	if _, operation, ok := parsePullRequestAPIRoute(request.URL.Path); request.Method != http.MethodGet || !ok || operation != "collection" {
		return false
	}
	for key, values := range request.URL.Query() {
		if (key != "state" && key != "limit" && key != "before") || len(values) != 1 {
			return false
		}
	}
	return true
}

// helperTaskListQueryAllowed accepts the paging parameters of the check
// helper's repository task list, each given once. Its other routes take no
// parameters.
func helperTaskListQueryAllowed(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	resource, remainder, ok := parseRepositoryAPIRoute(request.URL.Path)
	if !ok || resource != "tasks" || remainder != "" {
		return false
	}
	for key, values := range request.URL.Query() {
		if (key != "limit" && key != "before") || len(values) != 1 {
			return false
		}
	}
	return true
}

func parsePullRequestAPIRoute(requestPath string) (int64, string, bool) {
	const prefix = "/api/v1/repositories/"
	if !strings.HasPrefix(requestPath, prefix) {
		return 0, "", false
	}
	parts := strings.Split(strings.TrimPrefix(requestPath, prefix), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "pull-requests" {
		return 0, "", false
	}
	if len(parts) == 2 {
		return 0, "collection", true
	}
	number, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || number <= 0 {
		return 0, "", false
	}
	if len(parts) == 3 {
		return number, "show", true
	}
	if len(parts) == 4 && (parts[3] == "merge" || parts[3] == "close" || parts[3] == "reopen" || parts[3] == "diff" || parts[3] == "edit" || parts[3] == "mergeability") {
		return number, parts[3], true
	}
	if len(parts) == 5 && parts[3] == "review" {
		switch parts[4] {
		case "request":
			return number, "review_request", true
		case "submit":
			return number, "review_submit", true
		case "skip":
			return number, "review_skip", true
		}
	}
	return 0, "", false
}

func decodeAPIJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	return decodeAPIJSONLimit(writer, request, destination, maximumAPIRequest)
}

func decodeAPIJSONLimit(writer http.ResponseWriter, request *http.Request, destination any, limit int64) bool {
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "json_required", "Mutating API requests require Content-Type application/json.", nil)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	content, err := io.ReadAll(request.Body)
	var maximum *http.MaxBytesError
	if errors.As(err, &maximum) {
		writeAPIError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "The JSON request exceeds the supported size.", nil)
		return false
	}
	// JSON text is UTF-8 (RFC 8259), and a surrogate escape names a character
	// only with its pair. Decoding would put U+FFFD in place of either and so
	// store text other than the one sent.
	if err == nil && !jsoninput.Valid(content) {
		writeAPIError(writer, http.StatusBadRequest, "invalid_json", "The request body is not valid UTF-8 text, which JSON requires.", nil)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err == nil {
		err = decoder.Decode(destination)
	}
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_json", "The request body must contain one valid JSON object with known fields.", nil)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(writer, http.StatusBadRequest, "invalid_json", "The request body must contain exactly one JSON object.", nil)
		return false
	}
	return true
}

// refusePreparingAPI answers a request for a repository that is still being
// prepared after startup with a fixed 503 and reports whether it did. Call it
// after authorization, so the answer does not reveal the repository to an
// unauthenticated caller.
func (app *App) refusePreparingAPI(writer http.ResponseWriter, request *http.Request, repositoryID string) bool {
	if !app.Repositories.Preparing(repositoryID) {
		return false
	}
	writer.Header().Set("Retry-After", "30")
	writeAPIError(writer, unavailable(request, "repository read", repository.ErrRepositoryPreparing), "repository_preparing", "The repository is being prepared. Try again later.", nil)
	return true
}

func writeAPIMethodError(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "The HTTP method is not allowed for this API endpoint.", nil)
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string, details any) {
	description := pullrequest.ErrorDescription{Code: code, Message: message}
	if details != nil {
		if encoded, err := json.Marshal(details); err == nil {
			description.Details = encoded
		}
	}
	writeAPIJSON(writer, status, pullrequest.ErrorEnvelope{OK: false, Error: description})
}

func writeAPIJSON(writer http.ResponseWriter, status int, value any) {
	status, encoded := encodeAPIJSON(status, value)
	writeEncodedAPIJSON(writer, status, encoded)
}

// encodeAPIJSON returns the status and body writeAPIJSON sends for value.
func encodeAPIJSON(status int, value any) (int, []byte) {
	return encodeAPIJSONLimit(status, value, maximumAPIResponse)
}

func encodeAPIJSONLimit(status int, value any, limit int) (int, []byte) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(true)
	err := encoder.Encode(value)
	encoded := bidi.EscapeJSON(output.Bytes())
	if err != nil || len(encoded) > limit {
		output.Reset()
		_ = json.NewEncoder(&output).Encode(pullrequest.ErrorEnvelope{
			OK: false, Error: pullrequest.ErrorDescription{Code: "response_too_large", Message: "The API response exceeds the supported size."},
		})
		encoded = output.Bytes()
		status = http.StatusInsufficientStorage
	}
	return status, encoded
}

// writeEncodedAPIJSON sends a body encodeAPIJSON made.
func writeEncodedAPIJSON(writer http.ResponseWriter, status int, encoded []byte) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

// apiStatus is the status that answers err by its pull request problem
// code, for the API and the pages alike. Records that contradict each other
// are a fault retrying does not fix, answered as internal; a problem OwnGit
// did not classify is a Git or storage run that could not be completed now,
// answered as unavailable. Both are logged as step of request.
func apiStatus(request *http.Request, step string, err error) int {
	switch pullrequest.AsProblem(err).Code {
	case "invalid_repository", "invalid_pull_request_number", "invalid_title", "invalid_body", "invalid_note", "invalid_edit", "invalid_edit_revision", "invalid_branch", "ambiguous_branch", "reserved_ref", "same_branch", "invalid_review_choice", "invalid_review_decision", "invalid_reviewer_label", "invalid_revision",
		"invalid_task", "invalid_credential", "invalid_attempt", "invalid_attempt_id", "invalid_job_id", "invalid_check_definition", "invalid_worktree_state", "invalid_revision_oid", "invalid_cycle_id", "revision_not_recorded":
		return http.StatusUnprocessableEntity
	case "invalid_list_state", "invalid_list_limit", "invalid_list_before":
		return http.StatusBadRequest
	case "repository_not_found", "pull_request_not_found", "task_not_found", "configuration_not_found", "attempt_not_found", "log_not_recorded", "cycle_not_found":
		return http.StatusNotFound
	case "stale_revision", "stale_edit", "merge_conflict", "merge_blocked", "pull_request_not_open", "pull_request_exists", "pull_request_merged", "git_update_failed",
		"source_branch_missing", "target_branch_missing", "source_not_commit", "target_not_commit", "credential_not_found", "attempt_conflict", "cycle_conflict", "correction_budget_exhausted":
		return http.StatusConflict
	case "helper_authentication_required", "invalid_helper_credential", "admin_authentication_required", "invalid_admin_credentials", "admin_password_required":
		return http.StatusUnauthorized
	case "helper_credential_scope", "csrf_required":
		return http.StatusForbidden
	case "log_expired":
		return http.StatusGone
	case "unsupported_git":
		return http.StatusNotImplemented
	case "state_unavailable", "repository_unavailable", "repository_preparing", "repository_busy", "merge_reconciliation_pending", "pull_request_creation_reconciliation_pending":
		return unavailable(request, step, err)
	case "storage_changed":
		logFailure(request, step, err)
		return http.StatusConflict
	case "repository_integrity_error":
		return internalError(request, step, err)
	default:
		return unavailable(request, step, err)
	}
}
