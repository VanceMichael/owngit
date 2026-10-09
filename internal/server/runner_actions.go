package server

import (
	"net/http"

	"owngit/internal/actions"
	"owngit/internal/checkapi"
	"owngit/internal/state"
)

func (app *App) preflightRunnerActions(writer http.ResponseWriter, request *http.Request, repositoryID string, authority state.CheckJobCompletionAuthority) (*checkapi.RunnerActionsGrant, bool) {
	job, exists, err := app.Store.CheckJob(request.Context(), repositoryID, authority.JobID)
	if err != nil || !exists {
		if err == nil {
			err = state.ErrCheckJobNotFound
		}
		writeRunnerError(writer, request, err)
		return nil, false
	}
	if job.RunID == "" || job.Status != state.CheckJobClaimed {
		return nil, true
	}
	if _, err := app.Store.AuthorizeCheckJobSource(request.Context(), repositoryID, job.ID, authority.LeaseID, authority.CredentialID, authority.CredentialGeneration, app.now()); err != nil {
		writeRunnerError(writer, request, err)
		return nil, false
	}
	fail := func(code, message string) (*checkapi.RunnerActionsGrant, bool) {
		if _, err := app.Store.FailCheckJobBeforeStart(request.Context(), authority, state.CheckJobError, code+": "+message, app.now()); err != nil {
			writeRunnerError(writer, request, err)
		} else {
			writeAPIError(writer, http.StatusServiceUnavailable, code, message, nil)
		}
		return nil, false
	}
	const unreadablePlan = "The workflow plan could not be read, so this job did not start."
	encoded, exists, err := app.Store.ActionsJobPlan(request.Context(), repositoryID, job.ID)
	if err != nil || !exists {
		return fail("workflow.plan_unreadable", unreadablePlan)
	}
	plan, err := actions.DecodePlan(encoded, job.PlanDigest)
	if err != nil {
		return fail("workflow.plan_unreadable", unreadablePlan)
	}
	run, exists, err := app.Store.ActionsRun(request.Context(), repositoryID, job.RunID)
	if err != nil || !exists {
		return fail("workflow.plan_unreadable", unreadablePlan)
	}
	secrets, err := app.Store.ReadWorkflowSecrets(request.Context(), repositoryID, plan.SecretNames)
	if err != nil {
		return fail("workflow.secrets_unreadable", "The secrets of this repository could not be read, so this job did not start. An administrator can check them on the Secrets page.")
	}
	return &checkapi.RunnerActionsGrant{
		Plan: string(encoded), RunID: run.ID, RunNumber: run.Number, RunAttempt: run.RerunGeneration + 1, Secrets: secrets,
	}, true
}
