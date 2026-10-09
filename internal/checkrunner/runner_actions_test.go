package checkrunner_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"owngit/internal/actions"
	"owngit/internal/apiclient"
	"owngit/internal/checkapi"
	"owngit/internal/state"
)

func admitRunnerWorkflow(t *testing.T, fixture *runnerIntegrationFixture, plan actions.JobPlan) state.CheckJob {
	t.Helper()
	plan.JobKey = "workflow"
	encoded, digest, err := actions.EncodePlan(plan)
	noErr(t, err)
	checks := make([]state.CheckDefinition, len(plan.Steps))
	for index, step := range plan.Steps {
		name, command := actions.StepDisplay(step)
		checks[index] = state.CheckDefinition{Name: name, Command: command}
	}
	run, reused, err := fixture.store.AdmitActionsRun(fixture.ctx, state.ActionsRunRequest{
		Run: state.ActionsRun{RepositoryID: fixture.repository.ID, WorkflowPath: ".github/workflows/ci.yml",
			Event: "push", EventKey: "workflow/" + fixture.job.SourceOID, SourceOID: fixture.job.SourceOID,
			TriggerRef: "main", Facts: actions.RunFacts{WorkflowDigest: strings.Repeat("b", 64)}},
		Jobs: []state.ActionsJobRequest{{CheckJobRequest: state.CheckJobRequest{JobKey: "workflow", PlanDigest: digest,
			WorkflowDigest: strings.Repeat("b", 64), Checks: checks}, Plan: encoded}},
	}, fixture.job.AdmittedAt.Add(-time.Second))
	if err != nil || reused {
		t.Fatalf("admit workflow: reused=%v err=%v", reused, err)
	}
	jobs, err := fixture.store.ActionsRunJobs(fixture.ctx, fixture.repository.ID, run.ID)
	noErr(t, err)
	if len(jobs) != 1 {
		t.Fatalf("workflow jobs=%d", len(jobs))
	}
	return jobs[0]
}

func TestRunnerClaimProtocol(t *testing.T) {
	for _, row := range []struct {
		name     string
		input    any
		workflow bool
		only     bool
		invalid  bool
	}{
		{name: "invalid features", input: map[string]any{"features": true}, invalid: true},
		{name: "only unsupported workflow", only: true},
		{name: "released empty claim"},
		{name: "empty features", input: map[string]any{"features": []string{}}},
		{name: "unknown feature", input: map[string]any{"features": []string{"future-v1"}}},
		{name: "workflow feature", input: map[string]any{"features": []string{"workflows-v1"}}, workflow: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			fixture := newRunnerIntegrationFixture(t, "echo json-only")
			workflow := admitRunnerWorkflow(t, fixture, actions.JobPlan{Steps: []actions.Step{{Name: "workflow", Run: "echo actions-only"}}})
			if row.only {
				_, err := fixture.store.CancelCheckJob(fixture.ctx, fixture.repository.ID, fixture.job.ID, time.Now())
				noErr(t, err)
			}
			httpServer, origin := fixture.startHTTPServer(nil)
			defer httpServer.Close()
			content, err := fixture.client(origin).Do(fixture.ctx, http.MethodPost,
				"/api/v1/repositories/"+fixture.repository.ID+"/runner/claim", row.input)
			if row.invalid {
				if err == nil || fixture.readJob().Status != state.CheckJobPending {
					t.Fatal("an invalid claim was accepted or changed the queue")
				}
				return
			}
			noErr(t, err)
			var answer checkapi.JobResponse
			noErr(t, json.Unmarshal(content, &answer))
			want := fixture.job.ID
			if row.workflow {
				want = workflow.ID
			}
			if row.only {
				if answer.Job != nil {
					t.Fatal("an unsupported workflow was claimed")
				}
			} else if answer.Job == nil || answer.Job.ID != want {
				t.Fatal("claim did not select the expected lane")
			}
			if !row.workflow {
				pending, exists, err := fixture.store.CheckJob(fixture.ctx, fixture.repository.ID, workflow.ID)
				noErr(t, err)
				if !exists || pending.Status != state.CheckJobPending || !strings.Contains(pending.Summary, "note.runner_old") {
					t.Fatalf("unsupported workflow: status=%s summary=%q", pending.Status, pending.Summary)
				}
			}
		})
	}
}

func TestRunnerStartProtocol(t *testing.T) {
	for _, row := range []struct {
		name, code string
		large      bool
		change     func(*testing.T, *runnerIntegrationFixture, state.CheckJob)
	}{
		{name: "plan and named secrets"},
		{name: "full secret capacity", large: true},
		{name: "corrupt plan", code: "workflow.plan_unreadable", change: func(t *testing.T, f *runnerIntegrationFixture, job state.CheckJob) {
			noErr(t, f.store.Exec(f.ctx, `UPDATE actions_job_plans SET plan_json='{}' WHERE job_id=?`, job.ID))
		}},
		{name: "missing plan", code: "workflow.plan_unreadable", change: func(t *testing.T, f *runnerIntegrationFixture, job state.CheckJob) {
			noErr(t, f.store.Exec(f.ctx, `DELETE FROM actions_job_plans WHERE job_id=?`, job.ID))
		}},
		{name: "corrupt secrets", code: "workflow.secrets_unreadable", change: func(t *testing.T, f *runnerIntegrationFixture, _ state.CheckJob) {
			noErr(t, os.WriteFile(filepath.Join(f.root, "state", "workflow-secrets", f.repository.ID+".json"), []byte("{"), 0o600))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fixture := newRunnerIntegrationFixture(t, "echo json-only")
			const value = "synthetic-delivered-secret"
			names, saved, delivered := []string{"NAMED", "UNSET"}, []string{"NAMED", "UNREQUESTED"}, value
			if row.large {
				names = make([]string, state.MaxWorkflowSecrets)
				for index := range names {
					names[index] = fmt.Sprintf("SECRET_%d", index)
				}
				saved, delivered = names, strings.Repeat("\x01", state.MaxWorkflowSecretValueBytes)
			}
			for _, name := range saved {
				_, err := fixture.store.SetWorkflowSecret(fixture.ctx, fixture.repository.ID, name, delivered, state.Actor{}, time.Now())
				noErr(t, err)
			}
			workflow := admitRunnerWorkflow(t, fixture, actions.JobPlan{Steps: []actions.Step{{Name: "secret", Run: "echo ${{ secrets.NAMED }}\n# \u202e"}}, SecretNames: names})
			job, found, err := fixture.store.ClaimCheckJob(fixture.ctx, fixture.repository.ID, fixture.credential.ID, time.Now(), state.RunnerFeatureWorkflowsV1)
			if err != nil || !found || job.ID != workflow.ID {
				t.Fatalf("claim found=%v err=%v", found, err)
			}
			httpServer, origin := fixture.startHTTPServer(nil)
			defer httpServer.Close()
			if row.change != nil {
				row.change(t, fixture, job)
			}
			path := "/api/v1/repositories/" + fixture.repository.ID + "/runner/jobs/" + job.ID + "/start"
			input := checkapi.RunnerStartInput{LeaseID: job.LeaseID, AttemptID: stateIDForTest(t)}
			client := fixture.client(origin)
			client.MaximumResponse = checkapi.MaximumRunnerStartBytes
			content, err := client.DoWithHeaders(fixture.ctx, http.MethodPost, path, input, map[string]string{"X-OwnGit-Runner-Lease": job.LeaseID})
			stored, exists, readErr := fixture.store.CheckJob(fixture.ctx, fixture.repository.ID, job.ID)
			noErr(t, readErr)
			if row.code != "" {
				var problem *apiclient.Error
				if !errors.As(err, &problem) || problem.Code != row.code || !exists || stored.Status != state.CheckJobError || stored.StartedAt != nil || stored.AttemptID != "" || !strings.Contains(stored.Summary, row.code) {
					t.Fatalf("preflight code=%s status=%s started=%v attempt=%v err=%v", row.code, stored.Status, stored.StartedAt != nil, stored.AttemptID != "", err)
				}
			} else {
				noErr(t, err)
				var answer checkapi.RunnerStartResponse
				noErr(t, json.Unmarshal(content, &answer))
				if answer.Actions == nil || answer.Actions.RunID != workflow.RunID || answer.Actions.RunNumber != 1 || answer.Actions.RunAttempt != 1 || len(answer.Actions.Secrets) != len(names) || stored.Status != state.CheckJobStarted {
					t.Fatal("start did not grant exactly the plan, run identity and named secrets")
				}
				for _, name := range saved {
					if name != "UNREQUESTED" && answer.Actions.Secrets[name] != delivered {
						t.Fatal("a named secret was not delivered exactly")
					}
				}
				if !row.large && (answer.Actions.Secrets["UNSET"] != "" || answer.Actions.Secrets["UNREQUESTED"] != "") || row.large && len(content) <= 4<<20 {
					t.Fatal("secret selection or the maximum-size grant is incorrect")
				}
				_, err = actions.DecodePlan([]byte(answer.Actions.Plan), workflow.PlanDigest)
				noErr(t, err)
			}
			if _, err := client.DoWithHeaders(fixture.ctx, http.MethodPost, path, input, map[string]string{"X-OwnGit-Runner-Lease": job.LeaseID}); err == nil {
				t.Fatal("a second start grant was issued")
			}
		})
	}
}

func TestRunnerWorkflowExecution(t *testing.T) {
	for _, row := range []struct {
		name, status string
		steps        []actions.Step
		roles        []string
		missing      string
		timeout      string
	}{
		{name: "secret and step output", status: state.CheckJobPassed, steps: []actions.Step{
			{ID: "first", Name: "secret", Run: "echo ${{ secrets.NAMED }}\necho answer=ready >> \"$GITHUB_OUTPUT\"", Shell: "bash"},
			{Name: "output", Run: "echo ${{ steps.first.outputs.answer }}", Shell: "bash"},
		}, roles: []string{actions.RoleRun, actions.RoleRun}},
		{name: "failure stops ordinary steps", status: state.CheckJobFailed, steps: []actions.Step{
			{Name: "fail", Run: "exit 7"}, {Name: "skipped", Run: "echo NEVER_RUN"},
			{Name: "failure", If: "failure()", Run: "echo failure-observed"},
		}, roles: []string{actions.RoleRun, actions.RoleRun, actions.RoleRun}, missing: "NEVER_RUN"},
		{name: "tolerated step", status: state.CheckJobPassed, steps: []actions.Step{
			{Name: "tolerated", Run: "exit 7", ContinueOnError: "true"}, {Name: "next", Run: "echo continued"},
		}, roles: []string{actions.RoleTolerated, actions.RoleRun}},
		{name: "source changed before builtin", status: state.CheckJobIncomplete, steps: []actions.Step{
			{Name: "change", Run: "echo changed > source.txt"}, {Name: "checkout", Uses: "actions/checkout@v4"},
		}, roles: []string{actions.RoleRun, actions.RoleBuiltin}},
		{name: "source changed before skipped", status: state.CheckJobIncomplete, steps: []actions.Step{
			{Name: "change", Run: "echo changed > source.txt"}, {Name: "skipped", Run: "echo NEVER_RUN", If: "false"},
		}, roles: []string{actions.RoleRun, actions.RoleRun}},
		{name: "tolerated source change", status: state.CheckJobIncomplete, steps: []actions.Step{
			{Name: "change", Run: "echo changed > source.txt; exit 7", ContinueOnError: "true"}, {Name: "checkout", Uses: "actions/checkout@v4"},
		}, roles: []string{actions.RoleRun, actions.RoleBuiltin}},
		{name: "timeout before first step", status: state.CheckJobIncomplete, timeout: "0.000000000001", steps: []actions.Step{
			{Name: "timed out", Run: "echo NEVER_RUN"},
		}, roles: []string{actions.RoleRun}, missing: "NEVER_RUN"},
		{name: "ordinary skipped steps", status: state.CheckJobSkipped, steps: []actions.Step{
			{Name: "skipped", Run: "echo NEVER_RUN", If: "false"},
		}, roles: []string{actions.RoleRun}, missing: "NEVER_RUN"},
		{name: "builtins run nothing", status: state.CheckJobSkipped, steps: []actions.Step{
			{Name: "checkout", Uses: "actions/checkout@v4"}, {Name: "cache", Uses: "actions/cache@v4"},
		}, roles: []string{actions.RoleBuiltin, actions.RoleBuiltin}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fixture := newRunnerIntegrationFixture(t, "echo json-only")
			const secret = "synthetic-workflow-secret"
			_, err := fixture.store.SetWorkflowSecret(fixture.ctx, fixture.repository.ID, "NAMED", secret, state.Actor{}, time.Now())
			noErr(t, err)
			job := admitRunnerWorkflow(t, fixture, actions.JobPlan{Steps: row.steps, SecretNames: []string{"NAMED"}, TimeoutMinutes: row.timeout})
			httpServer, origin := fixture.startHTTPServer(nil)
			defer httpServer.Close()
			noErr(t, fixture.runner(fixture.client(origin)).Run(fixture.ctx))
			stored, exists, err := fixture.store.CheckJob(fixture.ctx, fixture.repository.ID, job.ID)
			if err != nil || !exists || stored.Status != row.status || stored.AttemptID == "" {
				t.Fatalf("workflow status=%s want=%s exists=%v err=%v", stored.Status, row.status, exists, err)
			}
			attempt, exists, err := fixture.store.CheckAttemptByID(fixture.ctx, fixture.repository.ID, stored.AttemptID)
			if err != nil || !exists || attempt.Status != row.status || len(attempt.Results) != len(row.roles) {
				t.Fatalf("attempt exists=%v status=%s want=%s results=%d err=%v", exists, attempt.Status, row.status, len(attempt.Results), err)
			}
			if row.name == "secret and step output" && (!strings.Contains(attempt.Results[0].OutputExcerpt, "[redacted]") || !strings.Contains(attempt.Results[1].OutputExcerpt, "ready")) {
				t.Fatalf("runtime delivery: masked=%v output_resolved=%v", strings.Contains(attempt.Results[0].OutputExcerpt, "[redacted]"), strings.Contains(attempt.Results[1].OutputExcerpt, "ready"))
			}
			for index, result := range attempt.Results {
				if result.Role != row.roles[index] || strings.Contains(result.OutputExcerpt+result.CleanupError, secret) || row.missing != "" && strings.Contains(result.OutputExcerpt, row.missing) {
					t.Fatalf("step %d role=%s or output boundary is incorrect", index, result.Role)
				}
			}
			_, found, err := fixture.store.ActionsJobPlan(fixture.ctx, fixture.repository.ID, job.ID)
			noErr(t, err)
			if found || fixture.readJob().Status != state.CheckJobPending {
				t.Fatal("completion kept the local plan or executed the next JSON job")
			}
		})
	}
}

func TestRunnerLostStartAnswer(t *testing.T) {
	for _, row := range []struct {
		name      string
		lost      bool
		oversized bool
	}{
		{name: "lost connection", lost: true},
		{name: "damaged grant"},
		{name: "over-limit answer", oversized: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			fixture := newRunnerIntegrationFixture(t, "echo json-only")
			job := admitRunnerWorkflow(t, fixture, actions.JobPlan{Steps: []actions.Step{{Name: "never", Run: "echo NEVER_RUN"}}})
			var starts atomic.Int32
			httpServer, origin := fixture.startHTTPServer(func(handler http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if strings.HasSuffix(request.URL.Path, "/start") {
						starts.Add(1)
						handler.ServeHTTP(httptest.NewRecorder(), request)
						if row.lost {
							connection, _, err := writer.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = connection.Close()
						} else {
							writer.Header().Set("Content-Type", "application/json")
							if row.oversized {
								_, _ = writer.Write([]byte(`{"ok":true,"padding":"` + strings.Repeat("v", checkapi.MaximumRunnerStartBytes) + `"}`))
							} else {
								_, _ = writer.Write([]byte(`{"ok":true}`))
							}
						}
						return
					}
					handler.ServeHTTP(writer, request)
				})
			})
			defer httpServer.Close()
			err := fixture.runner(fixture.client(origin)).Run(fixture.ctx)
			if err == nil {
				t.Fatal("an unreadable start answer was treated as a grant")
			}
			if row.oversized {
				var problem *apiclient.Error
				if !errors.As(err, &problem) || problem.Code != "response_too_large" {
					t.Fatalf("over-limit answer error=%v", err)
				}
			}
			_, err = fixture.store.ExpireCheckJobLeases(fixture.ctx, time.Now().Add(time.Hour))
			noErr(t, err)
			stored, exists, err := fixture.store.CheckJob(fixture.ctx, fixture.repository.ID, job.ID)
			if err != nil || !exists || stored.Status != state.CheckJobAmbiguous || stored.StartedAt == nil || starts.Load() != 1 {
				t.Fatalf("lost grant status=%s starts=%d err=%v", stored.Status, starts.Load(), err)
			}
			_, _, err = fixture.store.StartCheckJob(fixture.ctx, state.CheckJobStart{RepositoryID: fixture.repository.ID, JobID: job.ID,
				LeaseID: stored.LeaseID, CredentialID: fixture.credential.ID, CredentialGeneration: fixture.credential.Generation,
				AttemptID: stateIDForTest(t), Protection: state.ProtectionRunnerReported}, time.Now().Add(time.Hour))
			if !errors.Is(err, state.ErrCheckJobStartReplay) {
				t.Fatalf("replayed grant err=%v", err)
			}
			attempt, exists, err := fixture.store.CheckAttemptByID(fixture.ctx, fixture.repository.ID, stored.AttemptID)
			if err != nil || !exists || len(attempt.Results) != 0 {
				t.Fatal("execution was reported despite a lost grant")
			}
		})
	}
}

func TestRunnerCompletionRoles(t *testing.T) {
	for _, row := range []struct {
		name, role, status string
		workflow           bool
	}{
		{name: "JSON role", role: actions.RoleRun, status: actions.StatusPassed},
		{name: "JSON skipped", status: actions.StatusSkipped},
		{name: "JSON not run", status: actions.StatusNotRun},
		{name: "workflow unknown role", workflow: true, role: "unknown", status: actions.StatusPassed},
	} {
		t.Run(row.name, func(t *testing.T) {
			fixture := newRunnerIntegrationFixture(t, "echo json-only")
			if row.workflow {
				admitRunnerWorkflow(t, fixture, actions.JobPlan{Steps: []actions.Step{{Name: "workflow", Run: "echo actions-only"}}})
			}
			job, found, err := fixture.store.ClaimCheckJob(fixture.ctx, fixture.repository.ID, fixture.credential.ID, time.Now(), state.RunnerFeatureWorkflowsV1)
			if err != nil || !found {
				t.Fatalf("claim found=%v err=%v", found, err)
			}
			_, attempt, err := fixture.store.StartCheckJob(fixture.ctx, state.CheckJobStart{RepositoryID: fixture.repository.ID, JobID: job.ID,
				LeaseID: job.LeaseID, CredentialID: fixture.credential.ID, CredentialGeneration: fixture.credential.Generation,
				AttemptID: stateIDForTest(t), Protection: state.ProtectionRunnerReported}, time.Now())
			noErr(t, err)
			httpServer, origin := fixture.startHTTPServer(nil)
			defer httpServer.Close()
			check := attempt.Checks[0]
			input := checkapi.RunnerCompletionInput{LeaseID: job.LeaseID, Completion: checkapi.AttemptCompletion{
				WorktreeState: state.WorktreeClean, FinishedAt: time.Now(), Results: []checkapi.Result{{Name: check.Name,
					Command: check.Command, Role: row.role, Status: row.status}},
			}}
			_, err = fixture.client(origin).DoWithHeaders(fixture.ctx, http.MethodPost,
				"/api/v1/repositories/"+fixture.repository.ID+"/runner/jobs/"+job.ID+"/complete", input, map[string]string{"X-OwnGit-Runner-Lease": job.LeaseID})
			if err == nil {
				t.Fatal("invalid role or lane-specific status was accepted")
			}
			stored, exists, err := fixture.store.CheckAttemptByID(fixture.ctx, fixture.repository.ID, attempt.ID)
			if err != nil || !exists || stored.Status != state.AttemptPending || len(stored.Results) != 0 {
				t.Fatal("a refused completion changed the pending attempt")
			}
		})
	}
}

func TestRunnerAgainstReleasedServerProtocol(t *testing.T) {
	fixture := newRunnerIntegrationFixture(t, "echo released-server-ok")
	httpServer, origin := fixture.startHTTPServer(func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if strings.HasSuffix(request.URL.Path, "/claim") {
				var input checkapi.RunnerClaimInput
				if err := json.NewDecoder(request.Body).Decode(&input); err != nil || len(input.Features) != 1 || input.Features[0] != state.RunnerFeatureWorkflowsV1 {
					t.Error("the new runner did not advertise workflow support")
				}
				_ = request.Body.Close()
				// The released server ignores a claim body and returns JSON jobs.
				request.Body, request.ContentLength = http.NoBody, 0
			}
			handler.ServeHTTP(writer, request)
		})
	})
	defer httpServer.Close()
	noErr(t, fixture.runner(fixture.client(origin)).Run(fixture.ctx))
	if fixture.readJob().Status != state.CheckJobPassed {
		t.Fatal("the new runner did not complete the released JSON protocol")
	}
}
