package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"owngit/internal/checkapi"
	"owngit/internal/checkrun"
	"owngit/internal/checksource"
	"owngit/internal/logtext"
	"owngit/internal/repository"
	"owngit/internal/state"
)

const runnerLeaseHeader = "X-OwnGit-Runner-Lease"
const checkSourceReadRefusal = "The exact source contains a file this server cannot read within its size or memory limits."

// handleConfiguredCheckOwnerAPI serves owner-only policy, job and runner-token
// operations. Every mutation requires the current administrator password.
func (app *App) handleConfiguredCheckOwnerAPI(writer http.ResponseWriter, request *http.Request, repositoryID, resource, remainder string) {
	if !app.authorizeAdminAPI(writer, request) {
		return
	}
	if _, exists, err := app.Store.Repository(request.Context(), repositoryID); err != nil {
		writeAPIError(writer, unavailable(request, "repository record read", err), "state_unavailable", "OwnGit state is unavailable.", nil)
		return
	} else if !exists {
		writeAPIError(writer, http.StatusNotFound, "repository_not_found", "The repository does not exist.", nil)
		return
	}
	// Runner credentials are credential management, not repository use, so
	// they stay manageable (and revocable) while the repository is locked.
	if resource != "runner-credentials" && app.refusePreparingAPI(writer, request, repositoryID) {
		return
	}
	switch resource {
	case "check-policy":
		app.handleCheckPolicy(writer, request, repositoryID, remainder)
	case "check-jobs":
		app.handleCheckJobs(writer, request, repositoryID, remainder)
	case "runner-credentials":
		app.handleRunnerCredentials(writer, request, repositoryID, remainder)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
	}
}

func (app *App) handleCheckPolicy(writer http.ResponseWriter, request *http.Request, repositoryID, remainder string) {
	if remainder == "" && request.Method == http.MethodGet {
		policy, exists, err := app.Store.CheckPolicy(request.Context(), repositoryID)
		if err != nil {
			writeAPIError(writer, unavailable(request, "configured check policy read", err), "state_unavailable", "The configured-check policy could not be read.", nil)
			return
		}
		if !exists {
			writeAPIError(writer, http.StatusNotFound, "check_policy_not_found", "The repository has no configured-check policy.", nil)
			return
		}
		writeAPIJSON(writer, http.StatusOK, app.policyResponse(policy))
		return
	}
	if remainder == "" && request.Method == http.MethodPut {
		var input checkapi.PolicyInput
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		policy, err := app.Store.SetCheckPolicy(request.Context(), statePolicyInput(repositoryID, input), app.now())
		app.writePolicyAPIAnswer(writer, request, repositoryID, policy, err)
		return
	}
	if remainder == "save-and-enable" && request.Method == http.MethodPost {
		var input checkapi.SaveAndEnableInput
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		var base *state.ExpectedCheckPolicy
		if input.Expected != nil {
			base = &state.ExpectedCheckPolicy{Version: input.Expected.Version, Digest: input.Expected.Digest}
		}
		policy, err := app.Store.SaveCheckPolicyAndGrantConsent(request.Context(), statePolicyInput(repositoryID, input.Policy), base, app.now())
		app.writePolicyAPIAnswer(writer, request, repositoryID, policy, err)
		return
	}
	if (remainder == "enable" || remainder == "disable") && request.Method == http.MethodPost {
		var (
			policy state.CheckPolicy
			err    error
		)
		if remainder == "enable" {
			policy, err = app.Store.GrantCheckConsent(request.Context(), repositoryID, app.now())
		} else {
			policy, err = app.Store.RevokeCheckConsent(request.Context(), repositoryID, app.now())
		}
		if err != nil {
			switch {
			case errors.Is(err, state.ErrCheckPolicyMissing):
				writeAPIError(writer, http.StatusNotFound, "check_policy_not_found", err.Error(), nil)
			case errors.Is(err, state.ErrInvalidCheckPolicy):
				writeAPIError(writer, http.StatusConflict, "check_policy_incomplete", err.Error(), nil)
			default:
				// The cause can name internal state, so it stays in the server log.
				writeAPIError(writer, unavailable(request, "configured check consent change", err), "state_unavailable", "The configured-check consent could not be changed.", nil)
			}
			return
		}
		app.wakeChecks(repositoryID)
		writeAPIJSON(writer, http.StatusOK, app.policyResponse(policy))
		return
	}
	if remainder == "" {
		writeAPIMethodError(writer, http.MethodGet+", "+http.MethodPut)
		return
	}
	writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
}

func statePolicyInput(repositoryID string, input checkapi.PolicyInput) state.CheckPolicyInput {
	return state.CheckPolicyInput{
		RepositoryID: repositoryID, Executor: input.Executor, AllowedEvents: input.AllowedEvents,
		MaxTimeoutMS: input.MaxTimeoutMS, MaxOutputLimitBytes: input.MaxOutputLimitBytes,
		QueueLimit: input.QueueLimit, MaxActiveJobs: input.MaxActiveJobs, MaxLeaseMS: input.MaxLeaseMS,
		Execution: input.Execution,
	}
}

// writePolicyAPIAnswer answers a policy save, with or without enabling.
func (app *App) writePolicyAPIAnswer(writer http.ResponseWriter, request *http.Request, repositoryID string, policy state.CheckPolicy, err error) {
	switch {
	case errors.Is(err, state.ErrInvalidCheckPolicy):
		writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_check_policy", err.Error(), nil)
	case errors.Is(err, state.ErrCheckPolicyStale):
		writeAPIError(writer, http.StatusConflict, "check_policy_stale", err.Error(), nil)
	case errors.As(err, new(*state.PolicyError)):
		writeSettingUnreadable(writer, request, "configured check policy save", err)
	case err != nil:
		writeAPIError(writer, unavailable(request, "configured check policy save", err), "state_unavailable", "The configured-check policy could not be saved.", nil)
	default:
		app.wakeChecks(repositoryID)
		writeAPIJSON(writer, http.StatusOK, app.policyResponse(policy))
	}
}

func (app *App) handleCheckJobs(writer http.ResponseWriter, request *http.Request, repositoryID, remainder string) {
	if remainder == "" {
		if request.Method != http.MethodGet {
			writeAPIMethodError(writer, http.MethodGet)
			return
		}
		jobs, err := app.Store.LatestCheckJobs(request.Context(), repositoryID, 100)
		if err != nil {
			writeAPIError(writer, unavailable(request, "configured check job list read", err), "state_unavailable", "Configured-check jobs could not be read.", nil)
			return
		}
		response := checkapi.JobListResponse{OK: true, Jobs: make([]*checkapi.Job, 0, len(jobs))}
		for _, job := range jobs {
			response.Jobs = append(response.Jobs, jobJSON(job, nil))
		}
		writeAPIJSON(writer, http.StatusOK, response)
		return
	}
	parts := strings.Split(remainder, "/")
	jobID := parts[0]
	if !validAttemptID(jobID) {
		writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_job_id", "A valid configured-check job identifier is required.", nil)
		return
	}
	if len(parts) == 1 && request.Method == http.MethodGet {
		job, exists, err := app.Store.CheckJob(request.Context(), repositoryID, jobID)
		if err != nil {
			writeAPIError(writer, unavailable(request, "configured check job read", err), "state_unavailable", "The configured-check job could not be read.", nil)
			return
		}
		if !exists {
			writeAPIError(writer, http.StatusNotFound, "check_job_not_found", "The configured-check job does not exist.", nil)
			return
		}
		checks, err := app.jobChecks(request.Context(), job)
		if err != nil {
			writeJobRecordError(writer, request, err, jobChecksUnreadable, nil)
			return
		}
		response := checkapi.JobResponse{OK: true, Job: jobJSON(job, checks)}
		if job.AttemptID != "" {
			attempt, err := app.jobAttempt(request.Context(), job)
			if err != nil {
				writeJobRecordError(writer, request, err, jobAttemptUnreadable, nil)
				return
			}
			response.Attempt = app.attemptJSON(request, attempt)
		}
		writeAPIJSON(writer, http.StatusOK, response)
		return
	}
	if len(parts) == 2 && parts[1] == "log" && request.Method == http.MethodGet {
		job, exists, err := app.Store.CheckJob(request.Context(), repositoryID, jobID)
		if err != nil {
			writeAPIError(writer, unavailable(request, "configured check job read", err), "state_unavailable", "The configured-check job could not be read.", nil)
			return
		}
		if !exists {
			writeAPIError(writer, http.StatusNotFound, "check_job_not_found", "The configured-check job does not exist.", nil)
			return
		}
		if job.AttemptID == "" {
			writeAPIError(writer, http.StatusNotFound, "attempt_not_found", "The configured-check job has no attempt.", nil)
			return
		}
		attempt, err := app.jobAttempt(request.Context(), job)
		if err != nil {
			writeJobRecordError(writer, request, err, jobAttemptUnreadable, nil)
			return
		}
		app.writeCheckAttemptLog(writer, request, attempt)
		return
	}
	if len(parts) == 2 && request.Method == http.MethodPost {
		var (
			job state.CheckJob
			err error
		)
		switch parts[1] {
		case "cancel":
			job, err = app.Store.CancelCheckJob(request.Context(), repositoryID, jobID, app.now())
		case "rerun":
			job, _, err = app.rerunCheckJob(request.Context(), repositoryID, jobID)
		default:
			writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
			return
		}
		if err != nil {
			writeConfiguredJobError(writer, request, err)
			return
		}
		if parts[1] == "cancel" && job.Status != state.CheckJobCancelled && job.CancelRequestedAt == nil {
			// The job had already finished, so nothing was cancelled or
			// recorded. Reporting success would suggest work was stopped.
			writeAPIError(writer, http.StatusConflict, "check_job_finished", "The configured-check job already finished as "+job.Status+"; no cancellation was recorded.", nil)
			return
		}
		app.wakeChecks(repositoryID)
		// The answer reports the committed change. The job's commands did not
		// change and are in its detail, so a failed read of them cannot turn
		// this success into an error.
		writeAPIJSON(writer, http.StatusOK, checkapi.JobResponse{OK: true, Job: jobJSON(job, nil)})
		return
	}
	writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
}

func (app *App) handleRunnerCredentials(writer http.ResponseWriter, request *http.Request, repositoryID, remainder string) {
	parts := strings.Split(remainder, "/")
	switch {
	case remainder == "" && request.Method == http.MethodGet:
		credentials, err := app.Store.CheckRunnerCredentials(request.Context(), repositoryID)
		if err != nil {
			writeAPIError(writer, unavailable(request, "runner credential list read", err), "state_unavailable", "Runner credentials could not be read.", nil)
			return
		}
		response := checkapi.RunnerCredentialListResponse{OK: true, Credentials: make([]*checkapi.RunnerCredential, 0, len(credentials))}
		for _, credential := range credentials {
			response.Credentials = append(response.Credentials, runnerCredentialJSON(credential))
		}
		writeAPIJSON(writer, http.StatusOK, response)
	case remainder == "" && request.Method == http.MethodPost:
		var input checkapi.CreateCredentialInput
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		credential, token, created, err := app.Store.IssueCheckRunnerToken(request.Context(), repositoryID, input.Label, input.CreationID, app.now())
		if err != nil {
			// Every 4xx answer here means nothing was created, so a client can
			// take it as final. Only the unavailable answer leaves open whether
			// a credential exists.
			switch {
			case errors.Is(err, state.ErrInvalidCheckJob):
				writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_runner_credential", invalidCredentialInput, nil)
			case errors.Is(err, state.ErrCheckPolicyMissing):
				writeAPIError(writer, http.StatusNotFound, "check_policy_not_found", "The repository has no configured-check policy.", nil)
			case errors.Is(err, state.ErrCheckRunnerCreationConflict):
				writeAPIError(writer, http.StatusConflict, "creation_conflict", "The creation identity already belongs to different runner credential content.", nil)
			default:
				writeAPIError(writer, unavailable(request, "runner credential issue", err), "state_unavailable", "The runner credential could not be created.", nil)
			}
			return
		}
		response := checkapi.RunnerCredentialResponse{OK: true, Credential: runnerCredentialJSON(credential), RepositoryAddress: repositoryAddressCurrent(request)}
		if created {
			response.Token = token
		}
		writeAPIJSON(writer, http.StatusOK, response)
	case len(parts) == 1 && parts[0] != "" && request.Method == http.MethodDelete:
		if err := app.Store.RevokeCheckRunnerToken(request.Context(), repositoryID, parts[0], app.now()); errors.Is(err, state.ErrCheckRunnerRevoked) {
			writeAPIError(writer, http.StatusConflict, "runner_credential_not_found", "The runner credential was not found or was already revoked.", nil)
			return
		} else if err != nil {
			writeAPIError(writer, unavailable(request, "runner credential revoke", err), "state_unavailable", "The runner credential could not be revoked.", nil)
			return
		}
		app.wakeChecks(repositoryID)
		writeAPIJSON(writer, http.StatusOK, checkapi.OKResponse{OK: true})
	case len(parts) == 2 && parts[0] == "by-creation" && parts[1] != "" && request.Method == http.MethodDelete:
		if err := app.Store.RevokeCheckRunnerTokenByCreation(request.Context(), repositoryID, parts[1], app.now()); err != nil {
			writeAPIError(writer, unavailable(request, "runner credential revoke", err), "state_unavailable", "The runner credential could not be revoked.", nil)
			return
		}
		writeAPIJSON(writer, http.StatusOK, checkapi.OKResponse{OK: true})
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "The API endpoint does not exist.", nil)
	}
}

func (app *App) handleRunnerAPI(writer http.ResponseWriter, request *http.Request, repositoryID, remainder string) {
	credential, ok := app.authorizeRunner(writer, request, repositoryID)
	if !ok {
		return
	}
	if app.refusePreparingAPI(writer, request, repositoryID) {
		return
	}
	status := app.checkRuntimeStatus()
	if !status.Available {
		writeAPIError(writer, unavailable(request, "configured check runtime", checkrun.ErrRuntimeUnavailable), "check_runtime_unavailable", status.UnavailableReason, map[string]string{"reason": status.UnavailableCode})
		return
	}
	if remainder == "claim" {
		if request.Method != http.MethodPost {
			writeAPIMethodError(writer, http.MethodPost)
			return
		}
		var input checkapi.RunnerClaimInput
		if request.ContentLength != 0 && !decodeAPIJSON(writer, request, &input) {
			return
		}
		job, claimed, err := app.Store.ClaimCheckJob(request.Context(), repositoryID, credential.ID, app.now(), input.Features...)
		if err != nil {
			writeRunnerError(writer, request, err)
			return
		}
		if !claimed {
			writeAPIJSON(writer, http.StatusOK, checkapi.JobResponse{OK: true})
			return
		}
		checks, err := app.jobChecks(request.Context(), job)
		if err != nil {
			// The claim is committed and its lease belongs to this runner. The
			// answer names both, so the claim is not mistaken for an empty
			// queue, and it never hands out the job without its commands.
			writeJobRecordError(writer, request, err, jobChecksUnreadable, checkapi.ClaimedJob{JobID: job.ID, LeaseID: job.LeaseID})
			return
		}
		writeAPIJSON(writer, http.StatusOK, checkapi.JobResponse{OK: true, Job: jobJSON(job, checks)})
		return
	}
	parts := strings.Split(remainder, "/")
	if len(parts) < 2 || parts[0] != "jobs" || !validAttemptID(parts[1]) {
		writeAPIError(writer, http.StatusNotFound, "not_found", "The runner endpoint does not exist.", nil)
		return
	}
	jobID := parts[1]
	leaseID := request.Header.Get(runnerLeaseHeader)
	if len(parts) == 3 && parts[2] == "source" && request.Method == http.MethodGet {
		app.runnerSourceManifest(writer, request, repositoryID, jobID, leaseID, credential)
		return
	}
	if len(parts) == 5 && parts[2] == "files" && request.Method == http.MethodGet {
		app.runnerSourceBlob(writer, request, repositoryID, jobID, leaseID, parts[3], parts[4], credential)
		return
	}
	if len(parts) != 3 || request.Method != http.MethodPost {
		writeAPIError(writer, http.StatusNotFound, "not_found", "The runner endpoint does not exist.", nil)
		return
	}
	authority := state.CheckJobCompletionAuthority{JobID: jobID, LeaseID: leaseID, CredentialID: credential.ID, CredentialGeneration: credential.Generation}
	switch parts[2] {
	case "renew":
		job, err := app.Store.RenewCheckJobLease(request.Context(), authority, app.now())
		if err != nil {
			writeRunnerError(writer, request, err)
			return
		}
		writeAPIJSON(writer, http.StatusOK, checkapi.JobResponse{OK: true, Job: jobJSON(job, nil)})
	case "start":
		var input checkapi.RunnerStartInput
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		if input.LeaseID != leaseID || !validAttemptID(input.AttemptID) {
			writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_runner_start", "The runner start identity is invalid.", nil)
			return
		}
		grant, ready := app.preflightRunnerActions(writer, request, repositoryID, authority)
		if !ready {
			return
		}
		started, attempt, err := app.Store.StartCheckJob(request.Context(), state.CheckJobStart{
			RepositoryID: repositoryID, JobID: jobID, LeaseID: leaseID,
			CredentialID: credential.ID, CredentialGeneration: credential.Generation,
			Protection: state.ProtectionRunnerReported, AttemptID: input.AttemptID,
		}, app.now())
		var notStarted *state.CheckJobNotStartedError
		if errors.As(err, &notStarted) {
			// The job was not started and is still this runner's claim, so
			// the answer names it, as a claim that could not be handed over
			// does, and the runner reports it as not run.
			claimed := checkapi.ClaimedJob{JobID: jobID, LeaseID: leaseID}
			if errors.Is(err, state.ErrCheckJobCommandsUnavailable) {
				writeJobRecordError(writer, request, err, jobChecksUnreadable, claimed)
			} else {
				writeAPIError(writer, unavailable(request, "runner start", err), "state_unavailable", "The configured-check job could not be started.", claimed)
			}
			return
		}
		if err != nil {
			writeRunnerError(writer, request, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		status, encoded := encodeAPIJSONLimit(http.StatusOK, checkapi.RunnerStartResponse{
			JobResponse: checkapi.JobResponse{OK: true, Job: jobJSON(started, attempt.Checks), Attempt: app.attemptJSON(request, attempt)},
			Actions:     grant,
		}, checkapi.MaximumRunnerStartBytes)
		writeEncodedAPIJSON(writer, status, encoded)
	case "complete":
		var input checkapi.RunnerCompletionInput
		if !decodeAPIJSONLimit(writer, request, &input, maximumCheckUpload) {
			return
		}
		if input.LeaseID != leaseID {
			writeAPIError(writer, http.StatusConflict, "check_job_lease", "The job lease does not match.", nil)
			return
		}
		job, exists, err := app.Store.CheckJob(request.Context(), repositoryID, jobID)
		if err != nil {
			writeRunnerError(writer, request, err)
			return
		}
		if !exists || job.AttemptID == "" {
			writeRunnerError(writer, request, state.ErrCheckJobNotFound)
			return
		}
		completion, problem := completionFromUpload(input.Completion, repositoryID, job.TaskID, job.AttemptID)
		if problem != nil {
			writeAPIError(writer, apiStatus(request, "runner check completion", problem), problem.Code, problem.Message, nil)
			return
		}
		task, attempt, err := app.Store.CompleteCheckJobAttempt(request.Context(), completion, authority, app.now())
		if err != nil {
			writeRunnerError(writer, request, err)
			return
		}
		writeAPIJSON(writer, http.StatusOK, checkapi.TaskResponse{OK: true, Task: taskJSON(task), Attempt: app.attemptJSON(request, attempt)})
	case "unavailable":
		var input checkapi.RunnerUnavailableInput
		if !decodeAPIJSON(writer, request, &input) {
			return
		}
		if input.LeaseID != leaseID {
			writeAPIError(writer, http.StatusConflict, "check_job_lease", "The job lease does not match.", nil)
			return
		}
		job, err := app.Store.FailCheckJobBeforeStart(request.Context(), authority, input.Status, input.Summary, app.now())
		if err != nil {
			writeRunnerError(writer, request, err)
			return
		}
		writeAPIJSON(writer, http.StatusOK, checkapi.JobResponse{OK: true, Job: jobJSON(job, nil)})
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "The runner endpoint does not exist.", nil)
	}
}

func (app *App) runnerSourceManifest(writer http.ResponseWriter, request *http.Request, repositoryID, jobID, leaseID string, credential state.RunnerCredential) {
	job, err := app.Store.AuthorizeCheckJobSource(request.Context(), repositoryID, jobID, leaseID, credential.ID, credential.Generation, app.now())
	if err != nil {
		writeRunnerError(writer, request, err)
		return
	}
	// A busy repository is retried; any other listing failure is the source's
	// answer and is reported as a refusal below.
	var source *checksource.PinnedSource
	var entries []checksource.Entry
	var listErr error
	err = checksource.RetryWhileRepositoryBusy(request.Context(), runnerSourceBusyWait, func() error {
		pinned, err := app.Repositories.PinRepository(request.Context(), repositoryID, job.SourceOID, job.SourceOID)
		if err != nil {
			return err
		}
		if source, err = checksource.NewPinnedSource(pinned, repository.PinnedHead); err != nil {
			return err
		}
		entries, listErr = source.ListTree(request.Context(), job.Execution.Source.MetadataLimit)
		if errors.Is(listErr, repository.ErrPinnedRepositoryBusy) {
			return listErr
		}
		return nil
	})
	if err != nil {
		writeAPIError(writer, unavailable(request, "configured check source read", err), "check_source_unavailable", "The exact configured-check source is unavailable.", nil)
		return
	}
	if listErr != nil || checksource.ValidateEntries(entries, sourceLimits(job.Execution.Source)) != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "check_source_refused", "The exact source does not satisfy the configured materialization bounds.", nil)
		return
	}
	oids := make([]string, 0, len(entries))
	bound := app.Repositories.Git.ReadBound()
	for _, entry := range entries {
		if bound > 0 && entry.Size > bound {
			writeAPIError(writer, http.StatusUnprocessableEntity, "check_source_refused", checkSourceReadRefusal, nil)
			return
		}
		oids = append(oids, entry.OID)
	}
	err = checksource.RetryWhileRepositoryBusy(request.Context(), runnerSourceBusyWait, func() error {
		return source.CheckBlobRebuilds(request.Context(), oids)
	})
	if errors.Is(err, repository.ErrPinnedBlobTooLarge) {
		writeAPIError(writer, http.StatusUnprocessableEntity, "check_source_refused", checkSourceReadRefusal, nil)
		return
	}
	if err != nil {
		writeAPIError(writer, unavailable(request, "configured check source memory check", err), "check_source_unavailable", "The exact configured-check source is unavailable.", nil)
		return
	}
	response := checkapi.SourceManifest{OK: true, JobID: job.ID, CommitOID: job.SourceOID, ObjectFormat: source.ObjectFormat(), Limits: job.Execution.Source}
	for _, entry := range entries {
		response.Entries = append(response.Entries, checkapi.SourceEntry{Path: entry.Path, OID: entry.OID, Mode: entry.Mode, Type: entry.Type, Size: entry.Size})
	}
	writeAPIJSON(writer, http.StatusOK, response)
}

func (app *App) runnerSourceBlob(writer http.ResponseWriter, request *http.Request, repositoryID, jobID, leaseID, encodedPath, oid string, credential state.RunnerCredential) {
	job, err := app.Store.AuthorizeCheckJobSource(request.Context(), repositoryID, jobID, leaseID, credential.ID, credential.Generation, app.now())
	if err != nil {
		writeRunnerError(writer, request, err)
		return
	}
	decodedPath, err := base64.RawURLEncoding.DecodeString(encodedPath)
	if err != nil || len(decodedPath) == 0 || len(decodedPath) > job.Execution.Source.MaxPathBytes {
		writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_check_source_path", "The authorized source path is invalid.", nil)
		return
	}
	var blob repository.PinnedBlobChunk
	err = checksource.RetryWhileRepositoryBusy(request.Context(), runnerSourceBusyWait, func() error {
		pinned, err := app.Repositories.PinRepository(request.Context(), repositoryID, job.SourceOID, job.SourceOID)
		if err != nil {
			return err
		}
		blob, err = pinned.ReadBlob(request.Context(), repository.PinnedHead, string(decodedPath), 0,
			job.Execution.Source.MetadataLimit, job.Execution.Source.MaxFileBytes+1, job.Execution.Source.MaxFileBytes+1)
		return err
	})
	if err == nil && (blob.HasMore || blob.OID != oid || blob.Size != int64(len(blob.Content)) ||
		blob.Symlink || (blob.Mode != "100644" && blob.Mode != "100755")) {
		err = errSourceBlobMismatch
	}
	if err != nil {
		if errors.Is(err, repository.ErrPinnedBlobTooLarge) {
			// The exact source is present, but this server cannot read the
			// file without a Git process that may not fit in memory. Every
			// retry would repeat the same read, so the runner is told the
			// source does not satisfy the materialization bounds.
			writeAPIError(writer, http.StatusUnprocessableEntity, "check_source_refused",
				checkSourceReadRefusal, nil)
			return
		}
		writeAPIError(writer, unavailable(request, "configured check source blob read", err), "check_source_unavailable", "The authorized exact source blob is unavailable.", nil)
		return
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-OwnGit-Blob-OID", oid)
	writer.Header().Set("Content-Length", strconv.FormatInt(int64(len(blob.Content)), 10))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(blob.Content)
}

func (app *App) authorizeRunner(writer http.ResponseWriter, request *http.Request, repositoryID string) (state.RunnerCredential, bool) {
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="OwnGit runner"`)
		writeAPIError(writer, http.StatusUnauthorized, "runner_authentication_required", "A repository-scoped runner token is required.", nil)
		return state.RunnerCredential{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	credential, ok, err := app.Store.RunnerCredentialByToken(request.Context(), repositoryID, token, app.now())
	if errors.Is(err, state.ErrRunnerCredentialOtherRepository) {
		// Only the holder of a live token reaches this answer, and it names
		// no repository.
		if !boundAddressReadable(writer, request) {
			return state.RunnerCredential{}, false
		}
		writeAPIError(writer, http.StatusForbidden, "runner_credential_repository_mismatch", "This runner token belongs to another repository.", nil)
		return state.RunnerCredential{}, false
	}
	if err != nil {
		writeAPIError(writer, unavailable(request, "runner token check", err), "state_unavailable", "The runner token could not be verified.", nil)
		return state.RunnerCredential{}, false
	}
	if !ok || credential.Role != state.RunnerRoleExternal {
		writeAPIError(writer, http.StatusUnauthorized, "invalid_runner_credential", "The runner token is unknown or revoked.", nil)
		return state.RunnerCredential{}, false
	}
	return credential, boundAddressReadable(writer, request)
}

// jobJSON describes a job. checks are its captured commands when the answer
// carries them; see jobChecks.
func jobJSON(job state.CheckJob, checks []state.CheckDefinition) *checkapi.Job {
	result := &checkapi.Job{
		ID: job.ID, RepositoryID: job.RepositoryID, TaskID: job.TaskID, Trigger: job.Trigger, EventKey: job.EventKey,
		SourceOID: job.SourceOID, BaseOID: job.BaseOID, PullRequestNumber: job.PullRequestNumber, TriggerRef: job.TriggerRef,
		WorkflowPath: job.WorkflowPath, WorkflowOID: job.WorkflowOID, WorkflowDigest: job.WorkflowDigest,
		RunID: job.RunID, JobKey: job.JobKey, MatrixIndex: job.MatrixIndex, PlanDigest: job.PlanDigest,
		ConfigurationVersion: job.ConfigurationVersion, Executor: job.Executor, PolicyVersion: job.PolicyVersion,
		ConsentVersion: job.ConsentVersion, Limits: job.Limits, Execution: job.Execution,
		Status: job.Status, AttemptID: job.AttemptID, LeaseID: job.LeaseID, LeaseExpiresAt: job.LeaseExpiresAt,
		Protection: job.Protection, CancelRequested: job.CancelRequestedAt != nil, Summary: job.Summary,
		AdmittedAt: job.AdmittedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
	}
	for _, check := range checks {
		result.Checks = append(result.Checks, checkapi.CheckDefinition{Name: check.Name, Command: check.Command})
	}
	return result
}

// A job's captured configuration is stored in the transaction that admits the
// job, and the attempt it names in the transaction that binds it. Only
// repository deletion removes either, and it removes the job with them. So a
// job whose configuration (state.ErrCheckConfigurationMissing) or named
// attempt (errJobAttemptMissing) is missing is a damaged record. It is
// answered as unavailable, like a failed read, and never as a job without
// commands or a job that did not run.
var errJobAttemptMissing = errors.New("the attempt the job names is missing")

const (
	jobChecksUnreadable  = "The job's captured commands could not be read."
	jobAttemptUnreadable = "The configured-check attempt could not be read."
)

// jobChecks reads the commands a job captured when it was admitted. A runner
// executes them and an owner reads them.
func (app *App) jobChecks(ctx context.Context, job state.CheckJob) ([]state.CheckDefinition, error) {
	configuration, exists, err := app.Store.CheckConfiguration(ctx, job.RepositoryID, job.ConfigurationVersion)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, state.ErrCheckConfigurationMissing
	}
	return configuration.Checks, nil
}

// jobAttempt reads the attempt a job names. The caller checks that the job
// names one; a job without an attempt is the one honest "nothing ran".
func (app *App) jobAttempt(ctx context.Context, job state.CheckJob) (state.CheckAttempt, error) {
	attempt, exists, err := app.Store.CheckAttemptByID(ctx, job.RepositoryID, job.AttemptID)
	if err != nil {
		return state.CheckAttempt{}, err
	}
	if !exists {
		return state.CheckAttempt{}, errJobAttemptMissing
	}
	return attempt, nil
}

// writeJobRecordError answers a request that needed a record of a job. The
// cause can name internal state, so it stays in the server log, and the client
// receives unreadable or the missing record's fixed message.
func writeJobRecordError(writer http.ResponseWriter, request *http.Request, err error, unreadable string, details any) {
	message := unreadable
	switch {
	case errors.Is(err, state.ErrCheckConfigurationMissing):
		message = "The job's captured commands are missing."
	case errors.Is(err, errJobAttemptMissing):
		message = "The attempt this job names is missing."
	}
	writeAPIError(writer, unavailable(request, "job record read", err), "state_unavailable", message, details)
}

func (app *App) policyResponse(policy state.CheckPolicy) checkapi.PolicyResponse {
	return checkapi.PolicyResponse{OK: true, Policy: policyJSON(policy), Runtime: app.checkRuntimeStatus()}
}

func (app *App) checkRuntimeStatus() checkapi.RuntimeStatus {
	if app.CheckRuntimeUnavailableCode == "" {
		return checkapi.RuntimeStatus{Available: true}
	}
	reason := app.CheckRuntimeUnavailableReason
	if reason == "" {
		reason = "Configured checks are unavailable. Repair the check runtime and restart OwnGit."
	}
	return checkapi.RuntimeStatus{
		Available: false, UnavailableCode: app.CheckRuntimeUnavailableCode,
		UnavailableReason: reason,
	}
}

func policyJSON(policy state.CheckPolicy) *checkapi.Policy {
	return &checkapi.Policy{
		RepositoryID: policy.RepositoryID, Version: policy.Version, Digest: policy.Digest, Executor: policy.Executor,
		AllowedEvents: policy.AllowedEvents, MaxTimeoutMS: policy.MaxTimeoutMS, MaxOutputLimitBytes: policy.MaxOutputLimitBytes,
		QueueLimit: policy.QueueLimit, MaxActiveJobs: policy.MaxActiveJobs, MaxLeaseMS: policy.MaxLeaseMS,
		Execution: policy.Execution, ConsentVersion: policy.ConsentVersion, ConsentActive: policy.ConsentActive,
		RunnerGeneration: policy.RunnerGeneration, CreatedAt: policy.CreatedAt, UpdatedAt: policy.UpdatedAt,
	}
}

// invalidCredentialInput states the input rules shared by runner and helper
// credential issuance.
const invalidCredentialInput = "The label must be one line of UTF-8 text of at most 100 bytes, and a creation identifier must be 32 lowercase hex characters."

func runnerCredentialJSON(credential state.RunnerCredential) *checkapi.RunnerCredential {
	return &checkapi.RunnerCredential{
		ID: credential.ID, RepositoryID: credential.RepositoryID, Label: credential.Label, CreationID: credential.CreationID,
		Generation: credential.Generation, CreatedAt: credential.CreatedAt, RevokedAt: credential.RevokedAt, LastUsedAt: credential.LastUsedAt,
	}
}

// runnerSourceBusyWait bounds how long one runner source request waits while a
// push or another repository write holds the repository. It stays below the
// runner's request timeout, so the runner receives an answer.
const runnerSourceBusyWait = 20 * time.Second

// errSourceBlobMismatch is a source blob read that did not return the
// authorized entry whole: another object, a symbolic link or special mode,
// or more bytes than the read could hold.
var errSourceBlobMismatch = errors.New("the source blob read does not match the authorized entry")

func sourceLimits(limits state.CheckSourceLimits) checksource.Limits {
	return checksource.Limits{
		MaxEntries: limits.MaxEntries, MaxFileBytes: limits.MaxFileBytes, MaxTotalBytes: limits.MaxTotalBytes,
		MaxPathDepth: limits.MaxPathDepth, MaxPathBytes: limits.MaxPathBytes, MaxNameBytes: limits.MaxNameBytes,
		MetadataLimit: limits.MetadataLimit,
	}
}

// errRerunSourceMissing reports that the commit a job ran is no longer in
// the repository, so the job cannot run again.
var errRerunSourceMissing = errors.New("the job's commit is no longer in the repository")

// errRerunWorkflowChanged reports that the workflow file at the job's commit
// is not the one the job recorded, so the job cannot run again.
var errRerunWorkflowChanged = errors.New("the job's workflow file no longer matches the one the job recorded")

// rerunCheckJob queues a rerun of job jobID while the repository read lock
// shows the job's commit is still present. Unused object cleanup reads the
// unfinished jobs under the write lock, so it either sees the rerun and
// waits, or already removed the commit and the rerun is refused.
func (app *App) rerunCheckJob(ctx context.Context, repositoryID, jobID string) (state.CheckJob, bool, error) {
	original, found, err := app.Store.CheckJob(ctx, repositoryID, jobID)
	if err != nil {
		return state.CheckJob{}, false, err
	}
	if !found {
		return state.CheckJob{}, false, state.ErrCheckJobNotFound
	}
	var job state.CheckJob
	var deduped bool
	err = checksource.RetryWhileRepositoryBusy(ctx, runnerSourceBusyWait, func() error {
		pinned, err := app.Repositories.PinRepository(ctx, repositoryID, original.SourceOID, original.SourceOID)
		if err != nil {
			return err
		}
		return pinned.WhilePresent(ctx, func() (err error) {
			requested, err := rerunRequestedLimits(ctx, pinned, original)
			if err != nil {
				return err
			}
			job, deduped, err = app.Store.RerunCheckJob(ctx, repositoryID, jobID, requested, app.now())
			return err
		})
	})
	if errors.Is(err, repository.ErrPinnedObjectUnavailable) {
		err = errRerunSourceMissing
	}
	return job, deduped, err
}

// rerunRequestedLimits reads the timeout and output limit the job's workflow
// asked for, so the rerun applies the policy's current caps to them and not
// to the limits the original job ended up with. A workflow that cannot be
// read at the job's commit gives nil, with one log line, and the rerun keeps
// the job's own limits. A workflow that differs from the one the job recorded
// is an error, and so are a busy repository (the caller retries) and a
// canceled request.
func rerunRequestedLimits(ctx context.Context, pinned *repository.PinnedRepository, original state.CheckJob) (*state.CheckJobLimits, error) {
	// A job migrated from an earlier version records no source limits.
	metadataLimit := original.Execution.Source.MetadataLimit
	if metadataLimit <= 0 {
		metadataLimit = state.DefaultCheckSourceLimits().MetadataLimit
	}
	blob, document, err := checkrun.ReadPinnedWorkflow(ctx, pinned, metadataLimit)
	if errors.Is(err, repository.ErrPinnedRepositoryBusy) || ctx.Err() != nil {
		if err == nil {
			err = ctx.Err()
		}
		return nil, err
	}
	if err != nil {
		log.Printf("configured check rerun %s keeps its recorded limits: its workflow could not be read: %s", original.ID, logtext.Cause(err))
		return nil, nil
	}
	digest := sha256.Sum256(blob.Content)
	if hex.EncodeToString(digest[:]) != original.WorkflowDigest || (original.WorkflowOID != "" && blob.OID != original.WorkflowOID) {
		return nil, errRerunWorkflowChanged
	}
	return &state.CheckJobLimits{TimeoutMS: document.Limits.TimeoutMS, OutputLimitBytes: document.Limits.OutputLimitBytes}, nil
}

func writeConfiguredJobError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, state.ErrCheckJobNotFound):
		writeAPIError(writer, http.StatusNotFound, "check_job_not_found", "The configured-check job does not exist.", nil)
	case errors.Is(err, state.ErrCheckJobState), errors.Is(err, state.ErrCheckConsentRequired):
		writeAPIError(writer, http.StatusConflict, "check_job_state", err.Error(), nil)
	case errors.Is(err, errRerunSourceMissing):
		writeAPIError(writer, http.StatusConflict, "check_source_missing", "The job's commit is no longer in the repository, so the job cannot run again.", nil)
	case errors.Is(err, errRerunWorkflowChanged):
		writeAPIError(writer, http.StatusConflict, "check_workflow_changed", "The job's workflow file differs from the one the job recorded, so the job cannot run again.", nil)
	case errors.Is(err, state.ErrCheckCeilingExceeded):
		writeAPIError(writer, http.StatusConflict, "check_policy_above_ceilings", err.Error(), nil)
	case errors.As(err, new(*state.PolicyError)):
		writeSettingUnreadable(writer, request, "configured check job change", err)
	default:
		writeAPIError(writer, unavailable(request, "configured check job change", err), "state_unavailable", "The configured-check job could not be changed.", nil)
	}
}

func writeRunnerError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, state.ErrCheckJobNotFound):
		writeAPIError(writer, http.StatusNotFound, "check_job_not_found", "The configured-check job does not exist.", nil)
	case errors.Is(err, state.ErrCheckJobLease):
		writeAPIError(writer, http.StatusConflict, "check_job_lease", "The configured-check job lease is stale or foreign.", nil)
	case errors.Is(err, state.ErrCheckJobState), errors.Is(err, state.ErrCheckJobStartReplay):
		writeAPIError(writer, http.StatusConflict, "check_job_state", err.Error(), nil)
	case errors.Is(err, state.ErrCheckRunnerCredential):
		writeAPIError(writer, http.StatusUnauthorized, "invalid_runner_credential", "The runner token is unknown or revoked.", nil)
	case errors.Is(err, state.ErrCheckConsentRequired):
		writeAPIError(writer, http.StatusConflict, "check_consent_required", "Configured-check authority changed.", nil)
	default:
		// The cause can name internal state or host paths, so it stays in the
		// server log and the runner receives a fixed message.
		writeAPIError(writer, unavailable(request, "runner operation", err), "state_unavailable", "The runner operation failed.", nil)
	}
}
