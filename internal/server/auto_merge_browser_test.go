package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"owngit/internal/pullrequest"
	"owngit/internal/state"
	"owngit/internal/webui"
)

func enableRoute(number int64) string {
	return "/repositories/project/pull-requests/" + itoa(number) + "/auto-merge"
}

func TestAutoMergeBrowserPanelFormsAndNotices(t *testing.T) {
	fixture := newAPIFixture(t, false)
	server, client, jar := openBrowser(t, fixture)
	created, err := fixture.app.PullRequests.Create(context.Background(), pullrequest.CreateInput{
		Repository: "project", Title: "Auto", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	number := created.Number
	csrf := url.Values{"csrf": {cookieValue(t, jar, server.URL, generalCookie)}}
	route := enableRoute(number)

	// Before enabling, the page offers the opt-in and no waiting state.
	page := browserGET(t, client, server.URL+"/repositories/project/pull-requests/"+itoa(number))
	if !strings.Contains(page.body, `action="`+route+`"`) {
		t.Fatal("page does not offer enabling automatic merge")
	}
	if strings.Contains(page.body, route+"/cancel") {
		t.Fatal("page offers cancel before enabling")
	}

	// CSRF is required like every mutating browser form.
	if forged := browserForm(t, client, server.URL+route, url.Values{}, server.URL); forged.status != http.StatusForbidden {
		t.Fatalf("enable without CSRF status=%d", forged.status)
	}
	enabled := browserForm(t, client, server.URL+route, csrf, server.URL)
	if enabled.status != http.StatusSeeOther || enabled.header.Get("Location") != "/repositories/project/pull-requests/"+itoa(number)+"?notice=auto_merge_enabled" {
		t.Fatalf("enable redirect status=%d location=%q", enabled.status, enabled.header.Get("Location"))
	}

	listed := browserGET(t, client, server.URL+"/repositories/project/pull-requests?state=open")
	if !strings.Contains(listed.body, webui.Text(webui.LangEN, webui.MsgAutoMergeListBadge)) {
		t.Fatal("the list does not mark the automatic merge")
	}

	for _, lang := range []webui.Lang{webui.LangEN, webui.LangKO} {
		shown := browserGET(t, client, server.URL+enabled.header.Get("Location")+"&lang="+string(lang))
		if !strings.Contains(shown.body, `action="`+route+"/cancel\"") {
			t.Fatalf("%s: enabled page lacks the cancel form", lang)
		}
		for _, code := range []webui.MessageCode{webui.MsgAutoMergeTitle, webui.MsgAutoMergeCancelButton, webui.MsgAutoMergeWaitingChecks} {
			if !strings.Contains(shown.body, webui.Text(lang, code)) {
				t.Fatalf("%s: enabled page lacks %q", lang, webui.Text(lang, code))
			}
		}
	}

	canceled := browserForm(t, client, server.URL+route+"/cancel", csrf, server.URL)
	if canceled.status != http.StatusSeeOther || canceled.header.Get("Location") != "/repositories/project/pull-requests/"+itoa(number)+"?notice=auto_merge_canceled" {
		t.Fatalf("cancel redirect status=%d location=%q", canceled.status, canceled.header.Get("Location"))
	}
	again := browserGET(t, client, server.URL+canceled.header.Get("Location"))
	if !strings.Contains(again.body, `action="`+route+`"`) || strings.Contains(again.body, route+"/cancel") {
		t.Fatal("the canceled page does not offer enabling again")
	}
}

// Once the background loop merged an enabled request, the page no longer
// shows the panel or the result notice out of context.
func TestAutoMergeBrowserPageAfterBackgroundMerge(t *testing.T) {
	fixture := newAPIFixture(t, false)
	server, client, _ := openBrowser(t, fixture)
	created, err := fixture.app.PullRequests.Create(context.Background(), pullrequest.CreateInput{
		Repository: "project", Title: "Auto", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	_, err = fixture.app.PullRequests.EnableAutoMerge(context.Background(), "project", created.Number, state.Actor{})
	noErr(t, err)
	recordPassedCheckInStore(t, fixture, created.Source.OID)
	merged, _, err := fixture.app.PullRequests.ConvergeAutoMerge(context.Background(), "project", created.Number)
	noErr(t, err)
	if !merged {
		t.Fatal("convergence did not merge")
	}

	page := browserGET(t, client, server.URL+"/repositories/project/pull-requests/"+itoa(created.Number))
	if page.status != http.StatusOK {
		t.Fatalf("merged page status=%d", page.status)
	}
	if strings.Contains(page.body, enableRoute(created.Number)) {
		t.Fatal("the merged page still offers automatic merge")
	}
	if !strings.Contains(page.body, webui.Text(webui.LangEN, webui.MsgPRStateMerged)) {
		t.Fatal("the merged page does not show the merged state")
	}
}

// recordPassedCheckInStore writes a clean passing attempt for revision, the
// same rows a helper completion would.
func recordPassedCheckInStore(t *testing.T, fixture apiFixture, revision string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	task, err := fixture.store.CreateTask(ctx, "project", "Check", now.Add(-time.Second))
	noErr(t, err)
	const attemptID = "55555555555555555555555555555555"
	_, _, err = fixture.store.RegisterCheckAttempt(ctx, state.CheckAttempt{
		ID: attemptID, TaskID: task.ID, RepositoryID: "project", RevisionOID: revision,
		WorktreeState: state.WorktreeClean, StartedAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Second),
		Protection: state.ProtectionUnknown, ExecutionScope: state.ExecutionScopeInherited,
		Checks: []state.CheckDefinition{{Name: "unit", Command: "true"}},
	})
	noErr(t, err)
	exitCode := 0
	_, _, err = fixture.store.CompleteCheckAttempt(ctx, state.CheckCompletion{
		AttemptID: attemptID, RepositoryID: "project", TaskID: task.ID, FinishedAt: now, WorktreeState: state.WorktreeClean,
		Results: []state.CheckResult{{Name: "unit", Command: "true", Status: state.AttemptPassed, ExitCode: &exitCode, DurationMS: 1}},
	}, now)
	noErr(t, err)
}
