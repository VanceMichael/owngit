package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"owngit/internal/apiclient"
	"owngit/internal/auth"
	"owngit/internal/bidi"
	"owngit/internal/gitexec"
	"owngit/internal/githttp"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/server"
	"owngit/internal/state"
	"owngit/internal/testfixture"
)

func TestPRCommandsUseRemoteJSONAPIAndPrivatePasswordFile(t *testing.T) {
	type observedRequest struct {
		method string
		path   string
		body   map[string]any
	}
	observed := make(chan observedRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, password, ok := request.BasicAuth()
		if !ok || password != "shared-password" {
			t.Error("CLI request omitted the shared password")
		}
		var body map[string]any
		if request.Body != nil {
			content, _ := io.ReadAll(request.Body)
			if len(content) != 0 {
				if err := json.Unmarshal(content, &body); err != nil {
					t.Errorf("decode request body: %v", err)
				}
			}
		}
		observed <- observedRequest{method: request.Method, path: request.URL.Path, body: body}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("{\"ok\":true,\"pull_request\":{\"number\":1}}\n"))
	}))
	defer server.Close()
	passwordFile := filepath.Join(t.TempDir(), "password")
	noErr(t, os.WriteFile(passwordFile, []byte("shared-password\n"), 0o600))
	noErr(t, state.ProtectPrivatePath(passwordFile, false))
	remote := []string{"--server", server.URL, "--accept-insecure-http", "--repository", "project", "--password-file", passwordFile}
	tests := []struct {
		name   string
		args   []string
		method string
		path   string
		field  string
	}{
		{"create", append([]string{"create", "--title", "Feature", "--source", "feature", "--target", "main", "--review", "request"}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests", "title"},
		{"create without review", append([]string{"create", "--title", "Feature", "--source", "feature", "--target", "main"}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests", "title"},
		{"list", append([]string{"list"}, remote...), http.MethodGet, "/api/v1/repositories/project/pull-requests", ""},
		{"show", append([]string{"show", "--number", "1"}, remote...), http.MethodGet, "/api/v1/repositories/project/pull-requests/1", ""},
		{"review request", append([]string{"review", "request", "--number", "1", "--source-oid", strings.Repeat("a", 40), "--target-oid", strings.Repeat("b", 40)}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests/1/review/request", "source_oid"},
		{"review submit", append([]string{"review", "submit", "--number", "1", "--source-oid", strings.Repeat("a", 40), "--target-oid", strings.Repeat("b", 40), "--decision", "approved", "--reviewer", "existing-tool:test"}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests/1/review/submit", "reviewer_label"},
		{"review skip", append([]string{"review", "skip", "--number", "1", "--source-oid", strings.Repeat("a", 40), "--target-oid", strings.Repeat("b", 40)}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests/1/review/skip", "source_oid"},
		{"mergeability", append([]string{"mergeability", "--number", "1"}, remote...), http.MethodGet, "/api/v1/repositories/project/pull-requests/1/mergeability", ""},
		{"merge", append([]string{"merge", "--number", "1", "--source-oid", strings.Repeat("a", 40), "--target-oid", strings.Repeat("b", 40)}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests/1/merge", "source_oid"},
		{"auto-merge", append([]string{"auto-merge", "--number", "1"}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests/1/auto-merge", ""},
		{"auto-merge cancel", append([]string{"auto-merge", "--number", "1", "--cancel"}, remote...), http.MethodPost, "/api/v1/repositories/project/pull-requests/1/auto-merge/cancel", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := captureStdout(func() error { return prCommand(test.args) })
			noErr(t, err)
			if !strings.Contains(output, `"ok":true`) {
				t.Fatalf("command output=%q", output)
			}
			request := <-observed
			if request.method != test.method || request.path != test.path {
				t.Fatalf("request=%s %s, want %s %s", request.method, request.path, test.method, test.path)
			}
			if test.field != "" {
				if _, exists := request.body[test.field]; !exists {
					t.Fatalf("request body lacks %q: %#v", test.field, request.body)
				}
			} else if len(request.body) != 0 {
				t.Fatalf("GET command sent a body: %#v", request.body)
			}
		})
	}
}

// prCLIFixture is an OwnGit server in open mode with a repository named
// project, whose main branch has one commit and whose feature branch has one
// more.
type prCLIFixture struct {
	root, stateRoot string
	store           *state.Store
	manager         *repository.Manager
	server          *httptest.Server
	work            string
	sourceOID       string
	targetOID       string
	remoteFlags     []string
}

func newPRCLIFixture(t *testing.T) prCLIFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	repositoryRoot := filepath.Join(root, "repositories")
	noErr(t, os.Mkdir(repositoryRoot, 0o700))
	store, err := state.Open(ctx, stateRoot)
	noErr(t, err)
	t.Cleanup(func() { store.Close() })
	adminHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	noErr(t, store.CompleteSetup(ctx, repositoryRoot, "open", "", adminHash, true))
	runner, err := gitexec.New("", filepath.Join(stateRoot, "runtime"))
	noErr(t, err)
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoryRoot}
	if _, err := manager.Create(ctx, "project", "CLI integration fixture"); err != nil {
		t.Fatal(err)
	}
	gitHandler, err := githttp.New(runner, manager, "")
	noErr(t, err)
	authentication := &auth.Manager{Store: store}
	hosts := server.NewHostPolicy()
	application := &server.App{
		Store: store, Auth: authentication, Repositories: manager,
		PullRequests: &pullrequest.Service{Store: store, Repositories: manager},
		GitHTTP:      gitHandler, Hosts: hosts,
		Network: server.NewLiveNetwork(server.LiveNetworkConfig{Hosts: hosts}),
	}
	gitHandler.Authorize = application.AuthorizeGit
	httpServer := httptest.NewServer(application.Handler())
	t.Cleanup(httpServer.Close)

	work := filepath.Join(root, "work")
	runPRGit(t, "", "init", "--initial-branch=main", work)
	runPRGit(t, work, "config", "user.name", "CLI Test")
	runPRGit(t, work, "config", "user.email", "cli-test@example.invalid")
	noErr(t, os.WriteFile(filepath.Join(work, "base.txt"), []byte("base\n"), 0o600))
	runPRGit(t, work, "add", ".")
	runPRGit(t, work, "commit", "-m", "base")
	runPRGit(t, work, "remote", "add", "origin", httpServer.URL+"/git/project.git")
	runPRGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	targetOID := prGitOutput(t, work, "rev-parse", "HEAD")
	runPRGit(t, work, "checkout", "-b", "feature")
	noErr(t, os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature\n"), 0o600))
	runPRGit(t, work, "add", ".")
	runPRGit(t, work, "commit", "-m", "feature")
	runPRGit(t, work, "push", "origin", "HEAD:refs/heads/feature")
	return prCLIFixture{
		root: root, stateRoot: stateRoot, store: store, manager: manager, server: httpServer, work: work,
		sourceOID: prGitOutput(t, work, "rev-parse", "HEAD"), targetOID: targetOID,
		remoteFlags: []string{"--server", httpServer.URL, "--accept-insecure-http", "--repository", "project"},
	}
}

func TestPRCLIEndToEndKeepsPushIndependentAndMergesExactRevisions(t *testing.T) {
	ctx := context.Background()
	fixture := newPRCLIFixture(t)
	store, manager, sourceOID, targetOID := fixture.store, fixture.manager, fixture.sourceOID, fixture.targetOID
	if records, _, err := store.PullRequestSummaries(ctx, "project", "", 0, 1000); err != nil || len(records) != 0 {
		t.Fatalf("ordinary push created pull requests: records=%v err=%v", records, err)
	}

	remoteFlags := fixture.remoteFlags
	created := runPRCommandJSON(t, append([]string{
		"create", "--title", "CLI feature", "--source", "feature", "--target", "main", "--review", "request",
	}, remoteFlags...))
	if created.PullRequest == nil || created.PullRequest.Source.OID != sourceOID || created.PullRequest.Target.OID != targetOID {
		t.Fatalf("created pull request=%+v", created.PullRequest)
	}
	number := strconv.FormatInt(created.PullRequest.Number, 10)
	listed := runPRCommandJSON(t, append([]string{"list"}, remoteFlags...))
	if len(listed.Items) != 1 {
		t.Fatalf("CLI list returned %d pull requests", len(listed.Items))
	}
	shown := runPRCommandJSON(t, append([]string{"show", "--number", number}, remoteFlags...))
	if shown.PullRequest == nil || shown.PullRequest.Number != created.PullRequest.Number {
		t.Fatalf("CLI show result=%+v", shown.PullRequest)
	}
	runPRCommandJSON(t, append([]string{
		"review", "request", "--number", number, "--source-oid", sourceOID, "--target-oid", targetOID,
	}, remoteFlags...))
	changed := runPRCommandJSON(t, append([]string{
		"review", "submit", "--number", number, "--source-oid", sourceOID, "--target-oid", targetOID,
		"--decision", "changes_requested", "--reviewer", "existing-tool: cli-test",
	}, remoteFlags...))
	if changed.PullRequest == nil || !changed.PullRequest.MergeEligibility.Eligible || len(changed.PullRequest.MergeEligibility.Blockers) != 0 {
		t.Fatal("advisory changes_requested blocked the CLI pull request")
	}
	runPRCommandJSON(t, append([]string{
		"review", "skip", "--number", number, "--source-oid", sourceOID, "--target-oid", targetOID,
	}, remoteFlags...))
	closed := runPRCommandJSON(t, append([]string{"close", "--number", number}, remoteFlags...))
	if closed.PullRequest == nil || closed.PullRequest.State != state.PullRequestClosed {
		t.Fatalf("CLI close result=%+v", closed.PullRequest)
	}
	reopened := runPRCommandJSON(t, append([]string{"reopen", "--number", number}, remoteFlags...))
	if reopened.PullRequest == nil || reopened.PullRequest.State != state.PullRequestOpen {
		t.Fatalf("CLI reopen result=%+v", reopened.PullRequest)
	}
	merged := runPRCommandJSON(t, append([]string{
		"merge", "--number", number, "--source-oid", sourceOID, "--target-oid", targetOID,
	}, remoteFlags...))
	if merged.PullRequest == nil || merged.PullRequest.Merge == nil || merged.PullRequest.Merge.OID != sourceOID {
		t.Fatalf("CLI merge result=%+v", merged.PullRequest)
	}
	repositoryPath, _ := manager.Path("project")
	if got := prGitOutput(t, "", "--git-dir", repositoryPath, "rev-parse", "--verify", "refs/heads/main"); got != sourceOID {
		t.Fatalf("CLI merge target=%s, want %s", got, sourceOID)
	}
	err := prCommand(append([]string{"close", "--number", number}, remoteFlags...))
	if got := commandErrorCode(err); got != "pull_request_merged" {
		t.Fatalf("CLI close of a merged pull request error=%v code=%q", err, got)
	}
}

func TestPRCommandValidatesServerBeforeReadingPasswordFile(t *testing.T) {
	missingPassword := filepath.Join(t.TempDir(), "missing-password")
	err := prCommand([]string{
		"list", "--server", "http://user:secret@example.test", "--accept-insecure-http",
		"--repository", "project", "--password-file", missingPassword,
	})
	if got := commandErrorCode(err); got != "invalid_server" {
		t.Fatalf("credential-bearing server error=%v code=%q, want invalid_server", err, got)
	}
	err = prCommand([]string{
		"list", "--server", "http://example.test", "--repository", "project", "--password-file", missingPassword,
	})
	if got := commandErrorCode(err); got != "insecure_http_confirmation_required" {
		t.Fatalf("unconfirmed HTTP error=%v code=%q, want insecure_http_confirmation_required", err, got)
	}
}

func runPRCommandJSON(t *testing.T, arguments []string) pullrequest.SuccessEnvelope {
	t.Helper()
	output, err := captureStdout(func() error { return prCommand(arguments) })
	noErr(t, err)
	if strings.IndexFunc(output, bidi.Control) >= 0 {
		t.Fatalf("CLI JSON holds a direction control as it is: %q", output)
	}
	var envelope pullrequest.SuccessEnvelope
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode CLI JSON output: %v\n%s", err, output)
	}
	if !envelope.OK {
		t.Fatalf("CLI result reported ok=false: %s", output)
	}
	return envelope
}

func runPRGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = testfixture.GitEnvironment(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func prGitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = testfixture.GitEnvironment(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(arguments, " "), err, output, stderr.Bytes())
	}
	return strings.TrimSpace(string(output))
}

func commandErrorCode(err error) string {
	var problem *apiclient.Error
	if errors.As(err, &problem) {
		return problem.Code
	}
	return ""
}

func TestStructuredPRFailureContainsStableCodeWithoutCause(t *testing.T) {
	problem := &apiclient.Error{Code: "invalid_credentials", Message: "The password is invalid.", Cause: errors.New("synthetic secret detail")}
	var output bytes.Buffer
	if !writeStructuredCommandError(&output, problem) {
		t.Fatal("coded PR error was not handled")
	}
	if strings.Contains(output.String(), "synthetic secret detail") {
		t.Fatalf("structured error leaked its internal cause: %s", output.String())
	}
	var decoded struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	noErr(t, json.Unmarshal(output.Bytes(), &decoded))
	if decoded.OK || decoded.Error.Code != "invalid_credentials" {
		t.Fatalf("structured error=%+v", decoded)
	}
}

// The command line escapes direction controls in the JSON it prints, also in
// an answer from a server that did not, and in its error objects, so they
// cannot reorder the fields around a title in a terminal.
func TestCLIJSONEscapesDirectionControls(t *testing.T) {
	printed, err := captureStdout(func() error { return writeJSON([]byte("{\"title\":\"a\u202eb\u2066c\"}")) })
	noErr(t, err)
	if printed != `{"title":"a\u202eb\u2066c"}`+"\n" {
		t.Fatalf("printed %q", printed)
	}
	// Indented results, as owngit update, network and tailscale print them,
	// go through the same writer and keep a command's & and > readable.
	printed, err = captureStdout(func() error { return printJSON(struct{ Command string }{"a\u202eb && c > d"}) })
	noErr(t, err)
	if printed != "{\n  \"Command\": \"a\\u202eb && c > d\"\n}\n" {
		t.Fatalf("printed %q", printed)
	}
	var output bytes.Buffer
	writeStructuredCommandError(&output, cliProblem("invalid_title", "Refused \u202etitle"))
	if strings.IndexFunc(output.String(), bidi.Control) >= 0 || !strings.Contains(output.String(), `Refused \u202etitle`) {
		t.Fatalf("error object %q", output.String())
	}
}

// Text over its limit is refused with the server's code before anything is
// sent, whatever its characters. 70 KiB of "<" grows six times as JSON, so
// without the check it would be refused as too large a request instead, and a
// file over twice the limit is not read to its end.
func TestPRTextIsCheckedBeforeSending(t *testing.T) {
	fixture := newPRCLIFixture(t)
	write := func(name, content string) string {
		path := filepath.Join(fixture.root, name)
		noErr(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	escaping := write("escaping.md", strings.Repeat("<", 70<<10))
	long := write("long.md", strings.Repeat("a\r\n", state.MaximumPullRequestTextBytes+1))
	create := []string{"create", "--title", "Described", "--source", "feature", "--target", "main"}
	for _, test := range []struct {
		arguments []string
		code      string
	}{
		{append(create, "--body-file", escaping), "invalid_body"},
		{append(create, "--body-file", long), "invalid_body"},
		{[]string{"edit", "--number", "1", "--edit-revision", "0", "--body-file", escaping}, "invalid_body"},
		{[]string{"edit", "--number", "1", "--edit-revision", "0", "--title", strings.Repeat("<", 70<<10)}, "invalid_title"},
		{[]string{
			"review", "submit", "--number", "1", "--source-oid", fixture.sourceOID, "--target-oid", fixture.targetOID,
			"--decision", "approved", "--reviewer", "cli reviewer", "--note-file", escaping,
		}, "invalid_note"},
	} {
		err := prCommand(append(test.arguments, fixture.remoteFlags...))
		if got := commandErrorCode(err); got != test.code {
			t.Fatalf("%s: error=%v code=%q, want %s", test.arguments[:2], err, got, test.code)
		}
	}
	if records, _, err := fixture.store.PullRequestSummaries(context.Background(), "project", "", 0, 1000); err != nil || len(records) != 0 {
		t.Fatalf("a refused text created records=%v err=%v", records, err)
	}
}

// Text that is not UTF-8 is refused with its field's code, as the page
// refuses it, before anything is sent: from a file, from standard input and
// in a flag. JSON encoding would otherwise send U+FFFD in its place. Text in
// other scripts is sent exactly.
func TestPRTextThatIsNotUTF8IsRefused(t *testing.T) {
	fixture := newPRCLIFixture(t)
	invalid := filepath.Join(fixture.root, "invalid.md")
	noErr(t, os.WriteFile(invalid, []byte("a\xffb"), 0o600))
	stdin := func(content string) {
		path := filepath.Join(t.TempDir(), "stdin")
		noErr(t, os.WriteFile(path, []byte(content), 0o600))
		file, err := os.Open(path)
		noErr(t, err)
		previous := os.Stdin
		os.Stdin = file
		t.Cleanup(func() {
			os.Stdin = previous
			file.Close()
		})
	}
	create := func(arguments ...string) []string {
		return append(append([]string{"create", "--source", "feature", "--target", "main"}, arguments...), fixture.remoteFlags...)
	}
	review := append([]string{
		"review", "submit", "--number", "1", "--source-oid", fixture.sourceOID, "--target-oid", fixture.targetOID,
		"--decision", "approved", "--reviewer", "cli reviewer", "--note-file", invalid,
	}, fixture.remoteFlags...)
	for _, test := range []struct {
		name, stdin, code string
		arguments         []string
	}{
		{"file", "", "invalid_body", create("--title", "Described", "--body-file", invalid)},
		{"standard input", "a\xffb", "invalid_body", create("--title", "Described", "--body-file", "-")},
		{"flag", "", "invalid_title", create("--title", "a\xffb")},
		{"note", "", "invalid_note", review},
	} {
		if test.stdin != "" {
			stdin(test.stdin)
		}
		if got := commandErrorCode(prCommand(test.arguments)); got != test.code {
			t.Fatalf("%s: code=%q, want %s", test.name, got, test.code)
		}
	}
	if records, _, err := fixture.store.PullRequestSummaries(context.Background(), "project", "", 0, 1000); err != nil || len(records) != 0 {
		t.Fatalf("refused text created records=%v err=%v", records, err)
	}
	const text = "한글 שלום مرحبا 🙂\n"
	stdin(text)
	created := runPRCommandJSON(t, create("--title", "한글 제목", "--body-file", "-")).PullRequest
	if created.Title != "한글 제목" || created.Body == nil || *created.Body != text {
		t.Fatalf("multibyte text: title %q body %v", created.Title, created.Body)
	}
}

// Pull request text made with the command line, its edits, a review note and
// who made each change are in an offline backup and come back unchanged from
// a restore. A stale edit is refused on the way. The final description and
// the note are as long as allowed, in control characters that the backup's
// JSON writes as six bytes each, so the backup holds the longest text a pull
// request can have.
func TestPRTextSurvivesBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	fixture := newPRCLIFixture(t)
	write := func(name, content string) string {
		path := filepath.Join(fixture.root, name)
		noErr(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	created := runPRCommandJSON(t, append([]string{
		"create", "--title", "Described \u2067\u05e9\u2069", "--source", "feature", "--target", "main",
		"--body-file", write("body.md", "## Why\r\n\r\nFirst draft.\r\n"),
	}, fixture.remoteFlags...)).PullRequest
	if created.Body == nil || *created.Body != "## Why\n\nFirst draft.\n" || created.EditRevision != 0 {
		t.Fatalf("created=%+v", created)
	}
	number := strconv.FormatInt(created.Number, 10)
	longest := strings.Repeat("\x01", state.MaximumPullRequestTextBytes)
	edited := runPRCommandJSON(t, append([]string{
		"edit", "--number", number, "--edit-revision", "0", "--body-file", write("edit.md", longest),
	}, fixture.remoteFlags...)).PullRequest
	if edited.Title != "Described \u2067\u05e9\u2069" || *edited.Body != longest || edited.EditRevision != 1 {
		t.Fatalf("edited title %q, body of %d bytes, revision %d", edited.Title, len(*edited.Body), edited.EditRevision)
	}
	err := prCommand(append([]string{"edit", "--number", number, "--edit-revision", "0", "--title", "Stale"}, fixture.remoteFlags...))
	if got := commandErrorCode(err); got != "stale_edit" {
		t.Fatalf("stale edit error=%v code=%q", err, got)
	}
	// An empty --body-file path is refused rather than clearing the text.
	err = prCommand(append([]string{"edit", "--number", number, "--edit-revision", "1", "--body-file", ""}, fixture.remoteFlags...))
	if got := commandErrorCode(err); got != "invalid_arguments" {
		t.Fatalf("empty --body-file error=%v code=%q", err, got)
	}
	reviewed := runPRCommandJSON(t, append([]string{
		"review", "submit", "--number", number, "--source-oid", fixture.sourceOID, "--target-oid", fixture.targetOID,
		"--decision", "approved", "--reviewer", "cli reviewer", "--note-file", write("note.md", strings.Repeat("\x1f", state.MaximumPullRequestTextBytes)),
	}, fixture.remoteFlags...)).PullRequest
	if len(reviewed.ReviewNotes) != 1 || reviewed.ReviewNotes[0].Note != strings.Repeat("\x1f", state.MaximumPullRequestTextBytes) {
		t.Fatalf("review notes: %d", len(reviewed.ReviewNotes))
	}
	runPRCommandJSON(t, append([]string{
		"merge", "--number", number, "--source-oid", fixture.sourceOID, "--target-oid", fixture.targetOID,
	}, fixture.remoteFlags...))

	before, _, err := fixture.store.PullRequest(ctx, "project", created.Number)
	noErr(t, err)
	notesBefore, _, err := fixture.store.PullRequestReviewNotes(ctx, "project", created.Number, 10)
	noErr(t, err)
	access := state.Actor{Kind: state.ActorAccess}
	if before.CreatedBy != access || before.EditedBy != access || before.MergedBy != access || len(notesBefore) != 1 || notesBefore[0].Actor != access {
		t.Fatalf("recorded actors: %+v, notes %+v", before, notesBefore)
	}
	fixture.server.Close()
	noErr(t, fixture.store.Close())
	backup := filepath.Join(fixture.root, "backup")
	_, err = captureStdout(func() error { return backupState([]string{"--state-dir", fixture.stateRoot, "--output", backup}) })
	noErr(t, err)
	restoredState, restoredRepositories := filepath.Join(fixture.root, "restored-state"), filepath.Join(fixture.root, "restored-repositories")
	_, err = captureStdout(func() error {
		return restoreState([]string{"--input", backup, "--state-dir", restoredState, "--repository-root", restoredRepositories})
	})
	noErr(t, err)

	restored, err := state.Open(ctx, restoredState)
	noErr(t, err)
	defer restored.Close()
	after, _, err := restored.PullRequest(ctx, "project", created.Number)
	noErr(t, err)
	notesAfter, _, err := restored.PullRequestReviewNotes(ctx, "project", created.Number, 10)
	noErr(t, err)
	if before.Body != longest || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(notesBefore, notesAfter) {
		t.Fatalf("pull request text changed through backup and restore: body %d to %d bytes, notes %d to %d",
			len(before.Body), len(after.Body), len(notesBefore), len(notesAfter))
	}
}
