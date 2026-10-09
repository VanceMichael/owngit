package checkrunner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"owngit/internal/actions"
	"owngit/internal/actions/host"
	"owngit/internal/checkapi"
	"owngit/internal/checkexec"
)

func (runner *Runner) runActions(ctx context.Context, job *checkapi.Job, content []byte, workspace string) ([]checkexec.Result, []string, bool, string, error) {
	var answer checkapi.RunnerStartResponse
	if err := json.Unmarshal(content, &answer); err != nil || answer.Actions == nil || answer.Job == nil || answer.Job.ID != job.ID || answer.Job.LeaseID != job.LeaseID {
		return nil, nil, false, "", errors.New("workflow.start_unreadable: the one-shot start answer is invalid")
	}
	grant := answer.Actions
	plan, err := actions.DecodePlan([]byte(grant.Plan), job.PlanDigest)
	if err != nil || plan.JobKey != job.JobKey || plan.MatrixIndex != job.MatrixIndex || len(plan.Steps) != len(job.Checks) || len(job.Checks) == 0 || grant.RunID != job.RunID || grant.RunNumber < 1 || grant.RunAttempt < 1 {
		return nil, nil, false, "", errors.New("workflow.start_unreadable: the one-shot start plan or run identity is invalid")
	}
	for index, step := range plan.Steps {
		name, command := actions.StepDisplay(step)
		if name != job.Checks[index].Name || command != job.Checks[index].Command {
			return nil, nil, false, "", errors.New("workflow.start_unreadable: the plan does not match the captured step definitions")
		}
	}
	execution := host.RunJob(ctx, plan, actions.RunOptions{
		Workspace: workspace, Identity: actions.RunIdentity{ID: grant.RunID, Number: grant.RunNumber, Attempt: grant.RunAttempt},
		Secrets: grant.Secrets, Evaluator: evaluateRunnerStep, RunnerName: runner.RepositoryID,
		MaxTimeout: time.Duration(job.Limits.TimeoutMS) * time.Millisecond, OutputLimit: job.Limits.OutputLimitBytes,
	}, "")
	results := make([]checkexec.Result, len(job.Checks))
	roles := make([]string, len(job.Checks))
	notes := checkapi.LogBuffer{Limit: maximumRunnerLog}
	for _, note := range execution.Notes {
		notes.Add(note.Code + ": " + note.Detail + "\n")
	}
	for index, check := range job.Checks {
		results[index] = checkexec.Result{Name: check.Name, Command: check.Command, Status: actions.StatusSkipped}
		roles[index] = actions.RoleRun
	}
	facts := execution.Evidence()
	for index, step := range execution.Steps {
		if step.Index < 0 || step.Index >= len(results) {
			return nil, nil, false, "", errors.New("workflow.execution: the step engine returned an invalid step identity")
		}
		result := &results[step.Index]
		result.Status, result.ExitCode, result.Duration = facts[index].Status, step.ExitCode, step.Duration
		result.Output, result.OutputGap, result.Truncated, result.CleanupError = step.Output, step.OutputGap, step.Truncated, step.CleanupError
		roles[step.Index] = facts[index].Role
		for _, note := range step.Notes {
			notes.Add(note.Code + ": " + note.Detail + "\n")
		}
	}
	if execution.Error != "" {
		results[0].Status, results[0].Output = actions.StatusError, execution.Error
	}
	text, _ := notes.Result()
	return results, roles, execution.Cancelled, text, nil
}

func evaluateRunnerStep(key, text string, values map[string]any) (any, error) {
	success, _ := values["success"].(bool)
	failure, _ := values["failure"].(bool)
	cancelled, _ := values["cancelled"].(bool)
	context := actions.EvalContext{Values: values, Success: success, Failure: failure, Cancelled: cancelled}
	if key == "step.if" {
		return actions.Eval(text, key, context)
	}
	return actions.EvalTemplate(text, key, context)
}
