package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"owngit/internal/auth"
	"owngit/internal/gitexec"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
	"owngit/internal/testfixture"
)

// sealDigests fills the canonical attempt digests from the manifest's own
// facts, so a hand-written fixture stays valid without duplicating the
// algorithm.
func sealDigests(manifest *Manifest) {
	for index := range manifest.CheckAttempts {
		attempt := &manifest.CheckAttempts[index]
		var checks []state.CheckDefinition
		for _, configuration := range manifest.CheckConfigurations {
			if configuration.RepositoryID == attempt.RepositoryID && configuration.Version == attempt.ConfigurationVersion {
				for _, check := range configuration.Checks {
					checks = append(checks, state.CheckDefinition{Name: check.Name, Command: check.Command})
				}
			}
		}
		facts := state.CheckAttempt{
			ID: attempt.ID, TaskID: attempt.TaskID, RepositoryID: attempt.RepositoryID, RevisionOID: attempt.RevisionOID,
			WorktreeState: attempt.WorktreeState, CycleID: attempt.CycleID, TimeoutMS: attempt.TimeoutMS,
			OutputLimitBytes: attempt.OutputLimitBytes, StartedAt: attempt.StartedAt, CreatedAt: attempt.CreatedAt,
			Protection: attempt.Protection, ExecutionScope: attempt.ExecutionScope, CredentialID: attempt.CredentialID,
			JobID:  attempt.JobID,
			Checks: checks,
		}
		attempt.RegistrationDigest = state.RegistrationDigest(facts)
		if attempt.Status == state.AttemptPending {
			continue
		}
		facts.SubmittedWorktreeState = attempt.SubmittedWorktree
		facts.SubmittedCancelled = attempt.SubmittedCancelled
		facts.SubmittedTruncated = attempt.SubmittedTruncated
		facts.SubmittedLogDigest = attempt.SubmittedLogDigest
		facts.FinishedAt = attempt.FinishedAt
		var results []state.CheckResult
		for _, result := range manifest.CheckResults {
			if result.AttemptID != attempt.ID {
				continue
			}
			results = append(results, state.CheckResult{
				Position: result.Position, Name: result.Name, Command: result.Command, Status: result.Status,
				ExitCode: result.ExitCode, DurationMS: result.DurationMS, OutputExcerpt: result.OutputExcerpt,
				Truncated: result.Truncated, CleanupError: result.CleanupError,
			})
		}
		attempt.CompletionDigest = state.CompletionDigest(facts, results)
	}
}

func TestBackupSerializesOnlyAuthoritativeTaskAndCycleFacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := state.Open(ctx, filepath.Join(root, "state"))
	noErr(t, err)
	defer store.Close()
	adminHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	noErr(t, store.CompleteSetup(ctx, filepath.Join(root, "repositories"), "open", "", adminHash, true))
	now := time.Unix(1_800_000_000, 0)
	noErr(t, store.AddRepository(ctx, state.Repository{ID: "project", Name: "Project", CreatedAt: now}))
	task, err := store.CreateTask(ctx, "project", "Authoritative backup", now)
	noErr(t, err)
	cycleID := "fedcba9876543210fedcba9876543210"
	if _, _, err := store.ReserveCorrectionCycle(ctx, "project", task.ID, cycleID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempt := state.CheckAttempt{
		ID: "0123456789abcdef0123456789abcdef", TaskID: task.ID, RepositoryID: "project",
		RevisionOID: strings.Repeat("a", 40), WorktreeState: state.WorktreeClean,
		StartedAt: now.Add(2 * time.Minute), CreatedAt: now.Add(2 * time.Minute), CycleID: cycleID,
		Protection: state.ProtectionUnknown, ExecutionScope: state.ExecutionScopeInherited,
		Checks: []state.CheckDefinition{{Name: "unit", Command: "go test ./..."}},
	}
	if _, _, err := store.RegisterCheckAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.RecoverySnapshot(ctx)
	noErr(t, err)
	manifest := Manifest{Format: backupFormat, Version: backupVersion, CreatedAt: now, AccessMode: "open", AdminHash: adminHash}
	addCheckState(&manifest, snapshot)
	encoded, err := json.Marshal(manifest)
	noErr(t, err)
	var document map[string]any
	noErr(t, json.Unmarshal(encoded, &document))
	taskRecord := document["tasks"].([]any)[0].(map[string]any)
	if len(taskRecord) != 5 || taskRecord["id"] != task.ID || taskRecord["repository_id"] != "project" || taskRecord["title"] != "Authoritative backup" || taskRecord["created_at"] == nil || taskRecord["updated_at"] == nil {
		t.Fatalf("backup task facts=%v", taskRecord)
	}
	for _, field := range []string{
		"status", "correction_cycles_used", "initial_check_done", "last_registered_sequence",
		"last_applied_sequence", "pending_attempt_id", "last_applied_attempt_id", "last_applied_finished_at",
	} {
		if _, exists := taskRecord[field]; exists {
			t.Errorf("backup task contains derived field %q: %s", field, encoded)
		}
	}
	cycleRecord := document["check_cycles"].([]any)[0].(map[string]any)
	if len(cycleRecord) != 6 || cycleRecord["id"] != cycleID || cycleRecord["task_id"] != task.ID || cycleRecord["repository_id"] != "project" {
		t.Fatalf("backup cycle facts=%v", cycleRecord)
	}
	if _, exists := cycleRecord["attempt_id"]; exists {
		t.Errorf("backup cycle contains derived attempt pointer: %s", encoded)
	}
	tampered := bytes.Replace(encoded, []byte(`"tasks":[{`), []byte(`"tasks":[{"status":"`+state.TaskResolved+`",`), 1)
	manifestPath := filepath.Join(root, manifestName)
	noErr(t, os.WriteFile(manifestPath, tampered, 0o600))
	if _, err := readManifest(manifestPath); err == nil || !strings.Contains(err.Error(), `unknown field "status"`) {
		t.Fatalf("backup accepted a removed task projection field: %v", err)
	}
}

func TestBackupPreservesCheckRecordsAndDropsHelperAuthority(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	repositoriesRoot := filepath.Join(root, "repositories")
	noErr(t, os.Mkdir(repositoriesRoot, 0o700))
	store, err := state.Open(ctx, stateDir)
	noErr(t, err)
	adminHash, _ := auth.HashPassword("admin-password")
	noErr(t, store.CompleteSetup(ctx, repositoriesRoot, "open", "", adminHash, true))
	runner, err := gitexec.New("", filepath.Join(stateDir, "runtime"))
	noErr(t, err)
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoriesRoot}
	if _, err := manager.Create(ctx, "project", "check records"); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	task, err := store.CreateTask(ctx, "project", "Build the project", now)
	noErr(t, err)
	exit := 1
	attempt := state.CheckAttempt{
		ID: "0123456789abcdef0123456789abcdef", TaskID: task.ID, RepositoryID: "project",
		RevisionOID: strings.Repeat("a", 40), WorktreeState: state.WorktreeClean,
		Status: state.AttemptFailed, ExitCode: &exit, StartedAt: now, FinishedAt: now.Add(time.Second), DurationMS: 1000,
		Summary: "1 checks: 1 failed", CreatedAt: now,
		Protection: state.ProtectionUnknown, ExecutionScope: state.ExecutionScopeInherited,
		CredentialID: "credential-one", TimeoutMS: 600000, OutputLimitBytes: 65536,
		Checks:  []state.CheckDefinition{{Name: "unit", Command: "go test ./..."}},
		Results: []state.CheckResult{{Name: "unit", Command: "go test ./...", Status: state.AttemptFailed, ExitCode: &exit, DurationMS: 1000, OutputExcerpt: "FAIL"}},
	}
	if _, registered, err := store.RegisterCheckAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	} else if _, _, err := store.CompleteCheckAttempt(ctx, state.CheckCompletion{
		AttemptID: registered.ID, RepositoryID: "project", TaskID: registered.TaskID, Results: attempt.Results,
		FinishedAt: attempt.FinishedAt, WorktreeState: attempt.WorktreeState, Log: "raw log",
	}, now); err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("synthetic-helper-token"))
	if _, _, err := store.CreateHelperCredential(ctx, "project", "laptop", "", tokenHash[:], now); err != nil {
		t.Fatal(err)
	}
	noErr(t, store.Close())

	store, err = state.Open(ctx, stateDir)
	noErr(t, err)
	manager = &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoriesRoot}
	backup := filepath.Join(root, "backup")
	if _, err := CreateWithReport(ctx, store, manager, backup); err != nil {
		store.Close()
		t.Fatal(err)
	}
	noErr(t, store.Close())
	manifest, err := readManifest(filepath.Join(backup, manifestName))
	noErr(t, err)
	if manifest.Version != closedPullRequestBackupVersion || len(manifest.Tasks) != 1 || len(manifest.CheckConfigurations) != 1 || len(manifest.CheckAttempts) != 1 || len(manifest.CheckResults) != 1 {
		t.Fatalf("backup check records: version=%d tasks=%d configs=%d attempts=%d results=%d",
			manifest.Version, len(manifest.Tasks), len(manifest.CheckConfigurations), len(manifest.CheckAttempts), len(manifest.CheckResults))
	}
	// The server-issued order and the payload digests are portable, so a
	// restore keeps the same authority instead of re-deriving it from clocks.
	if manifest.CheckAttempts[0].Sequence != 1 || manifest.CheckAttempts[0].CompletionDigest == "" {
		t.Fatalf("backup lost the attempt order or digest: %+v", manifest.CheckAttempts[0])
	}

	restoredState := canonicalTestTarget(t, filepath.Join(root, "restored-state"))
	restoredRepositories := canonicalTestTarget(t, filepath.Join(root, "restored-repositories"))
	if _, err := RestoreWithReport(ctx, backup, restoredState, restoredRepositories, ""); err != nil {
		t.Fatal(err)
	}
	restored, err := state.Open(ctx, restoredState)
	noErr(t, err)
	defer restored.Close()
	tasks, err := restored.Tasks(ctx, "project")
	if err != nil || len(tasks) != 1 || tasks[0].Status != state.TaskActive || tasks[0].CorrectionCyclesUsed != 0 || !tasks[0].InitialCheckDone {
		t.Fatalf("restored tasks=%+v err=%v", tasks, err)
	}
	if tasks[0].LastAppliedAttemptID != attempt.ID || tasks[0].LastAppliedSequence != 1 {
		t.Fatalf("restored task applied attempt=%q sequence=%d", tasks[0].LastAppliedAttemptID, tasks[0].LastAppliedSequence)
	}
	attempts, err := restored.CheckAttempts(ctx, "project")
	if err != nil || len(attempts) != 1 || attempts[0].Status != state.AttemptFailed || len(attempts[0].Results) != 1 {
		t.Fatalf("restored attempts=%+v err=%v", attempts, err)
	}
	if attempts[0].Protection != state.ProtectionUnknown || attempts[0].ExecutionScope != state.ExecutionScopeInherited ||
		attempts[0].CredentialID != "credential-one" || attempts[0].TimeoutMS != 600000 || attempts[0].OutputLimitBytes != 65536 {
		t.Fatalf("restored attempt execution context=%+v", attempts[0])
	}
	configuration, exists, err := restored.LatestCheckConfiguration(ctx, "project")
	if err != nil || !exists || configuration.Version != 1 || len(configuration.Checks) != 1 {
		t.Fatalf("restored configuration=%+v exists=%v err=%v", configuration, exists, err)
	}
	// Helper authority is machine-local and must be recreated after restore.
	credentials, err := restored.HelperCredentials(ctx, "project")
	if err != nil || len(credentials) != 0 {
		t.Fatalf("restored helper credentials=%+v err=%v", credentials, err)
	}
	restoredRunner, err := gitexec.New("", filepath.Join(restoredState, "runtime-test"))
	noErr(t, err)
	restoredManager := &repository.Manager{Store: restored, Git: restoredRunner, Locks: gitexec.NewLocks(), Root: restoredRepositories}
	rebackup := filepath.Join(root, "backup-again")
	if _, err := CreateWithReport(ctx, restored, restoredManager, rebackup); err != nil {
		t.Fatalf("re-backup restored checks: %v", err)
	}
	rebacked, err := readManifest(filepath.Join(rebackup, manifestName))
	noErr(t, err)
	if !reflect.DeepEqual(rebacked.Tasks, manifest.Tasks) ||
		!reflect.DeepEqual(rebacked.CheckConfigurations, manifest.CheckConfigurations) ||
		!reflect.DeepEqual(rebacked.CheckCycles, manifest.CheckCycles) ||
		!reflect.DeepEqual(rebacked.CheckAttempts, manifest.CheckAttempts) ||
		!reflect.DeepEqual(rebacked.CheckResults, manifest.CheckResults) {
		t.Fatalf("restore and re-backup changed check facts:\nfirst=%+v\nagain=%+v", manifest, rebacked)
	}
	// Restore writes every portable repository fact, including the description
	// and the attempt counter, so the next backup records them unchanged.
	if len(manifest.Repositories) != 1 || manifest.Repositories[0].Description != "check records" || manifest.Repositories[0].AttemptSequence != 1 ||
		!reflect.DeepEqual(rebacked.Repositories, manifest.Repositories) {
		t.Fatalf("restore and re-backup changed repository facts:\nfirst=%+v\nagain=%+v", manifest.Repositories, rebacked.Repositories)
	}
}

func TestBackupForwardVersionIsRejectedAndVersionTwoStillRestores(t *testing.T) {
	validHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, manifestName)
	manifest := Manifest{
		Format: backupFormat, Version: backupVersion, CreatedAt: time.Now().UTC(),
		AccessMode: "open", AdminHash: validHash,
	}
	writeManifestFile(t, manifestPath, manifest)

	// A newer writer must be rejected loudly instead of losing new records.
	manifest.Version = backupVersion + 1
	writeManifestFile(t, manifestPath, manifest)
	if _, err := readManifest(manifestPath); err == nil || !strings.Contains(err.Error(), "unsupported backup version") {
		t.Fatalf("forward version error=%v", err)
	}

	// A version 2 backup without check records still validates.
	manifest.Version = pullRequestBackupVersion
	writeManifestFile(t, manifestPath, manifest)
	read, err := readManifest(manifestPath)
	noErr(t, err)
	if err := validateManifest(read); err != nil {
		t.Fatalf("version 2 backup rejected: %v", err)
	}
}

// The committed baseline formats predate checks, review event identities and
// imports, so a manifest of those versions cannot carry them.
func TestBaselineBackupVersionsRefuseNewerRecords(t *testing.T) {
	hash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	for _, version := range []int{legacyBackupVersion, pullRequestBackupVersion} {
		empty := Manifest{Format: backupFormat, Version: version, CreatedAt: time.Now().UTC(), AccessMode: "open", AdminHash: hash}
		if err := validateManifest(empty); err != nil {
			t.Fatalf("empty version %d manifest rejected: %v", version, err)
		}
	}
	for _, test := range []struct {
		name    string
		version int
		mutate  func(*Manifest)
		want    string
	}{
		{"v1 task", 1, func(m *Manifest) { m.Tasks = []TaskManifest{{ID: "task"}} }, "version 1 backup contains unsupported check metadata"},
		{"task", 2, func(m *Manifest) { m.Tasks = []TaskManifest{{ID: "task"}} }, "version 2 backup contains unsupported check metadata"},
		{"policy", 2, func(m *Manifest) { m.CheckPolicies = []CheckPolicyManifest{{RepositoryID: "project"}} }, "version 2 backup contains unsupported check metadata"},
		{"job", 2, func(m *Manifest) { m.CheckJobs = []CheckJobManifest{{ID: strings.Repeat("1", 32)}} }, "version 2 backup contains unsupported check metadata"},
		{"job identity", 2, func(m *Manifest) {
			m.CheckAttempts = []CheckAttemptManifest{{ID: strings.Repeat("3", 32), JobID: strings.Repeat("4", 32)}}
		}, "version 2 backup contains unsupported check metadata"},
		{"review event", 2, func(m *Manifest) {
			m.PullRequestReviews = []PullRequestReviewManifest{{RepositoryID: "project", PullRequestNumber: 1, ReviewEventID: strings.Repeat("5", 32)}}
		}, "version 2 backup contains an unsupported review event identity"},
		{"import source", 2, func(m *Manifest) { m.ImportSources = []ImportSourceManifest{{RepositoryID: "project"}} }, "version 2 backup contains unsupported import metadata"},
		{"import order evidence", 2, func(m *Manifest) { m.ImportRunOrderKnown = true }, "version 2 backup contains unsupported import metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := Manifest{Format: backupFormat, Version: test.version, CreatedAt: time.Now().UTC(), AccessMode: "open", AdminHash: hash}
			test.mutate(&manifest)
			if err := validateManifest(manifest); err == nil || err.Error() != test.want {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestBackupRejectsTamperedCheckMetadata(t *testing.T) {
	validHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	now := time.Now().UTC()
	checks := []CheckDefinitionManifest{{Name: "unit", Command: "go test ./..."}}
	encoded, err := json.Marshal(checks)
	noErr(t, err)
	hash := sha256.Sum256(encoded)
	attemptID := "0123456789abcdef0123456789abcdef"
	logExpiresAt := now.Add(30 * 24 * time.Hour)
	base := func() Manifest {
		manifest := Manifest{
			Format: backupFormat, Version: backupVersion, CreatedAt: now,
			AccessMode: "open", AdminHash: validHash,
			Repositories: []RepositoryManifest{
				{ID: "project", Name: "Project", CreatedAt: now, Empty: true, AttemptSequence: 1},
				{ID: "other", Name: "Other", CreatedAt: now, Empty: true},
			},
			Tasks: []TaskManifest{{
				ID: "task", RepositoryID: "project", Title: "Task", CreatedAt: now, UpdatedAt: now,
			}},
			CheckConfigurations: []CheckConfigurationManifest{
				{RepositoryID: "project", Version: 1, ConfigHash: hex.EncodeToString(hash[:]), Checks: checks, CreatedAt: now},
				{RepositoryID: "other", Version: 1, ConfigHash: hex.EncodeToString(hash[:]), Checks: checks, CreatedAt: now},
			},
			CheckAttempts: []CheckAttemptManifest{{
				ID: attemptID, TaskID: "task", RepositoryID: "project",
				RevisionOID: strings.Repeat("a", 40), WorktreeState: state.WorktreeClean, SubmittedWorktree: state.WorktreeClean,
				ConfigurationVersion: 1,
				Status:               state.AttemptFailed, StartedAt: now, FinishedAt: now.Add(time.Second), DurationMS: 1000,
				Summary: "1 checks: 1 failed", Protection: state.ProtectionUnknown,
				ExecutionScope: state.ExecutionScopeInherited, CreatedAt: now, Sequence: 1,
				LogID: attemptID, LogExpiresAt: &logExpiresAt,
				SubmittedLogDigest: strings.Repeat("4", 64), LogDigest: strings.Repeat("4", 64),
			}},
			CheckResults: []CheckResultManifest{{
				AttemptID: attemptID, Position: 0, Name: "unit", Command: "go test ./...",
				Status: state.AttemptFailed, DurationMS: 1000,
			}},
		}
		sealDigests(&manifest)
		return manifest
	}
	if err := validateManifest(base()); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	crossRepository := base()
	crossRepository.CheckAttempts[0].RepositoryID = "other"
	if err := validateManifest(crossRepository); err == nil || !strings.Contains(err.Error(), "different repositories") {
		t.Fatalf("cross-repository attempt error=%v", err)
	}

	badHash := base()
	badHash.CheckConfigurations[0].ConfigHash = strings.Repeat("0", 64)
	if err := validateManifest(badHash); err == nil || !strings.Contains(err.Error(), "hash does not match") {
		t.Fatalf("configuration hash error=%v", err)
	}

	gapped := base()
	gapped.CheckResults = append(gapped.CheckResults, CheckResultManifest{
		AttemptID: attemptID, Position: 2, Name: "unit", Command: "go test ./...", Status: state.AttemptFailed,
	})
	if err := validateManifest(gapped); err == nil || !strings.Contains(err.Error(), "dense ordered sequence") {
		t.Fatalf("gapped results error=%v", err)
	}

	inconsistent := base()
	inconsistent.CheckAttempts[0].Status = state.AttemptPassed
	if err := validateManifest(inconsistent); err == nil || !strings.Contains(err.Error(), "does not describe its results") {
		t.Fatalf("aggregate status error=%v", err)
	}

	zero := 0
	cleanupPassed := base()
	cleanupPassed.CheckAttempts[0].Status = state.AttemptPassed
	cleanupPassed.CheckAttempts[0].ExitCode = &zero
	cleanupPassed.CheckAttempts[0].Summary = "1 checks: 1 passed"
	cleanupPassed.CheckResults[0].Status = state.AttemptPassed
	cleanupPassed.CheckResults[0].ExitCode = &zero
	cleanupPassed.CheckResults[0].CleanupError = "owned process exit was not confirmed"
	sealDigests(&cleanupPassed)
	if err := validateManifest(cleanupPassed); err == nil || !strings.Contains(err.Error(), "does not describe its results") {
		t.Fatalf("cleanup-forged aggregate error=%v", err)
	}

	cleanupSummary := cleanupPassed
	cleanupSummary.CheckAttempts[0].Status = state.AttemptError
	sealDigests(&cleanupSummary)
	if err := validateManifest(cleanupSummary); err == nil || !strings.Contains(err.Error(), "summary does not describe") {
		t.Fatalf("cleanup-forged summary error=%v", err)
	}

	cleanupCancelled := func(cleanupFirst bool) Manifest {
		manifest := base()
		cleanup := CheckResultManifest{
			AttemptID: attemptID, Name: "cleanup", Command: "exit 0", Status: state.AttemptError,
			ExitCode: &zero, DurationMS: 1000, CleanupError: "owned process exit was not confirmed",
		}
		cancelled := CheckResultManifest{
			AttemptID: attemptID, Name: "remaining", Command: "exit 0", Status: state.AttemptCancelled,
		}
		if cleanupFirst {
			manifest.CheckResults = []CheckResultManifest{cleanup, cancelled}
		} else {
			manifest.CheckResults = []CheckResultManifest{cancelled, cleanup}
		}
		definitions := make([]CheckDefinitionManifest, 0, len(manifest.CheckResults))
		for index := range manifest.CheckResults {
			manifest.CheckResults[index].Position = index
			definitions = append(definitions, CheckDefinitionManifest{
				Name: manifest.CheckResults[index].Name, Command: manifest.CheckResults[index].Command,
			})
		}
		encoded, err := json.Marshal(definitions)
		noErr(t, err)
		configurationHash := sha256.Sum256(encoded)
		manifest.CheckConfigurations[0].Checks = definitions
		manifest.CheckConfigurations[0].ConfigHash = hex.EncodeToString(configurationHash[:])
		manifest.CheckAttempts[0].Status = state.AttemptCancelled
		manifest.CheckAttempts[0].ExitCode = &zero
		manifest.CheckAttempts[0].Summary = "2 checks: 1 error, 1 cancelled; cancelled"
		manifest.CheckAttempts[0].SubmittedCancelled = true
		sealDigests(&manifest)
		return manifest
	}
	for _, test := range []struct {
		name         string
		cleanupFirst bool
	}{
		{name: "cleanup first", cleanupFirst: true},
		{name: "cleanup last", cleanupFirst: false},
	} {
		t.Run("mixed cleanup recovery "+test.name, func(t *testing.T) {
			forged := cleanupCancelled(test.cleanupFirst)
			if err := validateManifest(forged); err == nil || !strings.Contains(err.Error(), "status does not describe") {
				t.Fatalf("cleanup-forged mixed aggregate error=%v", err)
			}

			canonical := cleanupCancelled(test.cleanupFirst)
			forgedDigest := canonical.CheckAttempts[0].CompletionDigest
			canonical.CheckAttempts[0].Status = state.AttemptError
			canonical.CheckAttempts[0].Summary = "2 checks: 1 error, 1 cancelled"
			sealDigests(&canonical)
			if canonical.CheckAttempts[0].CompletionDigest != forgedDigest {
				t.Fatal("derived cleanup verdict changed the submitted completion digest")
			}
			if err := validateManifest(canonical); err != nil {
				t.Fatalf("canonical mixed cleanup manifest rejected: %v", err)
			}
		})
	}

	// A result whose command disagrees with its configuration entry is
	// rejected, so evidence cannot be reattributed.
	mismatchedCommand := base()
	mismatchedCommand.CheckResults[0].Command = "go vet ./..."
	if err := validateManifest(mismatchedCommand); err == nil || !strings.Contains(err.Error(), "does not match its configuration entry") {
		t.Fatalf("mismatched result command error=%v", err)
	}

	// A manipulated clock cannot reorder work: the sequence is authority.
	badSequence := base()
	badSequence.CheckAttempts[0].Sequence = 0
	if err := validateManifest(badSequence); err == nil || !strings.Contains(err.Error(), "invalid check attempt sequence") {
		t.Fatalf("bad attempt sequence error=%v", err)
	}
	withCycle := base()
	withCycle.CheckCycles = []CheckCycleManifest{{
		ID: "ffffffffffffffffffffffffffffffff", TaskID: "task", RepositoryID: "project",
		Sequence: 1, ReservedAt: now, ReservedAfterSequence: 1,
	}}
	if err := validateManifest(withCycle); err != nil {
		t.Fatalf("valid reserved round rejected: %v", err)
	}
	crossRepositoryCycle := base()
	crossRepositoryCycle.CheckCycles = []CheckCycleManifest{{
		ID: "ffffffffffffffffffffffffffffffff", TaskID: "task", RepositoryID: "other",
		Sequence: 1, ReservedAt: now, ReservedAfterSequence: 1,
	}}
	if err := validateManifest(crossRepositoryCycle); err == nil || !strings.Contains(err.Error(), "different repositories") {
		t.Fatalf("cross-repository cycle error=%v", err)
	}

	// A pending registration has no verdict and no finish time.
	pendingWithResult := base()
	pendingWithResult.CheckAttempts[0].Status = state.AttemptPending
	if err := validateManifest(pendingWithResult); err == nil || !strings.Contains(err.Error(), "pending check attempt already has a result") {
		t.Fatalf("pending attempt with results error=%v", err)
	}

	// Restore validates check facts before it creates either destination or a
	// sibling staging directory. Version 1 also reaches counter validation even
	// though that format has no check records.
	invalidLog := base()
	invalidLog.CheckAttempts[0].LogID = "../outside"
	legacyCounter := Manifest{
		Format: backupFormat, Version: legacyBackupVersion, CreatedAt: now,
		AccessMode: "open", AdminHash: validHash,
		Repositories: []RepositoryManifest{{
			ID: "project", Name: "Project", CreatedAt: now, Empty: true, AttemptSequence: 1,
		}},
	}
	for _, test := range []struct {
		name     string
		manifest Manifest
	}{
		{name: "current log metadata", manifest: invalidLog},
		{name: "version one counter without attempts", manifest: legacyCounter},
	} {
		t.Run("restore rejects "+test.name+" before staging", func(t *testing.T) {
			if err := validateManifest(test.manifest); err == nil {
				t.Fatal("manifest validation accepted impossible check metadata")
			}
			invalidBackup := t.TempDir()
			writeManifestFile(t, filepath.Join(invalidBackup, manifestName), test.manifest)
			targetParent := t.TempDir()
			stateTarget := filepath.Join(targetParent, "state")
			repositoryTarget := filepath.Join(targetParent, "repositories")
			if _, err := RestoreWithReport(context.Background(), invalidBackup, stateTarget, repositoryTarget, ""); err == nil {
				t.Fatal("restore accepted impossible check metadata")
			}
			for _, target := range []string{stateTarget, repositoryTarget} {
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatalf("invalid restore wrote target %q: %v", target, err)
				}
				stages, err := filepath.Glob(target + ".owngit-restore-*")
				if err != nil || len(stages) != 0 {
					t.Fatalf("invalid restore left stages for %q: %v err=%v", target, stages, err)
				}
			}
		})
	}
}

func writeManifestFile(t *testing.T, path string, manifest Manifest) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	noErr(t, err)
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		file.Close()
		t.Fatal(err)
	}
	noErr(t, file.Close())
}

// TestBackupRejectsInvalidCheckGraphs covers graph relationships that the
// portable validation must reject instead of accepting a plausible-looking but
// inconsistent history.
func TestBackupRejectsInvalidCheckGraphs(t *testing.T) {
	validHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	now := time.Now().UTC()
	checks := []CheckDefinitionManifest{
		{Name: "unit", Command: "go test ./..."},
		{Name: "vet", Command: "go vet ./..."},
	}
	encoded, err := json.Marshal(checks)
	noErr(t, err)
	hash := sha256.Sum256(encoded)
	attemptID := "0123456789abcdef0123456789abcdef"
	cycleID := "fedcba9876543210fedcba9876543210"
	logExpiresAt := now.Add(30 * 24 * time.Hour)
	base := func() Manifest {
		manifest := Manifest{
			Format: backupFormat, Version: backupVersion, CreatedAt: now,
			AccessMode: "open", AdminHash: validHash,
			Repositories: []RepositoryManifest{{ID: "project", Name: "Project", CreatedAt: now, Empty: true, AttemptSequence: 1}},
			Tasks: []TaskManifest{{
				ID: "task", RepositoryID: "project", Title: "Task", CreatedAt: now, UpdatedAt: now,
			}},
			CheckConfigurations: []CheckConfigurationManifest{
				{RepositoryID: "project", Version: 1, ConfigHash: hex.EncodeToString(hash[:]), Checks: checks, CreatedAt: now},
			},
			CheckCycles: []CheckCycleManifest{
				{ID: cycleID, TaskID: "task", RepositoryID: "project", Sequence: 1, ReservedAt: now, ReservedAfterSequence: 0},
			},
			CheckAttempts: []CheckAttemptManifest{{
				ID: attemptID, TaskID: "task", RepositoryID: "project",
				RevisionOID: strings.Repeat("a", 40), WorktreeState: state.WorktreeClean, SubmittedWorktree: state.WorktreeClean,
				ConfigurationVersion: 1,
				Status:               state.AttemptFailed, StartedAt: now, FinishedAt: now.Add(time.Second), DurationMS: 1000,
				Summary: "2 checks: 2 failed", Protection: state.ProtectionUnknown,
				ExecutionScope: state.ExecutionScopeInherited, CreatedAt: now, Sequence: 1, CycleID: cycleID,
				LogID: attemptID, LogExpiresAt: &logExpiresAt,
				SubmittedLogDigest: strings.Repeat("4", 64), LogDigest: strings.Repeat("4", 64),
			}},
			CheckResults: []CheckResultManifest{
				{AttemptID: attemptID, Position: 0, Name: "unit", Command: "go test ./...", Status: state.AttemptFailed, DurationMS: 1000},
				{AttemptID: attemptID, Position: 1, Name: "vet", Command: "go vet ./...", Status: state.AttemptFailed, DurationMS: 1000},
			},
		}
		sealDigests(&manifest)
		return manifest
	}
	if err := validateManifest(base()); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	// A gap in the reserved budget numbering must not pass as a dense sequence.
	gappedCycles := base()
	gappedCycles.CheckCycles = append(gappedCycles.CheckCycles, CheckCycleManifest{
		ID: "11111111111111111111111111111111", TaskID: "task", RepositoryID: "project", Sequence: 3, ReservedAt: now, ReservedAfterSequence: 1,
	})
	if err := validateManifest(gappedCycles); err == nil {
		t.Error("cycle sequence gap was accepted")
	}
	overBudget := base()
	overBudget.CheckCycles[0].Sequence = int64(state.CorrectionCycleLimit + 1)
	if err := validateManifest(overBudget); err == nil {
		t.Error("cycle beyond the correction budget was accepted")
	}

	// Every configured check needs exactly one result.
	missingResult := base()
	missingResult.CheckResults = missingResult.CheckResults[:1]
	if err := validateManifest(missingResult); err == nil {
		t.Error("missing configured result was accepted")
	}

	// An attempt cannot refer to a cycle that was never reserved.
	orphanCycle := base()
	orphanCycle.CheckAttempts[0].CycleID = "ffffffffffffffffffffffffffffffff"
	if err := validateManifest(orphanCycle); err == nil {
		t.Error("orphan cycle reference was accepted")
	}

	// Server-observed task timestamps must remain ordered.
	mismatchedTime := base()
	mismatchedTime.Tasks[0].UpdatedAt = now.Add(-time.Hour)
	if err := validateManifest(mismatchedTime); err == nil {
		t.Error("reversed task timestamps were accepted")
	}

	// Canonical digests must be present and well formed.
	for name, mutate := range map[string]func(*Manifest){
		"registration":  func(manifest *Manifest) { manifest.CheckAttempts[0].RegistrationDigest = "" },
		"completion":    func(manifest *Manifest) { manifest.CheckAttempts[0].CompletionDigest = "" },
		"submitted log": func(manifest *Manifest) { manifest.CheckAttempts[0].SubmittedLogDigest = "" },
		"malformed log": func(manifest *Manifest) { manifest.CheckAttempts[0].LogDigest = "not-a-digest" },
	} {
		absent := base()
		mutate(&absent)
		if err := validateManifest(absent); err == nil {
			t.Errorf("absent %s digest was accepted", name)
		}
	}
}

// TestUnreleasedBackupVersionsAreRefused covers the development backup formats
// 3 through 8 and a newer format. They are refused before any file is written.
func TestUnreleasedBackupVersionsAreRefused(t *testing.T) {
	validHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	noErr(t, os.Mkdir(backup, 0o700))
	manifestPath := filepath.Join(backup, manifestName)
	manifest := Manifest{
		Format: backupFormat, Version: backupVersion, CreatedAt: time.Now().UTC(),
		AccessMode: "open", AdminHash: validHash,
	}
	for _, version := range []int{3, 4, 5, 6, 7, 8, backupVersion + 1} {
		manifest.Version = version
		writeManifestFile(t, manifestPath, manifest)
		want := fmt.Sprintf("backup uses the unreleased development format %d; this build supports versions 1, 2, 9, 10, 11, 12, and 13", version)
		if version > backupVersion {
			want = "unsupported backup version 14: this build supports versions 1, 2, 9, 10, 11, 12, and 13"
		}
		before, err := os.ReadFile(manifestPath)
		noErr(t, err)
		if _, err := readManifest(manifestPath); err == nil || err.Error() != want {
			t.Fatalf("backup version %d error=%v", version, err)
		}
		stateTarget := filepath.Join(root, fmt.Sprintf("state-%d", version))
		repositoryTarget := filepath.Join(root, fmt.Sprintf("repositories-%d", version))
		if _, err := RestoreWithReport(context.Background(), backup, stateTarget, repositoryTarget, ""); err == nil || err.Error() != want {
			t.Fatalf("restore backup version %d error=%v", version, err)
		}
		for _, target := range []string{stateTarget, repositoryTarget} {
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("refused backup version %d wrote target %q: %v", version, target, err)
			}
		}
		after, err := os.ReadFile(manifestPath)
		noErr(t, err)
		if !bytes.Equal(after, before) {
			t.Fatalf("refused backup version %d changed its input manifest", version)
		}
	}
}

// TestCommittedBaselineUpgradesAndRoundTripsThroughBackup uses the DDL copied
// from the committed baseline revision. It does not synthesize a baseline by
// removing current tables or depend on Git history at test time.
func TestCommittedBaselineUpgradesAndRoundTripsThroughBackup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	repositoriesRoot := filepath.Join(root, "repositories")
	noErr(t, os.Mkdir(repositoriesRoot, 0o700))
	remote := filepath.Join(repositoriesRoot, testfixture.BaselineRepositoryID+".git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", remote)
	work := filepath.Join(root, "baseline-work")
	runGit(t, "", "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.name", "Baseline Test")
	runGit(t, work, "config", "user.email", "baseline@example.invalid")
	noErr(t, os.WriteFile(filepath.Join(work, "base.txt"), []byte("base\n"), 0o600))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "baseline target")
	targetOID := gitOutput(t, work, "rev-parse", "HEAD")
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	runGit(t, work, "checkout", "-b", "feature")
	noErr(t, os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "baseline source")
	sourceOID := gitOutput(t, work, "rev-parse", "HEAD")
	runGit(t, work, "push", "origin", "HEAD:refs/heads/feature")
	sourceRef, targetRef := pullrequest.RevisionRefNames(testfixture.BaselinePullRequestNumber, sourceOID, targetOID)
	runGit(t, "", "--git-dir", remote, "update-ref", sourceRef, sourceOID)
	runGit(t, "", "--git-dir", remote, "update-ref", targetRef, targetOID)

	adminHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	if err := testfixture.CreateCommittedBaselineState(ctx, stateDir, testfixture.BaselineStateOptions{
		RepositoryRoot: repositoriesRoot, AdminPasswordHash: adminHash,
		SourceOID: sourceOID, TargetOID: targetOID,
	}); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("baseline upgrade failed: %v", err)
	}
	snapshot, err := store.RecoverySnapshot(ctx)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if len(snapshot.PullRequests) != 1 || len(snapshot.PullRequestRevisions) != 1 || len(snapshot.PullRequestReviews) != 1 || snapshot.PullRequestReviews[0].Status != state.ReviewApproved {
		store.Close()
		t.Fatalf("upgraded baseline history=%+v", snapshot)
	}
	task, err := store.CreateTask(ctx, testfixture.BaselineRepositoryID, "Baseline task", time.Now())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	runner, err := gitexec.New("", filepath.Join(root, "runtime"))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoriesRoot}
	backup := filepath.Join(root, "backup")
	if _, err := CreateWithReport(ctx, store, manager, backup); err != nil {
		store.Close()
		t.Fatalf("backup of the upgraded baseline failed: %v", err)
	}
	noErr(t, store.Close())

	restoredState := canonicalTestTarget(t, filepath.Join(root, "restored-state"))
	restoredRepositories := canonicalTestTarget(t, filepath.Join(root, "restored-repositories"))
	if _, err := RestoreWithReport(ctx, backup, restoredState, restoredRepositories, ""); err != nil {
		t.Fatalf("restore of the upgraded baseline failed: %v", err)
	}
	restored, err := state.Open(ctx, restoredState)
	noErr(t, err)
	restoredSettings, err := restored.Settings(ctx)
	if err != nil || !restoredSettings.Initialized || restoredSettings.RepositoryRoot != restoredRepositories || restoredSettings.AccessMode != "open" || restoredSettings.AccessSessionVersion != 1 || restoredSettings.AdminSessionVersion != 1 || restoredSettings.InsecureHTTPAccepted {
		restored.Close()
		t.Fatalf("restored baseline settings=%+v err=%v", restoredSettings, err)
	}
	if restoredHash, err := restored.PasswordHash(ctx, "admin"); err != nil || restoredHash != adminHash {
		restored.Close()
		t.Fatalf("restored administrator hash=%q err=%v", restoredHash, err)
	}
	if restoredRepository, exists, err := restored.Repository(ctx, testfixture.BaselineRepositoryID); err != nil || !exists || restoredRepository.Name != "Baseline project" || restoredRepository.Description != "Committed baseline fixture" {
		restored.Close()
		t.Fatalf("restored repository=%+v exists=%v err=%v", restoredRepository, exists, err)
	}
	if hosts, err := restored.TrustedHosts(ctx); err != nil || len(hosts) != 0 {
		restored.Close()
		t.Fatalf("restore revived machine-local trusted hosts: hosts=%v err=%v", hosts, err)
	}
	if _, exists, err := restored.Session(ctx, testfixture.BaselineSessionToken, "admin", time.Unix(1_800_000_000, 0)); err != nil || exists {
		restored.Close()
		t.Fatalf("restore revived a live session: exists=%v err=%v", exists, err)
	}
	tasks, err := restored.Tasks(ctx, testfixture.BaselineRepositoryID)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		restored.Close()
		t.Fatalf("restored tasks=%+v err=%v", tasks, err)
	}
	restoredSnapshot, err := restored.RecoverySnapshot(ctx)
	if err != nil || len(restoredSnapshot.PullRequests) != 1 || len(restoredSnapshot.PullRequestRevisions) != 1 || len(restoredSnapshot.PullRequestReviews) != 1 {
		restored.Close()
		t.Fatalf("restored baseline history=%+v err=%v", restoredSnapshot, err)
	}
	restoredRemote := filepath.Join(restoredRepositories, testfixture.BaselineRepositoryID+".git")
	assertRef(t, restoredRemote, "refs/heads/main", targetOID)
	assertRef(t, restoredRemote, "refs/heads/feature", sourceOID)
	assertRef(t, restoredRemote, sourceRef, sourceOID)
	assertRef(t, restoredRemote, targetRef, targetOID)

	restoredManager := &repository.Manager{Store: restored, Git: runner, Locks: gitexec.NewLocks(), Root: restoredRepositories}
	second := filepath.Join(root, "backup-again")
	if _, err := CreateWithReport(ctx, restored, restoredManager, second); err != nil {
		restored.Close()
		t.Fatalf("re-backup of the restored baseline failed: %v", err)
	}
	noErr(t, restored.Close())
	first, err := readManifest(filepath.Join(backup, manifestName))
	noErr(t, err)
	again, err := readManifest(filepath.Join(second, manifestName))
	noErr(t, err)
	if first.Version != closedPullRequestBackupVersion || again.Version != closedPullRequestBackupVersion || first.AccessMode != again.AccessMode || first.AccessHash != again.AccessHash || first.AdminHash != again.AdminHash || !reflect.DeepEqual(first.PullRequests, again.PullRequests) || !reflect.DeepEqual(first.PullRequestRevisions, again.PullRequestRevisions) || !reflect.DeepEqual(first.PullRequestReviews, again.PullRequestReviews) || !reflect.DeepEqual(first.Tasks, again.Tasks) {
		t.Fatalf("re-backup changed baseline records: first=%+v again=%+v", first, again)
	}
	if len(first.Repositories) != 1 || len(again.Repositories) != 1 || first.Repositories[0].ID != again.Repositories[0].ID || first.Repositories[0].Name != again.Repositories[0].Name || first.Repositories[0].Description != again.Repositories[0].Description || !reflect.DeepEqual(first.Repositories[0].Head, again.Repositories[0].Head) || !sameRefs(first.Repositories[0].Refs, again.Repositories[0].Refs) {
		t.Fatalf("re-backup changed repository history: first=%+v again=%+v", first.Repositories, again.Repositories)
	}
}
