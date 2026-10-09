package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"owngit/internal/actions"
	"owngit/internal/state"
)

func TestWorkflowRunnerCLI(t *testing.T) {
	for _, row := range []struct {
		name, shell, script string
		windows             bool
	}{
		{name: "default shell"},
		{name: "bash", shell: "bash", script: "printf '%s\\n' \"$PAYLOAD\"; echo CLI_ACTIONS_OK"},
		{name: "PowerShell", shell: "powershell", script: "Write-Output $env:PAYLOAD; Write-Output CLI_ACTIONS_OK", windows: true},
		{name: "cmd", shell: "cmd", script: "echo %PAYLOAD%\r\necho CLI_ACTIONS_OK", windows: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.windows && runtime.GOOS != "windows" {
				t.Skip("This shell requires Windows.")
			}
			fixture := newConfiguredCheckCLIFixture(t)
			_, err := fixture.store.SetCheckPolicy(fixture.ctx, state.CheckPolicyInput{
				RepositoryID: fixture.repository.ID, Executor: state.CheckExecutorExternalRunner, AllowedEvents: []string{"push"},
				MaxTimeoutMS: 60000, MaxOutputLimitBytes: 65536, MaxLeaseMS: 60000, MaxActiveJobs: 1, QueueLimit: 4,
			}, time.Now())
			noErr(t, err)
			_, err = fixture.store.GrantCheckConsent(fixture.ctx, fixture.repository.ID, time.Now())
			noErr(t, err)
			const secret = "synthetic-cli-workflow-secret"
			_, err = fixture.store.SetWorkflowSecret(fixture.ctx, fixture.repository.ID, "NAMED", secret, state.Actor{}, time.Now())
			noErr(t, err)
			script := row.script
			if script == "" {
				script = "printf '%s\\n' \"$PAYLOAD\"; echo CLI_ACTIONS_OK"
				if runtime.GOOS == "windows" {
					script = "Write-Output $env:PAYLOAD; Write-Output CLI_ACTIONS_OK"
				}
			}
			encoded, digest, err := actions.EncodePlan(actions.JobPlan{JobKey: "cli", SecretNames: []string{"NAMED"},
				Steps: []actions.Step{{Name: "CLI workflow", Run: script, Shell: row.shell, Env: map[string]string{"PAYLOAD": "${{ secrets.NAMED }}"}}}})
			noErr(t, err)
			name, command := actions.StepDisplay(actions.Step{Name: "CLI workflow", Run: script, Shell: row.shell})
			run, _, err := fixture.store.AdmitActionsRun(fixture.ctx, state.ActionsRunRequest{
				Run: state.ActionsRun{RepositoryID: fixture.repository.ID, WorkflowPath: ".github/workflows/cli.yml", Event: "push",
					EventKey: "cli/" + fixture.sourceOID, SourceOID: fixture.sourceOID, TriggerRef: "main", Facts: actions.RunFacts{WorkflowDigest: strings.Repeat("a", 64)}},
				Jobs: []state.ActionsJobRequest{{CheckJobRequest: state.CheckJobRequest{JobKey: "cli", PlanDigest: digest,
					WorkflowDigest: strings.Repeat("a", 64), Checks: []state.CheckDefinition{{Name: name, Command: command}}}, Plan: encoded}},
			}, time.Now())
			noErr(t, err)
			tokenFile := filepath.Join(fixture.root, "runner-token")
			configuredCheckCLIOutput(t, func() error {
				return runnerCredentialCommand(fixture.adminArguments("issue", "--label", "workflow runner", "--token-file", tokenFile))
			})
			noErr(t, runnerCommand([]string{"--server", fixture.httpServer.URL, "--accept-insecure-http", "--repository", fixture.repository.ID,
				"--token-file", tokenFile, "--workspace-root", filepath.Join(fixture.root, "runner-work"), "--once"}))
			jobs, err := fixture.store.ActionsRunJobs(fixture.ctx, fixture.repository.ID, run.ID)
			if err != nil || len(jobs) != 1 || jobs[0].Status != state.CheckJobPassed {
				t.Fatalf("workflow jobs=%d err=%v", len(jobs), err)
			}
			attempt, exists, err := fixture.store.CheckAttemptByID(fixture.ctx, fixture.repository.ID, jobs[0].AttemptID)
			if err != nil || !exists || len(attempt.Results) != 1 || attempt.Results[0].Role != actions.RoleRun || !strings.Contains(attempt.Results[0].OutputExcerpt, "CLI_ACTIONS_OK") || !strings.Contains(attempt.Results[0].OutputExcerpt, "[redacted]") || strings.Contains(attempt.Results[0].OutputExcerpt, secret) {
				t.Fatal("owngit runner did not record successful masked Actions evidence")
			}
		})
	}
}
