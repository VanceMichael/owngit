package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"owngit/internal/checkworkflow"
)

type checkJobFixture struct {
	store *Store
	now   time.Time
}

func newCheckJobFixture(t *testing.T) *checkJobFixture {
	t.Helper()
	store := openTestStore(t)
	completeTestSetup(t, store)
	now := time.Unix(1_800_000_000, 0)
	noErr(t, store.AddRepository(context.Background(), Repository{ID: "project", Name: "Project", CreatedAt: now}))
	noErr(t, store.AddRepository(context.Background(), Repository{ID: "other", Name: "Other", CreatedAt: now}))
	return &checkJobFixture{store: store, now: now}
}

func defaultPolicyInput() CheckPolicyInput {
	return CheckPolicyInput{
		RepositoryID: "project", Executor: CheckExecutorExternalRunner,
		AllowedEvents: []string{"push", "pull_request"},
		MaxTimeoutMS:  600000, MaxOutputLimitBytes: 65536,
		QueueLimit: 4, MaxActiveJobs: 1, MaxLeaseMS: 60000,
	}
}

func (fixture *checkJobFixture) setPolicy(t *testing.T, mutate func(*CheckPolicyInput)) CheckPolicy {
	t.Helper()
	input := defaultPolicyInput()
	if mutate != nil {
		mutate(&input)
	}
	policy, err := fixture.store.SetCheckPolicy(context.Background(), input, fixture.now)
	if err != nil {
		t.Fatalf("set policy: %v", err)
	}
	return policy
}

func (fixture *checkJobFixture) grantConsent(t *testing.T) CheckPolicy {
	t.Helper()
	policy, err := fixture.store.GrantCheckConsent(context.Background(), "project", fixture.now)
	if err != nil {
		t.Fatalf("grant consent: %v", err)
	}
	return policy
}

func (fixture *checkJobFixture) issueRunner(t *testing.T) (RunnerCredential, string) {
	t.Helper()
	credential, token, created, err := fixture.store.IssueCheckRunnerToken(context.Background(), "project", "laptop", "", fixture.now)
	if err != nil || !created || token == "" {
		t.Fatalf("issue runner token created=%v err=%v", created, err)
	}
	return credential, token
}

func pushJobRequest() CheckJobRequest {
	return CheckJobRequest{
		RepositoryID: "project", Trigger: "push", EventKey: "refs/heads/main@" + strings.Repeat("a", 40),
		SourceOID: strings.Repeat("a", 40), TriggerRef: "main",
		WorkflowDigest: strings.Repeat("b", 64),
		Checks:         []CheckDefinition{{Name: "unit", Command: "go test ./..."}},
	}
}

func pullRequestJobRequest() CheckJobRequest {
	request := pushJobRequest()
	request.Trigger = "pull_request"
	request.EventKey = "pr/1/" + strings.Repeat("a", 40) + "/" + strings.Repeat("c", 40)
	request.BaseOID = strings.Repeat("c", 40)
	request.PullRequestNumber = 1
	request.TriggerRef = "main"
	return request
}

// admit is the fixture shortcut for one accepted admission.
func (fixture *checkJobFixture) admit(t *testing.T, request CheckJobRequest) CheckJob {
	t.Helper()
	job, deduped, err := fixture.store.AdmitCheckJob(context.Background(), request, fixture.now)
	if err != nil || deduped {
		t.Fatalf("admit deduped=%v err=%v", deduped, err)
	}
	return job
}

// startFor is the start request of runner for job under the given lease.
func (fixture *checkJobFixture) startFor(job CheckJob, leaseID string, runner RunnerCredential) CheckJobStart {
	return CheckJobStart{
		RepositoryID: "project", JobID: job.ID, LeaseID: leaseID,
		CredentialID: runner.ID, CredentialGeneration: runner.Generation, AttemptID: attemptID(job.ID, fixture.now),
	}
}

func authorityFor(jobID string, leaseID string, runner RunnerCredential) CheckJobCompletionAuthority {
	return CheckJobCompletionAuthority{JobID: jobID, LeaseID: leaseID, CredentialID: runner.ID, CredentialGeneration: runner.Generation}
}

func passedCompletion(attempt CheckAttempt, finishedAt time.Time) CheckCompletion {
	exit := 0
	return CheckCompletion{
		AttemptID: attempt.ID, RepositoryID: attempt.RepositoryID, TaskID: attempt.TaskID,
		Results:    []CheckResult{{Name: "unit", Command: "go test ./...", Status: AttemptPassed, ExitCode: &exit, DurationMS: 1}},
		FinishedAt: finishedAt, WorktreeState: WorktreeClean,
	}
}

func completeJobAttempt(t *testing.T, store *Store, attempt CheckAttempt, status string, now time.Time) (Task, CheckAttempt) {
	t.Helper()
	exit := 0
	if status != AttemptPassed {
		exit = 1
	}
	completion := CheckCompletion{
		AttemptID: attempt.ID, RepositoryID: attempt.RepositoryID, TaskID: attempt.TaskID,
		Results: []CheckResult{{
			Name: "unit", Command: "go test ./...", Status: status, ExitCode: &exit, DurationMS: 10, OutputExcerpt: "out",
		}},
		FinishedAt: now, WorktreeState: WorktreeClean, Log: "log",
	}
	job, found, err := store.CheckJob(context.Background(), attempt.RepositoryID, attempt.JobID)
	if err != nil || !found {
		t.Fatalf("read job completion authority: found=%v err=%v", found, err)
	}
	authority := CheckJobCompletionAuthority{
		JobID: job.ID, LeaseID: job.LeaseID, CredentialID: job.CredentialID, CredentialGeneration: job.CredentialGeneration,
	}
	task, stored, err := store.CompleteCheckJobAttempt(context.Background(), completion, authority, now)
	if err != nil {
		t.Fatalf("complete job attempt: %v", err)
	}
	return task, stored
}

func TestCheckPolicyChangeInvalidatesConsent(t *testing.T) {
	fixture := newCheckJobFixture(t)
	policy := fixture.setPolicy(t, nil)
	if policy.Version != 1 || policy.Digest == "" || policy.RunnerGeneration != 0 {
		t.Fatalf("created policy=%+v", policy)
	}
	granted := fixture.grantConsent(t)
	if !granted.ConsentActive || granted.ConsentVersion != 1 || granted.ConsentDigest != granted.Digest {
		t.Fatalf("granted consent=%+v", granted)
	}
	if _, deduped, err := fixture.store.AdmitCheckJob(context.Background(), pushJobRequest(), fixture.now); err != nil || deduped {
		t.Fatalf("admission with consent deduped=%v err=%v", deduped, err)
	}
	changed := fixture.setPolicy(t, func(input *CheckPolicyInput) { input.QueueLimit = 8 })
	if changed.Version != 2 || changed.Digest == policy.Digest || changed.ConsentActive || changed.ConsentVersion != 1 {
		t.Fatalf("changed policy=%+v", changed)
	}
	if _, _, err := fixture.store.AdmitCheckJob(context.Background(), pushJobRequest(), fixture.now); !errors.Is(err, ErrCheckConsentRequired) {
		t.Fatalf("admission after policy change error=%v", err)
	}
	regranted := fixture.grantConsent(t)
	if !regranted.ConsentActive || regranted.ConsentVersion != 2 {
		t.Fatalf("regranted consent=%+v", regranted)
	}
	if _, _, err := fixture.store.AdmitCheckJob(context.Background(), pushJobRequest(), fixture.now); err != nil {
		t.Fatalf("admission after regrant: %v", err)
	}
	same := fixture.setPolicy(t, func(input *CheckPolicyInput) { input.QueueLimit = 8 })
	if same.Version != changed.Version {
		t.Fatalf("idempotent policy set version=%d", same.Version)
	}
}

func TestCheckPolicyRepositoriesListsOnlyRepositoriesWithAPolicy(t *testing.T) {
	fixture := newCheckJobFixture(t)
	ctx := context.Background()
	noErr(t, fixture.store.AddRepository(ctx, Repository{ID: "archive", Name: "archive", CreatedAt: fixture.now}))
	ids, err := fixture.store.CheckPolicyRepositories(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("repositories with a policy before any was set = %v, %v", ids, err)
	}
	fixture.setPolicy(t, nil)
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.RepositoryID = "archive" })
	ids, err = fixture.store.CheckPolicyRepositories(ctx)
	if err != nil || strings.Join(ids, ",") != "archive,project" {
		t.Fatalf("repositories with a policy = %v, %v; want archive,project in name order without other", ids, err)
	}
}

func TestImmutableContainerImageAcceptsIDsAndDigestsButRejectsTags(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, image := range []string{"sha256:" + digest, "example.invalid/checks@sha256:" + digest} {
		if !ImmutableContainerImage(image) {
			t.Fatalf("immutable image %q was rejected", image)
		}
	}
	for _, image := range []string{"checks:latest", "checks@sha256:short", "sha256:" + strings.Repeat("A", 64)} {
		if ImmutableContainerImage(image) {
			t.Fatalf("mutable or malformed image %q was accepted", image)
		}
	}
}

func TestContainerRuntimeOwnershipUsesExactJobContainerAndDaemon(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, func(input *CheckPolicyInput) {
		input.Executor = CheckExecutorContainer
		input.Execution.ContainerImage = "example.invalid/checks@sha256:" + strings.Repeat("a", 64)
	})
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	claimed, found, err := fixture.store.ClaimLocalCheckJob(context.Background(), "project", fixture.now)
	if err != nil || !found || claimed.ID != job.ID {
		t.Fatalf("local container claim=%+v found=%v err=%v", claimed, found, err)
	}
	authority := CheckJobCompletionAuthority{JobID: job.ID, LeaseID: claimed.LeaseID, CredentialID: claimed.CredentialID, CredentialGeneration: claimed.CredentialGeneration}
	containerID := strings.Repeat("b", 64)
	noErr(t, fixture.store.PlanCheckContainer(context.Background(), authority, "owngit-check-test", "daemon-one", fixture.now))
	noErr(t, fixture.store.ConfirmCheckContainer(context.Background(), job.ID, "owngit-check-test", containerID, "daemon-one"))
	ownership, err := fixture.store.ActiveCheckContainers(context.Background(), 10)
	if err != nil || len(ownership) != 1 || ownership[0].ContainerName != "owngit-check-test" || ownership[0].ContainerID != containerID || ownership[0].DaemonID != "daemon-one" {
		t.Fatalf("runtime ownership=%+v err=%v", ownership, err)
	}
	if err := fixture.store.ClearCheckContainer(context.Background(), job.ID, containerID, "other-daemon"); !errors.Is(err, ErrCheckJobRuntimeOwned) {
		t.Fatalf("foreign daemon clear error=%v", err)
	}
	noErr(t, fixture.store.ClearCheckContainer(context.Background(), job.ID, containerID, "daemon-one"))
}

func TestAdmitCheckJobDeduplicatesAndTightensLimits(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, func(input *CheckPolicyInput) {
		input.MaxTimeoutMS = 5000
		input.MaxOutputLimitBytes = 4096
	})
	fixture.grantConsent(t)
	request := pushJobRequest()
	request.TimeoutMS = 600000
	request.OutputLimitBytes = 1024
	job := fixture.admit(t, request)
	if job.Limits.TimeoutMS != 5000 || job.Limits.OutputLimitBytes != 1024 {
		t.Fatalf("effective limits=%+v", job.Limits)
	}
	if job.Executor != CheckExecutorExternalRunner || job.PolicyVersion != 1 || job.ConsentVersion != 1 || job.Status != CheckJobPending {
		t.Fatalf("admitted job=%+v", job)
	}
	again, deduped, err := fixture.store.AdmitCheckJob(context.Background(), request, fixture.now.Add(time.Minute))
	if err != nil || !deduped || again.ID != job.ID {
		t.Fatalf("dedup id=%q deduped=%v err=%v", again.ID, deduped, err)
	}
	if _, err := fixture.store.GrantCheckConsent(context.Background(), "project", fixture.now); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionRequiresAllowedEventAndOwnsStableTask(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.AllowedEvents = []string{checkworkflow.EventPush} })
	fixture.grantConsent(t)
	if _, _, err := fixture.store.AdmitCheckJob(context.Background(), pullRequestJobRequest(), fixture.now); !errors.Is(err, ErrCheckEventNotAllowed) {
		t.Fatalf("disallowed event error=%v", err)
	}
	first := fixture.admit(t, pushJobRequest())
	if !validAttemptID(first.TaskID) {
		t.Fatalf("job task identity=%q", first.TaskID)
	}
	if _, err := fixture.store.CancelCheckJob(context.Background(), "project", first.ID, fixture.now); err != nil {
		t.Fatal(err)
	}
	changed := pushJobRequest()
	changed.EventKey = "refs/heads/main@" + strings.Repeat("d", 40)
	changed.SourceOID = strings.Repeat("d", 40)
	second := fixture.admit(t, changed)
	if second.ID == first.ID || second.TaskID != first.TaskID {
		t.Fatalf("stable task first=%+v second=%+v", first, second)
	}
}

func TestPolicyChangeInterruptsStaleWorkWithoutStrandingEligibleJob(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	stale := fixture.admit(t, pushJobRequest())
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.QueueLimit++ })
	fixture.grantConsent(t)
	freshRequest := pushJobRequest()
	freshRequest.EventKey = "refs/heads/main@" + strings.Repeat("d", 40)
	freshRequest.SourceOID = strings.Repeat("d", 40)
	fresh := fixture.admit(t, freshRequest)
	runner, _ := fixture.issueRunner(t)
	claimed, found, err := fixture.store.ClaimCheckJob(context.Background(), "project", runner.ID, fixture.now.Add(time.Second))
	if err != nil || !found || claimed.ID != fresh.ID || claimed.Executor != CheckExecutorExternalRunner {
		t.Fatalf("eligible claim=%+v found=%v err=%v", claimed, found, err)
	}
	invalidated, found, err := fixture.store.CheckJob(context.Background(), "project", stale.ID)
	if err != nil || !found || invalidated.Status != CheckJobInterrupted || invalidated.FinishedAt == nil {
		t.Fatalf("stale job=%+v found=%v err=%v", invalidated, found, err)
	}
}

func TestLocalAndExternalClaimAuthoritiesAreSeparated(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.Executor = CheckExecutorHost })
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	if _, claimed, err := fixture.store.ClaimCheckJob(context.Background(), "project", runner.ID, fixture.now); err != nil || claimed {
		t.Fatalf("external credential claimed host job: claimed=%v err=%v", claimed, err)
	}
	claimed, found, err := fixture.store.ClaimLocalCheckJob(context.Background(), "project", fixture.now)
	if err != nil || !found || claimed.ID != job.ID || claimed.CredentialRole != RunnerRoleServer {
		t.Fatalf("local claim=%+v found=%v err=%v", claimed, found, err)
	}
}

func TestRevokedConsentCannotStartClaimedJob(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	claimed, found, err := fixture.store.ClaimCheckJob(context.Background(), "project", runner.ID, fixture.now)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if _, err := fixture.store.RevokeCheckConsent(context.Background(), "project", fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.StartCheckJob(context.Background(), fixture.startFor(job, claimed.LeaseID, runner), fixture.now.Add(2*time.Second)); err == nil {
		t.Fatal("revoked consent authorized a new start")
	}
	stored, _, err := fixture.store.CheckJob(context.Background(), "project", job.ID)
	if err != nil || stored.Status != CheckJobInterrupted || stored.StartedAt != nil {
		t.Fatalf("revoked consent job=%+v err=%v", stored, err)
	}
}

func TestAdmitCheckJobEnforcesQueueAndConcurrency(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.QueueLimit = 3 })
	fixture.grantConsent(t)
	var admitted sync.Map
	var failures int
	var lock sync.Mutex
	var wait sync.WaitGroup
	for index := 0; index < 12; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			request := pushJobRequest()
			request.EventKey = fmt.Sprintf("concurrent-%d", index)
			request.SourceOID = fmt.Sprintf("%040d", index)
			_, _, err := fixture.store.AdmitCheckJob(context.Background(), request, fixture.now)
			if errors.Is(err, ErrCheckQueueFull) {
				lock.Lock()
				failures++
				lock.Unlock()
				return
			}
			if err != nil {
				t.Errorf("concurrent admission: %v", err)
				return
			}
			admitted.Store(index, true)
		}(index)
	}
	wait.Wait()
	successes := 0
	admitted.Range(func(_, _ any) bool { successes++; return true })
	jobs, err := fixture.store.CheckJobs(context.Background(), "project")
	noErr(t, err)
	if successes != 3 || failures != 9 || len(jobs) != 3 {
		t.Fatalf("jobs=%d successes=%d failures=%d", len(jobs), successes, failures)
	}
	overflow := pushJobRequest()
	overflow.EventKey = "push-overflow"
	overflow.SourceOID = strings.Repeat("f", 40)
	if _, _, err := fixture.store.AdmitCheckJob(context.Background(), overflow, fixture.now); !errors.Is(err, ErrCheckQueueFull) {
		t.Fatalf("overflow error=%v", err)
	}
}

func TestConcurrentAdmissionDeduplicatesOneJob(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	request := pushJobRequest()
	ids := make([]string, 0, 8)
	var lock sync.Mutex
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			job, _, err := fixture.store.AdmitCheckJob(context.Background(), request, fixture.now)
			if err != nil {
				t.Errorf("concurrent dedup: %v", err)
				return
			}
			lock.Lock()
			ids = append(ids, job.ID)
			lock.Unlock()
		}()
	}
	wait.Wait()
	jobs, err := fixture.store.CheckJobs(context.Background(), "project")
	noErr(t, err)
	if len(jobs) != 1 {
		t.Fatalf("dedup created %d jobs", len(jobs))
	}
	for _, id := range ids {
		if id != jobs[0].ID {
			t.Fatalf("dedup returned foreign job %q", id)
		}
	}
}

func TestJobClaimStartCompleteLifecycle(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()

	if _, claimed, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now); err != nil || !claimed {
		t.Fatalf("first claim claimed=%v err=%v", claimed, err)
	}
	second, _ := fixture.issueRunner(t)
	if _, claimed, err := fixture.store.ClaimCheckJob(ctx, "project", second.ID, fixture.now); err != nil || claimed {
		t.Fatalf("second claim claimed=%v err=%v", claimed, err)
	}
	claimed, exists, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || !exists || claimed.Status != CheckJobClaimed || claimed.LeaseID == "" || claimed.LeaseExpiresAt == nil {
		t.Fatalf("claimed job=%+v exists=%v err=%v", claimed, exists, err)
	}
	if claimed.CredentialID != runner.ID || claimed.CredentialGeneration != runner.Generation || claimed.CredentialRole != RunnerRoleExternal {
		t.Fatalf("claim credential=%s generation=%d role=%s", claimed.CredentialID, claimed.CredentialGeneration, claimed.CredentialRole)
	}
	if _, _, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, strings.Repeat("0", 32), runner), fixture.now); !errors.Is(err, ErrCheckJobLease) {
		t.Fatalf("wrong lease error=%v", err)
	}
	_, attempt, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, claimed.LeaseID, runner), fixture.now)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if _, _, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, claimed.LeaseID, runner), fixture.now.Add(time.Second)); !errors.Is(err, ErrCheckJobStartReplay) {
		t.Fatalf("duplicate start error=%v", err)
	}
	if attempt.JobID != job.ID || attempt.ExecutionScope != ExecutionScopeExternalRunner || attempt.Protection != ProtectionUnknown {
		t.Fatalf("derived attempt=%+v", attempt)
	}
	completeJobAttempt(t, fixture.store, attempt, AttemptPassed, fixture.now.Add(2*time.Second))
	if _, replayedAttempt, err := fixture.store.RegisterCheckAttempt(ctx, attempt); err != nil || replayedAttempt.Sequence != attempt.Sequence {
		t.Fatalf("terminal registration replay=%+v err=%v", replayedAttempt, err)
	}
	stored, exists, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || !exists || stored.Status != CheckJobPassed || stored.FinishedAt == nil || stored.Summary == "" {
		t.Fatalf("terminal job=%+v exists=%v err=%v", stored, exists, err)
	}
	// The attempt completion replay never advances the job twice.
	completeJobAttempt(t, fixture.store, attempt, AttemptPassed, fixture.now.Add(2*time.Second))
	replayed, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || replayed.Status != CheckJobPassed {
		t.Fatalf("replay job=%+v err=%v", replayed, err)
	}
	if _, _, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, claimed.LeaseID, runner), fixture.now); !errors.Is(err, ErrCheckJobState) {
		t.Fatalf("start terminal job error=%v", err)
	}
}

// A job starts only together with its captured commands and its attempt.
// When either cannot be had, the start writes nothing: the job stays claimed
// under its lease, and the error says which step failed.
func TestJobStartsOnlyWithItsCommandsAndAttempt(t *testing.T) {
	for _, test := range []struct {
		name, statement   string
		commands, missing bool
	}{
		{"commands unreadable", `UPDATE check_configurations SET checks_json='{' WHERE repository_id='project'`, true, false},
		{"commands missing", `DELETE FROM check_configurations WHERE repository_id='project'`, true, true},
		{"attempt not written", `CREATE TRIGGER refuse_attempts BEFORE INSERT ON check_attempts BEGIN SELECT RAISE(ABORT, 'synthetic attempt write failure'); END`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckJobFixture(t)
			fixture.setPolicy(t, nil)
			fixture.grantConsent(t)
			job := fixture.admit(t, pushJobRequest())
			runner, _ := fixture.issueRunner(t)
			ctx := context.Background()
			claimed, _, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
			noErr(t, err)
			start := fixture.startFor(job, claimed.LeaseID, runner)
			noErr(t, fixture.store.Exec(ctx, test.statement))
			_, attempt, err := fixture.store.StartCheckJob(ctx, start, fixture.now)
			var notStarted *CheckJobNotStartedError
			if !errors.As(err, &notStarted) || errors.Is(err, ErrCheckJobCommandsUnavailable) != test.commands ||
				errors.Is(err, ErrCheckConfigurationMissing) != test.missing || attempt.ID != "" {
				t.Fatalf("refused start: attempt=%+v err=%v", attempt, err)
			}
			stored, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
			if err != nil || stored.Status != CheckJobClaimed || stored.StartedAt != nil || stored.AttemptID != "" || stored.LeaseID != claimed.LeaseID {
				t.Fatalf("job after a refused start: %+v err=%v", stored, err)
			}
			if _, found, err := fixture.store.CheckAttemptByID(ctx, "project", start.AttemptID); err != nil || found {
				t.Fatalf("attempt after a refused start: found=%v err=%v", found, err)
			}
			if _, err := fixture.store.FailCheckJobBeforeStart(ctx, authorityFor(job.ID, claimed.LeaseID, runner), CheckJobUnavailable, "The job was not started.", fixture.now); err != nil {
				t.Fatalf("not-run report after a refused start: %v", err)
			}
		})
	}

	// A job that starts names its attempt, which carries the job's commands.
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	claimed, _, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
	noErr(t, err)
	started, attempt, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, claimed.LeaseID, runner), fixture.now)
	if err != nil || attempt.ID != attemptID(job.ID, fixture.now) || attempt.JobID != job.ID || attempt.Status != AttemptPending ||
		started.AttemptID != attempt.ID || len(attempt.Checks) != 1 || attempt.Checks[0] != pushJobRequest().Checks[0] {
		t.Fatalf("started job=%+v attempt=%+v err=%v", started, attempt, err)
	}
	stored, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || stored.Status != CheckJobStarted || stored.AttemptID != attempt.ID {
		t.Fatalf("stored started job=%+v err=%v", stored, err)
	}
}

// Every step that needs a captured configuration reports a missing one under
// the same name: rerunning a job and completing an attempt, as starting does.
func TestMissingConfigurationHasOneName(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	ctx := context.Background()
	_, err := fixture.store.CancelCheckJob(ctx, "project", job.ID, fixture.now)
	noErr(t, err)
	task, err := fixture.store.CreateTask(ctx, "project", "Missing configuration", fixture.now)
	noErr(t, err)
	attempt := attemptFor(task, strings.Repeat("a", 40), fixture.now, AttemptPassed)
	_, _, err = fixture.store.RegisterCheckAttempt(ctx, attempt)
	noErr(t, err)
	noErr(t, fixture.store.Exec(ctx, `DELETE FROM check_configurations WHERE repository_id='project'`))

	if _, _, err := fixture.store.RerunCheckJob(ctx, "project", job.ID, nil, fixture.now); !errors.Is(err, ErrCheckConfigurationMissing) {
		t.Fatalf("rerun without its configuration: %v", err)
	}
	if _, _, err := fixture.store.CompleteCheckAttempt(ctx, CheckCompletion{
		AttemptID: attempt.ID, RepositoryID: "project", TaskID: task.ID, Results: attempt.Results,
		FinishedAt: attempt.FinishedAt, WorktreeState: attempt.WorktreeState,
	}, fixture.now); !errors.Is(err, ErrCheckConfigurationMissing) {
		t.Fatalf("completion without its configuration: %v", err)
	}
}

func TestJobOriginCannotBeForgedByHelperFacts(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.Executor = CheckExecutorExternalRunner })
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	_, started := fixture.claimAndStart(t, job, runner, ProtectionRunnerReported)
	// The start binds the attempt with the origin that the job owns.
	if started.ExecutionScope != ExecutionScopeExternalRunner || started.Protection != ProtectionRunnerReported || started.CredentialID != runner.ID {
		t.Fatalf("derived origin=%+v", started)
	}
	attempt := CheckAttempt{
		ID: started.ID, TaskID: job.TaskID, RepositoryID: "project",
		RevisionOID: job.SourceOID, WorktreeState: WorktreeClean, StartedAt: fixture.now, CreatedAt: fixture.now,
		JobID: job.ID, CredentialID: runner.ID,
		// A helper-style payload that claims inherited/unknown is overwritten.
		ExecutionScope: ExecutionScopeInherited, Protection: ProtectionUnknown,
		Checks: []CheckDefinition{{Name: "unit", Command: "go test ./..."}},
	}
	// A foreign credential cannot replay the job's attempt.
	foreign := attempt
	foreign.CredentialID = strings.Repeat("0", 32)
	if _, _, err := fixture.store.RegisterCheckAttempt(ctx, foreign); !errors.Is(err, ErrCheckJobCredential) {
		t.Fatalf("foreign credential error=%v", err)
	}
	if _, replayed, err := fixture.store.RegisterCheckAttempt(ctx, attempt); err != nil || replayed.Sequence != started.Sequence ||
		replayed.ExecutionScope != ExecutionScopeExternalRunner || replayed.Protection != ProtectionRunnerReported {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	otherTask, err := fixture.store.CreateTask(ctx, "project", "Other", fixture.now)
	noErr(t, err)
	// A second different attempt cannot bind the same job.
	bound := attempt
	bound.ID = attemptID("bound", fixture.now)
	bound.TaskID = otherTask.ID
	if _, _, err := fixture.store.RegisterCheckAttempt(ctx, bound); !errors.Is(err, ErrCheckJobAttemptBound) {
		t.Fatalf("second attempt error=%v", err)
	}
	// An unknown job and a foreign-repository job are rejected.
	unknown := attempt
	unknown.ID = attemptID("unknown", fixture.now)
	unknown.TaskID = otherTask.ID
	unknown.JobID = strings.Repeat("9", 32)
	if _, _, err := fixture.store.RegisterCheckAttempt(ctx, unknown); !errors.Is(err, ErrCheckJobNotFound) {
		t.Fatalf("unknown job error=%v", err)
	}
}

func TestJobAttemptFactsAndCompletionAuthorityAreExact(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	claimed, attempt := fixture.claimAndStart(t, job, runner, "")
	otherTask, err := fixture.store.CreateTask(ctx, "project", "Other", fixture.now)
	noErr(t, err)
	base := CheckAttempt{
		ID: attempt.ID, TaskID: job.TaskID, RepositoryID: "project",
		RevisionOID: job.SourceOID, WorktreeState: WorktreeClean, StartedAt: fixture.now, CreatedAt: fixture.now,
		JobID: job.ID, CredentialID: runner.ID,
		Checks: []CheckDefinition{{Name: "unit", Command: "go test ./..."}},
	}
	for _, test := range []struct {
		name   string
		mutate func(*CheckAttempt)
	}{
		{name: "revision", mutate: func(attempt *CheckAttempt) { attempt.RevisionOID = strings.Repeat("c", 40) }},
		{name: "commands", mutate: func(attempt *CheckAttempt) { attempt.Checks[0].Command = "true" }},
		{name: "limits", mutate: func(attempt *CheckAttempt) { attempt.TimeoutMS = job.Limits.TimeoutMS + 1 }},
		{name: "task", mutate: func(attempt *CheckAttempt) { attempt.TaskID = otherTask.ID }},
		{name: "correction cycle", mutate: func(attempt *CheckAttempt) { attempt.CycleID = strings.Repeat("c", 32) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Checks = append([]CheckDefinition(nil), base.Checks...)
			test.mutate(&candidate)
			if _, _, err := fixture.store.RegisterCheckAttempt(ctx, candidate); !errors.Is(err, ErrCheckJobFacts) {
				t.Fatalf("mismatched registration error=%v", err)
			}
		})
	}
	completion := passedCompletion(attempt, fixture.now.Add(time.Second))
	if _, _, err := fixture.store.CompleteCheckAttempt(ctx, completion, fixture.now.Add(time.Second)); !errors.Is(err, ErrCheckJobCompletionRequired) {
		t.Fatalf("ordinary completion error=%v", err)
	}
	wrong := authorityFor(job.ID, strings.Repeat("0", 32), runner)
	if _, _, err := fixture.store.CompleteCheckJobAttempt(ctx, completion, wrong, fixture.now.Add(time.Second)); !errors.Is(err, ErrCheckJobLease) {
		t.Fatalf("wrong completion authority error=%v", err)
	}
	correct := authorityFor(job.ID, claimed.LeaseID, runner)
	if _, stored, err := fixture.store.CompleteCheckJobAttempt(ctx, completion, correct, fixture.now.Add(time.Second)); err != nil || stored.Status != AttemptPassed {
		t.Fatalf("authorized completion=%+v err=%v", stored, err)
	}
}

func TestStartReplayNeverGrantsExecution(t *testing.T) {
	tests := []struct {
		name       string
		invalidate func(*testing.T, *checkJobFixture, RunnerCredential) time.Time
		wantStatus string
	}{
		{
			name: "consent revoked",
			invalidate: func(t *testing.T, fixture *checkJobFixture, _ RunnerCredential) time.Time {
				if _, err := fixture.store.RevokeCheckConsent(context.Background(), "project", fixture.now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				return fixture.now.Add(2 * time.Second)
			},
			wantStatus: CheckJobStarted,
		},
		{
			name: "credential revoked",
			invalidate: func(t *testing.T, fixture *checkJobFixture, runner RunnerCredential) time.Time {
				noErr(t, fixture.store.RevokeCheckRunnerToken(context.Background(), "project", runner.ID, fixture.now.Add(time.Second)))
				return fixture.now.Add(2 * time.Second)
			},
			wantStatus: CheckJobStarted,
		},
		{
			name: "policy changed",
			invalidate: func(t *testing.T, fixture *checkJobFixture, _ RunnerCredential) time.Time {
				fixture.setPolicy(t, func(input *CheckPolicyInput) {
					input.Executor = CheckExecutorExternalRunner
					input.QueueLimit++
				})
				return fixture.now.Add(2 * time.Second)
			},
			wantStatus: CheckJobStarted,
		},
		{
			name: "lease expired",
			invalidate: func(t *testing.T, fixture *checkJobFixture, _ RunnerCredential) time.Time {
				if expired, err := fixture.store.ExpireCheckJobLeases(context.Background(), fixture.now.Add(2*time.Minute)); err != nil || expired != 1 {
					t.Fatalf("expired=%d err=%v", expired, err)
				}
				return fixture.now.Add(3 * time.Minute)
			},
			wantStatus: CheckJobAmbiguous,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckJobFixture(t)
			fixture.setPolicy(t, func(input *CheckPolicyInput) { input.Executor = CheckExecutorExternalRunner })
			fixture.grantConsent(t)
			job := fixture.admit(t, pushJobRequest())
			runner, _ := fixture.issueRunner(t)
			ctx := context.Background()
			claimed, found, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
			if err != nil || !found {
				t.Fatalf("claim found=%v err=%v", found, err)
			}
			request := fixture.startFor(job, claimed.LeaseID, runner)
			request.Protection = ProtectionRunnerReported
			first, _, err := fixture.store.StartCheckJob(ctx, request, fixture.now)
			noErr(t, err)
			replayAt := test.invalidate(t, fixture, runner)
			if _, _, err := fixture.store.StartCheckJob(ctx, request, replayAt); !errors.Is(err, ErrCheckJobStartReplay) {
				t.Fatalf("start replay error=%v", err)
			}
			stored, found, err := fixture.store.CheckJob(ctx, "project", job.ID)
			if err != nil || !found || stored.Status != test.wantStatus || stored.StartedAt == nil ||
				!stored.StartedAt.Equal(*first.StartedAt) || stored.Protection != first.Protection {
				t.Fatalf("stored start facts=%+v found=%v err=%v", stored, found, err)
			}
		})
	}
}

func TestLeaseExpiryIsAmbiguousAndNeedsExplicitRerun(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	request := pushJobRequest()
	job := fixture.admit(t, request)
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	fixture.claimAndStart(t, job, runner, "")
	expiredAt := fixture.now.Add(2 * time.Minute)
	expired, err := fixture.store.ExpireCheckJobLeases(ctx, expiredAt)
	if err != nil || expired != 1 {
		t.Fatalf("expire count=%d err=%v", expired, err)
	}
	stored, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || stored.Status != CheckJobAmbiguous || stored.LeaseLostAt == nil {
		t.Fatalf("ambiguous job=%+v err=%v", stored, err)
	}
	// Re-observing the same event dedups onto the ambiguous job, so no silent
	// requeue happens.
	again, deduped, err := fixture.store.AdmitCheckJob(ctx, request, expiredAt)
	if err != nil || !deduped || again.ID != job.ID || again.Status != CheckJobAmbiguous {
		t.Fatalf("dedup after loss id=%q deduped=%v status=%s err=%v", again.ID, deduped, again.Status, err)
	}
	if _, claimed, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, expiredAt); err != nil || claimed {
		t.Fatalf("ambiguous claim claimed=%v err=%v", claimed, err)
	}
	rerun, deduped, err := fixture.store.RerunCheckJob(ctx, "project", job.ID, nil, expiredAt)
	if err != nil || deduped || rerun.ID == job.ID || rerun.Status != CheckJobPending || rerun.RerunRoot != job.ID || rerun.RerunGeneration != 1 {
		t.Fatalf("rerun=%+v deduped=%v err=%v", rerun, deduped, err)
	}
	if _, claimed, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, expiredAt); err != nil || !claimed {
		t.Fatalf("rerun claim claimed=%v err=%v", claimed, err)
	}
	if _, _, err := fixture.store.RerunCheckJob(ctx, "project", rerun.ID, nil, expiredAt); !errors.Is(err, ErrCheckJobState) {
		t.Fatalf("rerun pending error=%v", err)
	}
}

func TestLateCompletionAfterLeaseLossRecordsVerdict(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	_, attempt := fixture.claimAndStart(t, job, runner, "")
	if _, err := fixture.store.ExpireCheckJobLeases(ctx, fixture.now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	completeJobAttempt(t, fixture.store, attempt, AttemptFailed, fixture.now.Add(3*time.Minute))
	stored, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || stored.Status != CheckJobFailed || stored.LeaseLostAt == nil || stored.FinishedAt == nil {
		t.Fatalf("late completion job=%+v err=%v", stored, err)
	}
}

func TestRevokedCredentialCannotCompleteStartedJob(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	claimed, attempt := fixture.claimAndStart(t, job, runner, "")
	noErr(t, fixture.store.RevokeCheckRunnerToken(ctx, "project", runner.ID, fixture.now.Add(time.Second)))
	completion := passedCompletion(attempt, fixture.now.Add(2*time.Second))
	authority := authorityFor(job.ID, claimed.LeaseID, runner)
	if _, _, err := fixture.store.CompleteCheckJobAttempt(ctx, completion, authority, fixture.now.Add(2*time.Second)); !errors.Is(err, ErrCheckRunnerCredential) {
		t.Fatalf("revoked completion error=%v", err)
	}
	stored, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || stored.Status != CheckJobStarted {
		t.Fatalf("revoked completion changed job=%+v err=%v", stored, err)
	}
}

func TestClaimedJobCancellationPreventsStart(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	claimed, found, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	cancelled, err := fixture.store.CancelCheckJob(ctx, "project", job.ID, fixture.now.Add(time.Second))
	if err != nil || cancelled.Status != CheckJobCancelled || cancelled.ClaimedAt == nil || cancelled.StartedAt != nil ||
		cancelled.FinishedAt == nil || cancelled.CancelRequestedAt == nil || cancelled.LeaseID != claimed.LeaseID ||
		cancelled.CredentialID != runner.ID || cancelled.AttemptID != "" {
		t.Fatalf("cancelled claimed job=%+v err=%v", cancelled, err)
	}
	if _, _, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, claimed.LeaseID, runner), fixture.now.Add(2*time.Second)); !errors.Is(err, ErrCheckJobState) {
		t.Fatalf("start after cancellation error=%v", err)
	}
	stored, found, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || !found || stored.Status != CheckJobCancelled || stored.StartedAt != nil || stored.AttemptID != "" {
		t.Fatalf("stored cancelled claim=%+v found=%v err=%v", stored, found, err)
	}
	if _, err := fixture.store.RecoverySnapshot(ctx); err != nil {
		t.Fatalf("cancelled claim recovery: %v", err)
	}
}

func TestClaimCancellationRaceDoesNotMisreportStoppedExecution(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	ctx := context.Background()
	claimed, found, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	request := fixture.startFor(job, claimed.LeaseID, runner)
	begin := make(chan struct{})
	var wait sync.WaitGroup
	var started, cancelled CheckJob
	var startErr, cancelErr error
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-begin
		started, _, startErr = fixture.store.StartCheckJob(ctx, request, fixture.now.Add(time.Second))
	}()
	go func() {
		defer wait.Done()
		<-begin
		cancelled, cancelErr = fixture.store.CancelCheckJob(ctx, "project", job.ID, fixture.now.Add(time.Second))
	}()
	close(begin)
	wait.Wait()
	if cancelErr != nil {
		t.Fatalf("cancel race error=%v", cancelErr)
	}
	stored, found, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || !found {
		t.Fatalf("read raced job found=%v err=%v", found, err)
	}
	switch stored.Status {
	case CheckJobCancelled:
		if !errors.Is(startErr, ErrCheckJobState) || cancelled.Status != CheckJobCancelled || stored.StartedAt != nil || stored.FinishedAt == nil {
			t.Fatalf("cancel won start=%+v startErr=%v cancel=%+v stored=%+v", started, startErr, cancelled, stored)
		}
	case CheckJobStarted:
		if startErr != nil || started.Status != CheckJobStarted || cancelled.Status != CheckJobStarted || stored.StartedAt == nil || stored.CancelRequestedAt == nil || stored.FinishedAt != nil {
			t.Fatalf("start won start=%+v startErr=%v cancel=%+v stored=%+v", started, startErr, cancelled, stored)
		}
	default:
		t.Fatalf("unexpected raced job=%+v startErr=%v cancel=%+v", stored, startErr, cancelled)
	}
}

func TestJobCancellationSemantics(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	ctx := context.Background()
	pending := fixture.admit(t, pushJobRequest())
	cancelled, err := fixture.store.CancelCheckJob(ctx, "project", pending.ID, fixture.now)
	if err != nil || cancelled.Status != CheckJobCancelled || cancelled.FinishedAt == nil || cancelled.CancelRequestedAt == nil {
		t.Fatalf("cancelled pending=%+v err=%v", cancelled, err)
	}
	if _, claimed, err := fixture.store.ClaimCheckJob(ctx, "project", mustRunner(t, fixture).ID, fixture.now); err != nil || claimed {
		t.Fatalf("claim cancelled claimed=%v err=%v", claimed, err)
	}

	request := pushJobRequest()
	request.EventKey = "refs/heads/main@" + strings.Repeat("d", 40)
	request.SourceOID = strings.Repeat("d", 40)
	job := fixture.admit(t, request)
	runner, _ := fixture.issueRunner(t)
	claimed, attempt := fixture.claimAndStart(t, job, runner, "")
	if _, err := fixture.store.CancelCheckJob(ctx, "project", job.ID, fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	requested, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || requested.Status != CheckJobStarted || requested.CancelRequestedAt == nil {
		t.Fatalf("cancel request job=%+v err=%v", requested, err)
	}
	exit := 1
	completion := CheckCompletion{
		AttemptID: attempt.ID, RepositoryID: attempt.RepositoryID, TaskID: attempt.TaskID,
		Results:    []CheckResult{{Name: "unit", Command: "go test ./...", Status: AttemptCancelled, ExitCode: &exit, DurationMS: 1}},
		Cancelled:  true,
		FinishedAt: fixture.now.Add(2 * time.Second), WorktreeState: WorktreeClean,
	}
	authority := authorityFor(job.ID, claimed.LeaseID, runner)
	if _, _, err := fixture.store.CompleteCheckJobAttempt(ctx, completion, authority, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	target, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || target.Status != CheckJobCancelled {
		t.Fatalf("cancelled job=%+v err=%v", target, err)
	}
}

// A cancel that arrives after a job finished changes nothing and records no
// cancel intent, so the history does not suggest someone stopped the job.
func TestCancelLeavesAFinishedJobUnchanged(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	ctx := context.Background()
	job := fixture.admit(t, pushJobRequest())
	runner, _ := fixture.issueRunner(t)
	claimed, found, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if _, err := fixture.store.FailCheckJobBeforeStart(ctx, authorityFor(claimed.ID, claimed.LeaseID, runner), CheckJobError, "The run ended before execution began.", fixture.now); err != nil {
		t.Fatal(err)
	}
	returned, err := fixture.store.CancelCheckJob(ctx, "project", job.ID, fixture.now.Add(time.Second))
	if err != nil || returned.Status != CheckJobError || returned.CancelRequestedAt != nil {
		t.Fatalf("cancel of a finished job returned=%+v err=%v", returned, err)
	}
	stored, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || stored.Status != CheckJobError || stored.CancelRequestedAt != nil {
		t.Fatalf("finished job after cancel=%+v err=%v", stored, err)
	}
}

func mustRunner(t *testing.T, fixture *checkJobFixture) RunnerCredential {
	t.Helper()
	runner, _ := fixture.issueRunner(t)
	return runner
}

func TestRunnerCredentialRevocationAndScope(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	ctx := context.Background()
	runner, token := fixture.issueRunner(t)
	if runner.Generation != 1 {
		t.Fatalf("first generation=%d", runner.Generation)
	}
	resolved, found, err := fixture.store.RunnerCredentialByToken(ctx, "project", token, fixture.now)
	if err != nil || !found || resolved.ID != runner.ID {
		t.Fatalf("resolve runner found=%v err=%v", found, err)
	}
	// A live token for another repository is refused as such, so the holder
	// learns that the repository, not the token, is wrong.
	if _, found, err := fixture.store.RunnerCredentialByToken(ctx, "other", token, fixture.now); !errors.Is(err, ErrRunnerCredentialOtherRepository) || found {
		t.Fatalf("cross-repository resolve found=%v err=%v", found, err)
	}
	job := fixture.admit(t, pushJobRequest())
	claimed, found, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now)
	if err != nil || !found {
		t.Fatalf("claim claimed=%v err=%v", found, err)
	}
	noErr(t, fixture.store.RevokeCheckRunnerToken(ctx, "project", runner.ID, fixture.now))
	if err := fixture.store.RevokeCheckRunnerToken(ctx, "project", runner.ID, fixture.now); !errors.Is(err, ErrCheckRunnerRevoked) {
		t.Fatalf("double revoke error=%v", err)
	}
	for _, repositoryID := range []string{"project", "other"} {
		if _, found, err := fixture.store.RunnerCredentialByToken(ctx, repositoryID, token, fixture.now); err != nil || found {
			t.Fatalf("revoked resolve for %s found=%v err=%v", repositoryID, found, err)
		}
	}
	if _, _, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now); !errors.Is(err, ErrCheckRunnerCredential) {
		t.Fatalf("revoked claim error=%v", err)
	}
	if _, _, err := fixture.store.StartCheckJob(ctx, fixture.startFor(job, claimed.LeaseID, runner), fixture.now); err == nil {
		t.Fatal("revoked authority started claimed work")
	}
	interrupted, _, err := fixture.store.CheckJob(ctx, "project", job.ID)
	if err != nil || interrupted.Status != CheckJobInterrupted {
		t.Fatalf("revoked job=%+v err=%v", interrupted, err)
	}
	// New issuance constructs a different epoch-bound bearer at a higher generation.
	next, nextToken, created, err := fixture.store.IssueCheckRunnerToken(ctx, "project", "server", "", fixture.now)
	if err != nil || !created || next.Generation != runner.Generation+1 || nextToken == "" || nextToken == token {
		t.Fatalf("next generation=%d created=%v err=%v", next.Generation, created, err)
	}
	if _, found, err := fixture.store.RunnerCredentialByToken(ctx, "project", token, fixture.now); err != nil || found {
		t.Fatalf("retired bearer revived found=%v err=%v", found, err)
	}
	if _, found, err := fixture.store.RunnerCredentialByToken(ctx, "project", nextToken, fixture.now); err != nil || !found {
		t.Fatalf("new bearer found=%v err=%v", found, err)
	}
	// A job is scoped to its repository, so another repository cannot claim it.
	if _, _, err := fixture.store.ClaimCheckJob(ctx, "other", runner.ID, fixture.now); !errors.Is(err, ErrCheckRunnerCredential) {
		t.Fatalf("foreign repository claim error=%v", err)
	}
}

func TestRunnerCredentialCreationIsIdempotent(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	ctx := context.Background()
	creation := strings.Repeat("7", 32)
	first, token, created, err := fixture.store.IssueCheckRunnerToken(ctx, "project", "laptop", creation, fixture.now)
	if err != nil || !created || token == "" {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	again, replayToken, created, err := fixture.store.IssueCheckRunnerToken(ctx, "project", "laptop", creation, fixture.now)
	if err != nil || created || again.ID != first.ID || replayToken != "" {
		t.Fatalf("retry created=%v id=%q token=%t err=%v", created, again.ID, replayToken != "", err)
	}
	if _, _, _, err := fixture.store.IssueCheckRunnerToken(ctx, "project", "other", creation, fixture.now); !errors.Is(err, ErrCheckRunnerCreationConflict) {
		t.Fatalf("conflicting creation error=%v", err)
	}
	noErr(t, fixture.store.RevokeCheckRunnerTokenByCreation(ctx, "project", creation, fixture.now))
	retired, replayToken, created, err := fixture.store.IssueCheckRunnerToken(ctx, "project", "laptop", creation, fixture.now)
	if err != nil || created || replayToken != "" || retired.ID != first.ID || retired.RevokedAt == nil {
		t.Fatalf("revoked creation replay=%+v created=%v token=%t err=%v", retired, created, replayToken != "", err)
	}
	if _, found, err := fixture.store.RunnerCredentialByToken(ctx, "project", token, fixture.now); err != nil || found {
		t.Fatalf("compensated revoke found=%v err=%v", found, err)
	}
}

func TestUnchangedBranchHeadsDoNotRewriteObservations(t *testing.T) {
	fixture := newCheckJobFixture(t)
	ctx := context.Background()
	changes := func() int64 {
		var count int64
		noErr(t, fixture.store.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&count))
		return count
	}
	for scan := 0; scan < 2; scan++ {
		before := changes()
		for index := 0; index < 65; index++ {
			ref := fmt.Sprintf("refs/heads/branch-%02d", index)
			oid := fmt.Sprintf("%040d", index)
			noErr(t, fixture.store.RecordCheckObservation(ctx, "project", ref, oid, fixture.now.Add(time.Duration(scan*65+index)*time.Second)))
		}
		writes := changes() - before
		t.Logf("scan %d: %d changed observation rows", scan+1, writes)
		if scan == 1 && writes != 0 {
			t.Fatalf("an unchanged set of 65 heads wrote %d observation rows", writes)
		}
	}
}

func TestObservationsAreBoundedAndUpserted(t *testing.T) {
	if testing.Short() {
		t.Skip("fills and reads a 10,000-row SQLite observation window")
	}
	fixture := newCheckJobFixture(t)
	ctx := context.Background()
	// Seed the full window in one transaction; individual observations below
	// exercise eviction without thousands of separate write transactions.
	noErr(t, fixture.store.Exec(ctx, `WITH RECURSIVE branches(idx) AS (
		VALUES(0) UNION ALL SELECT idx+1 FROM branches WHERE idx+1<?
	) INSERT INTO check_observations(repository_id,ref_name,oid,observed_at)
		SELECT 'project',printf('refs/heads/branch-%05d',idx),printf('%040d',idx),?+idx*? FROM branches`,
		MaximumCheckObservations, fixture.now.UnixNano(), time.Second.Nanoseconds()))
	for index := MaximumCheckObservations; index < MaximumCheckObservations+6; index++ {
		ref := fmt.Sprintf("refs/heads/branch-%05d", index)
		oid := fmt.Sprintf("%040d", index)
		noErr(t, fixture.store.RecordCheckObservation(ctx, "project", ref, oid, fixture.now.Add(time.Duration(index)*time.Second)))
	}
	observations, err := fixture.store.CheckObservations(ctx, "project")
	if err != nil || len(observations) != MaximumCheckObservations {
		t.Fatalf("observation count=%d err=%v", len(observations), err)
	}
	if observations[0].RefName != fmt.Sprintf("refs/heads/branch-%05d", MaximumCheckObservations+5) {
		t.Fatalf("newest observation=%+v", observations[0])
	}
	updated := strings.Repeat("e", 40)
	updatedAt := fixture.now.Add(time.Duration(MaximumCheckObservations+6) * time.Second)
	noErr(t, fixture.store.RecordCheckObservation(ctx, "project", "refs/heads/branch-00063", updated, updatedAt))
	refreshed, err := fixture.store.CheckObservations(ctx, "project")
	if err != nil || refreshed[0].OID != updated || len(refreshed) != MaximumCheckObservations {
		t.Fatalf("upserted observation=%+v err=%v", refreshed[0], err)
	}
	// The same object again is not written: its observation time stays.
	noErr(t, fixture.store.RecordCheckObservation(ctx, "project", "refs/heads/branch-00063", updated, updatedAt.Add(time.Minute)))
	again, err := fixture.store.CheckObservations(ctx, "project")
	if err != nil || !again[0].ObservedAt.Equal(updatedAt) {
		t.Fatalf("an unchanged observation was rewritten: %+v err=%v", again[0], err)
	}
	for _, invalid := range []struct{ ref, oid string }{
		{"", strings.Repeat("a", 40)},
		{"main", strings.Repeat("a", 40)},
		{"refs/tags/newest", strings.Repeat("a", 40)},
		{"refs/owngit/private", strings.Repeat("a", 40)},
		{"refs/heads/@", strings.Repeat("a", 40)},
		{"refs/heads/main", "short"},
	} {
		if err := fixture.store.RecordCheckObservation(ctx, "project", invalid.ref, invalid.oid, fixture.now.Add(2*time.Hour)); !errors.Is(err, ErrInvalidCheckObservation) {
			t.Fatalf("invalid observation %q/%q error=%v", invalid.ref, invalid.oid, err)
		}
	}
	afterInvalid, err := fixture.store.CheckObservations(ctx, "project")
	if err != nil || len(afterInvalid) != MaximumCheckObservations || afterInvalid[0].RefName != "refs/heads/branch-00063" {
		t.Fatalf("invalid refs changed branch observations=%+v err=%v", afterInvalid, err)
	}
}

func TestAdmitCheckJobRejectsMalformedRequests(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	ctx := context.Background()
	base := pushJobRequest()
	tests := map[string]func(*CheckJobRequest){
		"unknown trigger": func(request *CheckJobRequest) { request.Trigger = "merge" },
		"push with pr facts": func(request *CheckJobRequest) {
			request.PullRequestNumber = 1
			request.BaseOID = strings.Repeat("c", 40)
		},
		"pr without base":     func(request *CheckJobRequest) { request.Trigger = "pull_request"; request.PullRequestNumber = 1 },
		"bad source":          func(request *CheckJobRequest) { request.SourceOID = "short" },
		"empty event key":     func(request *CheckJobRequest) { request.EventKey = "" },
		"bad trigger ref":     func(request *CheckJobRequest) { request.TriggerRef = "a b" },
		"standalone at ref":   func(request *CheckJobRequest) { request.TriggerRef = "@" },
		"other workflow":      func(request *CheckJobRequest) { request.WorkflowPath = "ci.yml" },
		"bad workflow digest": func(request *CheckJobRequest) { request.WorkflowDigest = "x" },
		"no checks":           func(request *CheckJobRequest) { request.Checks = nil },
		"negative limits":     func(request *CheckJobRequest) { request.TimeoutMS = -1 },
		"bad rerun root":      func(request *CheckJobRequest) { request.RerunRoot = "abc" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := base
			mutate(&request)
			if _, _, err := fixture.store.AdmitCheckJob(ctx, request, fixture.now); !errors.Is(err, ErrInvalidCheckJob) {
				t.Fatalf("request error=%v", err)
			}
		})
	}
	pr := pullRequestJobRequest()
	if _, deduped, err := fixture.store.AdmitCheckJob(ctx, pr, fixture.now); err != nil || deduped {
		t.Fatalf("pull request admission deduped=%v err=%v", deduped, err)
	}
}

func TestRerunAdmissionDeduplicatesUnderConcurrency(t *testing.T) {
	fixture := newCheckJobFixture(t)
	fixture.setPolicy(t, nil)
	fixture.grantConsent(t)
	job := fixture.admit(t, pushJobRequest())
	cancelled, err := fixture.store.CancelCheckJob(context.Background(), "project", job.ID, fixture.now)
	if err != nil || cancelled.Status != CheckJobCancelled {
		t.Fatalf("cancel err=%v", err)
	}
	ids := make([]string, 0, 4)
	var lock sync.Mutex
	var wait sync.WaitGroup
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			rerun, _, err := fixture.store.RerunCheckJob(context.Background(), "project", job.ID, nil, fixture.now.Add(time.Second))
			if err != nil {
				t.Errorf("concurrent rerun: %v", err)
				return
			}
			lock.Lock()
			ids = append(ids, rerun.ID)
			lock.Unlock()
		}()
	}
	wait.Wait()
	if len(ids) != 4 {
		t.Fatalf("rerun results=%d", len(ids))
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("rerun dedup returned different jobs: %v", ids)
		}
	}
	jobs, err := fixture.store.CheckJobs(context.Background(), "project")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
}

// claimAndStart claims job with runner and starts it with protection. It
// returns the claim and the attempt that the start registered.
func (fixture *checkJobFixture) claimAndStart(t *testing.T, job CheckJob, runner RunnerCredential, protection string) (CheckJob, CheckAttempt) {
	t.Helper()
	ctx := context.Background()
	var features []string
	if job.RunID != "" {
		features = []string{RunnerFeatureWorkflowsV1}
	}
	claimed, found, err := fixture.store.ClaimCheckJob(ctx, "project", runner.ID, fixture.now, features...)
	if err != nil || !found || claimed.ID != job.ID {
		t.Fatalf("claim job=%+v found=%v err=%v", claimed, found, err)
	}
	request := fixture.startFor(job, claimed.LeaseID, runner)
	request.Protection = protection
	_, attempt, err := fixture.store.StartCheckJob(ctx, request, fixture.now)
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	return claimed, attempt
}

// A rerun applies the policy's current caps to the workflow's request, so
// raising a cap lets it run longer and lowering one cuts it. Without a request
// it keeps the original job's effective limits, still cut by the caps.
func TestRerunLimitsFollowTheCurrentPolicyCaps(t *testing.T) {
	fixture := newCheckJobFixture(t)
	ctx := context.Background()
	fixture.setPolicy(t, func(input *CheckPolicyInput) { input.MaxTimeoutMS, input.MaxOutputLimitBytes = 5000, 2048 })
	fixture.grantConsent(t)
	request := pushJobRequest()
	request.TimeoutMS, request.OutputLimitBytes = 600000, 65536
	job := fixture.admit(t, request)
	if job.Limits.TimeoutMS != 5000 || job.Limits.OutputLimitBytes != 2048 {
		t.Fatalf("limits=%+v", job.Limits)
	}
	_, err := fixture.store.CancelCheckJob(ctx, "project", job.ID, fixture.now)
	noErr(t, err)
	asked := &CheckJobLimits{TimeoutMS: 600000, OutputLimitBytes: 65536}
	for _, test := range []struct {
		timeout, output int64
		requested       *CheckJobLimits
		want            CheckJobLimits
	}{
		{30000, 8192, asked, CheckJobLimits{TimeoutMS: 30000, OutputLimitBytes: 8192}},
		{600000, 65536, &CheckJobLimits{}, CheckJobLimits{TimeoutMS: checkworkflow.DefaultTimeoutMS, OutputLimitBytes: checkworkflow.DefaultOutputLimitBytes}},
		{30000, 8192, nil, CheckJobLimits{TimeoutMS: 5000, OutputLimitBytes: 2048}},
		{2000, 1024, asked, CheckJobLimits{TimeoutMS: 2000, OutputLimitBytes: 1024}},
	} {
		fixture.setPolicy(t, func(input *CheckPolicyInput) {
			input.MaxTimeoutMS, input.MaxOutputLimitBytes = test.timeout, test.output
		})
		fixture.grantConsent(t)
		rerun, _, err := fixture.store.RerunCheckJob(ctx, "project", job.ID, test.requested, fixture.now)
		noErr(t, err)
		if rerun.Limits != test.want {
			t.Fatalf("caps %d/%d: rerun limits=%+v, want %+v", test.timeout, test.output, rerun.Limits, test.want)
		}
		_, err = fixture.store.CancelCheckJob(ctx, "project", rerun.ID, fixture.now)
		noErr(t, err)
	}
}
