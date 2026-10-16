package automerge

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"owngit/internal/gitexec"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
)

type integrationFixture struct {
	t            *testing.T
	ctx          context.Context
	store        *state.Store
	manager      *repository.Manager
	service      *pullrequest.Service
	coordinator  *Coordinator
	work         string
	repositoryID string
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	repositoryRoot := filepath.Join(root, "repositories")
	noErr(t, os.Mkdir(repositoryRoot, 0o700))
	store, err := state.Open(ctx, stateDir)
	noErr(t, err)
	t.Cleanup(func() { _ = store.Close() })
	noErr(t, store.CompleteSetup(ctx, repositoryRoot, "open", "", "synthetic-admin-hash", true))
	runner, err := gitexec.New("", filepath.Join(stateDir, "runtime"))
	noErr(t, err)
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoryRoot}
	stored, err := manager.Create(ctx, "project", "auto merge integration")
	noErr(t, err)
	remote, err := manager.Path(stored.ID)
	noErr(t, err)
	work := filepath.Join(root, "work")
	runGit(t, "", "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.name", "Auto Test")
	runGit(t, work, "config", "user.email", "auto@example.invalid")
	runGit(t, work, "remote", "add", "origin", remote)

	service := &pullrequest.Service{Store: store, Repositories: manager}
	coordinator := New(store, service, func(string, ...any) {})
	return &integrationFixture{
		t: t, ctx: ctx, store: store, manager: manager, service: service,
		coordinator: coordinator, work: work, repositoryID: stored.ID,
	}
}

func (f *integrationFixture) commit(name, content, message string) string {
	f.t.Helper()
	noErr(f.t, os.WriteFile(filepath.Join(f.work, name), []byte(content), 0o600))
	runGit(f.t, f.work, "add", "--", name)
	runGit(f.t, f.work, "commit", "-m", message)
	return gitOutput(f.t, f.work, "rev-parse", "HEAD")
}

func (f *integrationFixture) push(ref string) {
	f.t.Helper()
	runGit(f.t, f.work, "push", "origin", ref)
}

func (f *integrationFixture) git(args ...string) {
	f.t.Helper()
	runGit(f.t, f.work, args...)
}

// recordPassedCheck writes one completed, clean passed attempt for revision,
// exactly as a helper completion would.
func (f *integrationFixture) recordPassedCheck(revision string) {
	f.t.Helper()
	now := time.Now().UTC()
	task, err := f.store.CreateTask(f.ctx, f.repositoryID, "Check the change", now.Add(-time.Second))
	noErr(f.t, err)
	const attemptID = "44444444444444444444444444444444"
	_, _, err = f.store.RegisterCheckAttempt(f.ctx, state.CheckAttempt{
		ID: attemptID, TaskID: task.ID, RepositoryID: f.repositoryID, RevisionOID: revision,
		WorktreeState: state.WorktreeClean, StartedAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Second),
		Protection: state.ProtectionUnknown, ExecutionScope: state.ExecutionScopeInherited,
		Checks: []state.CheckDefinition{{Name: "unit", Command: "true"}},
	})
	noErr(f.t, err)
	exitCode := 0
	_, _, err = f.store.CompleteCheckAttempt(f.ctx, state.CheckCompletion{
		AttemptID: attemptID, RepositoryID: f.repositoryID, TaskID: task.ID, FinishedAt: now, WorktreeState: state.WorktreeClean,
		Results: []state.CheckResult{{Name: "unit", Command: "true", Status: state.AttemptPassed, ExitCode: &exitCode, DurationMS: 1}},
	}, now)
	noErr(f.t, err)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", args, err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", args, err)
	}
	return string(bytes.TrimSpace(output))
}

func gitRef(t *testing.T, manager *repository.Manager, repositoryID, name string) string {
	t.Helper()
	path, _, exists, err := manager.ExistingPath(context.Background(), repositoryID)
	noErr(t, err)
	if !exists {
		t.Fatal("repository path missing")
	}
	command := exec.Command("git", "--git-dir", path, "rev-parse", "--verify", "--quiet", name)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(output))
}

// Scan after a helper-style check completion publishes the automatic merge
// through the real store, service and Git machinery, exactly once.
func TestIntegrationScanMergesAfterCheckCompletion(t *testing.T) {
	fixture := newIntegrationFixture(t)
	fixture.commit("base.txt", "base\n", "base")
	fixture.push("HEAD:refs/heads/main")
	fixture.git("checkout", "-b", "feature")
	source := fixture.commit("feature.txt", "feature\n", "feature")
	fixture.push("HEAD:refs/heads/feature")
	created, err := fixture.service.Create(fixture.ctx, pullrequest.CreateInput{
		Repository: fixture.repositoryID, Title: "Auto", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)

	_, err = fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	fixture.coordinator.Scan(fixture.ctx)
	if gitRef(t, fixture.manager, fixture.repositoryID, pullrequest.MergeReceiptRef(created.Number)) != "" {
		t.Fatal("a scan without checks merged")
	}

	fixture.recordPassedCheck(source)
	fixture.coordinator.Scan(fixture.ctx)
	shown, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, created.Number)
	noErr(t, err)
	if shown.State != state.PullRequestMerged || shown.Merge == nil {
		t.Fatalf("scan after checks did not merge: %+v", shown)
	}
	if target := gitRef(t, fixture.manager, fixture.repositoryID, "refs/heads/main"); target != shown.Merge.OID {
		t.Fatalf("target=%s merge oid=%s", target, shown.Merge.OID)
	}

	// A scan after the merge changes nothing.
	fixture.coordinator.Scan(fixture.ctx)
	intents, err := fixture.store.PullRequestMergeIntents(fixture.ctx, fixture.repositoryID, created.Number, false)
	noErr(t, err)
	if len(intents) != 1 || intents[0].Status != state.MergeIntentComplete {
		t.Fatalf("merge intents=%+v", intents)
	}
}

// A wake delivered by the started loop merges without anyone calling Scan.
func TestIntegrationWakeFromLoopMerges(t *testing.T) {
	fixture := newIntegrationFixture(t)
	fixture.commit("base.txt", "base\n", "base")
	fixture.push("HEAD:refs/heads/main")
	fixture.git("checkout", "-b", "feature")
	source := fixture.commit("feature.txt", "feature\n", "feature")
	fixture.push("HEAD:refs/heads/feature")
	created, err := fixture.service.Create(fixture.ctx, pullrequest.CreateInput{
		Repository: fixture.repositoryID, Title: "Auto", SourceBranch: "feature", TargetBranch: "main", ReviewChoice: "skip",
	})
	noErr(t, err)
	_, err = fixture.service.EnableAutoMerge(fixture.ctx, fixture.repositoryID, created.Number, state.Actor{})
	noErr(t, err)
	fixture.recordPassedCheck(source)

	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	fixture.coordinator.Start(ctx)
	defer fixture.coordinator.Stop()
	fixture.coordinator.Wake(fixture.repositoryID)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		shown, err := fixture.service.Show(fixture.ctx, fixture.repositoryID, created.Number)
		noErr(t, err)
		if shown.State == state.PullRequestMerged {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the wake did not converge the pull request")
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
