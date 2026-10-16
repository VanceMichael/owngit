package server

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"

	"owngit/internal/pullrequest"
	"owngit/internal/state"
)

// TestUIAutoMergeSmokeServer is a manual harness (skipped by default). Run it
// with OWNGIT_UI_SMOKE=1 to inspect the automatic merge panel in a browser; it
// prints the address and blocks until killed.
func TestUIAutoMergeSmokeServer(t *testing.T) {
	if os.Getenv("OWNGIT_UI_SMOKE") == "" {
		t.Skip("manual browser smoke harness; set OWNGIT_UI_SMOKE=1")
	}
	fixture := newAPIFixture(t, false)
	ctx := context.Background()
	created, err := fixture.app.PullRequests.Create(ctx, pullrequest.CreateInput{
		Repository: "project", Title: "Wait for checks", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.PullRequests.EnableAutoMerge(ctx, "project", created.Number, state.Actor{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture.app.Handler())
	t.Cleanup(server.Close)
	page := server.URL + "/repositories/project/pull-requests/" + itoa(created.Number)
	t.Logf("SMOKE_URL=%s", page)
	select {}
}
