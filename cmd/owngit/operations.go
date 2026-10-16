package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"owngit/internal/apiclient"
	"owngit/internal/checkapi"
	"owngit/internal/pullrequest"
	"owngit/internal/state"
)

// This file holds the remote operations shared by the command line and the
// MCP server. Each takes a context and a resolved connection and returns the
// server's JSON result, or a structured *apiclient.Error. None of them reads
// flags or writes to stdout or stderr; the caller decides how to present the
// result.

// credentialKind says how a connection authenticates. The kind is implied by
// the flag that read the secret, so a later kind (such as a personal key) can
// be added without changing the operations.
type credentialKind string

const (
	credentialNone           credentialKind = "none"
	credentialSharedPassword credentialKind = "shared_password"
	credentialHelperToken    credentialKind = "helper_token"
)

type credential struct {
	kind   credentialKind
	secret string
}

// connection is a resolved target: a validated server origin, a repository
// identifier (empty for server-wide operations), and the credential sent to
// that origin.
type connection struct {
	server     *url.URL
	repository string
	credential credential
}

func (target connection) client() *apiclient.Client {
	switch target.credential.kind {
	case credentialHelperToken:
		client := apiclient.NewBearer(target.server, target.credential.secret)
		client.MaximumRequest = checkapi.MaximumUploadBytes
		return client
	case credentialSharedPassword:
		return apiclient.New(target.server, target.credential.secret)
	default:
		return apiclient.New(target.server, "")
	}
}

// textClient is the client for a request that carries pull request text,
// which may be larger than an ordinary request.
func (target connection) textClient() *apiclient.Client {
	client := target.client()
	client.MaximumRequest = pullrequest.MaximumTextRequestBytes
	return client
}

func (target connection) repositoryPath() string {
	return "/api/v1/repositories/" + url.PathEscape(target.repository)
}

func (target connection) pullRequestPath(number int64) string {
	return target.repositoryPath() + "/pull-requests/" + strconv.FormatInt(number, 10)
}

func (target connection) taskPath(taskID string) string {
	return target.repositoryPath() + "/tasks/" + url.PathEscape(taskID)
}

// Repository operations use general access. Listing and creating address the
// server, so they ignore the connection's repository.

const repositoryCollectionPath = "/api/v1/repositories"

func listRepositories(ctx context.Context, target connection) ([]byte, error) {
	return target.client().Do(ctx, http.MethodGet, repositoryCollectionPath, nil)
}

func showRepository(ctx context.Context, target connection) ([]byte, error) {
	if err := requireIdentifier(target.repository, "repository"); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodGet, target.repositoryPath(), nil)
}

// repositoryInput is the body of a repository creation request.
type repositoryInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func createRepository(ctx context.Context, target connection, input repositoryInput) ([]byte, error) {
	return target.client().Do(ctx, http.MethodPost, repositoryCollectionPath, input)
}

// Pull request operations. The server validates titles, branches, object IDs
// and review values; these functions refuse inputs that would address the
// wrong resource, and check text with the server's own rules before sending
// it (see textProblem).

// pullRequestListQuery selects a page of listPullRequests. The zero value is
// the server's first page of every state.
type pullRequestListQuery struct {
	State  string `json:"state"`
	Limit  int    `json:"limit"`
	Before int64  `json:"before"`
}

func listPullRequests(ctx context.Context, target connection, query pullRequestListQuery) ([]byte, error) {
	values := url.Values{}
	if query.State != "" {
		values.Set("state", query.State)
	}
	if query.Limit != 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Before != 0 {
		values.Set("before", strconv.FormatInt(query.Before, 10))
	}
	path := target.repositoryPath() + "/pull-requests"
	if len(values) != 0 {
		path += "?" + values.Encode()
	}
	return target.client().Do(ctx, http.MethodGet, path, nil)
}

func showPullRequest(ctx context.Context, target connection, number int64) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodGet, target.pullRequestPath(number), nil)
}

func createPullRequest(ctx context.Context, target connection, input pullrequest.CreateInput) ([]byte, error) {
	if _, err := input.CheckText(); err != nil {
		return nil, textProblem(err)
	}
	return target.textClient().Do(ctx, http.MethodPost, target.repositoryPath()+"/pull-requests", input)
}

// editPullRequest replaces the title, the description, or both, of the pull
// request still at the edit revision input names.
func editPullRequest(ctx context.Context, target connection, number int64, input pullrequest.EditInput) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	if _, err := input.CheckText(); err != nil {
		return nil, textProblem(err)
	}
	return target.textClient().Do(ctx, http.MethodPost, target.pullRequestPath(number)+"/edit", input)
}

// markPullRequestReview requests a review (action "request") or records that
// review was skipped (action "skip") for exact revisions.
func markPullRequestReview(ctx context.Context, target connection, number int64, action string, input pullrequest.RevisionInput) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	if action != "request" && action != "skip" {
		return nil, cliProblem("invalid_arguments", "The review action must be request or skip.")
	}
	return target.client().Do(ctx, http.MethodPost, target.pullRequestPath(number)+"/review/"+action, input)
}

// submitPullRequestReview records a review decision with a supplied reviewer
// label for exact revisions.
func submitPullRequestReview(ctx context.Context, target connection, number int64, input pullrequest.ReviewSubmitInput) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	if _, err := input.CheckText(); err != nil {
		return nil, textProblem(err)
	}
	return target.textClient().Do(ctx, http.MethodPost, target.pullRequestPath(number)+"/review/submit", input)
}

// textProblem reports the server's refusal of a text as the command line
// reports any problem. The text is checked before it is sent because JSON
// escaping can grow it up to six times: a text over its limit would otherwise
// be refused as too large a request, or not, depending on its characters.
func textProblem(err error) error {
	problem := pullrequest.AsProblem(err)
	return cliProblem(problem.Code, problem.Message)
}

func mergePullRequest(ctx context.Context, target connection, number int64, input pullrequest.RevisionInput) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodPost, target.pullRequestPath(number)+"/merge", input)
}

// pullRequestDiff reads what a pull request changes: its current pair, or the
// pair pinned names, which must be the current pair or one recorded for the
// pull request.
func pullRequestDiff(ctx context.Context, target connection, number int64, pinned pullrequest.RevisionInput) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	if (pinned.SourceOID == "") != (pinned.TargetOID == "") {
		return nil, cliProblem("invalid_arguments", "Pin both the source and the target object ID, or neither.")
	}
	path := target.pullRequestPath(number) + "/diff"
	if pinned.SourceOID != "" {
		path += "?" + url.Values{"source_oid": {pinned.SourceOID}, "target_oid": {pinned.TargetOID}}.Encode()
	}
	return target.client().Do(ctx, http.MethodGet, path, nil)
}

// pullRequestMergeability reads whether a pull request can merge now, for
// its current pair; with expected set, the answer is stale unless that pair
// is still current.
func pullRequestMergeability(ctx context.Context, target connection, number int64, expected pullrequest.RevisionInput) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	if (expected.SourceOID == "") != (expected.TargetOID == "") {
		return nil, cliProblem("invalid_arguments", "Give both the source and the target object ID, or neither.")
	}
	path := target.pullRequestPath(number) + "/mergeability"
	if expected.SourceOID != "" {
		path += "?" + url.Values{"source_oid": {expected.SourceOID}, "target_oid": {expected.TargetOID}}.Encode()
	}
	return target.client().Do(ctx, http.MethodGet, path, nil)
}

// setPullRequestClosed closes (closed true) or reopens a pull request. Neither
// changes a branch, so no object IDs are needed.
func setPullRequestClosed(ctx context.Context, target connection, number int64, closed bool) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	action := "/reopen"
	if closed {
		action = "/close"
	}
	return target.client().Do(ctx, http.MethodPost, target.pullRequestPath(number)+action, struct{}{})
}

// enablePullRequestAutoMerge opts an open pull request into automatic merging.
func enablePullRequestAutoMerge(ctx context.Context, target connection, number int64) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodPost, target.pullRequestPath(number)+"/auto-merge", struct{}{})
}

// cancelPullRequestAutoMerge removes an enabled automatic merge intent.
func cancelPullRequestAutoMerge(ctx context.Context, target connection, number int64) ([]byte, error) {
	if err := requirePullRequestNumber(number); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodPost, target.pullRequestPath(number)+"/auto-merge/cancel", struct{}{})
}

func requirePullRequestNumber(number int64) error {
	if number <= 0 {
		return cliProblem("invalid_arguments", "The pull request number must be positive.")
	}
	return nil
}

// Check task, cycle, and evidence operations. They authenticate with a
// repository-scoped helper credential.

func createTask(ctx context.Context, target connection, title string) ([]byte, error) {
	return target.client().Do(ctx, http.MethodPost, target.repositoryPath()+"/tasks", checkapi.CreateTaskInput{Title: title})
}

// taskListQuery selects a page of listTasks, newest first. The zero value is
// the server's first page.
type taskListQuery struct {
	Limit  int    `json:"limit"`
	Before string `json:"before"`
}

// listTasks lists one page of the repository's check tasks with their
// budgets, newest first.
func listTasks(ctx context.Context, target connection, query taskListQuery) ([]byte, error) {
	values := url.Values{}
	if query.Limit != 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Before != "" {
		values.Set("before", query.Before)
	}
	path := target.repositoryPath() + "/tasks"
	if len(values) != 0 {
		path += "?" + values.Encode()
	}
	return target.client().Do(ctx, http.MethodGet, path, nil)
}

func showTask(ctx context.Context, target connection, taskID string) ([]byte, error) {
	if err := requireIdentifier(taskID, "task"); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodGet, target.taskPath(taskID), nil)
}

// reserveCycle reserves one automatic correction round. The round identity is
// generated here, so a retried request cannot reserve a second round.
func reserveCycle(ctx context.Context, target connection, taskID string) ([]byte, error) {
	if err := requireIdentifier(taskID, "task"); err != nil {
		return nil, err
	}
	cycleID, err := state.RandomID()
	if err != nil {
		return nil, err
	}
	return postWithRetry(ctx, target.client(), target.taskPath(taskID)+"/cycles", checkapi.CreateCycleInput{CycleID: cycleID})
}

func listCycles(ctx context.Context, target connection, taskID string) ([]byte, error) {
	if err := requireIdentifier(taskID, "task"); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodGet, target.taskPath(taskID)+"/cycles", nil)
}

func readAttemptLog(ctx context.Context, target connection, attemptID string) ([]byte, error) {
	if err := requireIdentifier(attemptID, "attempt"); err != nil {
		return nil, err
	}
	return target.client().Do(ctx, http.MethodGet, target.repositoryPath()+"/check-attempts/"+url.PathEscape(attemptID)+"/log", nil)
}

// latestCheckConfiguration reads the configuration recorded most recently in
// the repository, from any branch. A check run never selects commands from it.
func latestCheckConfiguration(ctx context.Context, target connection) ([]byte, error) {
	return target.client().Do(ctx, http.MethodGet, target.repositoryPath()+"/check-configurations/latest", nil)
}

func requireIdentifier(value, name string) error {
	if value == "" {
		return cliProblem("invalid_arguments", "The "+name+" identifier is required.")
	}
	return nil
}

// postWithRetry retries a lost response with the same body. The cycle
// reservation, attempt registration, and attempt completion are idempotent by
// identity and payload, so a retry cannot create a second record or consume a
// second correction round. A cancelled context stops the retries.
func postWithRetry(ctx context.Context, client *apiclient.Client, path string, body any) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, lastErr
			case <-timer.C:
			}
		}
		content, err := client.Do(ctx, http.MethodPost, path, body)
		if err == nil {
			return content, nil
		}
		lastErr = err
		var problem *apiclient.Error
		if !errors.As(err, &problem) || (problem.Code != "connection_failed" && problem.Code != "invalid_response") {
			return nil, err
		}
	}
	return nil, lastErr
}
