// Package checkrunner implements the separately connected configured-check
// runner protocol. It has no direct access to OwnGit state or repositories.
package checkrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"owngit/internal/actions"
	"owngit/internal/apiclient"
	"owngit/internal/checkapi"
	"owngit/internal/checkexec"
	"owngit/internal/checksource"
	"owngit/internal/state"
)

const (
	leaseHeader      = "X-OwnGit-Runner-Lease"
	maximumRunnerLog = checkexec.KeptOutputBytes

	defaultRetryInitial = time.Second
	defaultRetryMax     = time.Minute
	// maximumRetryAfter caps a server's Retry-After request, so a wrong
	// header cannot park the runner for hours.
	maximumRetryAfter = 10 * time.Minute
)

// Runner polls one repository with one repository-scoped bearer token.
//
// Without Once it keeps running while OwnGit restarts or the network drops:
// a transient failure is retried with capped, jittered exponential backoff,
// and the runner stops only when the server refuses it permanently. With Once
// it makes a single attempt and returns any failure.
type Runner struct {
	Client        *apiclient.Client
	RepositoryID  string
	WorkspaceRoot string
	PollInterval  time.Duration
	Once          bool
	Logf          func(string, ...any)
	// RetryInitial and RetryMax bound the wait after a transient failure.
	// Zero values use one second and one minute.
	RetryInitial time.Duration
	RetryMax     time.Duration
}

func (runner *Runner) Run(ctx context.Context) error {
	if runner == nil || runner.Client == nil || runner.RepositoryID == "" || runner.WorkspaceRoot == "" {
		return errors.New("configured-check runner is incomplete")
	}
	workspaceRoot, err := checksource.AcquireWorkspaceRoot(runner.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("acquire runner workspace root: %w", err)
	}
	defer workspaceRoot.Close()
	removed, more, cleanupErr := workspaceRoot.Cleanup(1000)
	if cleanupErr != nil {
		return fmt.Errorf("clean interrupted runner workspaces: %w", cleanupErr)
	}
	if more {
		runner.log("runner startup workspace cleanup removed=%d; additional unrecognized or stale entries remain", removed)
	}
	interval := runner.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	failures := 0
	var outageStarted time.Time
	for {
		jobID, err := runner.runOne(ctx, workspaceRoot)
		worked := jobID != ""
		if err != nil && worked {
			// The server keeps a claimed or started job until its lease
			// expires and then marks it ambiguous, because the commands may
			// have run. Name the job so the operator can find it.
			runner.log("configured-check job %s ended without a confirmed result; OwnGit marks it ambiguous when its lease expires unless it already recorded an outcome (check it with owngit check-job show): %v", jobID, err)
		}
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if runner.Once {
			return err
		}
		kind := classifyFailure(err, worked)
		if kind == failureTransient {
			if failures == 0 {
				outageStarted = time.Now()
				runner.log("OwnGit is unreachable or unavailable, retrying until it answers: %v", err)
			}
			failures++
			if err := sleepContext(ctx, runner.retryDelay(failures, err)); err != nil {
				return err
			}
			continue
		}
		if failures > 0 {
			runner.log("OwnGit answered again after %s; the runner resumed", time.Since(outageStarted).Round(time.Second))
			failures = 0
		}
		if kind == failurePermanent {
			return stoppedError(err)
		}
		if worked {
			continue
		}
		if err := sleepContext(ctx, interval); err != nil {
			return err
		}
	}
}

type failureKind int

const (
	failureNone failureKind = iota
	// failureTransient is an outage: no answer, a timeout, a server error, or
	// a request to slow down. Retrying later can succeed.
	failureTransient
	// failureJob is a refusal that concerns one claimed job, such as a lease
	// that expired while OwnGit restarted. The runner continues.
	failureJob
	// failurePermanent is a refusal the runner cannot fix by retrying, such as
	// a revoked token or a server that does not speak this protocol.
	failurePermanent
)

func classifyFailure(err error, claimed bool) failureKind {
	if err == nil {
		return failureNone
	}
	var problem *apiclient.Error
	if !errors.As(err, &problem) {
		return failurePermanent
	}
	status := problem.ResponseStatus
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return failurePermanent
	case problem.Code == "connection_failed",
		problem.Code == "invalid_response" && problem.Cause != nil && status == 0,
		status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return failureTransient
	case claimed && status >= 400:
		return failureJob
	default:
		return failurePermanent
	}
}

// retryDelay is the wait before the given consecutive retry: exponential from
// RetryInitial up to RetryMax, with jitter so runners do not return in step,
// and never shorter than a Retry-After the server asked for.
func (runner *Runner) retryDelay(failures int, err error) time.Duration {
	initial, ceiling := runner.RetryInitial, runner.RetryMax
	if initial <= 0 {
		initial = defaultRetryInitial
	}
	if ceiling <= 0 {
		ceiling = defaultRetryMax
	}
	delay := initial
	for step := 1; step < failures && delay < ceiling; step++ {
		delay *= 2
	}
	delay = min(delay, ceiling)
	delay = delay/2 + rand.N(delay/2+1)
	var problem *apiclient.Error
	if errors.As(err, &problem) && problem.RetryAfter > delay {
		delay = min(problem.RetryAfter, maximumRetryAfter)
	}
	return delay
}

// stoppedError explains why a continuously running runner stopped.
func stoppedError(err error) error {
	var problem *apiclient.Error
	if !errors.As(err, &problem) {
		return &apiclient.Error{Code: "runner_stopped", Message: "The runner stopped because the server answer could not be used: " + err.Error(), Cause: err}
	}
	message := "The runner stopped because the server refused a request that retrying cannot fix: " + problem.Message
	switch {
	case problem.Code == "runner_credential_repository_mismatch":
		message = "The runner stopped because its token belongs to another repository. Start it with --repository set to the repository the token was issued for, or issue a token for this repository with owngit runner-credential issue."
	case problem.ResponseStatus == http.StatusUnauthorized || problem.ResponseStatus == http.StatusForbidden:
		message = "The runner stopped because the server refused its token, which is unknown or revoked. Issue a new token with owngit runner-credential issue, then start the runner again."
	}
	return &apiclient.Error{Code: problem.Code, Message: message, Details: problem.Details, Cause: err, Status: problem.Status, ResponseStatus: problem.ResponseStatus}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runOne claims and runs at most one job. A returned job ID names a job it
// handled; with an error, that job's outcome is still open. An error without a
// job ID means the runner knows of no job whose outcome it still owes: it
// claimed nothing, it already reported the claimed job as not run, or the
// claim answer was lost or unusable so it never learned the job. The server
// marks such an unknown claim ambiguous when its lease expires.
func (runner *Runner) runOne(ctx context.Context, workspaceRoot *checksource.WorkspaceRoot) (string, error) {
	base := "/api/v1/repositories/" + url.PathEscape(runner.RepositoryID) + "/runner"
	content, err := runner.Client.Do(ctx, http.MethodPost, base+"/claim", checkapi.RunnerClaimInput{Features: []string{state.RunnerFeatureWorkflowsV1}})
	if err != nil {
		return runner.notHandedOver(ctx, base, checkapi.ClaimedJob{}, err)
	}
	var claimed checkapi.JobResponse
	if err := json.Unmarshal(content, &claimed); err != nil {
		return "", fmt.Errorf("decode claimed job: %w", err)
	}
	if claimed.Job == nil {
		return "", nil
	}
	job := claimed.Job
	if job.ID == "" {
		return "", errors.New("server returned an invalid external-runner claim")
	}
	if job.LeaseID == "" || job.Executor != state.CheckExecutorExternalRunner {
		return job.ID, errors.New("server returned an invalid external-runner claim")
	}
	runner.log("claimed configured-check job %s", job.ID)
	leaseContext, cancelLease := context.WithCancel(ctx)
	defer cancelLease()
	leaseErrors := make(chan error, 1)
	go runner.renewLease(leaseContext, cancelLease, base, job, leaseErrors)

	_, workspace, materializeErr := workspaceRoot.PrepareJob(job.ID)
	remote := &remoteSource{client: runner.Client, base: base, job: job}
	if materializeErr == nil {
		materializeErr = remote.load(leaseContext)
	}
	var materialization *checksource.Result
	if materializeErr == nil {
		materialization, materializeErr = checksource.Materialize(leaseContext, remote, workspace, checksource.Options{
			Limits: checksource.Limits{
				MaxEntries: job.Execution.Source.MaxEntries, MaxFileBytes: job.Execution.Source.MaxFileBytes,
				MaxTotalBytes: job.Execution.Source.MaxTotalBytes, MaxPathDepth: job.Execution.Source.MaxPathDepth,
				MaxPathBytes: job.Execution.Source.MaxPathBytes, MaxNameBytes: job.Execution.Source.MaxNameBytes,
				MetadataLimit: job.Execution.Source.MetadataLimit,
			},
			Timeout: time.Duration(job.Limits.TimeoutMS) * time.Millisecond,
		})
	}
	if materializeErr != nil {
		cancelLease()
		<-leaseErrors
		_ = workspaceRoot.RemoveJob(job.ID)
		status := state.CheckJobUnavailable
		if ctx.Err() != nil {
			status = state.CheckJobInterrupted
		}
		reportErr := runner.reportNotRun(ctx, base, job.ID, job.LeaseID, status,
			"Exact configured-check source is unavailable or workspace ownership is uncertain: "+materializeErr.Error())
		if reportErr != nil {
			return job.ID, errors.Join(materializeErr, reportErr)
		}
		runner.log("reported configured-check job %s unavailable: %v", job.ID, materializeErr)
		return job.ID, nil
	}

	attemptID, err := state.RandomID()
	if err != nil {
		_ = workspaceRoot.RemoveJob(job.ID)
		return job.ID, err
	}
	startInput := checkapi.RunnerStartInput{LeaseID: job.LeaseID, AttemptID: attemptID}
	startClient := *runner.Client
	startClient.MaximumResponse = checkapi.MaximumRunnerStartBytes
	startContent, err := startClient.DoWithHeaders(leaseContext, http.MethodPost, base+"/jobs/"+job.ID+"/start", startInput, leaseHeaders(job.LeaseID))
	if err != nil {
		_ = workspaceRoot.RemoveJob(job.ID)
		cancelLease()
		<-leaseErrors
		return runner.notHandedOver(ctx, base, checkapi.ClaimedJob{JobID: job.ID, LeaseID: job.LeaseID}, err)
	}

	var results []checkexec.Result
	var roles []string
	var cancelled bool
	var notes string
	if job.RunID != "" {
		results, roles, cancelled, notes, err = runner.runActions(leaseContext, job, startContent, workspace)
		if err != nil {
			cancelLease()
			<-leaseErrors
			_ = workspaceRoot.RemoveJob(job.ID)
			return job.ID, err
		}
	} else {
		definitions := make([]checkexec.Definition, 0, len(job.Checks))
		for _, check := range job.Checks {
			definitions = append(definitions, checkexec.Definition{Name: check.Name, Command: check.Command})
		}
		results, cancelled = checkexec.Run(leaseContext, definitions, checkexec.Options{
			Dir: workspace, TempRoot: filepath.Dir(workspace), Timeout: time.Duration(job.Limits.TimeoutMS) * time.Millisecond,
			OutputLimit: job.Limits.OutputLimitBytes,
		})
	}
	workflowSafeCleanupMessage := func(message string, err error) string {
		if job.RunID != "" {
			return message + " failed."
		}
		return bounded(message + ": " + err.Error())
	}
	submittedWorktree := state.WorktreeClean
	verifyContext, cancelVerify := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	clean, verifyErr := checksource.VerifyResult(verifyContext, materialization)
	cancelVerify()
	if verifyErr != nil {
		submittedWorktree = state.WorktreeUnknown
		if len(results) != 0 {
			results[len(results)-1].Status = checkexec.StatusError
			results[len(results)-1].CleanupError = workflowSafeCleanupMessage("verify private runner workspace", verifyErr)
		}
	} else if !clean {
		submittedWorktree = state.WorktreeDirty
		if job.RunID != "" {
			facts := make([]actions.StepEvidence, len(results))
			for index, result := range results {
				facts[index] = actions.StepEvidence{Status: result.Status, Role: roles[index], CleanupError: result.CleanupError}
			}
			if index := actions.MarkSourceChanged(facts); index >= 0 {
				results[index].Status, roles[index] = facts[index].Status, facts[index].Role
			}
		} else if len(results) != 0 && results[len(results)-1].Status == checkexec.StatusPassed {
			results[len(results)-1].Status = checkexec.StatusIncomplete
		}
	}
	if cleanupErr := workspaceRoot.RemoveJob(job.ID); cleanupErr != nil && len(results) != 0 {
		results[len(results)-1].Status = checkexec.StatusError
		results[len(results)-1].CleanupError = workflowSafeCleanupMessage("remove private runner workspace", cleanupErr)
	}
	cancelLease()
	leaseErr := <-leaseErrors
	if leaseErr != nil && !errors.Is(leaseErr, context.Canceled) && len(results) != 0 {
		results[len(results)-1].Status = checkexec.StatusError
		results[len(results)-1].CleanupError = workflowSafeCleanupMessage("runner lease became uncertain", leaseErr)
	}
	log, logTruncated := runnerLog(results, notes)
	completion := checkapi.AttemptCompletion{
		Results: runnerResults(results, roles...), Cancelled: cancelled, FinishedAt: time.Now().UTC(),
		WorktreeState: submittedWorktree, Log: log, LogTruncated: logTruncated,
	}
	input := checkapi.RunnerCompletionInput{LeaseID: job.LeaseID, Completion: completion}
	_, err = runner.Client.DoWithHeaders(context.WithoutCancel(ctx), http.MethodPost, base+"/jobs/"+job.ID+"/complete", input, leaseHeaders(job.LeaseID))
	if err == nil {
		runner.log("completed configured-check job %s", job.ID)
	}
	return job.ID, err
}

// notHandedOver settles a job that the server holds as claimed but could not
// hand over, on a claim or on a start. The server names such a job in the
// error details; the runner reports it as not run and returns the server's
// error without a job ID, so the loop waits as it does for any unavailable
// answer. held is the job the runner already holds (zero on a claim): details
// that name another job settle nothing. Without such details the held job's
// outcome stays open, and its ID is returned.
func (runner *Runner) notHandedOver(ctx context.Context, base string, held checkapi.ClaimedJob, err error) (string, error) {
	claimed, committed := claimedJob(err)
	if !committed || (held.JobID != "" && claimed != held) {
		return held.JobID, err
	}
	if reportErr := runner.reportNotRun(ctx, base, claimed.JobID, claimed.LeaseID, state.CheckJobUnavailable,
		"The claimed job could not be handed to the runner: "+err.Error()); reportErr != nil {
		return claimed.JobID, errors.Join(err, reportErr)
	}
	runner.log("reported configured-check job %s unavailable: %v", claimed.JobID, err)
	return "", err
}

// claimedJob returns the job and lease that the server still holds as claimed
// after a failed claim or start. The server names them in the error details;
// an error without them names no such job.
func claimedJob(err error) (checkapi.ClaimedJob, bool) {
	var problem *apiclient.Error
	var claimed checkapi.ClaimedJob
	if !errors.As(err, &problem) || len(problem.Details) == 0 || json.Unmarshal(problem.Details, &claimed) != nil {
		return checkapi.ClaimedJob{}, false
	}
	return claimed, claimed.JobID != "" && claimed.LeaseID != ""
}

// reportNotRun records the outcome of a claimed job that ran nothing, so the
// server does not wait for its lease to expire and call it ambiguous. The
// report is sent even when the runner is stopping.
func (runner *Runner) reportNotRun(ctx context.Context, base, jobID, leaseID, status, summary string) error {
	input := checkapi.RunnerUnavailableInput{LeaseID: leaseID, Status: status, Summary: bounded(summary)}
	_, err := runner.Client.DoWithHeaders(context.WithoutCancel(ctx), http.MethodPost, base+"/jobs/"+jobID+"/unavailable", input, leaseHeaders(leaseID))
	return err
}

func (runner *Runner) renewLease(ctx context.Context, cancel context.CancelFunc, base string, job *checkapi.Job, done chan<- error) {
	ticker := time.NewTicker(checkapi.LeaseRenewDelay(time.Now(), job.LeaseExpiresAt))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- ctx.Err()
			return
		case <-ticker.C:
			content, err := runner.Client.DoWithHeaders(ctx, http.MethodPost, base+"/jobs/"+job.ID+"/renew", nil, leaseHeaders(job.LeaseID))
			if err != nil {
				cancel()
				done <- err
				return
			}
			var response checkapi.JobResponse
			if err := json.Unmarshal(content, &response); err != nil || response.Job == nil {
				cancel()
				if err == nil {
					err = errors.New("lease renewal returned no job")
				}
				done <- err
				return
			}
			if response.Job.CancelRequested {
				cancel()
				done <- nil
				return
			}
		}
	}
}

type remoteSource struct {
	client   *apiclient.Client
	base     string
	job      *checkapi.Job
	manifest *checkapi.SourceManifest
	paths    map[string]string
}

func (source *remoteSource) load(ctx context.Context) error {
	content, err := source.client.DoWithHeaders(ctx, http.MethodGet, source.base+"/jobs/"+source.job.ID+"/source", nil, leaseHeaders(source.job.LeaseID))
	if err != nil {
		return err
	}
	var manifest checkapi.SourceManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return err
	}
	if manifest.JobID != source.job.ID || manifest.CommitOID != source.job.SourceOID || manifest.ObjectFormat == "" || manifest.Limits != source.job.Execution.Source {
		return errors.New("source manifest does not match the immutable job")
	}
	source.manifest = &manifest
	source.paths = make(map[string]string, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if entry.Type == "blob" {
			if _, exists := source.paths[entry.OID]; !exists {
				source.paths[entry.OID] = entry.Path
			}
		}
	}
	return nil
}

func (source *remoteSource) ListTree(ctx context.Context, metadataLimit int64) ([]checksource.Entry, error) {
	if source.manifest == nil {
		return nil, errors.New("source manifest has not been loaded")
	}
	entries := make([]checksource.Entry, 0, len(source.manifest.Entries))
	for _, entry := range source.manifest.Entries {
		entries = append(entries, checksource.Entry{Path: entry.Path, OID: entry.OID, Mode: entry.Mode, Type: entry.Type, Size: entry.Size})
	}
	return entries, nil
}

func (source *remoteSource) CheckBlobRebuilds(ctx context.Context, oids []string) error {
	return nil
}

// ReadBlob checks that the server sent the requested manifest blob at its
// listed size. checksource.Materialize hashes the bytes against oid before it
// writes them, so the content itself is verified there.
func (source *remoteSource) ReadBlob(ctx context.Context, oid string, expectedSize int64) ([]byte, error) {
	if source.manifest == nil || expectedSize < 0 || expectedSize > source.manifest.Limits.MaxFileBytes {
		return nil, errors.New("source blob was requested without a valid manifest bound")
	}
	path, exists := source.paths[oid]
	if !exists {
		return nil, errors.New("source blob is not present in the authorized manifest")
	}
	encodedPath := base64.RawURLEncoding.EncodeToString([]byte(path))
	content, headers, err := source.client.GetBytes(ctx, source.base+"/jobs/"+source.job.ID+"/files/"+encodedPath+"/"+url.PathEscape(oid), leaseHeaders(source.job.LeaseID), expectedSize)
	if err != nil {
		return nil, err
	}
	if headers.Get("X-OwnGit-Blob-OID") != oid || int64(len(content)) != expectedSize {
		return nil, errors.New("source blob identity or size does not match the manifest")
	}
	return content, nil
}

func (source *remoteSource) ObjectFormat() string {
	if source.manifest == nil {
		return ""
	}
	return source.manifest.ObjectFormat
}

func (source *remoteSource) CommitOID() string {
	if source.manifest == nil {
		return ""
	}
	return source.manifest.CommitOID
}

func leaseHeaders(leaseID string) map[string]string { return map[string]string{leaseHeader: leaseID} }

func runnerResults(results []checkexec.Result, roles ...string) []checkapi.Result {
	converted := make([]checkapi.Result, 0, len(results))
	for index, result := range results {
		role := ""
		if index < len(roles) {
			role = roles[index]
		}
		excerpt, cut := checkapi.ClipLog(result.Output, state.MaximumCheckExcerptBytes, result.OutputGap)
		truncated := result.Truncated || cut
		cleanupError := result.CleanupError
		if cleanupError != "" {
			cleanupError = bounded(cleanupError)
		}
		converted = append(converted, checkapi.Result{
			Name: result.Name, Command: result.Command, Status: result.Status, ExitCode: result.ExitCode, Role: role,
			DurationMS: result.Duration.Milliseconds(), OutputExcerpt: excerpt, Truncated: truncated,
			CleanupError: cleanupError,
		})
	}
	return converted
}

func runnerLog(results []checkexec.Result, notes ...string) (string, bool) {
	log := checkapi.LogBuffer{Limit: maximumRunnerLog}
	for _, note := range notes {
		log.Add(note)
	}
	for _, result := range results {
		// Output may be far larger than the log, so it is added as its own part
		// rather than copied whole into one string before the cut.
		log.Add("[" + result.Status + "] " + result.Command + "\n")
		log.AddClipped(result.Output, result.OutputGap)
		log.Add("\n")
	}
	return log.Result()
}

func bounded(value string) string {
	return checkapi.SummaryText(value, 500)
}

func (runner *Runner) log(format string, arguments ...any) {
	if runner.Logf != nil {
		runner.Logf(format, arguments...)
	}
}
