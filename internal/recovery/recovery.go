package recovery

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"owngit/internal/auth"
	"owngit/internal/gitexec"
	"owngit/internal/importgit"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
	"owngit/internal/statepath"
)

var (
	errManifestTooLarge = errors.New("backup manifest is larger than the 64 MiB that backup version 10 and older allow")
	errManifestStart    = errors.New("backup manifest does not start with its format and version")
)

// errDirectReviewManifest refuses records of the removed built-in review,
// which only unreleased development builds wrote.
var errDirectReviewManifest = errors.New("backup contains direct-review records written by an unreleased development build; this build cannot restore them")

const (
	manifestName        = "manifest.json"
	backupFormat        = "owngit-offline-backup"
	legacyBackupVersion = 1
	// pullRequestBackupVersion added durable pull request records. Versions 1
	// and 2 were written by the committed baseline.
	pullRequestBackupVersion = 2
	// checkBackupVersion added checks, jobs, imports and review event
	// identities; the 1.0 releases wrote it. Versions 3 through 8 were
	// written only by unreleased development builds and are refused.
	checkBackupVersion = 9
	// closedPullRequestBackupVersion added closed pull requests and merges
	// recorded as already up to date. Releases 1.0.3 to 1.1.2 wrote it and
	// restore it, so a backup without newer records is still written in it.
	closedPullRequestBackupVersion = 10
	recordsBackupVersion           = 11
	// workflowBackupVersion added workflow runs and their check evidence.
	workflowBackupVersion = 12
	// autoMergeBackupVersion adds the per-pull-request auto-merge intent. A
	// backup without one is still written in format 12.
	autoMergeBackupVersion = 13
	backupVersion          = autoMergeBackupVersion
	// manifestLimit is the most a backup holds: 1 GiB of OwnGit records in
	// its manifest, repositories not counted. Backup refuses a larger state
	// rather than cutting it short, and restore refuses a larger file, so
	// the memory a restore of any supplied file needs is what a real state
	// of that size needs.
	manifestLimit = 1 << 30
	// format10ManifestLimit is the manifest size that readers of version 10
	// and older accept.
	format10ManifestLimit = 64 << 20
	// maxManifestStringBytes is the longest string a manifest holds: the
	// longest text a record holds (an import receipt) with every byte
	// escaped, which takes at most six bytes.
	maxManifestStringBytes = 6 * state.MaxImportIntentJSONBytes
	pendingRestoreName     = state.IncompleteRestoreMarkerName
)

type Manifest struct {
	Format                     string                        `json:"format"`
	Version                    int                           `json:"version"`
	CreatedAt                  time.Time                     `json:"created_at"`
	AccessMode                 string                        `json:"access_mode"`
	AccessHash                 string                        `json:"access_password_hash,omitempty"`
	AdminHash                  string                        `json:"admin_password_hash"`
	Repositories               []RepositoryManifest          `json:"repositories"`
	PullRequests               []PullRequestManifest         `json:"pull_requests,omitempty"`
	PullRequestRevisions       []PullRequestRevisionManifest `json:"pull_request_revisions,omitempty"`
	PullRequestReviews         []PullRequestReviewManifest   `json:"pull_request_reviews,omitempty"`
	PullRequestMergeIntents    []PullRequestMergeManifest    `json:"pull_request_merge_intents,omitempty"`
	PullRequestAutoMerges      []PullRequestAutoMergeManifest `json:"pull_request_auto_merges,omitempty"`
	Tasks                      []TaskManifest                `json:"tasks,omitempty"`
	CheckConfigurations        []CheckConfigurationManifest  `json:"check_configurations,omitempty"`
	CheckCycles                []CheckCycleManifest          `json:"check_cycles,omitempty"`
	CheckAttempts              []CheckAttemptManifest        `json:"check_attempts,omitempty"`
	CheckResults               []CheckResultManifest         `json:"check_results,omitempty"`
	CheckPolicies              []CheckPolicyManifest         `json:"check_policies,omitempty"`
	CheckJobs                  []CheckJobManifest            `json:"check_jobs,omitempty"`
	ActionsRuns                []state.ActionsRun            `json:"actions_runs,omitempty"`
	ImportSources              []ImportSourceManifest        `json:"import_sources,omitempty"`
	ImportRuns                 []ImportRunManifest           `json:"import_runs,omitempty"`
	ImportRunOrderKnown        bool                          `json:"import_run_order_known,omitempty"`
	ImportHEADOwnershipVersion int                           `json:"import_head_ownership_version,omitempty"`
	ImportObservations         []ImportObservationManifest   `json:"import_observations,omitempty"`
	ImportIntents              []ImportIntentManifest        `json:"import_intents,omitempty"`
}

type TaskManifest struct {
	ID           string    `json:"id"`
	RepositoryID string    `json:"repository_id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type CheckCycleManifest struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	RepositoryID string    `json:"repository_id"`
	Sequence     int64     `json:"sequence"`
	ReservedAt   time.Time `json:"reserved_at"`
	// ReservedAfterSequence is the repository attempt counter observed inside
	// the reservation transaction.
	ReservedAfterSequence int64 `json:"reserved_after_sequence"`
}

type CheckConfigurationManifest struct {
	RepositoryID string                    `json:"repository_id"`
	Version      int64                     `json:"version"`
	ConfigHash   string                    `json:"config_hash"`
	Checks       []CheckDefinitionManifest `json:"checks"`
	CreatedAt    time.Time                 `json:"created_at"`
}

type CheckDefinitionManifest struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type CheckAttemptManifest struct {
	ID                   string    `json:"id"`
	TaskID               string    `json:"task_id"`
	RepositoryID         string    `json:"repository_id"`
	RevisionOID          string    `json:"revision_oid"`
	WorktreeState        string    `json:"worktree_state"`
	SubmittedWorktree    string    `json:"submitted_worktree_state,omitempty"`
	ConfigurationVersion int64     `json:"configuration_version"`
	Status               string    `json:"status"`
	ExitCode             *int      `json:"exit_code,omitempty"`
	StartedAt            time.Time `json:"started_at"`
	FinishedAt           time.Time `json:"finished_at"`
	DurationMS           int64     `json:"duration_ms"`
	Summary              string    `json:"summary"`
	Protection           string    `json:"protection,omitempty"`
	ExecutionScope       string    `json:"execution_scope,omitempty"`
	CredentialID         string    `json:"credential_id,omitempty"`
	// JobID links a server-owned automatic job. Empty preserves the exact
	// historical helper facts and digest.
	JobID            string     `json:"job_id,omitempty"`
	TimeoutMS        int64      `json:"timeout_ms,omitempty"`
	OutputLimitBytes int64      `json:"output_limit_bytes,omitempty"`
	LogID            string     `json:"log_id,omitempty"`
	LogExpiresAt     *time.Time `json:"log_expires_at,omitempty"`
	LogTruncated     bool       `json:"log_truncated,omitempty"`
	LogError         string     `json:"log_error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	// Sequence is the server-issued repository-wide order. The digests make a
	// retransmit idempotent without touching an accepted log.
	Sequence           int64  `json:"sequence,omitempty"`
	CycleID            string `json:"cycle_id,omitempty"`
	RegistrationDigest string `json:"registration_digest,omitempty"`
	CompletionDigest   string `json:"completion_digest,omitempty"`
	SubmittedLogDigest string `json:"submitted_log_digest,omitempty"`
	SubmittedTruncated bool   `json:"submitted_truncated,omitempty"`
	SubmittedCancelled bool   `json:"submitted_cancelled,omitempty"`
	LogDigest          string `json:"log_digest,omitempty"`
}

type CheckResultManifest struct {
	AttemptID     string `json:"attempt_id"`
	Position      int    `json:"position"`
	Name          string `json:"name"`
	Command       string `json:"command"`
	Status        string `json:"status"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	OutputExcerpt string `json:"output_excerpt,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	CleanupError  string `json:"cleanup_error,omitempty"`
	Role          string `json:"role,omitempty"`
}

type CheckPolicyManifest struct {
	RunWorkflows        bool                         `json:"run_workflows,omitempty"`
	RepositoryID        string                       `json:"repository_id"`
	PolicyVersion       int64                        `json:"policy_version"`
	PolicyDigest        string                       `json:"policy_digest"`
	Executor            string                       `json:"executor"`
	AllowedEvents       []string                     `json:"allowed_events"`
	MaxTimeoutMS        int64                        `json:"max_timeout_ms"`
	MaxOutputLimitBytes int64                        `json:"max_output_limit_bytes"`
	QueueLimit          int                          `json:"queue_limit"`
	MaxActiveJobs       int                          `json:"max_active_jobs"`
	MaxLeaseMS          int64                        `json:"max_lease_ms"`
	Execution           state.CheckExecutionSettings `json:"execution"`
	ConsentVersion      int64                        `json:"consent_version,omitempty"`
	ConsentDigest       string                       `json:"consent_digest,omitempty"`
	RunnerGeneration    int64                        `json:"runner_generation,omitempty"`
	CreatedAt           time.Time                    `json:"created_at"`
	UpdatedAt           time.Time                    `json:"updated_at"`
}

type CheckJobLimitsManifest struct {
	TimeoutMS        int64 `json:"timeout_ms"`
	OutputLimitBytes int64 `json:"output_limit_bytes"`
}

type CheckJobManifest struct {
	RunID                string                       `json:"run_id,omitempty"`
	JobKey               string                       `json:"job_key,omitempty"`
	MatrixIndex          int                          `json:"matrix_index,omitempty"`
	PlanDigest           string                       `json:"plan_digest,omitempty"`
	Tolerated            bool                         `json:"tolerated,omitempty"`
	ConcurrencyGroup     string                       `json:"concurrency_group,omitempty"`
	MaxParallel          int                          `json:"max_parallel,omitempty"`
	ID                   string                       `json:"id"`
	RepositoryID         string                       `json:"repository_id"`
	TaskID               string                       `json:"task_id"`
	Trigger              string                       `json:"trigger"`
	EventKey             string                       `json:"event_key"`
	SourceOID            string                       `json:"source_oid"`
	BaseOID              string                       `json:"base_oid,omitempty"`
	PullRequestNumber    int64                        `json:"pull_request_number,omitempty"`
	TriggerRef           string                       `json:"trigger_ref"`
	WorkflowPath         string                       `json:"workflow_path"`
	WorkflowOID          string                       `json:"workflow_oid,omitempty"`
	WorkflowDigest       string                       `json:"workflow_digest"`
	ConfigurationVersion int64                        `json:"configuration_version"`
	Executor             string                       `json:"executor"`
	PolicyVersion        int64                        `json:"policy_version"`
	ConsentVersion       int64                        `json:"consent_version"`
	Limits               CheckJobLimitsManifest       `json:"limits"`
	Execution            state.CheckExecutionSettings `json:"execution"`
	DedupDigest          string                       `json:"dedup_digest"`
	RerunRoot            string                       `json:"rerun_root,omitempty"`
	RerunGeneration      int64                        `json:"rerun_generation,omitempty"`
	Status               string                       `json:"status"`
	AttemptID            string                       `json:"attempt_id,omitempty"`
	LeaseID              string                       `json:"lease_id,omitempty"`
	LeaseExpiresAt       *time.Time                   `json:"lease_expires_at,omitempty"`
	CredentialID         string                       `json:"credential_id,omitempty"`
	CredentialGeneration int64                        `json:"credential_generation,omitempty"`
	CredentialRole       string                       `json:"credential_role,omitempty"`
	Protection           string                       `json:"protection,omitempty"`
	AdmittedAt           time.Time                    `json:"admitted_at"`
	ClaimedAt            *time.Time                   `json:"claimed_at,omitempty"`
	StartedAt            *time.Time                   `json:"started_at,omitempty"`
	FinishedAt           *time.Time                   `json:"finished_at,omitempty"`
	LeaseLostAt          *time.Time                   `json:"lease_lost_at,omitempty"`
	CancelRequestedAt    *time.Time                   `json:"cancel_requested_at,omitempty"`
	InterruptedAt        *time.Time                   `json:"interrupted_at,omitempty"`
	Summary              string                       `json:"summary,omitempty"`
}

type RepositoryManifest struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	// AttemptSequence is the repository-wide counter that issues attempt
	// sequences, so a restore keeps issuing higher sequences.
	AttemptSequence int64  `json:"attempt_sequence,omitempty"`
	Head            Head   `json:"head"`
	Refs            []Ref  `json:"refs"`
	Empty           bool   `json:"empty"`
	Bundle          string `json:"bundle,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	// Since format 11: the names of a renamed repository and its own
	// policies, omitted when it was never renamed or keeps every default.
	Names  []RepositoryNameManifest  `json:"names,omitempty"`
	Policy *RepositoryPolicyManifest `json:"policy,omitempty"`
	// Since format 11: "sha256" for an empty SHA-256 repository. A bundle
	// names its own object format, so a repository with one never records
	// it here, and an empty repository without it uses SHA-1.
	ObjectFormat string `json:"object_format,omitempty"`
}

type RepositoryNameManifest struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	CreatedAt  time.Time  `json:"created_at"`
	AliasUntil *time.Time `json:"alias_until,omitempty"`
}

type RepositoryPolicyManifest struct {
	RetainHistory        *bool     `json:"retain_history,omitempty"`
	ProtectDefaultBranch bool      `json:"protect_default_branch,omitempty"`
	ExtraRefPrefixes     []string  `json:"extra_ref_prefixes,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type Head struct {
	Symbolic string `json:"symbolic,omitempty"`
	OID      string `json:"oid,omitempty"`
}

type Ref struct {
	Name string `json:"name"`
	OID  string `json:"oid"`
}

type PullRequestManifest struct {
	RepositoryID   string     `json:"repository_id"`
	Number         int64      `json:"number"`
	Title          string     `json:"title"`
	SourceBranch   string     `json:"source_branch"`
	TargetBranch   string     `json:"target_branch"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	MergeSourceOID string     `json:"merge_source_oid,omitempty"`
	MergeTargetOID string     `json:"merge_target_oid,omitempty"`
	MergeOID       string     `json:"merge_oid,omitempty"`
	MergeReceipt   string     `json:"merge_receipt_ref,omitempty"`
	MergedAt       *time.Time `json:"merged_at,omitempty"`
	// Since format 11.
	Body         string      `json:"body,omitempty"`
	EditRevision int64       `json:"edit_revision,omitempty"`
	EditedAt     *time.Time  `json:"edited_at,omitempty"`
	CreatedBy    state.Actor `json:"created_by,omitzero"`
	EditedBy     state.Actor `json:"edited_by,omitzero"`
	MergedBy     state.Actor `json:"merged_by,omitzero"`
}

type PullRequestRevisionManifest struct {
	RepositoryID      string    `json:"repository_id"`
	PullRequestNumber int64     `json:"pull_request_number"`
	SourceOID         string    `json:"source_oid"`
	TargetOID         string    `json:"target_oid"`
	RecordedAt        time.Time `json:"recorded_at"`
}

type PullRequestReviewManifest struct {
	RepositoryID      string    `json:"repository_id"`
	PullRequestNumber int64     `json:"pull_request_number"`
	Sequence          int64     `json:"sequence"`
	SourceOID         string    `json:"source_oid"`
	TargetOID         string    `json:"target_oid"`
	Status            string    `json:"status"`
	ReviewerLabel     string    `json:"reviewer_label,omitempty"`
	Provenance        string    `json:"provenance"`
	ReviewEventID     string    `json:"review_event_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	// Since format 11.
	Note  string      `json:"note,omitempty"`
	Actor state.Actor `json:"actor,omitzero"`
}

type PullRequestMergeManifest struct {
	RepositoryID      string    `json:"repository_id"`
	PullRequestNumber int64     `json:"pull_request_number"`
	SourceOID         string    `json:"source_oid"`
	TargetOID         string    `json:"target_oid"`
	Mode              string    `json:"mode,omitempty"`
	TreeOID           string    `json:"tree_oid,omitempty"`
	ResultOID         string    `json:"result_oid,omitempty"`
	ReceiptRef        string    `json:"receipt_ref"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// PullRequestAutoMergeManifest is one open pull request's opt-in auto-merge
// intent. Since format 13.
type PullRequestAutoMergeManifest struct {
	RepositoryID      string       `json:"repository_id"`
	PullRequestNumber int64        `json:"pull_request_number"`
	EnabledAt         time.Time    `json:"enabled_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
	EnabledBy         state.Actor  `json:"enabled_by,omitzero"`
}

type pendingRestore struct {
	Version          int    `json:"version"`
	StateTarget      string `json:"state_target"`
	RepositoryTarget string `json:"repository_target"`
	StateStage       string `json:"state_stage"`
	RepositoryStage  string `json:"repository_stage"`
}

type commandRunner interface {
	Run(context.Context, string, io.Reader, ...string) (gitexec.Result, error)
}

// CreateWithReport writes an offline backup and reports its capture-time notices.
func CreateWithReport(ctx context.Context, store *state.Store, manager *repository.Manager, output string) (CaptureReport, error) {
	if err := manager.PrepareStorageIdentities(ctx); err != nil {
		return CaptureReport{}, fmt.Errorf("prepare repository storage before backup: %w", err)
	}
	service := &pullrequest.Service{Store: store, Repositories: manager}
	if err := service.ReconcileAll(ctx); err != nil {
		return CaptureReport{}, fmt.Errorf("reconcile pull request state before backup: %w", err)
	}
	return create(ctx, store, manager, manager.Git, output, manifestLimit)
}

// CreateWhileServing writes a backup while OwnGit serves. manager must be
// the serving process's manager, whose locks every Git writer takes; it has
// already recovered each repository while preparing it. Git writes wait
// only while the backup reads refs; the report says how long.
func CreateWhileServing(ctx context.Context, store *state.Store, manager *repository.Manager, output string) (CaptureReport, error) {
	return create(ctx, store, manager, manager.Git, output, manifestLimit)
}

// create writes a backup whose manifest holds at most limit bytes.
func create(ctx context.Context, store *state.Store, manager *repository.Manager, runner commandRunner, output string, limit int64) (CaptureReport, error) {
	var report CaptureReport
	err := createBackup(ctx, store, manager, runner, output, limit, &report)
	// A full disk is no defect of OwnGit's data; the stage is gone and the
	// message names the folder.
	if diskFull(err) {
		err = &SpaceError{Dir: filepath.Dir(output), Err: err}
	}
	return report, err
}

func createBackup(ctx context.Context, store *state.Store, manager *repository.Manager, runner commandRunner, output string, limit int64, report *CaptureReport) error {
	// The output is held until create returns; see state.Destination for
	// why its stage is then used by path.
	destination, err := state.OpenDestination(output)
	if err != nil {
		return err
	}
	defer destination.Close()
	absolute := destination.Path
	if err := requireAbsent(absolute, "backup"); err != nil {
		return err
	}
	if err := requireApartFromStorage(store, manager, absolute); err != nil {
		return err
	}
	hold, err := manager.HoldForBackup()
	if err != nil {
		return err
	}
	defer hold.Close()
	captured, err := capture(ctx, store, manager, runner, hold, report)
	if err != nil {
		return err
	}
	snapshot := captured.snapshot
	report.Repositories = len(captured.repositories)
	report.Captured = true
	for _, capturedRepository := range captured.repositories {
		report.AliasBranches = append(report.AliasBranches, capturedRepository.aliasBranches...)
	}
	manifest := Manifest{
		Format: backupFormat, CreatedAt: captured.at,
		AccessMode: snapshot.AccessMode, AccessHash: snapshot.AccessPasswordHash, AdminHash: snapshot.AdminPasswordHash,
	}
	addPullRequestState(&manifest, snapshot)
	addCheckState(&manifest, snapshot)
	addImportState(&manifest, snapshot)
	for index := range captured.repositories {
		item := &captured.repositories[index].item
		if item.Empty {
			hold.Release(item.ID)
			if captured.repositories[index].objectFormat == repository.ObjectFormatSHA256 {
				item.ObjectFormat = repository.ObjectFormatSHA256
			}
		} else {
			item.Bundle = path.Join("repositories", item.ID+".bundle")
			// Replaced by the bundle's digest, which has the same length,
			// so the size checked below is the size written.
			item.SHA256 = strings.Repeat("0", sha256.Size*2)
		}
		manifest.Repositories = append(manifest.Repositories, *item)
	}
	addRepositoryRecords(&manifest, snapshot)
	// A state too large for a backup is refused before anything is written.
	version, err := backupManifestVersion(manifest, format10ManifestLimit, limit)
	if err != nil {
		return err
	}

	parent := filepath.Dir(absolute)
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	stage, err := destination.CreateStage("." + filepath.Base(absolute) + ".owngit-backup-" + suffix)
	if err != nil {
		return fmt.Errorf("create staged backup: %w", err)
	}
	published := false
	defer func() {
		if !published {
			destination.ReleaseStage()
			_ = os.RemoveAll(stage)
		}
	}()
	bundles := filepath.Join(stage, "repositories")
	if err := os.Mkdir(bundles, 0o700); err != nil {
		return err
	}
	captureRoot := filepath.Join(stage, "capture")
	if err := os.Mkdir(captureRoot, 0o700); err != nil {
		return err
	}
	for index := range manifest.Repositories {
		item := &manifest.Repositories[index]
		if item.Empty {
			continue
		}
		bundlePath := filepath.Join(stage, filepath.FromSlash(item.Bundle))
		if err := bundleCaptured(ctx, runner, captured.repositories[index], captureRoot, bundlePath); err != nil {
			return err
		}
		if err := syncRegularFile(bundlePath); err != nil {
			return err
		}
		if item.SHA256, err = fileSHA256(bundlePath); err != nil {
			return err
		}
		// The bundle holds every object it needs now.
		hold.Release(item.ID)
	}
	if err := os.Remove(captureRoot); err != nil {
		return err
	}
	manifest.Version = version
	if err := validateManifest(manifest); err != nil {
		return fmt.Errorf("validate completed backup manifest: %w", err)
	}
	manifestPath := filepath.Join(stage, manifestName)
	file, err := state.CreatePrivateFile(manifestPath)
	if err != nil {
		return err
	}
	digest := sha256.New()
	if err := writeManifest(io.MultiWriter(file, digest), manifest); err != nil {
		file.Close()
		return err
	}
	report.ManifestSHA256 = hex.EncodeToString(digest.Sum(nil))
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := syncDirectory(bundles); err != nil {
		return err
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireAbsent(absolute, "backup"); err != nil {
		return err
	}
	destination.ReleaseStage()
	if err := publishBackup(stage, absolute); err != nil {
		return fmt.Errorf("publish completed backup: %w", err)
	}
	published = true
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("backup was published but its parent directory could not be synchronized: %w", err)
	}
	return nil
}

// CheckBackupFolder refuses dir as the folder of new backups when a backup
// in it would overlap the state or repository storage.
func CheckBackupFolder(store *state.Store, manager *repository.Manager, dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	return requireApartFromStorage(store, manager, filepath.Join(absolute, "backup"))
}

// ErrBackupOverlapsStorage refuses a backup folder that overlaps OwnGit's
// state or repository storage.
var ErrBackupOverlapsStorage = errors.New("backup destination must not overlap state or repository storage")

func requireApartFromStorage(store *state.Store, manager *repository.Manager, output string) error {
	stateRoot, err := canonicalExistingDirectory(store.Dir(), "state storage")
	if err != nil {
		return err
	}
	repositoryRoot, err := canonicalExistingDirectory(manager.RepositoryRoot(), "repository storage")
	if err != nil {
		return err
	}
	if pathsOverlap(output, stateRoot) || pathsOverlap(output, repositoryRoot) {
		return ErrBackupOverlapsStorage
	}
	return nil
}

type restoreOperations struct {
	report          *RestoreReport
	rename          func(string, string) error
	openState       func(context.Context, string) (*state.Store, error)
	prepareExisting func(context.Context, *repository.Manager, *gitexec.Runner) error
	// rehearsal, set by Verify, is told what the backup holds and how each
	// repository's checks ended.
	rehearsal *Verification
	// input, when set, is the backup folder already held, which restore
	// reads instead of opening its path.
	input *backupInput
	// manifestSHA256, when set, is the SHA-256 the manifest must have: the
	// backup is then that one backup, whose bundles the manifest's digests
	// bind to it. A manifest that cannot be read or differs fails with
	// ErrReplaced, since the folder no longer holds that backup.
	manifestSHA256 string
}

func defaultRestoreOperations() restoreOperations {
	return restoreOperations{
		rename:    renameNoReplace,
		openState: state.Open,
		prepareExisting: func(ctx context.Context, manager *repository.Manager, hookRuntime *gitexec.Runner) error {
			return manager.PrepareExistingForRuntime(ctx, hookRuntime)
		},
	}
}

// RestoreWithReport restores the backup into two new folders and names
// repositories with malformed objects, preserving their history for repair.
// When ctx ends first, it returns an *Interrupted error that says what it left.
func RestoreWithReport(ctx context.Context, input, stateDirectory, repositoryRoot, gitPath string) (RestoreReport, error) {
	var report RestoreReport
	operations := defaultRestoreOperations()
	operations.report = &report
	err := restore(ctx, input, stateDirectory, repositoryRoot, gitPath, operations)
	if err == nil {
		return report, nil
	}
	if ctx.Err() == nil {
		return RestoreReport{}, err
	}
	// A restore stopped by its context removed its stages, or rolled the
	// state back; an error that says otherwise is passed on.
	if errors.Is(err, ctx.Err()) && absentPath(stateDirectory) && absentPath(repositoryRoot) {
		return RestoreReport{}, &Interrupted{What: "restore", Detail: "nothing was restored", Cause: err}
	}
	return RestoreReport{}, &Interrupted{What: "restore", Detail: err.Error(), Cause: err}
}

// Interrupted is work that stopped because its context ended. Detail, when
// set, says what the work left.
type Interrupted struct {
	What, Detail string
	Cause        error
}

func (e *Interrupted) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("the %s was interrupted", e.What)
	}
	return fmt.Sprintf("the %s was interrupted: %s", e.What, e.Detail)
}

func (e *Interrupted) Unwrap() error { return e.Cause }

// absentPath reports whether nothing exists at path.
func absentPath(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func restore(ctx context.Context, input, stateDirectory, repositoryRoot, gitPath string, operations restoreOperations) error {
	source := operations.input
	if source == nil {
		var err error
		if source, err = openBackupInput(input); err != nil {
			return err
		}
		defer source.Close()
	}
	inputRoot := source.path
	// Both destinations are held until restore returns; see
	// state.Destination for why their stages are then used by path.
	stateDestination, err := state.OpenStateDestination(stateDirectory)
	if err != nil {
		return err
	}
	defer stateDestination.Close()
	stateTarget := stateDestination.Path
	if err := requireAbsent(stateTarget, "state"); err != nil {
		return err
	}
	repositoryDestination, err := state.OpenDestination(repositoryRoot)
	if err != nil {
		return err
	}
	defer repositoryDestination.Close()
	repositoryTarget := repositoryDestination.Path
	if err := requireAbsent(repositoryTarget, "repository"); err != nil {
		return err
	}
	if pathsOverlap(stateTarget, repositoryTarget) || pathsOverlap(inputRoot, stateTarget) || pathsOverlap(inputRoot, repositoryTarget) {
		return errors.New("backup, state, and repository paths must not overlap")
	}
	// Both folders are published with the rename that never replaces
	// anything, so a file system without it is refused before any work.
	for _, dir := range []string{stateDestination.Dir(), repositoryDestination.Dir()} {
		if err := requireExclusiveRename(dir); err != nil {
			return err
		}
	}
	if err := requireAttributesInPlace(repositoryDestination.Dir()); err != nil {
		return err
	}

	manifest, digest, err := source.readManifest()
	if operations.manifestSHA256 != "" && (err != nil || digest != operations.manifestSHA256) {
		if err == nil {
			err = errors.New("its manifest is another one")
		}
		return fmt.Errorf("%s: %w: %v", input, ErrReplaced, err)
	}
	if err != nil {
		return err
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	// Only a valid manifest is reported: its repository IDs are then
	// portable names, safe to show.
	rehearsal := operations.rehearsal
	rehearsal.begin(manifest)
	// A restore that the repository folder has no room for is refused
	// before anything is created, and says so; it is no defect of the
	// backup.
	if err := checkSpace(source, filepath.Dir(repositoryTarget), manifest.Repositories); err != nil {
		return err
	}

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	// A stage is removed only once it was created here.
	var stateStage, repositoryStage string
	cleanupStateStage := true
	cleanupRepositoryStage := true
	defer func() {
		if cleanupStateStage && stateStage != "" {
			stateDestination.ReleaseStage()
			_ = os.RemoveAll(stateStage)
		}
		if cleanupRepositoryStage && repositoryStage != "" {
			repositoryDestination.ReleaseStage()
			_ = os.RemoveAll(repositoryStage)
		}
	}()
	if stateStage, err = stateDestination.CreateStage(filepath.Base(stateTarget) + ".owngit-restore-" + suffix); err != nil {
		return fmt.Errorf("create staged state: %w", err)
	}
	if repositoryStage, err = repositoryDestination.CreateStage(filepath.Base(repositoryTarget) + ".owngit-restore-" + suffix); err != nil {
		return fmt.Errorf("create staged repository root: %w", err)
	}
	runner, err := gitexec.New(gitPath, filepath.Join(stateStage, statepath.Runtime))
	if err != nil {
		return err
	}
	// Every repository is checked, and each failure is named, before
	// restore stops. An interruption or a full disk stops it at once and
	// leaves the repository not checked: neither is a defect of the backup.
	var failures []error
	for index, item := range manifest.Repositories {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := restoreRepository(ctx, runner, source, repositoryStage, item)
		// The actual restore reports semantic faults; a rehearsal checks
		// whether the backup can be restored, without refusing that history.
		if err == nil {
			err = operations.report.checkObjects(ctx, runner, repositoryStage, item.ID)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if diskFull(err) {
			return &SpaceError{Dir: filepath.Dir(repositoryTarget), Err: err}
		}
		rehearsal.record(index, err)
		if err != nil {
			failures = append(failures, repositoryFailure(item.ID, err))
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}

	store, err := operations.openState(ctx, stateStage)
	if err != nil {
		return err
	}
	snapshot := recoveryState(manifest)
	if err := store.RestoreRecoveryState(ctx, repositoryTarget, snapshot); err != nil {
		store.Close()
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}

	store, err = operations.openState(ctx, stateStage)
	if err != nil {
		return fmt.Errorf("reopen staged state: %w", err)
	}
	hookRuntime := *runner
	hookRuntime.HomeDir = filepath.Join(stateTarget, statepath.Runtime, statepath.GitHome)
	hookRuntime.TempDir = filepath.Join(stateTarget, statepath.Runtime, statepath.Temporary)
	hookRuntime.GlobalConfigPath = filepath.Join(stateTarget, statepath.Runtime, statepath.GitConfig)
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoryStage}
	prepareErr := operations.prepareExisting(ctx, manager, &hookRuntime)
	var reconcileErr error
	if prepareErr == nil {
		reconcileErr = (&pullrequest.Service{Store: store, Repositories: manager}).ReconcileAll(ctx)
	}
	closeErr := store.Close()
	if prepareErr != nil {
		return fmt.Errorf("rebuild staged repository hooks: %w", prepareErr)
	}
	if reconcileErr != nil {
		return fmt.Errorf("reconcile staged pull request merges: %w", reconcileErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pending := pendingRestore{
		Version: 1, StateTarget: stateTarget, RepositoryTarget: repositoryTarget,
		StateStage: stateStage, RepositoryStage: repositoryStage,
	}
	if err := writePendingRestore(stateStage, pending); err != nil {
		return err
	}
	if err := writePendingRestore(repositoryStage, pending); err != nil {
		return err
	}
	if err := syncDirectory(stateStage); err != nil {
		return err
	}
	if err := syncDirectory(repositoryStage); err != nil {
		return err
	}
	if err := requireAbsent(stateTarget, "state"); err != nil {
		return err
	}
	stateDestination.ReleaseStage()
	if err := operations.rename(stateStage, stateTarget); err != nil {
		return fmt.Errorf("publish guarded state directory: %w", err)
	}
	cleanupStateStage = false

	publishRepositoryErr := syncDirectory(filepath.Dir(stateTarget))
	if publishRepositoryErr == nil {
		publishRepositoryErr = ctx.Err()
	}
	if publishRepositoryErr == nil {
		publishRepositoryErr = requireAbsent(repositoryTarget, "repository")
	}
	if publishRepositoryErr == nil {
		repositoryDestination.ReleaseStage()
		publishRepositoryErr = operations.rename(repositoryStage, repositoryTarget)
	}
	if publishRepositoryErr != nil {
		if rollbackErr := operations.rename(stateTarget, stateStage); rollbackErr != nil {
			cleanupRepositoryStage = false
			return fmt.Errorf("publish repository root: %v; guarded state remains pending at %s and staged repositories remain at %s because rollback failed: %w", publishRepositoryErr, stateTarget, repositoryStage, rollbackErr)
		}
		if syncErr := syncDirectory(filepath.Dir(stateTarget)); syncErr != nil {
			cleanupRepositoryStage = false
			return fmt.Errorf("publish repository root: %v; rolled-back stages remain at %s and %s because rollback could not be synchronized: %w", publishRepositoryErr, stateStage, repositoryStage, syncErr)
		}
		cleanupStateStage = true
		return fmt.Errorf("publish repository root: %w", publishRepositoryErr)
	}
	cleanupRepositoryStage = false
	if err := syncDirectory(filepath.Dir(repositoryTarget)); err != nil {
		return fmt.Errorf("restored paths remain blocked by completion markers because repository publication could not be synchronized: %w", err)
	}

	if err := os.Remove(filepath.Join(repositoryTarget, pendingRestoreName)); err != nil {
		return fmt.Errorf("restored paths remain blocked by completion markers; follow the interrupted-restore procedure: %w", err)
	}
	if err := syncDirectory(repositoryTarget); err != nil {
		return fmt.Errorf("restored state remains blocked while repository completion is synchronized: %w", err)
	}
	if err := os.Remove(filepath.Join(stateTarget, pendingRestoreName)); err != nil {
		return fmt.Errorf("restored paths remain blocked by a completion marker; follow the interrupted-restore procedure: %w", err)
	}
	if err := syncDirectory(stateTarget); err != nil {
		return fmt.Errorf("restored state was completed but its marker removal could not be synchronized: %w", err)
	}
	return nil
}

func inspectRepository(ctx context.Context, runner commandRunner, repositoryPath string, stored state.Repository, refStorage string) (RepositoryManifest, []AliasBranch, error) {
	item := RepositoryManifest{
		ID: stored.ID, Name: stored.Name, Description: stored.Description, CreatedAt: stored.CreatedAt,
		AttemptSequence: stored.AttemptSequence,
	}
	refs, aliases, err := readRefsAndAliases(ctx, runner, repositoryPath)
	if err != nil {
		return RepositoryManifest{}, nil, err
	}
	if refStorage == refStorageFiles {
		missing, err := readMissingBranchAliases(ctx, runner, repositoryPath, refs)
		if err != nil {
			return RepositoryManifest{}, nil, err
		}
		aliases = append(aliases, missing...)
	}
	// Reftable currently reports resolved aliases only; unresolved discovery needs backend support.
	slices.SortFunc(aliases, func(left, right AliasBranch) int { return strings.Compare(left.Name, right.Name) })
	for index := range aliases {
		aliases[index].Repository = stored.ID
	}
	item.Refs = refs
	if item.Head, err = readHead(ctx, runner, repositoryPath, refStorage); err != nil {
		return RepositoryManifest{}, nil, err
	}
	// A symbolic HEAD may name a branch that does not exist yet: no file at
	// its path, or a folder of other branches such as main/topic. Anything
	// else there that Git did not list, such as a link Git skips without a
	// warning, is a ref Git could not read.
	if item.Head.Symbolic != "" && refStorage == refStorageFiles && !slices.ContainsFunc(refs, func(ref Ref) bool { return ref.Name == item.Head.Symbolic }) && !slices.ContainsFunc(aliases, func(alias AliasBranch) bool { return alias.Name == item.Head.Symbolic }) {
		info, err := os.Lstat(filepath.Join(repositoryPath, filepath.FromSlash(item.Head.Symbolic)))
		if !errors.Is(err, os.ErrNotExist) && (err != nil || !info.IsDir()) {
			return RepositoryManifest{}, nil, fmt.Errorf("HEAD names %s, which exists but cannot be read", item.Head.Symbolic)
		}
	}
	// Only a symbolic HEAD can name a branch that does not exist yet, so a
	// repository is empty only when HEAD is symbolic and no ref exists.
	item.Empty = len(refs) == 0 && item.Head.OID == ""
	return item, aliases, nil
}

// readHead reads HEAD as symbolic or detached. The files backend keeps it
// in the HEAD file, read without starting Git; the reftable backend keeps
// it in its tables, so Git reads it there. A HEAD that is neither fails, so
// history is never left out. A detached HEAD's object is checked by the
// caller.
func readHead(ctx context.Context, runner commandRunner, repositoryPath, refStorage string) (Head, error) {
	if refStorage == refStorageFiles {
		return readHeadFile(repositoryPath)
	}
	symbolic, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "symbolic-ref", "--quiet", "HEAD")
	if err == nil {
		return Head{Symbolic: strings.TrimSpace(string(symbolic.Stdout))}, nil
	}
	if code, ok := gitexec.ExitCode(err); !ok || code != 1 {
		return Head{}, fmt.Errorf("read HEAD: %w", err)
	}
	// Exit status 1 without a message: HEAD is detached.
	detached, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return Head{}, fmt.Errorf("detached HEAD cannot be resolved: %w", err)
	}
	return Head{OID: strings.TrimSpace(string(detached.Stdout))}, nil
}

// readHeadFile parses a files-backend HEAD (gitrepository-layout(5)): a
// regular file holding "ref: " and a ref name, or an object ID.
func readHeadFile(repositoryPath string) (Head, error) {
	headPath := filepath.Join(repositoryPath, "HEAD")
	info, err := os.Lstat(headPath)
	if err != nil {
		return Head{}, fmt.Errorf("read HEAD: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return Head{}, errors.New("HEAD is not a file Git writes")
	}
	content, err := os.ReadFile(headPath)
	if err != nil {
		return Head{}, fmt.Errorf("read HEAD: %w", err)
	}
	line, _ := strings.CutSuffix(string(content), "\n")
	if target, symbolic := strings.CutPrefix(line, "ref:"); symbolic {
		target = strings.TrimLeft(target, " \t")
		if !validRefName(target) {
			return Head{}, errors.New("HEAD names an invalid ref")
		}
		return Head{Symbolic: target}, nil
	}
	if !validOID(line) {
		return Head{}, errors.New("HEAD is neither a ref nor an object ID")
	}
	return Head{OID: line}, nil
}

func readRefs(ctx context.Context, runner commandRunner, repositoryPath string) ([]Ref, error) {
	refs, _, err := readRefsAndAliases(ctx, runner, repositoryPath)
	return refs, err
}

func readRefsAndAliases(ctx context.Context, runner commandRunner, repositoryPath string) ([]Ref, []AliasBranch, error) {
	result, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)")
	if err != nil {
		return nil, nil, err
	}
	// Git leaves out a ref it cannot read and only warns, so a warning is
	// an incomplete list, never the refs of the repository.
	if warning, _, _ := strings.Cut(strings.TrimSpace(string(result.Stderr)), "\n"); warning != "" {
		return nil, nil, fmt.Errorf("Git could not read every ref: %q", warning)
	}
	var refs []Ref
	var aliases []AliasBranch
	for _, line := range strings.Split(strings.TrimRight(string(result.Stdout), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 || (parts[2] != "" && !validRefName("refs/"+parts[2])) {
			return nil, nil, errors.New("Git returned malformed ref data")
		}
		refs = append(refs, Ref{Name: parts[0], OID: parts[1]})
		if parts[2] != "" && strings.HasPrefix(parts[0], "refs/heads/") {
			// for-each-ref resolves a symbolic chain to its final target.
			// The reconnect command needs the branch's immediate target.
			target, err := readAliasTarget(ctx, runner, repositoryPath, parts[0])
			if err != nil {
				return nil, nil, err
			}
			aliases = append(aliases, AliasBranch{Name: parts[0], Target: target})
		}
	}
	return refs, aliases, nil
}

func readAliasTarget(ctx context.Context, runner commandRunner, repositoryPath, name string) (string, error) {
	symbolic, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "symbolic-ref", "--no-recurse", name)
	if err != nil {
		return "", fmt.Errorf("read alias branch %q: %w", name, err)
	}
	target := strings.TrimRight(string(symbolic.Stdout), "\r\n")
	// A symbolic target can also be a one-level ref such as HEAD.
	if !validRefName("refs/" + target) {
		return "", fmt.Errorf("Git returned an invalid target for alias branch %q", name)
	}
	return target, nil
}

// Git omits unresolved aliases from for-each-ref; symbolic refs are never packed.
func readMissingBranchAliases(ctx context.Context, runner commandRunner, repositoryPath string, refs []Ref) ([]AliasBranch, error) {
	const maximumEntries = 1000000
	const maximumRefBytes = 4096
	root, err := os.OpenRoot(repositoryPath)
	if err != nil {
		return nil, err
	}
	for _, component := range []string{"refs", "heads"} {
		info, err := root.Lstat(component)
		if os.IsNotExist(err) {
			root.Close()
			return nil, nil
		}
		if err != nil || !info.IsDir() {
			root.Close()
			if err == nil {
				err = fmt.Errorf("%s is not a directory", component)
			}
			return nil, fmt.Errorf("inspect loose branches: %w", err)
		}
		next, err := root.OpenRoot(component)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	defer root.Close()
	listed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		listed[ref.Name] = true
	}
	var aliases []AliasBranch
	entries := 0
	var scan func(*os.Root, string) error
	scan = func(directory *os.Root, prefix string) error {
		file, err := directory.Open(".")
		if err != nil {
			return err
		}
		defer file.Close()
		for {
			batch, readErr := file.ReadDir(128)
			for _, entry := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries++
				if entries > maximumEntries {
					return fmt.Errorf("loose branch scan exceeds %d entries", maximumEntries)
				}
				name := prefix + "/" + entry.Name()
				info, err := directory.Lstat(entry.Name())
				if err != nil {
					return err
				}
				if info.IsDir() {
					child, err := directory.OpenRoot(entry.Name())
					if err != nil {
						return err
					}
					err = scan(child, name)
					child.Close()
					if err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("loose branch %q is not a regular file", name)
				}
				if listed[name] || !validRefName(name) {
					continue
				}
				ref, err := directory.Open(entry.Name())
				if err != nil {
					return err
				}
				content, readErr := io.ReadAll(io.LimitReader(ref, maximumRefBytes+1))
				err = errors.Join(readErr, ref.Close())
				if err != nil {
					return err
				}
				if len(content) > maximumRefBytes {
					return fmt.Errorf("loose branch %q exceeds %d bytes", name, maximumRefBytes)
				}
				if !bytes.HasPrefix(content, []byte("ref: ")) {
					return fmt.Errorf("Git did not list loose branch %q", name)
				}
				target, err := readAliasTarget(ctx, runner, repositoryPath, name)
				if err != nil {
					return err
				}
				aliases = append(aliases, AliasBranch{Name: name, Target: target, MissingTarget: true})
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if err := scan(root, "refs/heads"); err != nil {
		return nil, fmt.Errorf("inspect loose branches: %w", err)
	}
	repositoryRoot, err := os.OpenRoot(repositoryPath)
	if err != nil {
		return nil, err
	}
	defer repositoryRoot.Close()
	targets := make(map[string]string, len(aliases))
	for _, alias := range aliases {
		targets[alias.Name] = alias.Target
	}
	for index := range aliases {
		// Follow each unresolved chain without inventing an object or looping.
		visited := make(map[string]bool)
		current := aliases[index].Name
		missing := false
		for range 128 {
			if listed[current] || visited[current] {
				break
			}
			visited[current] = true
			if target, known := targets[current]; known {
				current = target
				continue
			}
			result, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "symbolic-ref", "--quiet", "--no-recurse", "--", current)
			if err == nil {
				target := strings.TrimRight(string(result.Stdout), "\r\n")
				if !validRefName("refs/" + target) {
					return nil, fmt.Errorf("Git returned an invalid target for alias branch %q", current)
				}
				targets[current] = target
				current = target
				continue
			}
			if code, ok := gitexec.ExitCode(err); !ok || code != 1 {
				return nil, fmt.Errorf("read alias target %q: %w", current, err)
			}
			info, err := repositoryRoot.Lstat(filepath.FromSlash(current))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			// Listed refs include packed entries; a directory alone is not a ref.
			missing = os.IsNotExist(err) || (err == nil && info.IsDir())
			break
		}
		aliases[index].MissingTarget = missing
		aliases[index].UnresolvedTarget = !missing
	}
	return aliases, nil
}

// repositoryFailure names the repository whose restore check failed.
func repositoryFailure(id string, err error) error {
	return fmt.Errorf("validate and restore repository %q: %w", id, err)
}

// copyBundle copies the bundle of item into a new private file in
// repositoryStage, hashing the bytes as it copies them, and returns the
// copy once its digest is the one the manifest records. Git then reads the
// copy, so it restores exactly the bytes that were checked, whatever
// happens to the backup meanwhile. The bundle is opened once, and refused
// unless it is the regular file that the path named before the open, not a
// link; exactly the size it had at the open is read, so a file that grows
// cannot keep the check going. The copy stops when ctx ends.
func copyBundle(ctx context.Context, input *backupInput, repositoryStage string, item RepositoryManifest) (string, error) {
	source, opened, err := input.openBundle(item)
	if errors.Is(err, errNotRegular) {
		return "", errors.New("bundle is not a regular file")
	}
	if err != nil {
		return "", fmt.Errorf("open bundle: %w", err)
	}
	defer source.Close()
	copyPath := filepath.Join(repositoryStage, "."+item.ID+".bundle")
	target, err := state.CreatePrivateFile(copyPath)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	copied, err := io.Copy(io.MultiWriter(target, hash), contextReader{ctx: ctx, reader: io.LimitReader(source, opened.Size())})
	if closeErr := target.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		err = fmt.Errorf("copy bundle: %w", err)
	case copied != opened.Size():
		err = errors.New("bundle changed while it was read")
	case hex.EncodeToString(hash.Sum(nil)) != item.SHA256:
		err = errors.New("bundle checksum mismatch: the bundle is not the one the manifest records")
	}
	if err != nil {
		_ = os.Remove(copyPath)
		return "", err
	}
	return copyPath, nil
}

// contextReader reads until ctx ends.
// bundleObjectFormat reads the object format that a bundle's header names:
// a version 2 bundle is SHA-1, and a version 3 bundle names its format in
// an @object-format capability, SHA-1 when it has none (gitformat-bundle(5)).
// The repository is created with that format before Git reads the bundle,
// because Git cannot index a pack of another object format.
func bundleObjectFormat(bundlePath string) (string, error) {
	file, err := os.Open(bundlePath)
	if err != nil {
		return "", fmt.Errorf("open bundle: %w", err)
	}
	defer file.Close()
	// Capabilities are short; the limit bounds a header that never ends.
	header := bufio.NewReader(io.LimitReader(file, 64<<10))
	line, err := header.ReadString('\n')
	if err != nil {
		return "", errors.New("bundle header is incomplete")
	}
	switch line {
	case "# v2 git bundle\n":
		return repository.ObjectFormatSHA1, nil
	case "# v3 git bundle\n":
	default:
		return "", errors.New("file is not a Git bundle of a supported version")
	}
	format := repository.ObjectFormatSHA1
	for {
		next, err := header.Peek(1)
		if err != nil {
			return "", errors.New("bundle header is incomplete")
		}
		if next[0] != '@' {
			return format, nil
		}
		line, err := header.ReadString('\n')
		if err != nil {
			return "", errors.New("bundle header is incomplete")
		}
		if value, found := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "@object-format="); found {
			if value != repository.ObjectFormatSHA1 && value != repository.ObjectFormatSHA256 {
				return "", fmt.Errorf("bundle uses unsupported object format %q", value)
			}
			format = value
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// restoreRepository restores item from its bundle into a new repository in
// repositoryStage and checks it: the bundle has the digest that the
// manifest records (copyBundle), is complete, the refs and HEAD
// are the ones the manifest records, and git fsck finds every object that
// they reach. Git computed each object's name from its content when it
// indexed the bundle, so this check verifies connectivity; the actual restore
// also checks object contents and reports malformed history separately.
func restoreRepository(ctx context.Context, runner commandRunner, input *backupInput, repositoryStage string, item RepositoryManifest) error {
	repositoryPath := filepath.Join(repositoryStage, item.ID+".git")
	objectFormat := repository.ObjectFormatSHA1
	if item.ObjectFormat != "" {
		objectFormat = item.ObjectFormat
	}
	var bundlePath string
	if !item.Empty {
		var err error
		if bundlePath, err = copyBundle(ctx, input, repositoryStage, item); err != nil {
			return err
		}
		defer os.Remove(bundlePath)
		if objectFormat, err = bundleObjectFormat(bundlePath); err != nil {
			return err
		}
	}
	if _, err := runner.Run(ctx, "", nil, "init", "--bare", "--object-format="+objectFormat, "--initial-branch=main", repositoryPath); err != nil {
		return err
	}
	if !item.Empty {
		if _, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "bundle", "verify", bundlePath); err != nil {
			return fmt.Errorf("verify bundle: %w", err)
		}
		if err := checkBundleRefs(ctx, runner, bundlePath, item); err != nil {
			return err
		}
		arguments := []string{"--git-dir", ".", "fetch", "--no-tags", "--no-write-fetch-head", bundlePath}
		for _, ref := range item.Refs {
			arguments = append(arguments, ref.Name+":"+ref.Name)
		}
		if item.Head.OID != "" {
			arguments = append(arguments, "HEAD")
		}
		if _, err := runner.Run(ctx, repositoryPath, nil, arguments...); err != nil {
			return fmt.Errorf("import bundle: %w", err)
		}
	}
	if item.Head.Symbolic != "" {
		if _, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "symbolic-ref", "HEAD", item.Head.Symbolic); err != nil {
			return err
		}
	} else if item.Head.OID != "" {
		if _, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "update-ref", "--no-deref", "HEAD", item.Head.OID); err != nil {
			return err
		}
	}
	actual, err := readRefs(ctx, runner, repositoryPath)
	if err != nil {
		return err
	}
	if !sameRefs(actual, item.Refs) {
		return errors.New("restored refs do not match the manifest")
	}
	for _, ref := range item.Refs {
		if _, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "cat-file", "-e", ref.OID+"^{object}"); err != nil {
			return fmt.Errorf("restored object %s is missing: %w", ref.OID, err)
		}
	}
	if _, err := runner.Run(ctx, repositoryPath, nil, "--git-dir", ".", "fsck", "--connectivity-only", "--no-progress", "--no-dangling"); err != nil {
		return fmt.Errorf("git fsck: %w", err)
	}
	return nil
}

func checkedInputRoot(input string) (string, error) {
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect backup source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("backup source must be a real directory")
	}
	return canonicalExistingDirectory(absolute, "backup source")
}

func canonicalExistingDirectory(directory, label string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	identity, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve %s identity: %w", label, err)
	}
	info, err := os.Stat(identity)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", label)
	}
	return filepath.Clean(identity), nil
}

func requireAbsent(target, label string) error {
	info, err := os.Lstat(target)
	if err == nil {
		if info.IsDir() {
			if _, markerErr := os.Lstat(filepath.Join(target, pendingRestoreName)); markerErr == nil {
				return fmt.Errorf("%s destination contains an incomplete OwnGit restore; preserve it and follow the interrupted-restore procedure", label)
			}
		}
		return fmt.Errorf("%s destination already exists", label)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s destination: %w", label, err)
	}
	return nil
}

func writePendingRestore(root string, pending pendingRestore) error {
	markerPath := filepath.Join(root, pendingRestoreName)
	file, err := state.CreatePrivateFile(markerPath)
	if err != nil {
		return fmt.Errorf("create incomplete restore marker: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(pending); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func syncRegularFile(filePath string) error {
	file, err := os.OpenFile(filePath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := state.ProtectPrivateHandle(file, false); err != nil {
		file.Close()
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func backupManifestVersion(manifest Manifest, format10Limit, limit int64) (int, error) {
	manifest.Version = backupVersion
	var counter manifestCounter
	if err := writeManifest(&counter, manifest); err != nil {
		return 0, err
	}
	if cost := counter.charged + recordsCost(reflect.ValueOf(manifest)); cost > limit {
		return 0, fmt.Errorf("cannot back up this state: its OwnGit records take %d MiB, and a backup holds at most %d MiB of them (repositories are not counted)", (cost+1<<20-1)>>20, limit>>20)
	}
	if format13Content(manifest) != "" {
		return backupVersion, nil
	}
	if format12Content(manifest) != "" {
		return workflowBackupVersion, nil
	}
	if format11Content(manifest) == "" && counter.written <= format10Limit {
		return closedPullRequestBackupVersion, nil
	}
	return recordsBackupVersion, nil
}

// manifestCounter counts the bytes of a written manifest and the bytes that
// restore charges for them.
type manifestCounter struct {
	text             manifestText
	written, charged int64
}

func (counter *manifestCounter) Write(content []byte) (int, error) {
	for _, character := range content {
		_, charged, err := counter.text.next(character)
		if err != nil {
			return 0, err
		}
		if charged {
			counter.charged++
		}
	}
	counter.written += int64(len(content))
	return len(content), nil
}

// writeManifest writes the manifest one record at a time, so the document
// is never held whole; without whitespace between tokens, so its size is the
// size of its records; and without HTML escaping: a manifest is never HTML,
// and escaping would make each <, > and & in a description six bytes.
func writeManifest(destination io.Writer, manifest Manifest) error {
	output := bufio.NewWriter(destination)
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	write := func(value any) error {
		encoded.Reset()
		if err := encoder.Encode(value); err != nil {
			return fmt.Errorf("encode backup manifest: %w", err)
		}
		_, err := output.Write(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
		return err
	}
	value := reflect.ValueOf(manifest)
	separator := "{"
	for index, field := range manifestFields() {
		fieldValue := value.Field(index)
		isList := fieldValue.Kind() == reflect.Slice
		if field.omitEmpty && (fieldValue.IsZero() || isList && fieldValue.Len() == 0) {
			continue
		}
		output.WriteString(separator + strconv.Quote(field.name) + ":")
		separator = ","
		if !isList || fieldValue.IsNil() {
			if err := write(fieldValue.Interface()); err != nil {
				return err
			}
			continue
		}
		output.WriteByte('[')
		for element := range fieldValue.Len() {
			if element > 0 {
				output.WriteByte(',')
			}
			if err := write(fieldValue.Index(element).Interface()); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	}
	output.WriteString("}\n")
	return output.Flush()
}

// manifestField is a top-level manifest field: its JSON name and whether it
// is left out when empty. Fields are in the order of Manifest.
type manifestField struct {
	name      string
	omitEmpty bool
}

func manifestFields() []manifestField {
	manifestType := reflect.TypeFor[Manifest]()
	fields := make([]manifestField, manifestType.NumField())
	for index := range fields {
		name, options, _ := strings.Cut(manifestType.Field(index).Tag.Get("json"), ",")
		fields[index] = manifestField{name: name, omitEmpty: options == "omitempty"}
	}
	return fields
}

// decodeManifest reads a manifest of size bytes with memory bounded by its
// cost, which the budget limits (manifestBudget): it refuses a file larger
// than limit (or than the 64 MiB of version 10 and older) before and while
// reading, and decodes one value at a time, charging each list element, map
// entry and record behind a pointer before it exists, so neither the
// document nor any record is held whole. Each record must hold every field
// its format always writes, so an empty or partial record stops the read at
// once. Format and version come first in every manifest OwnGit writes; they
// decide how the rest is read, so a backup written by a newer OwnGit is
// refused as such rather than for its new fields.
func decodeManifest(input io.Reader, size, limit int64) (Manifest, error) {
	tooLarge := fmt.Errorf("backup manifest is larger than the %d MiB a backup holds", limit>>20)
	if size > limit {
		return Manifest{}, tooLarge
	}
	budget := &manifestBudget{limit: limit}
	reader := &manifestReader{source: input, budget: budget, limit: limit, tooLarge: tooLarge}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var manifest Manifest
	decodeErr := func(err error) (Manifest, error) {
		if reader.refused != nil {
			err = reader.refused
		}
		return Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return decodeErr(err)
	}
	fields := fieldsOf(reflect.TypeFor[Manifest]())
	value := reflect.ValueOf(&manifest).Elem()
	leading := []string{"format", "version"}
	seen := make([]bool, len(fields.list))
	position := 0
	for ; decoder.More(); position++ {
		token, err := decoder.Token()
		if err != nil {
			return decodeErr(err)
		}
		key := token.(string)
		switch key {
		case "direct_review_settings", "direct_review_task_contexts", "direct_review_requests":
			return Manifest{}, errDirectReviewManifest
		}
		if position < len(leading) && key != leading[position] {
			return Manifest{}, errManifestStart
		}
		field, err := fields.take(key, seen)
		if err != nil {
			return decodeErr(err)
		}
		if err := decodeValue(decoder, value.Field(field.index), budget); err != nil {
			return decodeErr(err)
		}
		if key != "version" {
			continue
		}
		if manifest.Format != backupFormat {
			return Manifest{}, errors.New("unsupported backup format")
		}
		if err := validateBackupVersion(manifest.Version); err != nil {
			return Manifest{}, err
		}
		if manifest.Version < recordsBackupVersion {
			reader.limit, reader.tooLarge = format10ManifestLimit, errManifestTooLarge
			if size > reader.limit {
				return Manifest{}, errManifestTooLarge
			}
		}
	}
	if position < len(leading) {
		return Manifest{}, errManifestStart
	}
	if _, err := decoder.Token(); err != nil {
		return decodeErr(err)
	}
	if err := fields.complete(seen); err != nil {
		return decodeErr(err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Manifest{}, errors.New("backup manifest contains trailing data")
	}
	return manifest, nil
}

// manifestBudget limits the cost of a manifest, which is what backup
// checks and restore charges as it reads: every byte except whitespace
// between tokens (manifestText), plus the in-memory size of every list
// element (twice, for the list's growth), map entry (twice, for the map's)
// and record behind a pointer. So the budget bounds the memory a manifest
// takes, however small its records.
type manifestBudget struct {
	limit, used int64
}

func (budget *manifestBudget) charge(cost int64) error {
	budget.used += cost
	if budget.used > budget.limit {
		return fmt.Errorf("backup manifest holds more than the %d MiB of records a backup holds", budget.limit>>20)
	}
	return nil
}

func sliceElementCost(list reflect.Type) int64 { return 2 * int64(list.Elem().Size()) }

func mapEntryCost(table reflect.Type) int64 {
	return 2 * int64(table.Key().Size()+table.Elem().Size())
}

// recordsCost is what decodeValue charges for value beyond its bytes.
func recordsCost(value reflect.Value) int64 {
	valueType := value.Type()
	if !streamed(valueType) {
		if valueType.Kind() == reflect.Pointer && !value.IsNil() {
			return int64(valueType.Elem().Size())
		}
		return 0
	}
	var cost int64
	switch valueType.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			cost = int64(valueType.Elem().Size()) + recordsCost(value.Elem())
		}
	case reflect.Struct:
		for _, field := range fieldsOf(valueType).list {
			cost += recordsCost(value.Field(field.index))
		}
	case reflect.Slice:
		for index := range value.Len() {
			cost += sliceElementCost(valueType) + recordsCost(value.Index(index))
		}
	case reflect.Map:
		for entry := value.MapRange(); entry.Next(); {
			cost += mapEntryCost(valueType) + recordsCost(entry.Value())
		}
	}
	return cost
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// streamed reports whether decodeValue reads a value of this type piece by
// piece: a list, a map, a record, or a record behind a pointer. Anything
// else is one JSON scalar, or a type with its own decoding such as
// time.Time, which encoding/json decodes whole.
func streamed(valueType reflect.Type) bool {
	if valueType.Implements(unmarshalerType) || reflect.PointerTo(valueType).Implements(unmarshalerType) {
		return false
	}
	switch valueType.Kind() {
	case reflect.Slice, reflect.Map, reflect.Struct:
		return true
	case reflect.Pointer:
		return streamed(valueType.Elem()) && valueType.Elem().Kind() == reflect.Struct
	}
	return false
}

// decodeValue decodes the next JSON value into target as encoding/json
// does, but a list, map or record one piece at a time, charging budget for
// each piece before it exists.
func decodeValue(decoder *json.Decoder, target reflect.Value, budget *manifestBudget) error {
	valueType := target.Type()
	if !streamed(valueType) {
		if err := decoder.Decode(target.Addr().Interface()); err != nil {
			return err
		}
		if valueType.Kind() == reflect.Pointer && !target.IsNil() {
			return budget.charge(int64(valueType.Elem().Size()))
		}
		return nil
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		target.SetZero()
		return nil
	}
	open := json.Delim('{')
	if valueType.Kind() == reflect.Slice {
		open = '['
	}
	if token != open {
		return fmt.Errorf("json: cannot unmarshal %v into Go value of type %s", token, valueType)
	}
	switch valueType.Kind() {
	case reflect.Pointer:
		if err := budget.charge(int64(valueType.Elem().Size())); err != nil {
			return err
		}
		target.Set(reflect.New(valueType.Elem()))
		return decodeRecord(decoder, target.Elem(), budget)
	case reflect.Struct:
		return decodeRecord(decoder, target, budget)
	case reflect.Slice:
		target.Set(reflect.MakeSlice(valueType, 0, 0))
		for decoder.More() {
			if err := budget.charge(sliceElementCost(valueType)); err != nil {
				return err
			}
			element := reflect.New(valueType.Elem()).Elem()
			if err := decodeValue(decoder, element, budget); err != nil {
				return err
			}
			target.Set(reflect.Append(target, element))
		}
	case reflect.Map:
		target.Set(reflect.MakeMap(valueType))
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key := reflect.ValueOf(token).Convert(valueType.Key())
			if target.MapIndex(key).IsValid() {
				return fmt.Errorf("json: key %q appears twice", token)
			}
			if err := budget.charge(mapEntryCost(valueType)); err != nil {
				return err
			}
			element := reflect.New(valueType.Elem()).Elem()
			if err := decodeValue(decoder, element, budget); err != nil {
				return err
			}
			target.SetMapIndex(key, element)
		}
	}
	_, err = decoder.Token()
	return err
}

// decodeRecord decodes the fields of a record whose opening brace was read.
func decodeRecord(decoder *json.Decoder, target reflect.Value, budget *manifestBudget) error {
	fields := fieldsOf(target.Type())
	seen := make([]bool, len(fields.list))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		field, err := fields.take(token.(string), seen)
		if err != nil {
			return err
		}
		if err := decodeValue(decoder, target.Field(field.index), budget); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return fields.complete(seen)
}

// recordFields are the JSON fields of a record type. A field without
// omitempty or omitzero is required: every OwnGit version that wrote the
// record wrote it.
type recordFields struct {
	list   []recordField
	byName map[string]int
}

type recordField struct {
	name     string
	index    int
	required bool
}

var recordFieldsCache sync.Map

func fieldsOf(recordType reflect.Type) *recordFields {
	if cached, ok := recordFieldsCache.Load(recordType); ok {
		return cached.(*recordFields)
	}
	fields := &recordFields{byName: map[string]int{}}
	for index := range recordType.NumField() {
		structField := recordType.Field(index)
		name, options, _ := strings.Cut(structField.Tag.Get("json"), ",")
		if !structField.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = structField.Name
		}
		fields.byName[name] = len(fields.list)
		fields.list = append(fields.list, recordField{name: name, index: index, required: !strings.Contains(options, "omit")})
	}
	recordFieldsCache.Store(recordType, fields)
	return fields
}

func (fields *recordFields) take(name string, seen []bool) (recordField, error) {
	position, known := fields.byName[name]
	if !known {
		return recordField{}, fmt.Errorf("json: unknown field %q", name)
	}
	if seen[position] {
		return recordField{}, fmt.Errorf("json: field %q appears twice", name)
	}
	seen[position] = true
	return fields.list[position], nil
}

func (fields *recordFields) complete(seen []bool) error {
	for position, field := range fields.list {
		if field.required && !seen[position] {
			return fmt.Errorf("backup record lacks its %q field", field.name)
		}
	}
	return nil
}

// manifestText is the rule backup and restore apply to the bytes of a
// manifest: whitespace between tokens is not charged and shrinks to one
// space, which keeps tokens apart as encoding/json sees them; every other
// byte is charged; and a string longer than maxManifestStringBytes is
// refused.
type manifestText struct {
	inString, escaped, space bool
	stringBytes              int
}

// next reports whether character is kept and whether it is charged.
func (text *manifestText) next(character byte) (kept, charged bool, err error) {
	if text.inString {
		text.stringBytes++
		if text.stringBytes > maxManifestStringBytes {
			return false, false, errors.New("backup manifest holds a text longer than any OwnGit record")
		}
		switch {
		case text.escaped:
			text.escaped = false
		case character == '\\':
			text.escaped = true
		case character == '"':
			text.inString = false
		}
		return true, true, nil
	}
	if character == ' ' || character == '\t' || character == '\n' || character == '\r' {
		kept = !text.space
		text.space = true
		return kept, false, nil
	}
	text.space = false
	if character == '"' {
		text.inString, text.stringBytes = true, 0
	}
	return true, true, nil
}

// manifestReader passes a manifest to the JSON decoder by manifestText,
// charging budget, so runs of whitespace never fill the decoder's buffer,
// and refuses input longer than limit (with tooLarge). A refusal is final:
// the decoder may read again after an error.
type manifestReader struct {
	source      io.Reader
	budget      *manifestBudget
	text        manifestText
	limit, read int64
	tooLarge    error
	refused     error
}

func (reader *manifestReader) Read(buffer []byte) (int, error) {
	for reader.refused == nil {
		count, err := reader.source.Read(buffer)
		reader.read += int64(count)
		if reader.read > reader.limit {
			reader.refused = reader.tooLarge
			return 0, reader.refused
		}
		kept, charged := 0, int64(0)
		for _, character := range buffer[:count] {
			keep, charge, textErr := reader.text.next(character)
			if textErr != nil {
				reader.refused = textErr
				return 0, reader.refused
			}
			if charge {
				charged++
			}
			if keep {
				buffer[kept] = character
				kept++
			}
		}
		if chargeErr := reader.budget.charge(charged); chargeErr != nil {
			reader.refused = chargeErr
			return 0, reader.refused
		}
		if kept > 0 || err != nil {
			return kept, err
		}
	}
	return 0, reader.refused
}

func addPullRequestState(manifest *Manifest, snapshot state.RecoveryState) {
	for _, record := range snapshot.PullRequests {
		manifest.PullRequests = append(manifest.PullRequests, PullRequestManifest{
			RepositoryID: record.RepositoryID, Number: record.Number, Title: record.Title,
			SourceBranch: record.SourceBranch, TargetBranch: record.TargetBranch, Status: record.Status,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
			MergeSourceOID: record.MergeSourceOID, MergeTargetOID: record.MergeTargetOID,
			MergeOID: record.MergeOID, MergeReceipt: record.MergeReceipt, MergedAt: record.MergedAt,
			Body: record.Body, EditRevision: record.EditRevision, EditedAt: record.EditedAt,
			CreatedBy: record.CreatedBy, EditedBy: record.EditedBy, MergedBy: record.MergedBy,
		})
	}
	for _, revision := range snapshot.PullRequestRevisions {
		manifest.PullRequestRevisions = append(manifest.PullRequestRevisions, PullRequestRevisionManifest{
			RepositoryID: revision.RepositoryID, PullRequestNumber: revision.PullRequestNumber,
			SourceOID: revision.SourceOID, TargetOID: revision.TargetOID, RecordedAt: revision.RecordedAt,
		})
	}
	for _, review := range snapshot.PullRequestReviews {
		manifest.PullRequestReviews = append(manifest.PullRequestReviews, PullRequestReviewManifest{
			RepositoryID: review.RepositoryID, PullRequestNumber: review.PullRequestNumber, Sequence: review.Sequence,
			SourceOID: review.SourceOID, TargetOID: review.TargetOID, Status: review.Status,
			ReviewerLabel: review.ReviewerLabel, Provenance: review.Provenance, ReviewEventID: review.ReviewEventID, CreatedAt: review.CreatedAt,
			Note: review.Note, Actor: review.Actor,
		})
	}
	for _, intent := range snapshot.PullRequestMergeIntents {
		manifest.PullRequestMergeIntents = append(manifest.PullRequestMergeIntents, PullRequestMergeManifest{
			RepositoryID: intent.RepositoryID, PullRequestNumber: intent.PullRequestNumber,
			SourceOID: intent.SourceOID, TargetOID: intent.TargetOID, Mode: intent.Mode,
			TreeOID: intent.TreeOID, ResultOID: intent.ResultOID, ReceiptRef: intent.ReceiptRef,
			Status: intent.Status, CreatedAt: intent.CreatedAt, UpdatedAt: intent.UpdatedAt,
		})
	}
	for _, intent := range snapshot.PullRequestAutoMerges {
		manifest.PullRequestAutoMerges = append(manifest.PullRequestAutoMerges, PullRequestAutoMergeManifest{
			RepositoryID: intent.RepositoryID, PullRequestNumber: intent.PullRequestNumber,
			EnabledAt: intent.EnabledAt, UpdatedAt: intent.UpdatedAt, EnabledBy: intent.EnabledBy,
		})
	}
}

// addRepositoryRecords adds each repository's names and, unless it keeps
// every default, its policy.
func addRepositoryRecords(manifest *Manifest, snapshot state.RecoveryState) {
	index := make(map[string]*RepositoryManifest, len(manifest.Repositories))
	for position := range manifest.Repositories {
		index[manifest.Repositories[position].ID] = &manifest.Repositories[position]
	}
	for _, name := range snapshot.RepositoryNames {
		item := index[name.RepositoryID]
		item.Names = append(item.Names, RepositoryNameManifest{Name: name.Name, Kind: name.Kind, CreatedAt: name.CreatedAt, AliasUntil: name.AliasUntil})
	}
	for _, policy := range snapshot.RepositoryPolicies {
		if policy.IsDefault() {
			continue
		}
		index[policy.RepositoryID].Policy = &RepositoryPolicyManifest{
			RetainHistory: policy.RetainHistory, ProtectDefaultBranch: policy.ProtectDefaultBranch,
			ExtraRefPrefixes: policy.ExtraRefPrefixes, UpdatedAt: policy.UpdatedAt,
		}
	}
}

// format11Content names the first record in manifest that format 10 cannot
// hold, or returns "" when there is none. Create writes format 11 only for
// such a backup, and a manifest of an older version that holds one is
// refused instead of losing it.
func format11Content(manifest Manifest) string {
	for _, item := range manifest.Repositories {
		if len(item.Names) != 0 {
			return "a renamed repository"
		}
		if item.Policy != nil {
			return "a repository policy"
		}
		if item.ObjectFormat != "" {
			return "an empty SHA-256 repository"
		}
	}
	for _, record := range manifest.PullRequests {
		if record.Body != "" || record.EditRevision != 0 || record.EditedAt != nil {
			return "a pull request description or edit"
		}
		if record.CreatedBy != (state.Actor{}) || record.EditedBy != (state.Actor{}) || record.MergedBy != (state.Actor{}) {
			return "a pull request actor"
		}
	}
	for _, review := range manifest.PullRequestReviews {
		if review.Note != "" || review.Actor != (state.Actor{}) {
			return "a review note or actor"
		}
	}
	for _, source := range manifest.ImportSources {
		if source.OverwriteDiverged || source.FollowUpstreamDeletions || len(source.ExtraRefPrefixes) != 0 {
			return "an import refresh option"
		}
	}
	// Format 10 readers accept check limits up to the default check
	// ceilings, which were the record bounds of that format.
	for _, policy := range manifest.CheckPolicies {
		if policy.Execution.HasContainerOptions() {
			return "a container option of a check policy"
		}
		if len(state.DefaultCheckCeilings.Exceeded(state.CheckCeilingValues{
			TimeoutMS: policy.MaxTimeoutMS, OutputLimitBytes: policy.MaxOutputLimitBytes,
			QueueLimit: policy.QueueLimit, MaxActiveJobs: policy.MaxActiveJobs, Execution: policy.Execution,
		})) != 0 {
			return "a check policy limit above the earlier bounds"
		}
	}
	for _, job := range manifest.CheckJobs {
		if job.Execution.HasContainerOptions() {
			return "a container option of a check job"
		}
		if len(state.DefaultCheckCeilings.Exceeded(state.CheckCeilingValues{
			TimeoutMS: job.Limits.TimeoutMS, OutputLimitBytes: job.Limits.OutputLimitBytes, Execution: job.Execution,
		})) != 0 {
			return "a check job limit above the earlier bounds"
		}
	}
	// Import history outlives the choices that made it: refs of an extra
	// namespace and deletions stay recorded after the namespace is removed
	// or following deletions is turned off, and format 10 readers accept
	// only HEAD, branches and tags, and no deletion.
	for _, observation := range manifest.ImportObservations {
		if !format10ImportRef(observation.RefName) {
			return "an import record outside branches and tags"
		}
	}
	for _, intent := range manifest.ImportIntents {
		for _, refs := range []map[string]string{intent.Expected, intent.Desired, intent.Observed} {
			for ref := range refs {
				if _, retained := intent.Retained[ref]; !retained && !format10ImportRef(ref) {
					return "an import record outside branches and tags"
				}
			}
		}
		for _, desired := range intent.Desired {
			if desired == "" {
				return "an import deletion"
			}
		}
	}
	return ""
}

// format13Content names the first record in manifest that only format 13
// holds, or returns "" when a format 12 reader could hold the backup.
func format13Content(manifest Manifest) string {
	if len(manifest.PullRequestAutoMerges) != 0 {
		return "an enabled pull request auto-merge"
	}
	return ""
}

func format12Content(manifest Manifest) string {
	if len(manifest.ActionsRuns) != 0 {
		return "workflow runs"
	}
	for _, policy := range manifest.CheckPolicies {
		if policy.RunWorkflows {
			return "a workflow-enabled check policy"
		}
		for _, event := range policy.AllowedEvents {
			if event == state.ActionsEventDispatch || event == state.ActionsEventSchedule {
				return "a workflow event in a check policy"
			}
		}
	}
	for _, job := range manifest.CheckJobs {
		if job.RunID != "" || job.JobKey != "" || job.MatrixIndex != 0 || job.PlanDigest != "" || job.Tolerated || job.ConcurrencyGroup != "" || job.MaxParallel != 0 || job.Status == state.CheckJobWaiting || job.Status == state.CheckJobSkipped || job.Trigger == state.ActionsEventDispatch || job.Trigger == state.ActionsEventSchedule {
			return "workflow job facts"
		}
	}
	for _, attempt := range manifest.CheckAttempts {
		if attempt.Status == state.AttemptSkipped {
			return "a skipped workflow attempt"
		}
	}
	for _, result := range manifest.CheckResults {
		if result.Role != "" || result.Status == "skipped" || result.Status == "not_run" {
			return "workflow step facts"
		}
	}
	return ""
}

// format10ImportRef reports whether a format 10 reader accepts name in an
// import record: HEAD, a branch or a tag.
func format10ImportRef(name string) bool {
	return name == state.ImportHeadRef ||
		len(name) > len("refs/heads/") && strings.HasPrefix(name, "refs/heads/") ||
		len(name) > len("refs/tags/") && strings.HasPrefix(name, "refs/tags/")
}

func addCheckState(manifest *Manifest, snapshot state.RecoveryState) {
	manifest.ActionsRuns = snapshot.ActionsRuns
	for _, task := range snapshot.Tasks {
		manifest.Tasks = append(manifest.Tasks, TaskManifest{
			ID: task.ID, RepositoryID: task.RepositoryID, Title: task.Title,
			CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		})
	}
	for _, configuration := range snapshot.CheckConfigurations {
		item := CheckConfigurationManifest{
			RepositoryID: configuration.RepositoryID, Version: configuration.Version,
			ConfigHash: configuration.ConfigHash, CreatedAt: configuration.CreatedAt,
		}
		for _, check := range configuration.Checks {
			item.Checks = append(item.Checks, CheckDefinitionManifest{Name: check.Name, Command: check.Command})
		}
		manifest.CheckConfigurations = append(manifest.CheckConfigurations, item)
	}
	for _, cycle := range snapshot.CheckCycles {
		manifest.CheckCycles = append(manifest.CheckCycles, CheckCycleManifest{
			ID: cycle.ID, TaskID: cycle.TaskID, RepositoryID: cycle.RepositoryID,
			Sequence: cycle.Sequence, ReservedAt: cycle.ReservedAt, ReservedAfterSequence: cycle.ReservedAfterSequence,
		})
	}
	for _, attempt := range snapshot.CheckAttempts {
		manifest.CheckAttempts = append(manifest.CheckAttempts, CheckAttemptManifest{
			ID: attempt.ID, TaskID: attempt.TaskID, RepositoryID: attempt.RepositoryID, RevisionOID: attempt.RevisionOID,
			WorktreeState: attempt.WorktreeState, SubmittedWorktree: attempt.SubmittedWorktreeState,
			ConfigurationVersion: attempt.ConfigurationVersion, Status: attempt.Status,
			ExitCode: attempt.ExitCode, StartedAt: attempt.StartedAt, FinishedAt: attempt.FinishedAt, DurationMS: attempt.DurationMS,
			Summary: attempt.Summary, Protection: attempt.Protection, ExecutionScope: attempt.ExecutionScope,
			CredentialID: attempt.CredentialID, JobID: attempt.JobID, TimeoutMS: attempt.TimeoutMS, OutputLimitBytes: attempt.OutputLimitBytes,
			LogID: attempt.LogID, LogExpiresAt: attempt.LogExpiresAt, LogTruncated: attempt.LogTruncated,
			LogError: attempt.LogError, CreatedAt: attempt.CreatedAt,
			Sequence: attempt.Sequence, CycleID: attempt.CycleID, RegistrationDigest: attempt.RegistrationDigest,
			CompletionDigest: attempt.CompletionDigest, SubmittedLogDigest: attempt.SubmittedLogDigest,
			SubmittedTruncated: attempt.SubmittedTruncated, SubmittedCancelled: attempt.SubmittedCancelled,
			LogDigest: attempt.LogDigest,
		})
	}
	for _, result := range snapshot.CheckResults {
		manifest.CheckResults = append(manifest.CheckResults, CheckResultManifest{
			AttemptID: result.AttemptID, Position: result.Position, Name: result.Name, Command: result.Command,
			Status: result.Status, ExitCode: result.ExitCode, DurationMS: result.DurationMS,
			OutputExcerpt: result.OutputExcerpt, Truncated: result.Truncated, CleanupError: result.CleanupError, Role: result.Role,
		})
	}
	for _, policy := range snapshot.CheckPolicies {
		manifest.CheckPolicies = append(manifest.CheckPolicies, CheckPolicyManifest{
			RepositoryID: policy.RepositoryID, PolicyVersion: policy.Version, PolicyDigest: policy.Digest, RunWorkflows: policy.RunWorkflows,
			Executor: policy.Executor, AllowedEvents: policy.AllowedEvents, MaxTimeoutMS: policy.MaxTimeoutMS,
			MaxOutputLimitBytes: policy.MaxOutputLimitBytes, QueueLimit: policy.QueueLimit, MaxActiveJobs: policy.MaxActiveJobs,
			MaxLeaseMS: policy.MaxLeaseMS, Execution: policy.Execution, ConsentVersion: policy.ConsentVersion, ConsentDigest: policy.ConsentDigest,
			RunnerGeneration: policy.RunnerGeneration, CreatedAt: policy.CreatedAt, UpdatedAt: policy.UpdatedAt,
		})
	}
	for _, job := range snapshot.CheckJobs {
		manifest.CheckJobs = append(manifest.CheckJobs, CheckJobManifest{
			ID: job.ID, RepositoryID: job.RepositoryID, TaskID: job.TaskID, Trigger: job.Trigger, EventKey: job.EventKey,
			SourceOID: job.SourceOID, BaseOID: job.BaseOID, PullRequestNumber: job.PullRequestNumber, TriggerRef: job.TriggerRef,
			WorkflowPath: job.WorkflowPath, WorkflowOID: job.WorkflowOID, WorkflowDigest: job.WorkflowDigest,
			ConfigurationVersion: job.ConfigurationVersion, Executor: job.Executor, PolicyVersion: job.PolicyVersion,
			ConsentVersion: job.ConsentVersion,
			Limits:         CheckJobLimitsManifest{TimeoutMS: job.Limits.TimeoutMS, OutputLimitBytes: job.Limits.OutputLimitBytes},
			Execution:      job.Execution,
			DedupDigest:    job.DedupDigest, RerunRoot: job.RerunRoot, RerunGeneration: job.RerunGeneration, Status: job.Status,
			AttemptID: job.AttemptID, LeaseID: job.LeaseID, LeaseExpiresAt: job.LeaseExpiresAt, CredentialID: job.CredentialID,
			CredentialGeneration: job.CredentialGeneration, CredentialRole: job.CredentialRole, Protection: job.Protection, AdmittedAt: job.AdmittedAt,
			ClaimedAt: job.ClaimedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt, LeaseLostAt: job.LeaseLostAt,
			CancelRequestedAt: job.CancelRequestedAt, InterruptedAt: job.InterruptedAt, Summary: job.Summary,
			RunID: job.RunID, JobKey: job.JobKey, MatrixIndex: job.MatrixIndex, PlanDigest: job.PlanDigest,
			Tolerated: job.Tolerated, ConcurrencyGroup: job.ConcurrencyGroup, MaxParallel: job.MaxParallel,
		})
	}
}

func recoveryState(manifest Manifest) state.RecoveryState {
	snapshot := state.RecoveryState{
		AccessMode: manifest.AccessMode, AccessPasswordHash: manifest.AccessHash, AdminPasswordHash: manifest.AdminHash,
		ActionsRuns: manifest.ActionsRuns,
	}
	for _, item := range manifest.Repositories {
		snapshot.Repositories = append(snapshot.Repositories, state.Repository{
			ID: item.ID, Name: item.Name, Description: item.Description, CreatedAt: item.CreatedAt,
			AttemptSequence: item.AttemptSequence,
		})
		for _, name := range item.Names {
			snapshot.RepositoryNames = append(snapshot.RepositoryNames, state.RepositoryName{
				Name: name.Name, RepositoryID: item.ID, Kind: name.Kind, CreatedAt: name.CreatedAt, AliasUntil: name.AliasUntil,
			})
		}
		if item.Policy != nil {
			snapshot.RepositoryPolicies = append(snapshot.RepositoryPolicies, state.RepositoryPolicy{
				RepositoryID: item.ID, RetainHistory: item.Policy.RetainHistory, ProtectDefaultBranch: item.Policy.ProtectDefaultBranch,
				ExtraRefPrefixes: item.Policy.ExtraRefPrefixes, UpdatedAt: item.Policy.UpdatedAt,
			})
		}
	}
	attachImportState(&snapshot, manifest)
	for _, record := range manifest.PullRequests {
		snapshot.PullRequests = append(snapshot.PullRequests, state.PullRequest{
			RepositoryID: record.RepositoryID, Number: record.Number, Title: record.Title,
			SourceBranch: record.SourceBranch, TargetBranch: record.TargetBranch, Status: record.Status,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
			MergeSourceOID: record.MergeSourceOID, MergeTargetOID: record.MergeTargetOID,
			MergeOID: record.MergeOID, MergeReceipt: record.MergeReceipt, MergedAt: record.MergedAt,
			Body: record.Body, EditRevision: record.EditRevision, EditedAt: record.EditedAt,
			CreatedBy: record.CreatedBy, EditedBy: record.EditedBy, MergedBy: record.MergedBy,
		})
	}
	for _, revision := range manifest.PullRequestRevisions {
		snapshot.PullRequestRevisions = append(snapshot.PullRequestRevisions, state.PullRequestRevision{
			RepositoryID: revision.RepositoryID, PullRequestNumber: revision.PullRequestNumber,
			SourceOID: revision.SourceOID, TargetOID: revision.TargetOID, RecordedAt: revision.RecordedAt,
		})
	}
	for _, review := range manifest.PullRequestReviews {
		snapshot.PullRequestReviews = append(snapshot.PullRequestReviews, state.PullRequestReview{
			RepositoryID: review.RepositoryID, PullRequestNumber: review.PullRequestNumber, Sequence: review.Sequence,
			SourceOID: review.SourceOID, TargetOID: review.TargetOID, Status: review.Status,
			ReviewerLabel: review.ReviewerLabel, Provenance: review.Provenance, ReviewEventID: review.ReviewEventID, CreatedAt: review.CreatedAt,
			Note: review.Note, Actor: review.Actor,
		})
	}
	for _, intent := range manifest.PullRequestMergeIntents {
		snapshot.PullRequestMergeIntents = append(snapshot.PullRequestMergeIntents, state.PullRequestMergeIntent{
			RepositoryID: intent.RepositoryID, PullRequestNumber: intent.PullRequestNumber,
			SourceOID: intent.SourceOID, TargetOID: intent.TargetOID, Mode: intent.Mode,
			TreeOID: intent.TreeOID, ResultOID: intent.ResultOID, ReceiptRef: intent.ReceiptRef,
			Status: intent.Status, CreatedAt: intent.CreatedAt, UpdatedAt: intent.UpdatedAt,
		})
	}
	for _, intent := range manifest.PullRequestAutoMerges {
		snapshot.PullRequestAutoMerges = append(snapshot.PullRequestAutoMerges, state.PullRequestAutoMerge{
			RepositoryID: intent.RepositoryID, PullRequestNumber: intent.PullRequestNumber,
			EnabledAt: intent.EnabledAt, UpdatedAt: intent.UpdatedAt, EnabledBy: intent.EnabledBy,
		})
	}
	for _, task := range manifest.Tasks {
		snapshot.Tasks = append(snapshot.Tasks, state.RecoveryTask{
			ID: task.ID, RepositoryID: task.RepositoryID, Title: task.Title,
			CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		})
	}
	for _, configuration := range manifest.CheckConfigurations {
		item := state.CheckConfiguration{
			RepositoryID: configuration.RepositoryID, Version: configuration.Version,
			ConfigHash: configuration.ConfigHash, CreatedAt: configuration.CreatedAt,
		}
		for _, check := range configuration.Checks {
			item.Checks = append(item.Checks, state.CheckDefinition{Name: check.Name, Command: check.Command})
		}
		snapshot.CheckConfigurations = append(snapshot.CheckConfigurations, item)
	}
	for _, cycle := range manifest.CheckCycles {
		snapshot.CheckCycles = append(snapshot.CheckCycles, state.RecoveryCheckCycle{
			ID: cycle.ID, TaskID: cycle.TaskID, RepositoryID: cycle.RepositoryID,
			Sequence: cycle.Sequence, ReservedAt: cycle.ReservedAt, ReservedAfterSequence: cycle.ReservedAfterSequence,
		})
	}
	for _, attempt := range manifest.CheckAttempts {
		protection, scope := attempt.Protection, attempt.ExecutionScope
		if protection == "" {
			protection = state.ProtectionUnknown
		}
		if scope == "" {
			scope = state.ExecutionScopeInherited
		}
		snapshot.CheckAttempts = append(snapshot.CheckAttempts, state.CheckAttempt{
			ID: attempt.ID, TaskID: attempt.TaskID, RepositoryID: attempt.RepositoryID, RevisionOID: attempt.RevisionOID,
			WorktreeState: attempt.WorktreeState, SubmittedWorktreeState: attempt.SubmittedWorktree,
			ConfigurationVersion: attempt.ConfigurationVersion, Status: attempt.Status,
			ExitCode: attempt.ExitCode, StartedAt: attempt.StartedAt, FinishedAt: attempt.FinishedAt, DurationMS: attempt.DurationMS,
			Summary: attempt.Summary, Protection: protection, ExecutionScope: scope,
			CredentialID: attempt.CredentialID, JobID: attempt.JobID, TimeoutMS: attempt.TimeoutMS, OutputLimitBytes: attempt.OutputLimitBytes,
			LogID: attempt.LogID, LogExpiresAt: attempt.LogExpiresAt, LogTruncated: attempt.LogTruncated,
			LogError: attempt.LogError, CreatedAt: attempt.CreatedAt,
			Sequence: attempt.Sequence, CycleID: attempt.CycleID, RegistrationDigest: attempt.RegistrationDigest,
			CompletionDigest: attempt.CompletionDigest, SubmittedLogDigest: attempt.SubmittedLogDigest,
			SubmittedTruncated: attempt.SubmittedTruncated, SubmittedCancelled: attempt.SubmittedCancelled,
			LogDigest: attempt.LogDigest,
		})
	}
	for _, result := range manifest.CheckResults {
		snapshot.CheckResults = append(snapshot.CheckResults, state.CheckResultRecord{
			AttemptID: result.AttemptID,
			CheckResult: state.CheckResult{
				Position: result.Position, Name: result.Name, Command: result.Command, Status: result.Status,
				ExitCode: result.ExitCode, DurationMS: result.DurationMS, OutputExcerpt: result.OutputExcerpt,
				Truncated: result.Truncated, CleanupError: result.CleanupError, Role: result.Role,
			},
		})
	}
	for _, policy := range manifest.CheckPolicies {
		snapshot.CheckPolicies = append(snapshot.CheckPolicies, state.CheckPolicy{
			RepositoryID: policy.RepositoryID, Version: policy.PolicyVersion, Digest: policy.PolicyDigest, RunWorkflows: policy.RunWorkflows,
			Executor: policy.Executor, AllowedEvents: policy.AllowedEvents, MaxTimeoutMS: policy.MaxTimeoutMS,
			MaxOutputLimitBytes: policy.MaxOutputLimitBytes, QueueLimit: policy.QueueLimit, MaxActiveJobs: policy.MaxActiveJobs,
			MaxLeaseMS: policy.MaxLeaseMS, Execution: policy.Execution, ConsentVersion: policy.ConsentVersion, ConsentDigest: policy.ConsentDigest,
			RunnerGeneration: policy.RunnerGeneration, CreatedAt: policy.CreatedAt, UpdatedAt: policy.UpdatedAt,
		})
	}
	for _, job := range manifest.CheckJobs {
		snapshot.CheckJobs = append(snapshot.CheckJobs, state.CheckJob{
			ID: job.ID, RepositoryID: job.RepositoryID, TaskID: job.TaskID, Trigger: job.Trigger, EventKey: job.EventKey,
			SourceOID: job.SourceOID, BaseOID: job.BaseOID, PullRequestNumber: job.PullRequestNumber, TriggerRef: job.TriggerRef,
			WorkflowPath: job.WorkflowPath, WorkflowOID: job.WorkflowOID, WorkflowDigest: job.WorkflowDigest,
			ConfigurationVersion: job.ConfigurationVersion, Executor: job.Executor, PolicyVersion: job.PolicyVersion,
			ConsentVersion: job.ConsentVersion,
			Limits:         state.CheckJobLimits{TimeoutMS: job.Limits.TimeoutMS, OutputLimitBytes: job.Limits.OutputLimitBytes},
			Execution:      job.Execution,
			DedupDigest:    job.DedupDigest, RerunRoot: job.RerunRoot, RerunGeneration: job.RerunGeneration, Status: job.Status,
			AttemptID: job.AttemptID, LeaseID: job.LeaseID, LeaseExpiresAt: job.LeaseExpiresAt, CredentialID: job.CredentialID,
			CredentialGeneration: job.CredentialGeneration, CredentialRole: job.CredentialRole, Protection: job.Protection, AdmittedAt: job.AdmittedAt,
			ClaimedAt: job.ClaimedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt, LeaseLostAt: job.LeaseLostAt,
			CancelRequestedAt: job.CancelRequestedAt, InterruptedAt: job.InterruptedAt, Summary: job.Summary,
			RunID: job.RunID, JobKey: job.JobKey, MatrixIndex: job.MatrixIndex, PlanDigest: job.PlanDigest,
			Tolerated: job.Tolerated, ConcurrencyGroup: job.ConcurrencyGroup, MaxParallel: job.MaxParallel,
		})
	}
	return snapshot
}

// validateBackupVersion accepts the committed baseline formats, the released
// formats and the current format. Every other version below the current one
// was written only by unreleased development builds.
func validateBackupVersion(version int) error {
	switch {
	case version == legacyBackupVersion || version == pullRequestBackupVersion || version == checkBackupVersion ||
		version == closedPullRequestBackupVersion || version == recordsBackupVersion ||
		version == workflowBackupVersion || version == autoMergeBackupVersion:
		return nil
	case version > backupVersion:
		return fmt.Errorf("unsupported backup version %d: this build supports versions 1, 2, %d, %d, %d, %d, and %d", version, checkBackupVersion, closedPullRequestBackupVersion, recordsBackupVersion, workflowBackupVersion, backupVersion)
	default:
		return fmt.Errorf("backup uses the unreleased development format %d; this build supports versions 1, 2, %d, %d, %d, %d, and %d", version, checkBackupVersion, closedPullRequestBackupVersion, recordsBackupVersion, workflowBackupVersion, backupVersion)
	}
}

func validateManifest(manifest Manifest) error {
	if manifest.Format != backupFormat {
		return errors.New("unsupported backup format")
	}
	if err := validateBackupVersion(manifest.Version); err != nil {
		return err
	}
	if manifest.CreatedAt.IsZero() || auth.ValidatePasswordHash(manifest.AdminHash) != nil {
		return errors.New("backup manifest metadata is incomplete")
	}
	if manifest.AccessMode != "open" && manifest.AccessMode != "password" {
		return errors.New("backup access mode is invalid")
	}
	if manifest.AccessMode == "password" && auth.ValidatePasswordHash(manifest.AccessHash) != nil {
		return errors.New("backup access password hash is invalid")
	}
	if manifest.AccessMode == "open" && manifest.AccessHash != "" {
		return errors.New("open backup unexpectedly contains an access password hash")
	}
	ids := make(map[string]bool)
	bundles := make(map[string]bool)
	repositoryRefs := make(map[string]map[string]string)
	for _, item := range manifest.Repositories {
		if repository.ValidateID(item.ID) != nil || item.Name == "" || len(item.Name) > 100 || len(item.Description) > state.MaximumRepositoryDescriptionBytes || ids[item.ID] {
			return errors.New("backup repository metadata is invalid or not portable")
		}
		ids[item.ID] = true
		if item.CreatedAt.IsZero() {
			return errors.New("backup repository creation time is invalid")
		}
		for _, name := range item.Names {
			if repository.ValidateID(name.Name) != nil || repository.ValidateName(name.Name, "") != nil {
				return fmt.Errorf("backup repository name %q is not a valid repository address", name.Name)
			}
		}
		if item.Head.Symbolic != "" && item.Head.OID != "" {
			return errors.New("backup HEAD has conflicting forms")
		}
		if item.Head.Symbolic != "" && (!strings.HasPrefix(item.Head.Symbolic, "refs/heads/") || !validRefName(item.Head.Symbolic)) {
			return errors.New("backup symbolic HEAD is invalid")
		}
		if item.Head.OID != "" && !validOID(item.Head.OID) {
			return errors.New("backup detached HEAD is invalid")
		}
		refNames := make(map[string]bool)
		repositoryRefs[item.ID] = make(map[string]string)
		for _, ref := range item.Refs {
			if !validRefName(ref.Name) || !validOID(ref.OID) || refNames[ref.Name] {
				return errors.New("backup ref list is invalid")
			}
			refNames[ref.Name] = true
			repositoryRefs[item.ID][ref.Name] = ref.OID
		}
		if item.Empty {
			if len(item.Refs) != 0 || item.Head.OID != "" || item.Bundle != "" || item.SHA256 != "" {
				return errors.New("empty repository backup contains bundle data")
			}
			if item.ObjectFormat != "" && item.ObjectFormat != repository.ObjectFormatSHA256 {
				return errors.New("backup repository object format is invalid")
			}
			continue
		}
		if item.ObjectFormat != "" {
			return errors.New("backup records an object format for a repository whose bundle names it")
		}
		expectedBundle := path.Join("repositories", item.ID+".bundle")
		if (len(item.Refs) == 0 && item.Head.OID == "") || item.Bundle != expectedBundle || !validSHA256(item.SHA256) || bundles[item.Bundle] {
			return errors.New("repository bundle metadata is invalid")
		}
		bundles[item.Bundle] = true
	}
	if manifest.Version == legacyBackupVersion {
		if len(manifest.PullRequests) != 0 || len(manifest.PullRequestRevisions) != 0 || len(manifest.PullRequestReviews) != 0 || len(manifest.PullRequestMergeIntents) != 0 || len(manifest.PullRequestAutoMerges) != 0 {
			return errors.New("version 1 backup contains unsupported pull request metadata")
		}
	}
	if manifest.Version < autoMergeBackupVersion {
		if content := format13Content(manifest); content != "" {
			return fmt.Errorf("version %d backup contains %s, which only version %d holds", manifest.Version, content, autoMergeBackupVersion)
		}
	}
	if manifest.Version < workflowBackupVersion {
		if content := format12Content(manifest); content != "" {
			return fmt.Errorf("version %d backup contains %s, which only version %d holds", manifest.Version, content, workflowBackupVersion)
		}
	}
	if manifest.Version < recordsBackupVersion {
		if content := format11Content(manifest); content != "" {
			return fmt.Errorf("version %d backup contains %s, which only version %d holds", manifest.Version, content, recordsBackupVersion)
		}
	}
	if manifest.Version < closedPullRequestBackupVersion {
		for _, record := range manifest.PullRequests {
			if record.Status == state.PullRequestClosed {
				return fmt.Errorf("version %d backup contains an unsupported closed pull request", manifest.Version)
			}
		}
		for _, intent := range manifest.PullRequestMergeIntents {
			if intent.Mode == "up_to_date" {
				return fmt.Errorf("version %d backup contains an unsupported up-to-date merge", manifest.Version)
			}
		}
	}
	if manifest.Version < checkBackupVersion {
		if len(manifest.Tasks) != 0 || len(manifest.CheckConfigurations) != 0 || len(manifest.CheckCycles) != 0 || len(manifest.CheckAttempts) != 0 || len(manifest.CheckResults) != 0 || len(manifest.CheckPolicies) != 0 || len(manifest.CheckJobs) != 0 {
			return fmt.Errorf("version %d backup contains unsupported check metadata", manifest.Version)
		}
		for _, review := range manifest.PullRequestReviews {
			if review.ReviewEventID != "" {
				return fmt.Errorf("version %d backup contains an unsupported review event identity", manifest.Version)
			}
		}
		if len(manifest.ImportSources) != 0 || len(manifest.ImportRuns) != 0 || manifest.ImportRunOrderKnown || manifest.ImportHEADOwnershipVersion != 0 || len(manifest.ImportObservations) != 0 || len(manifest.ImportIntents) != 0 {
			return fmt.Errorf("version %d backup contains unsupported import metadata", manifest.Version)
		}
	}
	snapshot := recoveryState(manifest)
	if err := state.ValidateRepositoryRecords(snapshot); err != nil {
		return fmt.Errorf("backup repository metadata is invalid: %w", err)
	}
	if err := state.ValidatePullRequestRecovery(snapshot); err != nil {
		return fmt.Errorf("backup pull request metadata is invalid: %w", err)
	}
	for _, revision := range snapshot.PullRequestRevisions {
		sourceRef, targetRef := pullrequest.RevisionRefNames(revision.PullRequestNumber, revision.SourceOID, revision.TargetOID)
		refs := repositoryRefs[revision.RepositoryID]
		if refs[sourceRef] != revision.SourceOID || refs[targetRef] != revision.TargetOID {
			return errors.New("backup is missing a protected pull request revision ref")
		}
	}
	type requestKey struct {
		repositoryID string
		number       int64
	}
	mergeIntents := make(map[requestKey][]state.PullRequestMergeIntent)
	for _, intent := range snapshot.PullRequestMergeIntents {
		key := requestKey{intent.RepositoryID, intent.PullRequestNumber}
		mergeIntents[key] = append(mergeIntents[key], intent)
		refs := repositoryRefs[intent.RepositoryID]
		if intent.Mode == "merge_commit" && intent.Status != state.MergeIntentPreparing {
			treeRef := pullrequest.MergeTreeRef(intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)
			if refs[treeRef] != intent.TreeOID {
				return errors.New("backup is missing its protected merge tree ref")
			}
		}
		if intent.Status == state.MergeIntentReady || intent.Status == state.MergeIntentComplete {
			resultRef := pullrequest.MergeResultRef(intent.PullRequestNumber, intent.SourceOID, intent.TargetOID)
			if refs[resultRef] != intent.ResultOID {
				return errors.New("backup is missing its protected merge result ref")
			}
		}
	}
	knownReceipts := make(map[string]map[string]bool)
	for _, record := range snapshot.PullRequests {
		receiptRef := pullrequest.MergeReceiptRef(record.Number)
		receiptOID, exists := repositoryRefs[record.RepositoryID][receiptRef]
		if !exists {
			if record.Status == state.PullRequestMerged {
				return errors.New("completed backup merge is missing its protected receipt")
			}
			continue
		}
		if _, err := state.PullRequestMergeReceiptOwner(record, mergeIntents[requestKey{record.RepositoryID, record.Number}], receiptOID); err != nil {
			return fmt.Errorf("backup %w", err)
		}
		if knownReceipts[record.RepositoryID] == nil {
			knownReceipts[record.RepositoryID] = make(map[string]bool)
		}
		knownReceipts[record.RepositoryID][receiptRef] = true
	}
	for repositoryID, refs := range repositoryRefs {
		for ref := range refs {
			if strings.HasPrefix(ref, "refs/owngit/pull-requests/") && strings.HasSuffix(ref, "/merge-receipt") && !knownReceipts[repositoryID][ref] {
				return errors.New("backup merge receipt has no durable owner")
			}
		}
	}
	if err := state.ValidateCheckRecovery(snapshot); err != nil {
		return fmt.Errorf("backup check metadata is invalid: %w", err)
	}
	if err := validateImportManifest(manifest, snapshot); err != nil {
		return err
	}
	return nil
}

func checkBundleRefs(ctx context.Context, runner commandRunner, bundlePath string, item RepositoryManifest) error {
	heads, err := runner.Run(ctx, "", nil, "bundle", "list-heads", bundlePath)
	if err != nil {
		return err
	}
	listed, bundledHead, err := parseBundleHeads(heads.Stdout)
	if err != nil {
		return err
	}
	if !sameRefs(listed, item.Refs) {
		present := make(map[Ref]bool, len(listed))
		for _, ref := range listed {
			present[ref] = true
		}
		for _, ref := range item.Refs {
			if !present[ref] {
				return fmt.Errorf("bundle refs do not match the manifest: ref %q", ref.Name)
			}
		}
		return errors.New("bundle refs do not match the manifest")
	}
	if item.Head.OID != "" && bundledHead != item.Head.OID {
		return errors.New("bundle refs do not match the manifest: HEAD")
	}
	return nil
}

func parseBundleHeads(output []byte) ([]Ref, string, error) {
	var refs []Ref
	var head string
	if len(output) == 0 {
		return refs, head, nil
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(output), "\n"), "\n") {
		oid, name, found := strings.Cut(line, " ")
		if !found || !validOID(oid) || name == "" {
			return nil, "", errors.New("Git returned malformed bundle heads")
		}
		if name == "HEAD" {
			head = oid
			continue
		}
		refs = append(refs, Ref{Name: name, OID: oid})
	}
	return refs, head, nil
}

func sameRefs(left, right []Ref) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]Ref(nil), left...)
	rightCopy := append([]Ref(nil), right...)
	sort.Slice(leftCopy, func(i, j int) bool { return leftCopy[i].Name < leftCopy[j].Name })
	sort.Slice(rightCopy, func(i, j int) bool { return rightCopy[i].Name < rightCopy[j].Name })
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func fileSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validRefName(name string) bool {
	return strings.HasPrefix(name, "refs/") && importgit.CheckRefFormat(name) == nil
}

func validOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, character := range oid {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func randomSuffix() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	leftKey := pathComparisonKey(left)
	rightKey := pathComparisonKey(right)
	if pathContains(leftKey, rightKey) || pathContains(rightKey, leftKey) {
		return true
	}
	if existingPathContains(left, right) || existingPathContains(right, left) {
		return true
	}
	leftAncestor, leftSuffix, leftOK := nearestExistingPath(left)
	rightAncestor, rightSuffix, rightOK := nearestExistingPath(right)
	return leftOK && rightOK && os.SameFile(leftAncestor, rightAncestor) && componentPathsOverlap(leftSuffix, rightSuffix)
}

func pathComparisonKey(value string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(value)
	}
	return value
}

func pathContains(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func existingPathContains(parent, candidate string) bool {
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return false
	}
	current := candidate
	for {
		if info, statErr := os.Stat(current); statErr == nil && os.SameFile(parentInfo, info) {
			return true
		}
		next := filepath.Dir(current)
		if next == current {
			return false
		}
		current = next
	}
}

func nearestExistingPath(value string) (os.FileInfo, []string, bool) {
	current := value
	var suffix []string
	for {
		info, err := os.Stat(current)
		if err == nil {
			return info, suffix, true
		}
		if !os.IsNotExist(err) {
			return nil, nil, false
		}
		next := filepath.Dir(current)
		if next == current {
			return nil, nil, false
		}
		suffix = append([]string{pathComparisonKey(filepath.Base(current))}, suffix...)
		current = next
	}
}

func componentPathsOverlap(left, right []string) bool {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
