package server

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func prNumber(t *testing.T, response *http.Response) int64 {
	t.Helper()
	number := decodeAPISuccess(t, response).PullRequest.Number
	if number <= 0 {
		t.Fatal("response carried no pull request number")
	}
	return number
}

func createPullRequestAt(t *testing.T, base string) int64 {
	t.Helper()
	response := apiRequest(t, http.MethodPost, base+"/pull-requests", map[string]any{
		"title": "Auto", "source_branch": "feature", "target_branch": "main", "review": "skip",
	}, "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create status=%d error=%q", response.StatusCode, apiErrorCode(t, response))
	}
	return prNumber(t, response)
}

func autoMergeRoute(base string, number int64, action string) string {
	route := base + "/pull-requests/" + strconv.FormatInt(number, 10) + "/auto-merge"
	if action != "" {
		route += "/" + action
	}
	return route
}

func TestAutoMergeAPIEnableShowCancel(t *testing.T) {
	fixture := newAPIFixture(t, false)
	server := serve(t, fixture.app.Handler())
	base := server.URL + "/api/v1/repositories/project"
	number := createPullRequestAt(t, base)

	enable := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, ""), map[string]any{}, "", "")
	if enable.StatusCode != http.StatusOK {
		t.Fatalf("enable status=%d error=%q", enable.StatusCode, apiErrorCode(t, enable))
	}
	enabled := decodeAPISuccess(t, enable).PullRequest
	if enabled.AutoMerge == nil || !enabled.AutoMerge.Enabled || enabled.AutoMerge.Reason != "waiting_for_checks" {
		t.Fatalf("enable response=%+v", enabled.AutoMerge)
	}

	// GET is not a mutation.
	methodNotAllowed := apiRequest(t, http.MethodGet, autoMergeRoute(base, number, ""), nil, "", "")
	if methodNotAllowed.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET auto-merge status=%d", methodNotAllowed.StatusCode)
	}

	shownResponse := apiRequest(t, http.MethodGet, base+"/pull-requests/"+strconv.FormatInt(number, 10), nil, "", "")
	if shownResponse.StatusCode != http.StatusOK {
		t.Fatalf("show status=%d", shownResponse.StatusCode)
	}
	shown := decodeAPISuccess(t, shownResponse).PullRequest
	if shown.AutoMerge == nil || !shown.AutoMerge.Enabled || shown.AutoMerge.SourceOID != fixture.sourceOID || shown.AutoMerge.TargetOID != fixture.targetOID {
		t.Fatalf("show auto-merge=%+v", shown.AutoMerge)
	}

	listed := apiRequest(t, http.MethodGet, base+"/pull-requests", nil, "", "")
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", listed.StatusCode)
	}
	page := decodeAPISuccess(t, listed)
	if len(page.Items) != 1 || page.Items[0].AutoMerge == nil || !page.Items[0].AutoMerge.Enabled {
		t.Fatalf("list auto-merge=%+v", page.Items)
	}

	// Enabling again is idempotent.
	again := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, ""), map[string]any{}, "", "")
	if again.StatusCode != http.StatusOK {
		t.Fatalf("second enable status=%d error=%q", again.StatusCode, apiErrorCode(t, again))
	}

	cancel := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, "cancel"), map[string]any{}, "", "")
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("cancel status=%d error=%q", cancel.StatusCode, apiErrorCode(t, cancel))
	}
	if decodeAPISuccess(t, cancel).PullRequest.AutoMerge != nil {
		t.Fatal("cancel response still carried the intent")
	}
	missing := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, "cancel"), map[string]any{}, "", "")
	if missing.StatusCode != http.StatusConflict || apiErrorCode(t, missing) != "auto_merge_not_enabled" {
		t.Fatalf("second cancel status=%d error=%q", missing.StatusCode, apiErrorCode(t, missing))
	}

	unknown := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, "bogus"), map[string]any{}, "", "")
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown auto-merge route status=%d", unknown.StatusCode)
	}
}

func TestAutoMergeAPIRejectsNonOpenAndNonJSON(t *testing.T) {
	fixture := newAPIFixture(t, false)
	server := serve(t, fixture.app.Handler())
	base := server.URL + "/api/v1/repositories/project"
	number := createPullRequestAt(t, base)

	closeResponse := apiRequest(t, http.MethodPost, base+"/pull-requests/"+strconv.FormatInt(number, 10)+"/close", map[string]any{}, "", "")
	if closeResponse.StatusCode != http.StatusOK {
		t.Fatalf("close status=%d", closeResponse.StatusCode)
	}
	enable := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, ""), map[string]any{}, "", "")
	if enable.StatusCode != http.StatusConflict || apiErrorCode(t, enable) != "pull_request_not_open" {
		t.Fatalf("enable on closed status=%d error=%q", enable.StatusCode, apiErrorCode(t, enable))
	}

	missing := apiRequest(t, http.MethodPost, autoMergeRoute(base, 99, ""), map[string]any{}, "", "")
	if missing.StatusCode != http.StatusNotFound || apiErrorCode(t, missing) != "pull_request_not_found" {
		t.Fatalf("enable missing status=%d error=%q", missing.StatusCode, apiErrorCode(t, missing))
	}

	// Reopen, then refuse a non-JSON body like every mutating endpoint.
	reopened := apiRequest(t, http.MethodPost, base+"/pull-requests/"+strconv.FormatInt(number, 10)+"/reopen", map[string]any{}, "", "")
	if reopened.StatusCode != http.StatusOK {
		t.Fatalf("reopen status=%d", reopened.StatusCode)
	}
	response := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, ""), nil, "", "")
	if response.StatusCode != http.StatusUnsupportedMediaType || apiErrorCode(t, response) != "json_required" {
		t.Fatalf("enable without JSON status=%d error=%q", response.StatusCode, apiErrorCode(t, response))
	}
}

// A helper's terminal check completion signals the auto-merge wake hook,
// which the serving process wires to its convergence loop.
func TestCheckCompletionWakesAutoMerge(t *testing.T) {
	fixture := newAPIFixture(t, false)
	base, token := helperAPI(t, fixture, "laptop", time.Now())
	number := createPullRequestAt(t, base)
	enable := apiRequest(t, http.MethodPost, autoMergeRoute(base, number, ""), map[string]any{}, "", "")
	if enable.StatusCode != http.StatusOK {
		t.Fatalf("enable status=%d", enable.StatusCode)
	}

	woken := make(chan string, 4)
	fixture.app.OnCheckCompletion = func(repositoryID string) {
		woken <- repositoryID
	}
	taskID := createCheckTask(t, base, token, "Check the change")
	body := attemptUploadBodyWithID(fixture.sourceOID, "clean", "passed", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	response := recordAttempt(t, base, taskID, token, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("completed attempt status=%d error=%q", response.StatusCode, apiErrorCode(t, response))
	}
	select {
	case repositoryID := <-woken:
		if repositoryID != "project" {
			t.Fatalf("woken repository=%q", repositoryID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the check completion did not wake auto-merge convergence")
	}
}
