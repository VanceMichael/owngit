package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"owngit/internal/checkworkflow"
)

// Operator-selected configured-check execution modes.
const (
	CheckExecutorHost           = "host"
	CheckExecutorContainer      = "container"
	CheckExecutorExternalRunner = "external_runner"
)

// RunnerFeatureWorkflowsV1 identifies runners that execute Actions job plans.
const RunnerFeatureWorkflowsV1 = "workflows-v1"

const runnerOldNote = "note.runner_old: A runner without workflows-v1 cannot run this job. Update the runner."

// Job states. Pending, waiting, claimed, and started are unfinished. The remaining
// states are terminal and are never silently requeued: ambiguous and
// interrupted work needs an explicit rerun.
const (
	CheckJobPending     = "pending"
	CheckJobWaiting     = "waiting"
	CheckJobSkipped     = "skipped"
	CheckJobClaimed     = "claimed"
	CheckJobStarted     = "started"
	CheckJobPassed      = "passed"
	CheckJobFailed      = "failed"
	CheckJobError       = "error"
	CheckJobCancelled   = "cancelled"
	CheckJobIncomplete  = "incomplete"
	CheckJobUnavailable = "unavailable"
	CheckJobAmbiguous   = "ambiguous"
	CheckJobInterrupted = "interrupted"
)

const (
	// MinimumCheckLeaseMS and MaximumCheckLeaseMS bound one claim lease. The
	// policy value is the lease duration and its own cap. A running job
	// renews its lease, so the lease does not bound how long checks run.
	MinimumCheckLeaseMS int64 = 1000
	MaximumCheckLeaseMS int64 = 24 * 60 * 60 * 1000
	// MaximumCheckQueueLimit bounds unfinished jobs per repository.
	MaximumCheckQueueLimit = 10000
	// MaximumCheckActiveJobs bounds simultaneously claimed or started jobs.
	MaximumCheckActiveJobs = 1000
	// MaximumCheckEventKeyBytes and MaximumCheckTriggerRefBytes bound the
	// observed trigger context.
	MaximumCheckEventKeyBytes   = 200
	MaximumCheckTriggerRefBytes = 200
	// MaximumCheckObservations retains branch decisions, including heads with
	// no job, up to the maximum queue size. Observing more heads than one scan
	// handles must not evict unchanged heads on each pass.
	MaximumCheckObservations = MaximumCheckQueueLimit
)

// Sentinel errors are mapped to stable runner and owner API responses.
var (
	// ErrCheckPolicyMissing reports that the repository has no operator policy.
	ErrCheckPolicyMissing = errors.New("check policy does not exist")
	// ErrCheckPolicyStale reports an approval quoting a policy generation that
	// is no longer the stored one. Consent names the exact generation it was
	// granted for, so an approval for a policy nobody read is refused.
	ErrCheckPolicyStale = errors.New("the check policy changed since it was read")
	// ErrCheckConsentRequired reports that admission needs current consent.
	ErrCheckConsentRequired = errors.New("check execution consent is required")
	// ErrCheckEventNotAllowed reports an event excluded by operator policy.
	ErrCheckEventNotAllowed = errors.New("the check event is not allowed by policy")
	// ErrCheckQueueFull reports a transactional admission bound.
	ErrCheckQueueFull = errors.New("the check queue is full")
	// ErrInvalidCheckPolicy reports malformed operator policy input.
	ErrInvalidCheckPolicy = errors.New("invalid check policy")
	// ErrInvalidCheckJob reports malformed job input.
	ErrInvalidCheckJob = errors.New("invalid check job")
	// ErrCheckJobNotFound reports a job that does not exist in the repository.
	ErrCheckJobNotFound = errors.New("check job does not exist")
	// ErrCheckJobState reports a transition from the wrong state.
	ErrCheckJobState = errors.New("the check job is not in a state that allows this transition")
	// ErrCheckJobStartReplay reports that start is a one-shot execution grant.
	ErrCheckJobStartReplay = errors.New("the check job start was already recorded")
	// ErrCheckJobLease reports a stale or foreign lease.
	ErrCheckJobLease = errors.New("the check job lease does not match")
	// ErrCheckJobCredential reports a missing or mismatched runner credential.
	ErrCheckJobCredential = errors.New("the check job runner credential does not match")
	// ErrCheckJobAttemptBound reports an attempt bound to another job.
	ErrCheckJobAttemptBound = errors.New("the check job is already bound to another attempt")
	// ErrCheckJobFacts reports evidence that differs from the admitted job.
	ErrCheckJobFacts = errors.New("the check attempt does not match its automatic job")
	// ErrCheckJobCompletionRequired keeps ordinary helper completion separate.
	ErrCheckJobCompletionRequired = errors.New("automatic check completion requires job authority")
	// ErrCheckRunnerCredential reports unknown, revoked, or foreign authority.
	ErrCheckRunnerCredential = errors.New("the runner credential is unknown or revoked")
	// ErrRunnerCredentialOtherRepository reports a live runner token that was
	// presented for a repository other than the one it was issued for.
	ErrRunnerCredentialOtherRepository = errors.New("the runner credential belongs to another repository")
	// ErrCheckRunnerRevoked reports a revoke of a missing or retired credential.
	ErrCheckRunnerRevoked = errors.New("the runner credential was not found or was already revoked")
	// ErrCheckRunnerCreationConflict reports a reused creation identity with a
	// different label.
	ErrCheckRunnerCreationConflict = errors.New("runner credential creation identity was reused with different content")
	// ErrInvalidCheckObservation reports malformed observation input.
	ErrInvalidCheckObservation = errors.New("invalid check observation")
	// ErrCheckJobCommandsUnavailable reports that a job's captured commands
	// could not be read. The wrapped cause is ErrCheckConfigurationMissing when
	// the record does not exist, and the read error otherwise.
	ErrCheckJobCommandsUnavailable = errors.New("the job's captured commands could not be read")
	// ErrCheckConfigurationMissing reports a captured check configuration that
	// a job or an attempt names but that does not exist. Only repository
	// deletion removes one, together with the jobs and attempts that name it,
	// so this is a damaged record rather than a job without commands.
	ErrCheckConfigurationMissing = errors.New("the captured check configuration is missing")
)

// CheckPolicy is the durable operator policy. It is portable except for the
// local consent flag and authority epoch.
type CheckPolicy struct {
	RepositoryID string
	// Version is the monotonic policy generation. It increments only on a real
	// policy change, so a job can capture the exact generation it used.
	Version  int64
	Digest   string
	Executor string
	// AllowedEvents is the canonical sorted set of check and workflow events.
	AllowedEvents       []string
	RunWorkflows        bool
	MaxTimeoutMS        int64
	MaxOutputLimitBytes int64
	QueueLimit          int
	MaxActiveJobs       int
	MaxLeaseMS          int64
	Execution           CheckExecutionSettings
	ConsentVersion      int64
	ConsentDigest       string
	// ConsentActive is machine-local. A policy change clears it, and a restore
	// starts without it, so a stale confirmation cannot launch work.
	ConsentActive bool
	// RunnerGeneration is monotonic per repository and portable, so restored
	// runner authority never reuses a generation.
	RunnerGeneration int64
	// AuthorityEpoch is local, nonsecret authority replaced on restore.
	AuthorityEpoch string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CheckPolicyInput is the operator-supplied policy. Version, digest, consent,
// generation, and epoch are server-owned.
type CheckPolicyInput struct {
	// Nil enables workflows on a first save and preserves an existing setting.
	RunWorkflows        *bool
	RepositoryID        string
	Executor            string
	AllowedEvents       []string
	MaxTimeoutMS        int64
	MaxOutputLimitBytes int64
	QueueLimit          int
	MaxActiveJobs       int
	MaxLeaseMS          int64
	Execution           CheckExecutionSettings
}

// CheckJobLimits are the effective execution limits captured by a job.
type CheckJobLimits struct {
	TimeoutMS        int64 `json:"timeout_ms"`
	OutputLimitBytes int64 `json:"output_limit_bytes"`
}

// CheckJob is one durable automatic job. Status and attempt linkage establish
// causal order; timestamps preserve observed wall time across clock corrections.
// Lease deadlines govern live execution and are never adjusted to order history.
type CheckJob struct {
	ID                   string
	RepositoryID         string
	TaskID               string
	Trigger              string
	EventKey             string
	SourceOID            string
	BaseOID              string
	PullRequestNumber    int64
	TriggerRef           string
	WorkflowPath         string
	WorkflowOID          string
	WorkflowDigest       string
	ConfigurationVersion int64
	Executor             string
	PolicyVersion        int64
	ConsentVersion       int64
	Limits               CheckJobLimits
	Execution            CheckExecutionSettings
	DedupDigest          string
	RerunRoot            string
	RerunGeneration      int64
	Status               string
	AttemptID            string
	LeaseID              string
	LeaseExpiresAt       *time.Time
	CredentialID         string
	CredentialGeneration int64
	CredentialRole       string
	Protection           string
	AdmittedAt           time.Time
	ClaimedAt            *time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	LeaseLostAt          *time.Time
	CancelRequestedAt    *time.Time
	InterruptedAt        *time.Time
	Summary              string
	// RunID is empty for JSON jobs. These facts survive Actions plan deletion.
	RunID            string `json:"run_id,omitempty"`
	JobKey           string `json:"job_key,omitempty"`
	MatrixIndex      int    `json:"matrix_index,omitempty"`
	PlanDigest       string `json:"plan_digest,omitempty"`
	Tolerated        bool   `json:"tolerated,omitempty"`
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`
	MaxParallel      int    `json:"max_parallel,omitempty"`
}

// CheckJobRequest is one admission request. Policy facts and the executor are
// read from the durable policy inside the admission transaction.
type CheckJobRequest struct {
	RepositoryID      string
	Trigger           string
	EventKey          string
	SourceOID         string
	BaseOID           string
	PullRequestNumber int64
	TriggerRef        string
	WorkflowPath      string
	WorkflowOID       string
	WorkflowDigest    string
	Checks            []CheckDefinition
	// Zero limits fall back to the bounded defaults and are then capped by the
	// operator policy.
	TimeoutMS        int64
	OutputLimitBytes int64
	RunID            string
	JobKey           string
	MatrixIndex      int
	PlanDigest       string
	Tolerated        bool
	ConcurrencyGroup string
	MaxParallel      int
	Waiting          bool
	RerunRoot        string
	RerunGeneration  int64
}

// CheckJobStart is a runner-reported start transition.
type CheckJobStart struct {
	RepositoryID         string
	JobID                string
	LeaseID              string
	CredentialID         string
	CredentialGeneration int64
	// Protection is optional. Empty means unknown.
	Protection string
	// AttemptID names the attempt that the start registers for the job.
	AttemptID string
}

// CheckJobCompletionAuthority binds a completion to the claim that started
// the automatic job. Ordinary helper completion cannot supply this authority.
type CheckJobCompletionAuthority struct {
	JobID                string
	LeaseID              string
	CredentialID         string
	CredentialGeneration int64
}

// RunnerCredential is revocable, repository-scoped runner authority. The raw
// token is never stored.
type RunnerCredential struct {
	ID           string
	RepositoryID string
	Label        string
	CreationID   string
	Generation   int64
	Role         string
	CreatedAt    time.Time
	RevokedAt    *time.Time
	LastUsedAt   *time.Time
}

// CheckObservation is the latest observed object for one refs/ ref.
type CheckObservation struct {
	RepositoryID string
	RefName      string
	OID          string
	ObservedAt   time.Time
}

// SetCheckPolicy stores the operator policy. A retransmit with the same facts
// is idempotent; a real change increments the generation and clears local
// consent, so an old confirmation cannot silently authorize new limits.
func (s *Store) SetCheckPolicy(ctx context.Context, input CheckPolicyInput, now time.Time) (CheckPolicy, error) {
	return s.writeCheckPolicy(ctx, input, nil, false, now)
}

// SaveCheckPolicyAndGrantConsent stores the policy and turns checks on for
// exactly that policy, in one transaction. Consent names the digest of the
// submitted facts, so it can never land on a policy someone else stored.
//
// base, when given, is the stored policy the caller showed the change
// against; Version 0 with an empty digest means no policy existed. Any other
// stored policy refuses with ErrCheckPolicyStale and nothing is written, so
// the change the owner reviewed is the change that is made.
func (s *Store) SaveCheckPolicyAndGrantConsent(ctx context.Context, input CheckPolicyInput, base *ExpectedCheckPolicy, now time.Time) (CheckPolicy, error) {
	return s.writeCheckPolicy(ctx, input, base, true, now)
}

// CandidateCheckPolicy validates input against the stored policy without writing.
// Saving the same input stores its digest while the stored policy is unchanged.
func (s *Store) CandidateCheckPolicy(ctx context.Context, input CheckPolicyInput) (CheckPolicy, error) {
	existing, exists, err := readCheckPolicyTx(ctx, s.db, input.RepositoryID)
	if err != nil {
		return CheckPolicy{}, err
	}
	return candidateCheckPolicy(input, existing, exists)
}

func candidateCheckPolicy(input CheckPolicyInput, existing CheckPolicy, exists bool) (CheckPolicy, error) {
	allowed, err := normalizeCheckEvents(input.AllowedEvents)
	if err != nil {
		return CheckPolicy{}, err
	}
	execution, err := normalizeCheckExecutionSettings(input.Executor, input.Execution)
	if err != nil {
		// A structured field refusal already carries ErrInvalidCheckPolicy, and
		// re-wrapping it with %v would flatten it back into a sentence. Any
		// other execution error keeps its original wrapping.
		if fieldErr := (*CheckPolicyFieldError)(nil); errors.As(err, &fieldErr) {
			return CheckPolicy{}, err
		}
		return CheckPolicy{}, fmt.Errorf("%w: %v", ErrInvalidCheckPolicy, err)
	}
	input.Execution = execution
	if err := validateCheckPolicyInput(input, allowed); err != nil {
		return CheckPolicy{}, err
	}
	runWorkflows := true
	if exists {
		runWorkflows = existing.RunWorkflows
	}
	if input.RunWorkflows != nil {
		runWorkflows = *input.RunWorkflows
	}
	candidate := CheckPolicy{
		RepositoryID: input.RepositoryID, Executor: input.Executor, AllowedEvents: allowed,
		MaxTimeoutMS: input.MaxTimeoutMS, MaxOutputLimitBytes: input.MaxOutputLimitBytes,
		QueueLimit: input.QueueLimit, MaxActiveJobs: input.MaxActiveJobs, MaxLeaseMS: input.MaxLeaseMS,
		Execution: input.Execution, RunWorkflows: runWorkflows,
	}
	candidate.Digest = checkPolicyDigest(candidate)
	return candidate, nil
}

func (s *Store) writeCheckPolicy(ctx context.Context, input CheckPolicyInput, base *ExpectedCheckPolicy, enable bool, now time.Time) (CheckPolicy, error) {
	if now.IsZero() {
		return CheckPolicy{}, fmt.Errorf("%w: missing time", ErrInvalidCheckPolicy)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckPolicy{}, err
	}
	defer tx.Rollback()
	existing, exists, err := readCheckPolicyTx(ctx, tx, input.RepositoryID)
	if err != nil {
		return CheckPolicy{}, err
	}
	candidate, err := candidateCheckPolicy(input, existing, exists)
	if err != nil {
		return CheckPolicy{}, err
	}
	if base != nil && (base.Version != existing.Version || base.Digest != existing.Digest) {
		return CheckPolicy{}, ErrCheckPolicyStale
	}
	ceilings, err := checkCeilings(ctx, tx)
	if err != nil {
		return CheckPolicy{}, err
	}
	if exceeded := ceilings.Exceeded(candidate.CeilingValues()); len(exceeded) != 0 {
		refusals := make([]error, len(exceeded))
		for index, field := range exceeded {
			refusals[index] = field
		}
		return CheckPolicy{}, errors.Join(refusals...)
	}
	stored := existing
	switch {
	case exists && existing.Digest == candidate.Digest:
	case !exists:
		candidate.Version = 1
		candidate.AuthorityEpoch, err = RandomID()
		if err != nil {
			return CheckPolicy{}, err
		}
		candidate.CreatedAt = now.UTC()
		candidate.UpdatedAt = candidate.CreatedAt
		executionJSON, err := checkExecutionSettingsJSON(candidate.Execution)
		if err != nil {
			return CheckPolicy{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO check_policies(
			repository_id,policy_version,policy_digest,executor,allowed_events,max_timeout_ms,max_output_limit_bytes,
			queue_limit,max_active_jobs,max_lease_ms,execution_json,consent_version,consent_digest,consent_active,runner_generation,
			authority_epoch,created_at,updated_at,run_workflows
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,0,'',0,0,?,?,?,?)`,
			candidate.RepositoryID, candidate.Version, candidate.Digest, candidate.Executor, marshalCheckEvents(candidate.AllowedEvents),
			candidate.MaxTimeoutMS, candidate.MaxOutputLimitBytes, candidate.QueueLimit, candidate.MaxActiveJobs, candidate.MaxLeaseMS,
			executionJSON, candidate.AuthorityEpoch, candidate.CreatedAt.Unix(), candidate.UpdatedAt.Unix(), boolInt(candidate.RunWorkflows)); err != nil {
			return CheckPolicy{}, err
		}
		stored = candidate
	default:
		candidate.Version = existing.Version + 1
		candidate.AuthorityEpoch = existing.AuthorityEpoch
		candidate.ConsentVersion = existing.ConsentVersion
		candidate.ConsentDigest = existing.ConsentDigest
		candidate.RunnerGeneration = existing.RunnerGeneration
		candidate.CreatedAt = existing.CreatedAt
		candidate.UpdatedAt = now.UTC()
		executionJSON, err := checkExecutionSettingsJSON(candidate.Execution)
		if err != nil {
			return CheckPolicy{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE check_policies SET
			policy_version=?,policy_digest=?,executor=?,allowed_events=?,max_timeout_ms=?,max_output_limit_bytes=?,
			queue_limit=?,max_active_jobs=?,max_lease_ms=?,execution_json=?,consent_active=0,updated_at=?,run_workflows=?
			WHERE repository_id=?`,
			candidate.Version, candidate.Digest, candidate.Executor, marshalCheckEvents(candidate.AllowedEvents),
			candidate.MaxTimeoutMS, candidate.MaxOutputLimitBytes, candidate.QueueLimit, candidate.MaxActiveJobs, candidate.MaxLeaseMS,
			executionJSON, candidate.UpdatedAt.Unix(), boolInt(candidate.RunWorkflows), candidate.RepositoryID); err != nil {
			return CheckPolicy{}, err
		}
		if err := interruptStalePendingCheckJobsTx(ctx, tx, candidate, now); err != nil {
			return CheckPolicy{}, err
		}
		stored = candidate
	}
	// Turning push checks on observes the branch heads afresh: an observation
	// left from while push was off must not make an unchanged head look
	// handled. A head that already has a job is still not queued again.
	if policyAllowsCheckEvent(candidate, checkworkflow.EventPush) && !(exists && policyAllowsCheckEvent(existing, checkworkflow.EventPush)) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM check_observations WHERE repository_id=?`, input.RepositoryID); err != nil {
			return CheckPolicy{}, err
		}
	}
	if enable {
		stored, err = grantCheckConsentTx(ctx, tx, input.RepositoryID, &ExpectedCheckPolicy{Version: stored.Version, Digest: candidate.Digest}, now)
		if err != nil {
			return CheckPolicy{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CheckPolicy{}, err
	}
	return stored, nil
}

// CheckPolicy reads the operator policy of one repository.
func (s *Store) CheckPolicy(ctx context.Context, repositoryID string) (CheckPolicy, bool, error) {
	return readCheckPolicyTx(ctx, s.db, repositoryID)
}

// CheckPolicyRepositories returns the IDs of the repositories that have an
// operator policy, in the order of Repositories. Only these can have
// configured checks, so background work over checks reads no other
// repository.
func (s *Store) CheckPolicyRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id FROM check_policies p JOIN repositories r ON r.id=p.repository_id
		ORDER BY lower(r.name),r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func readCheckPolicyTx(ctx context.Context, queryer querier, repositoryID string) (CheckPolicy, bool, error) {
	policy, err := scanCheckPolicy(queryer.QueryRowContext(ctx, checkPolicySelect+` WHERE repository_id=?`, repositoryID))
	if errors.Is(err, sql.ErrNoRows) {
		return CheckPolicy{}, false, nil
	}
	return policy, err == nil, err
}

const checkPolicySelect = `SELECT repository_id,policy_version,policy_digest,executor,allowed_events,max_timeout_ms,
	max_output_limit_bytes,queue_limit,max_active_jobs,max_lease_ms,execution_json,consent_version,consent_digest,consent_active,
	runner_generation,authority_epoch,created_at,updated_at,run_workflows FROM check_policies`

func scanCheckPolicy(scanner rowScanner) (CheckPolicy, error) {
	var policy CheckPolicy
	var allowedEvents, executionJSON string
	var consentActive, runWorkflows int
	var createdAt, updatedAt int64
	if err := scanner.Scan(&policy.RepositoryID, &policy.Version, &policy.Digest, &policy.Executor, &allowedEvents,
		&policy.MaxTimeoutMS, &policy.MaxOutputLimitBytes, &policy.QueueLimit, &policy.MaxActiveJobs, &policy.MaxLeaseMS, &executionJSON,
		&policy.ConsentVersion, &policy.ConsentDigest, &consentActive, &policy.RunnerGeneration, &policy.AuthorityEpoch,
		&createdAt, &updatedAt, &runWorkflows); err != nil {
		return CheckPolicy{}, err
	}
	if err := json.Unmarshal([]byte(allowedEvents), &policy.AllowedEvents); err != nil {
		return CheckPolicy{}, err
	}
	if err := json.Unmarshal([]byte(executionJSON), &policy.Execution); err != nil {
		return CheckPolicy{}, err
	}
	policy.ConsentActive = consentActive != 0
	policy.RunWorkflows = runWorkflows != 0
	policy.CreatedAt = unixTime(createdAt)
	policy.UpdatedAt = unixTime(updatedAt)
	return policy, nil
}

// GrantCheckConsent confirms the current policy. It is idempotent while the
// confirmation is current, and it never changes the policy generation.
func (s *Store) GrantCheckConsent(ctx context.Context, repositoryID string, now time.Time) (CheckPolicy, error) {
	return s.grantCheckConsent(ctx, repositoryID, nil, now)
}

// ExpectedCheckPolicy is the policy generation a caller believes it is
// approving. An operator approves a policy they have read, so the identity of
// that reading travels with the approval.
type ExpectedCheckPolicy struct {
	// Version is the policy generation the caller read.
	Version int64
	// Digest is that generation's identity. Both are required together: a
	// version alone could be reused across a change and back again, and a
	// digest alone does not say which generation stored it.
	Digest string
}

// Complete reports whether the caller stated a whole identity.
//
// A half-stated expectation is not a weaker expectation, it is an unusable
// one, and GrantCheckConsentFor refuses it rather than treating it as no
// expectation at all.
func (e ExpectedCheckPolicy) Complete() bool { return e.Version > 0 && e.Digest != "" }

// GrantCheckConsentFor binds consent to one exact policy generation.
//
// The expected identity is compared inside the same transaction that writes
// the consent, and before the idempotent-success path. Reading the policy in
// the caller and granting afterwards is not equivalent: another writer can
// replace the policy between those two steps, and the approval would land on a
// generation nobody read. ErrCheckPolicyStale reports exactly that.
//
// The expectation is mandatory here. A caller that supplies only a version, or
// only a digest, or nothing at all, has not identified what it is approving,
// and is refused with ErrCheckPolicyStale. Falling back to the unchecked path
// in that case would turn a malformed submission into a granted consent, which
// is the exact outcome this function exists to prevent. Callers with no
// identity to quote use GrantCheckConsent instead, and do so deliberately.
func (s *Store) GrantCheckConsentFor(ctx context.Context, repositoryID string, expected ExpectedCheckPolicy, now time.Time) (CheckPolicy, error) {
	if !expected.Complete() {
		return CheckPolicy{}, ErrCheckPolicyStale
	}
	return s.grantCheckConsent(ctx, repositoryID, &expected, now)
}

// grantCheckConsent records consent. A nil expectation is the legacy contract
// used by GrantCheckConsent, where the caller states no identity; it is never
// reachable from a partially filled ExpectedCheckPolicy.
func (s *Store) grantCheckConsent(ctx context.Context, repositoryID string, expected *ExpectedCheckPolicy, now time.Time) (CheckPolicy, error) {
	if now.IsZero() {
		return CheckPolicy{}, fmt.Errorf("%w: missing time", ErrInvalidCheckPolicy)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckPolicy{}, err
	}
	defer tx.Rollback()
	policy, err := grantCheckConsentTx(ctx, tx, repositoryID, expected, now)
	if err != nil {
		return CheckPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return CheckPolicy{}, err
	}
	return policy, nil
}

func grantCheckConsentTx(ctx context.Context, tx *sql.Tx, repositoryID string, expected *ExpectedCheckPolicy, now time.Time) (CheckPolicy, error) {
	policy, exists, err := readCheckPolicyTx(ctx, tx, repositoryID)
	if err != nil {
		return CheckPolicy{}, err
	}
	if !exists {
		return CheckPolicy{}, ErrCheckPolicyMissing
	}
	// Checked before the legacy and idempotent paths below. A stale approval
	// must be refused outright, never answered with the success of an approval
	// that was granted for some other generation.
	if expected != nil && (expected.Version != policy.Version || expected.Digest != policy.Digest) {
		return CheckPolicy{}, ErrCheckPolicyStale
	}
	if policy.Execution.Legacy {
		return CheckPolicy{}, fmt.Errorf("%w: the policy must be reconfigured with current execution settings", ErrInvalidCheckPolicy)
	}
	if policy.ConsentActive && policy.ConsentDigest == policy.Digest && policy.ConsentVersion > 0 {
		return policy, nil
	}
	policy.ConsentVersion++
	policy.ConsentDigest = policy.Digest
	policy.ConsentActive = true
	policy.UpdatedAt = now.UTC()
	// The write names the generation it read. If a concurrent transaction
	// replaced the policy after this one read it, no row matches and the
	// approval is refused rather than landing on the newer generation.
	result, err := tx.ExecContext(ctx, `UPDATE check_policies SET consent_version=?,consent_digest=?,consent_active=1,updated_at=?
		WHERE repository_id=? AND policy_version=? AND policy_digest=?`,
		policy.ConsentVersion, policy.ConsentDigest, policy.UpdatedAt.Unix(), repositoryID, policy.Version, policy.Digest)
	if err != nil {
		return CheckPolicy{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CheckPolicy{}, err
	}
	if affected != 1 {
		return CheckPolicy{}, ErrCheckPolicyStale
	}
	return policy, nil
}

// RevokeCheckConsent clears the local confirmation while keeping its
// generation, so pre-revoke consent cannot revive.
func (s *Store) RevokeCheckConsent(ctx context.Context, repositoryID string, now time.Time) (CheckPolicy, error) {
	if now.IsZero() {
		return CheckPolicy{}, fmt.Errorf("%w: missing time", ErrInvalidCheckPolicy)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckPolicy{}, err
	}
	defer tx.Rollback()
	policy, exists, err := readCheckPolicyTx(ctx, tx, repositoryID)
	if err != nil {
		return CheckPolicy{}, err
	}
	if !exists {
		return CheckPolicy{}, ErrCheckPolicyMissing
	}
	if !policy.ConsentActive {
		if err := tx.Commit(); err != nil {
			return CheckPolicy{}, err
		}
		return policy, nil
	}
	policy.ConsentActive = false
	policy.UpdatedAt = now.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE check_policies SET consent_active=0,updated_at=? WHERE repository_id=?`,
		policy.UpdatedAt.Unix(), repositoryID); err != nil {
		return CheckPolicy{}, err
	}
	if err := interruptStalePendingCheckJobsTx(ctx, tx, policy, now); err != nil {
		return CheckPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return CheckPolicy{}, err
	}
	return policy, nil
}

func checkConsentCurrent(policy CheckPolicy) bool {
	return policy.ConsentActive && policy.ConsentVersion > 0 && policy.ConsentDigest == policy.Digest
}

func policyAllowsCheckEvent(policy CheckPolicy, event string) bool {
	return slices.Contains(policy.AllowedEvents, event)
}

func checkJobAuthorityCurrent(job CheckJob, policy CheckPolicy) bool {
	return checkConsentCurrent(policy) && job.PolicyVersion == policy.Version && job.ConsentVersion == policy.ConsentVersion &&
		job.Executor == policy.Executor && policyAllowsCheckEvent(policy, job.Trigger)
}

// checkPolicyDigest is the canonical identity of the operator-selected facts.
func checkPolicyDigest(policy CheckPolicy) string {
	fields := []string{
		"check-policy-v1", policy.RepositoryID, policy.Executor, strings.Join(policy.AllowedEvents, ","),
		fmt.Sprint(policy.MaxTimeoutMS), fmt.Sprint(policy.MaxOutputLimitBytes),
		fmt.Sprint(policy.QueueLimit), fmt.Sprint(policy.MaxActiveJobs), fmt.Sprint(policy.MaxLeaseMS),
	}
	if policy.RunWorkflows {
		fields = append(fields, "run_workflows", "true")
	}
	if policy.Execution.Legacy {
		return digestFields(fields...)
	}
	executionJSON, err := checkExecutionSettingsJSON(policy.Execution)
	if err != nil {
		return ""
	}
	fields[0] = "check-policy-v2"
	fields = append(fields, executionJSON)
	return digestFields(fields...)
}

// normalizeCheckEvents canonicalizes the event set.
//
// This is the earliest validation a policy meets, so its refusals are
// structured like every later one. A caller that only asked "did this fail"
// still sees ErrInvalidCheckPolicy through Unwrap, while a caller rendering a
// form can find out which control to point at. Returning a plain sentence here
// would leave the very first refusal a reader meets as the one with no field
// attached.
func normalizeCheckEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return nil, &CheckPolicyFieldError{Field: FieldAllowedEvents, Rule: RuleRequired}
	}
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		if event != checkworkflow.EventPush && event != checkworkflow.EventPullRequest && event != ActionsEventDispatch && event != ActionsEventSchedule {
			return nil, unknownValueError(FieldAllowedEvents, event)
		}
		if seen[event] {
			return nil, &CheckPolicyFieldError{Field: FieldAllowedEvents, Rule: RuleDuplicate, Value: event}
		}
		seen[event] = true
	}
	normalized := make([]string, 0, len(seen))
	for event := range seen {
		normalized = append(normalized, event)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func marshalCheckEvents(events []string) string {
	encoded, err := json.Marshal(events)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func validateCheckPolicyInput(input CheckPolicyInput, allowed []string) error {
	if input.RepositoryID == "" {
		return fmt.Errorf("%w: missing repository", ErrInvalidCheckPolicy)
	}
	// Each rejection below is structured, so a caller can attach it to the
	// control that carries the field. The bounds are unchanged: they are still
	// the constants enforced here and nowhere else.
	if !validCheckExecutor(input.Executor) {
		return unknownValueError(FieldExecutor, input.Executor)
	}
	if len(allowed) == 0 {
		return &CheckPolicyFieldError{Field: FieldAllowedEvents, Rule: RuleRequired}
	}
	// Each numeric field is checked against its published range, the same one
	// CheckPolicyBoundsFor reports to an interface. One table, so the rule
	// that refuses a value and the range shown beside the control cannot
	// drift apart.
	for _, field := range []struct {
		name  string
		value int64
	}{
		{FieldMaxTimeoutMS, input.MaxTimeoutMS},
		{FieldMaxOutputLimitBytes, input.MaxOutputLimitBytes},
		{FieldQueueLimit, int64(input.QueueLimit)},
		{FieldMaxActiveJobs, int64(input.MaxActiveJobs)},
		{FieldMaxLeaseMS, input.MaxLeaseMS},
	} {
		bounds, _ := CheckPolicyBoundsFor(field.name)
		if field.value < bounds.Min || field.value > bounds.Max {
			return boundedRangeError(field.name, field.value)
		}
	}
	return nil
}

func validCheckExecutor(value string) bool {
	switch value {
	case CheckExecutorHost, CheckExecutorContainer, CheckExecutorExternalRunner:
		return true
	default:
		return false
	}
}

func executionScopeForExecutor(executor string) string {
	switch executor {
	case CheckExecutorContainer:
		return ExecutionScopeContainer
	case CheckExecutorExternalRunner:
		return ExecutionScopeExternalRunner
	default:
		return ExecutionScopeInherited
	}
}

func validProtectionForExecutor(executor, protection string) bool {
	switch protection {
	case ProtectionUnknown:
		return true
	}
	switch executor {
	case CheckExecutorHost:
		return protection == ProtectionHost
	case CheckExecutorContainer:
		return protection == ProtectionContainer
	case CheckExecutorExternalRunner:
		return protection == ProtectionRunnerReported
	default:
		return false
	}
}

// IssueCheckRunnerToken creates one revocable repository-scoped bearer token.
// OwnGit constructs the token and binds it to the local authority epoch,
// credential identity, and generation. An idempotent creation replay returns
// the original metadata without redisclosing the token.
func (s *Store) IssueCheckRunnerToken(ctx context.Context, repositoryID, label, creationID string, now time.Time) (RunnerCredential, string, bool, error) {
	if repositoryID == "" || now.IsZero() {
		return RunnerCredential{}, "", false, fmt.Errorf("%w: invalid runner credential", ErrInvalidCheckJob)
	}
	if creationID != "" && !validAttemptID(creationID) {
		return RunnerCredential{}, "", false, fmt.Errorf("%w: invalid creation identity", ErrInvalidCheckJob)
	}
	label = trimLabel(label)
	if label == "" {
		label = "check runner"
	}
	if !validText(label, 100) {
		return RunnerCredential{}, "", false, fmt.Errorf("%w: invalid label", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunnerCredential{}, "", false, err
	}
	defer tx.Rollback()
	policy, exists, err := readCheckPolicyTx(ctx, tx, repositoryID)
	if err != nil {
		return RunnerCredential{}, "", false, err
	}
	if !exists {
		return RunnerCredential{}, "", false, ErrCheckPolicyMissing
	}
	if creationID != "" {
		existing, found, err := runnerCredentialByCreationTx(ctx, tx, repositoryID, creationID)
		if err != nil {
			return RunnerCredential{}, "", false, err
		}
		if found {
			if existing.Label != label {
				return RunnerCredential{}, "", false, ErrCheckRunnerCreationConflict
			}
			return existing, "", false, nil
		}
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `UPDATE check_policies SET runner_generation=runner_generation+1,updated_at=? WHERE repository_id=? RETURNING runner_generation`,
		now.UTC().Unix(), repositoryID).Scan(&generation); err != nil {
		return RunnerCredential{}, "", false, err
	}
	id, err := RandomID()
	if err != nil {
		return RunnerCredential{}, "", false, err
	}
	secret, err := RandomID()
	if err != nil {
		return RunnerCredential{}, "", false, err
	}
	token := formatRunnerToken(policy.AuthorityEpoch, generation, id, secret)
	tokenHash := sha256.Sum256([]byte(token))
	credential := RunnerCredential{ID: id, RepositoryID: repositoryID, Label: label, CreationID: creationID, Generation: generation, Role: RunnerRoleExternal, CreatedAt: now.UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_runner_credentials(id,repository_id,label,creation_id,generation,role,token_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		credential.ID, credential.RepositoryID, credential.Label, credential.CreationID, credential.Generation, credential.Role, tokenHash[:], credential.CreatedAt.Unix()); err != nil {
		return RunnerCredential{}, "", false, err
	}
	if err := tx.Commit(); err != nil {
		return RunnerCredential{}, "", false, err
	}
	return credential, token, true, nil
}

func runnerCredentialByCreationTx(ctx context.Context, queryer querier, repositoryID, creationID string) (RunnerCredential, bool, error) {
	credential, err := scanRunnerCredential(queryer.QueryRowContext(ctx, runnerCredentialSelect+` WHERE repository_id=? AND creation_id=?`, repositoryID, creationID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunnerCredential{}, false, nil
	}
	return credential, err == nil, err
}

const runnerTokenPrefix = "owngit-runner-v1"

func formatRunnerToken(epoch string, generation int64, id, secret string) string {
	return strings.Join([]string{runnerTokenPrefix, epoch, strconv.FormatInt(generation, 10), id, secret}, ".")
}

func parseRunnerToken(token string) (string, int64, string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != runnerTokenPrefix || !validAttemptID(parts[1]) || !validAttemptID(parts[3]) || !validAttemptID(parts[4]) {
		return "", 0, "", false
	}
	generation, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || generation <= 0 {
		return "", 0, "", false
	}
	return parts[1], generation, parts[3], true
}

// RunnerCredentialByToken resolves a live credential and records its use. The
// encoded local epoch makes every pre-restore token unverifiable. A token that
// is live for another repository returns ErrRunnerCredentialOtherRepository,
// so only its holder learns that the repository, not the token, is wrong. An
// empty repositoryID, an address that reaches no repository, is no
// repository's, so a live token is another repository's.
func (s *Store) RunnerCredentialByToken(ctx context.Context, repositoryID, token string, now time.Time) (RunnerCredential, bool, error) {
	epoch, generation, id, valid := parseRunnerToken(token)
	if !valid || now.IsZero() {
		return RunnerCredential{}, false, nil
	}
	tokenHash := sha256.Sum256([]byte(token))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunnerCredential{}, false, err
	}
	defer tx.Rollback()
	notFound := func() (RunnerCredential, bool, error) {
		elsewhere, err := runnerTokenLiveElsewhereTx(ctx, tx, repositoryID, epoch, generation, id, tokenHash[:])
		if err != nil {
			return RunnerCredential{}, false, err
		}
		if elsewhere {
			return RunnerCredential{}, false, ErrRunnerCredentialOtherRepository
		}
		return RunnerCredential{}, false, nil
	}
	policy, exists, err := readCheckPolicyTx(ctx, tx, repositoryID)
	if err != nil {
		return RunnerCredential{}, false, err
	}
	if !exists || policy.AuthorityEpoch != epoch {
		return notFound()
	}
	credential, err := scanRunnerCredential(tx.QueryRowContext(ctx, runnerCredentialSelect+` WHERE repository_id=? AND id=? AND generation=? AND token_hash=? AND revoked_at IS NULL`, repositoryID, id, generation, tokenHash[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return notFound()
	}
	if err != nil {
		return RunnerCredential{}, false, err
	}
	value := now.UTC()
	result, err := tx.ExecContext(ctx, `UPDATE check_runner_credentials SET last_used_at=? WHERE id=? AND revoked_at IS NULL`, value.Unix(), credential.ID)
	if err != nil {
		return RunnerCredential{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return RunnerCredential{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RunnerCredential{}, false, err
	}
	credential.LastUsedAt = &value
	return credential, true, nil
}

// runnerTokenLiveElsewhereTx reports whether the presented token, matched by
// its full hash, is a live external-runner credential of a repository other
// than repositoryID under that repository's current authority epoch.
func runnerTokenLiveElsewhereTx(ctx context.Context, tx *sql.Tx, repositoryID, epoch string, generation int64, id string, tokenHash []byte) (bool, error) {
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT repository_id FROM check_runner_credentials WHERE id=? AND generation=? AND token_hash=? AND role=? AND revoked_at IS NULL AND repository_id<>?`,
		id, generation, tokenHash, RunnerRoleExternal, repositoryID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	policy, exists, err := readCheckPolicyTx(ctx, tx, owner)
	if err != nil {
		return false, err
	}
	return exists && policy.AuthorityEpoch == epoch, nil
}

// CheckRunnerCredentials lists the authority of one repository in creation
// order, including revoked rows.
func (s *Store) CheckRunnerCredentials(ctx context.Context, repositoryID string) ([]RunnerCredential, error) {
	rows, err := s.db.QueryContext(ctx, runnerCredentialSelect+` WHERE repository_id=? ORDER BY created_at,id`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var credentials []RunnerCredential
	for rows.Next() {
		credential, err := scanRunnerCredential(rows)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

// RevokeCheckRunnerToken retires one credential. An unstarted claim bound to
// it is interrupted atomically; an already started job keeps its evidence but
// cannot submit a new completion with the retired authority.
func (s *Store) RevokeCheckRunnerToken(ctx context.Context, repositoryID, id string, now time.Time) error {
	if repositoryID == "" || !validAttemptID(id) || now.IsZero() {
		return ErrCheckRunnerRevoked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE check_runner_credentials SET revoked_at=? WHERE repository_id=? AND id=? AND revoked_at IS NULL`, now.UTC().Unix(), repositoryID, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrCheckRunnerRevoked
	}
	if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='interrupted',lease_id='',lease_expires_at=NULL,
		finished_at=?,interrupted_at=?,summary='Runner authority was revoked before start.'
		WHERE repository_id=? AND credential_id=? AND status='claimed'`, now.UTC().UnixNano(), now.UTC().UnixNano(), repositoryID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeCheckRunnerTokenByCreation is an idempotent compensating revoke after
// a lost issue response.
func (s *Store) RevokeCheckRunnerTokenByCreation(ctx context.Context, repositoryID, creationID string, now time.Time) error {
	if creationID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `UPDATE check_runner_credentials SET revoked_at=? WHERE repository_id=? AND creation_id=? AND revoked_at IS NULL RETURNING id`,
		now.UTC().Unix(), repositoryID, creationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='interrupted',lease_id='',lease_expires_at=NULL,
		finished_at=?,interrupted_at=?,summary='Runner authority was revoked before start.'
		WHERE repository_id=? AND credential_id=? AND status='claimed'`, now.UTC().UnixNano(), now.UTC().UnixNano(), repositoryID, id); err != nil {
		return err
	}
	return tx.Commit()
}

const runnerCredentialSelect = `SELECT id,repository_id,label,creation_id,generation,role,created_at,revoked_at,last_used_at FROM check_runner_credentials`

func scanRunnerCredential(scanner rowScanner) (RunnerCredential, error) {
	var credential RunnerCredential
	var createdAt int64
	var revokedAt, lastUsedAt sql.NullInt64
	if err := scanner.Scan(&credential.ID, &credential.RepositoryID, &credential.Label, &credential.CreationID, &credential.Generation, &credential.Role, &createdAt, &revokedAt, &lastUsedAt); err != nil {
		return RunnerCredential{}, err
	}
	if !validRunnerRole(credential.Role) {
		return RunnerCredential{}, errors.New("invalid stored runner role")
	}
	credential.CreatedAt = unixTime(createdAt)
	if revokedAt.Valid {
		value := unixTime(revokedAt.Int64)
		credential.RevokedAt = &value
	}
	if lastUsedAt.Valid {
		value := unixTime(lastUsedAt.Int64)
		credential.LastUsedAt = &value
	}
	return credential, nil
}

func liveRunnerCredentialTx(ctx context.Context, queryer querier, repositoryID, id string) (RunnerCredential, bool, error) {
	credential, err := scanRunnerCredential(queryer.QueryRowContext(ctx, runnerCredentialSelect+` WHERE repository_id=? AND id=? AND revoked_at IS NULL`, repositoryID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RunnerCredential{}, false, nil
	}
	return credential, err == nil, err
}

// RecordCheckObservation upserts one observed ref and keeps the newest bounded
// set per repository.
func (s *Store) RecordCheckObservation(ctx context.Context, repositoryID, refName, oid string, now time.Time) error {
	if repositoryID == "" || !validObservationRef(refName) || !validObjectID(oid) || now.IsZero() {
		return ErrInvalidCheckObservation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Read in the write transaction so only a new ref can increase the count.
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT oid FROM check_observations WHERE repository_id=? AND ref_name=?`, repositoryID, refName).Scan(&stored)
	if err == nil && stored == oid {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	newRef := errors.Is(err, sql.ErrNoRows)
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_observations(repository_id,ref_name,oid,observed_at) VALUES(?,?,?,?)
		ON CONFLICT(repository_id,ref_name) DO UPDATE SET oid=excluded.oid,observed_at=excluded.observed_at`,
		repositoryID, refName, oid, now.UTC().UnixNano()); err != nil {
		return err
	}
	if newRef {
		if _, err := tx.ExecContext(ctx, `DELETE FROM check_observations WHERE rowid IN (
			SELECT rowid FROM check_observations WHERE repository_id=? ORDER BY observed_at DESC,ref_name DESC LIMIT -1 OFFSET ?
		)`, repositoryID, MaximumCheckObservations); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CheckPendingRepositories lists the repositories with a pending job, the one
// holding the oldest pending job first.
func (s *Store) CheckPendingRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id FROM check_jobs WHERE status='pending'
		GROUP BY repository_id ORDER BY MIN(admitted_at),repository_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CheckEventsWithJobs reports which of eventKeys already have a job of the
// given trigger in the repository, under any policy version. The coordinator
// uses it for branch heads it has no observation for, so a head it saw before
// the bounded observation set dropped it is not queued a second time.
func (s *Store) CheckEventsWithJobs(ctx context.Context, repositoryID, trigger string, eventKeys []string) (map[string]bool, error) {
	found := make(map[string]bool, len(eventKeys))
	if len(eventKeys) == 0 {
		return found, nil
	}
	arguments := make([]any, 0, len(eventKeys)+2)
	arguments = append(arguments, repositoryID, trigger)
	for _, key := range eventKeys {
		arguments = append(arguments, key)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventKeys)), ",")
	arguments = append(arguments, arguments...)
	rows, err := s.db.QueryContext(ctx, `SELECT event_key FROM check_jobs WHERE repository_id=? AND trigger_kind=? AND event_key IN (`+placeholders+`)
		UNION SELECT event_key FROM actions_runs WHERE repository_id=? AND event=? AND event_key IN (`+placeholders+`)`, arguments...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, err
		}
		found[key] = true
	}
	return found, closeRows(rows)
}

// DeleteCheckObservation records a branch deletion by removing its last live
// object. A later recreation is therefore admitted as a new ref event.
func (s *Store) DeleteCheckObservation(ctx context.Context, repositoryID, refName string) error {
	if repositoryID == "" || !validObservationRef(refName) {
		return ErrInvalidCheckObservation
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM check_observations WHERE repository_id=? AND ref_name=?`, repositoryID, refName)
	return err
}

// CheckObservations lists the retained observations newest first.
func (s *Store) CheckObservations(ctx context.Context, repositoryID string) ([]CheckObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id,ref_name,oid,observed_at FROM check_observations WHERE repository_id=? ORDER BY observed_at DESC,ref_name DESC`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []CheckObservation
	for rows.Next() {
		var observation CheckObservation
		var observedAt int64
		if err := rows.Scan(&observation.RepositoryID, &observation.RefName, &observation.OID, &observedAt); err != nil {
			return nil, err
		}
		observation.ObservedAt = unixNanoTime(observedAt)
		observations = append(observations, observation)
	}
	return observations, rows.Err()
}

// ValidCheckObservationRef reports whether a branch ref can be observed.
func ValidCheckObservationRef(refName string) bool { return validObservationRef(refName) }

func validObservationRef(value string) bool {
	const prefix = "refs/heads/"
	if len(value) <= len(prefix) || len(value) > 500 || !strings.HasPrefix(value, prefix) {
		return false
	}
	branch := strings.TrimPrefix(value, prefix)
	return branch != "@" && validBranchText(branch)
}

// AdmitCheckJob admits one bounded job. Identical effective conditions dedup
// to the existing job, and the queue bound is enforced in the same transaction.
func (s *Store) AdmitCheckJob(ctx context.Context, request CheckJobRequest, now time.Time) (CheckJob, bool, error) {
	if request.RunID != "" {
		return CheckJob{}, false, fmt.Errorf("%w: admit the workflow run with its plans", ErrInvalidCheckJob)
	}
	if now.IsZero() {
		return CheckJob{}, false, fmt.Errorf("%w: missing time", ErrInvalidCheckJob)
	}
	if err := validateCheckJobRequest(request); err != nil {
		return CheckJob{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, false, err
	}
	defer tx.Rollback()
	job, deduped, err := admitCheckJobTx(ctx, tx, request, now)
	if err != nil {
		return CheckJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return job, deduped, err
	}
	return job, deduped, nil
}

// RerunCheckJob admits a fresh job for one terminal job. The rerun
// keeps the exact trigger facts but carries a distinct generation, so it never
// dedups onto the original and never silently requeues possibly executed work.
//
// The rerun's limits come from the same rule as a new job's: the workflow's
// request, cut to the policy's current caps. requested is that request, the
// workflow's own timeout and output limit, each zero when the workflow set
// none. Nil keeps the original job's effective limits as the request.
func (s *Store) RerunCheckJob(ctx context.Context, repositoryID, jobID string, requested *CheckJobLimits, now time.Time) (CheckJob, bool, error) {
	if repositoryID == "" || !validAttemptID(jobID) || now.IsZero() {
		return CheckJob{}, false, fmt.Errorf("%w: invalid rerun", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, false, err
	}
	defer tx.Rollback()
	original, exists, err := readCheckJobTx(ctx, tx, repositoryID, jobID)
	if err != nil {
		return CheckJob{}, false, err
	}
	if !exists {
		return CheckJob{}, false, ErrCheckJobNotFound
	}
	if original.RunID != "" {
		return CheckJob{}, false, fmt.Errorf("%w: rerun the workflow run instead", ErrCheckJobState)
	}
	if !terminalCheckJob(original.Status) {
		return CheckJob{}, false, ErrCheckJobState
	}
	configuration, exists, err := readCheckConfigurationTx(ctx, tx, repositoryID, original.ConfigurationVersion)
	if err != nil {
		return CheckJob{}, false, err
	}
	if !exists {
		return CheckJob{}, false, ErrCheckConfigurationMissing
	}
	root := original.RerunRoot
	if root == "" {
		root = original.ID
	}
	// An outstanding rerun already carries the request, so a second rerun does
	// not queue another generation until the first finishes.
	outstanding, found, err := readUnfinishedRerunTx(ctx, tx, repositoryID, root)
	if err != nil {
		return CheckJob{}, false, err
	}
	if found {
		return outstanding, true, nil
	}
	var maximum int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rerun_generation),0) FROM check_jobs WHERE repository_id=? AND (id=? OR rerun_root=?)`,
		repositoryID, root, root).Scan(&maximum); err != nil {
		return CheckJob{}, false, err
	}
	if requested == nil {
		requested = &original.Limits
	}
	request := CheckJobRequest{
		RepositoryID: repositoryID, Trigger: original.Trigger, EventKey: original.EventKey,
		SourceOID: original.SourceOID, BaseOID: original.BaseOID, PullRequestNumber: original.PullRequestNumber,
		TriggerRef: original.TriggerRef, WorkflowPath: original.WorkflowPath, WorkflowOID: original.WorkflowOID,
		WorkflowDigest: original.WorkflowDigest, Checks: configuration.Checks,
		TimeoutMS: requested.TimeoutMS, OutputLimitBytes: requested.OutputLimitBytes,
		RerunRoot: root, RerunGeneration: maximum + 1,
	}
	job, deduped, err := admitCheckJobTx(ctx, tx, request, now)
	if err != nil {
		return CheckJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return job, deduped, err
	}
	return job, deduped, nil
}

func admitCheckJobTx(ctx context.Context, tx *sql.Tx, request CheckJobRequest, now time.Time) (CheckJob, bool, error) {
	policy, exists, err := readCheckPolicyTx(ctx, tx, request.RepositoryID)
	if err != nil {
		return CheckJob{}, false, err
	}
	if !exists {
		return CheckJob{}, false, ErrCheckPolicyMissing
	}
	if !checkConsentCurrent(policy) {
		return CheckJob{}, false, ErrCheckConsentRequired
	}
	if !policyAllowsCheckEvent(policy, request.Trigger) {
		return CheckJob{}, false, fmt.Errorf("%w: %s", ErrCheckEventNotAllowed, request.Trigger)
	}
	ceilings, err := checkCeilings(ctx, tx)
	if err != nil {
		return CheckJob{}, false, err
	}
	if err := admissionError(ceilings.Exceeded(policy.CeilingValues())); err != nil {
		return CheckJob{}, false, err
	}
	if request.RunID != "" && !policy.RunWorkflows {
		return CheckJob{}, false, ErrActionsWorkflowsOff
	}
	taskID := automaticCheckTaskID(request)
	title := "Automatic checks"
	if request.RunID != "" {
		title = "Workflows"
	}
	if err := ensureAutomaticCheckTaskTx(ctx, tx, request.RepositoryID, taskID, title, now); err != nil {
		return CheckJob{}, false, err
	}
	limits := effectiveCheckJobLimits(request, policy)
	configurationVersion, configHash, err := ensureCheckConfiguration(ctx, tx, request.RepositoryID, request.Checks, now)
	if err != nil {
		return CheckJob{}, false, err
	}
	workflowPath := request.WorkflowPath
	if workflowPath == "" {
		workflowPath = checkworkflow.Path
	}
	job := CheckJob{
		RepositoryID: request.RepositoryID, TaskID: taskID, Trigger: request.Trigger, EventKey: request.EventKey,
		SourceOID: request.SourceOID, BaseOID: request.BaseOID, PullRequestNumber: request.PullRequestNumber,
		TriggerRef: request.TriggerRef, WorkflowPath: workflowPath, WorkflowOID: request.WorkflowOID,
		WorkflowDigest: request.WorkflowDigest, ConfigurationVersion: configurationVersion,
		Executor: policy.Executor, PolicyVersion: policy.Version, ConsentVersion: policy.ConsentVersion,
		Limits: limits, Execution: policy.Execution, RerunRoot: request.RerunRoot, RerunGeneration: request.RerunGeneration,
		Status: CheckJobPending, Protection: ProtectionUnknown, AdmittedAt: now.UTC(),
	}
	job.RunID, job.JobKey, job.MatrixIndex, job.PlanDigest = request.RunID, request.JobKey, request.MatrixIndex, request.PlanDigest
	job.Tolerated, job.ConcurrencyGroup, job.MaxParallel = request.Tolerated, request.ConcurrencyGroup, request.MaxParallel
	if request.Waiting {
		if request.RunID == "" {
			return CheckJob{}, false, fmt.Errorf("%w: only a workflow job can wait", ErrInvalidCheckJob)
		}
		job.Status = CheckJobWaiting
	}
	job.DedupDigest = checkJobDedupDigest(job, configHash)
	existing, found, err := readCheckJobByDedupTx(ctx, tx, request.RepositoryID, job.DedupDigest)
	if err != nil {
		return CheckJob{}, false, err
	}
	if found {
		return existing, true, nil
	}
	active, err := countUnfinishedCheckJobsTx(ctx, tx, request.RepositoryID)
	if err != nil {
		return CheckJob{}, false, err
	}
	if active >= policy.QueueLimit {
		return job, false, ErrCheckQueueFull
	}
	id, err := RandomID()
	if err != nil {
		return CheckJob{}, false, err
	}
	job.ID = id
	limitsJSON, err := json.Marshal(job.Limits)
	if err != nil {
		return CheckJob{}, false, err
	}
	executionJSON, err := checkExecutionSettingsJSON(job.Execution)
	if err != nil {
		return CheckJob{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_jobs(
		id,repository_id,task_id,trigger_kind,event_key,source_oid,base_oid,pull_request_number,trigger_ref,workflow_path,
		workflow_oid,workflow_digest,configuration_version,executor,policy_version,consent_version,limits_json,execution_json,
		dedup_digest,rerun_root,rerun_generation,status,protection,admitted_at,
		run_id,job_key,matrix_index,plan_digest,tolerated,concurrency_group,max_parallel
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.RepositoryID, job.TaskID, job.Trigger, job.EventKey, job.SourceOID, job.BaseOID, job.PullRequestNumber,
		job.TriggerRef, job.WorkflowPath, job.WorkflowOID, job.WorkflowDigest, job.ConfigurationVersion,
		job.Executor, job.PolicyVersion, job.ConsentVersion, string(limitsJSON), executionJSON, job.DedupDigest,
		job.RerunRoot, job.RerunGeneration, job.Status, job.Protection, job.AdmittedAt.UnixNano(),
		job.RunID, job.JobKey, job.MatrixIndex, job.PlanDigest, boolInt(job.Tolerated), job.ConcurrencyGroup, job.MaxParallel); err != nil {
		// A concurrent admission may have won the dedup index. Returning that
		// job keeps the observation exactly-once without a second queue slot.
		existing, found, readErr := readCheckJobByDedupTx(ctx, tx, request.RepositoryID, job.DedupDigest)
		if readErr == nil && found {
			return existing, true, nil
		}
		return CheckJob{}, false, err
	}
	return job, false, nil
}

func automaticCheckTaskID(request CheckJobRequest) string {
	identity := request.TriggerRef
	if request.Trigger == checkworkflow.EventPullRequest {
		identity = strconv.FormatInt(request.PullRequestNumber, 10)
	}
	lane := "automatic-check-task-v1"
	if request.RunID != "" {
		lane = "automatic-workflow-task-v1"
	}
	return digestFields(lane, request.RepositoryID, request.Trigger, identity)[:32]
}

func ensureAutomaticCheckTaskTx(ctx context.Context, tx *sql.Tx, repositoryID, taskID, title string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,repository_id,title,created_at,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`, taskID, repositoryID, title, now.UTC().Unix(), now.UTC().Unix()); err != nil {
		return err
	}
	var storedRepository string
	if err := tx.QueryRowContext(ctx, `SELECT repository_id FROM tasks WHERE id=?`, taskID).Scan(&storedRepository); err != nil {
		return err
	}
	if storedRepository != repositoryID {
		return errors.New("automatic check task belongs to another repository")
	}
	return nil
}

// effectiveCheckJobLimits is the one rule for a job's limits, for new jobs and
// reruns alike: the workflow's request (the default when it set none), raised
// to the minimum and cut to the policy's current caps.
func effectiveCheckJobLimits(request CheckJobRequest, policy CheckPolicy) CheckJobLimits {
	timeout := request.TimeoutMS
	if timeout <= 0 {
		timeout = checkworkflow.DefaultTimeoutMS
	}
	output := request.OutputLimitBytes
	if output <= 0 {
		output = checkworkflow.DefaultOutputLimitBytes
	}
	return CheckJobLimits{
		TimeoutMS:        min(max(timeout, checkworkflow.MinimumTimeoutMS), policy.MaxTimeoutMS),
		OutputLimitBytes: min(max(output, checkworkflow.MinimumOutputLimitBytes), policy.MaxOutputLimitBytes),
	}
}

// checkJobDedupDigest covers every effective execution condition, not merely
// the commit and workflow bytes.
func checkJobDedupDigest(job CheckJob, configHash string) string {
	fields := []string{
		"check-job-v1", job.RepositoryID, job.TaskID, job.Trigger, job.EventKey,
		job.SourceOID, job.BaseOID, fmt.Sprint(job.PullRequestNumber), job.TriggerRef,
		job.WorkflowPath, job.WorkflowOID, job.WorkflowDigest, configHash,
		job.Executor, fmt.Sprint(job.PolicyVersion), fmt.Sprint(job.ConsentVersion),
		fmt.Sprint(job.Limits.TimeoutMS), fmt.Sprint(job.Limits.OutputLimitBytes),
		job.RerunRoot, fmt.Sprint(job.RerunGeneration),
	}
	if job.Execution.Legacy && job.RunID == "" {
		return digestFields(fields...)
	}
	executionJSON, err := checkExecutionSettingsJSON(job.Execution)
	if err != nil {
		return ""
	}
	fields[0] = "check-job-v2"
	fields = append(fields, executionJSON)
	if job.RunID != "" {
		fields[0] = "check-job-v3"
		fields = append(fields, job.JobKey, strconv.Itoa(job.MatrixIndex), job.PlanDigest)
	}
	return digestFields(fields...)
}

func validateCheckJobRequest(request CheckJobRequest) error {
	if request.RepositoryID == "" {
		return fmt.Errorf("%w: missing repository", ErrInvalidCheckJob)
	}
	switch request.Trigger {
	case checkworkflow.EventPush:
		if request.PullRequestNumber != 0 || request.BaseOID != "" {
			return fmt.Errorf("%w: a push job cannot carry pull request facts", ErrInvalidCheckJob)
		}
	case checkworkflow.EventPullRequest:
		if request.PullRequestNumber <= 0 || !validObjectID(request.BaseOID) {
			return fmt.Errorf("%w: a pull request job needs a number and a base object", ErrInvalidCheckJob)
		}
	case ActionsEventDispatch, ActionsEventSchedule:
		if request.RunID == "" || request.PullRequestNumber != 0 || request.BaseOID != "" {
			return fmt.Errorf("%w: invalid workflow trigger facts", ErrInvalidCheckJob)
		}
	default:
		return fmt.Errorf("%w: unknown trigger %q", ErrInvalidCheckJob, request.Trigger)
	}
	if !validObjectID(request.SourceOID) {
		return fmt.Errorf("%w: invalid source object", ErrInvalidCheckJob)
	}
	if len(request.EventKey) == 0 || len(request.EventKey) > MaximumCheckEventKeyBytes || strings.ContainsAny(request.EventKey, "\x00\r\n") {
		return fmt.Errorf("%w: invalid event key", ErrInvalidCheckJob)
	}
	if request.TriggerRef == "" || request.TriggerRef == "@" || len(request.TriggerRef) > MaximumCheckTriggerRefBytes || !validBranchText(request.TriggerRef) {
		return fmt.Errorf("%w: invalid trigger ref", ErrInvalidCheckJob)
	}
	if request.WorkflowPath != "" && request.WorkflowPath != checkworkflow.Path && !(request.RunID != "" && validActionsWorkflowPath(request.WorkflowPath)) {
		return fmt.Errorf("%w: unsupported workflow path %q", ErrInvalidCheckJob, request.WorkflowPath)
	}
	if !validDigest(request.WorkflowDigest) {
		return fmt.Errorf("%w: invalid workflow digest", ErrInvalidCheckJob)
	}
	if request.WorkflowOID != "" && !validObjectID(request.WorkflowOID) {
		return fmt.Errorf("%w: invalid workflow object", ErrInvalidCheckJob)
	}
	if len(request.Checks) == 0 || len(request.Checks) > MaximumCheckDefinitions {
		return fmt.Errorf("%w: invalid check count", ErrInvalidCheckJob)
	}
	for _, check := range request.Checks {
		if !validText(check.Name, MaximumCheckNameBytes) || !validText(check.Command, MaximumCheckCommandBytes) {
			return fmt.Errorf("%w: invalid check definition", ErrInvalidCheckJob)
		}
	}
	if request.TimeoutMS < 0 || request.OutputLimitBytes < 0 {
		return fmt.Errorf("%w: negative limits", ErrInvalidCheckJob)
	}
	if request.RerunRoot != "" && !validAttemptID(request.RerunRoot) {
		return fmt.Errorf("%w: invalid rerun root", ErrInvalidCheckJob)
	}
	if request.RerunGeneration < 0 {
		return fmt.Errorf("%w: invalid rerun generation", ErrInvalidCheckJob)
	}
	return validateActionsJobFacts(CheckJob{
		RunID: request.RunID, JobKey: request.JobKey, MatrixIndex: request.MatrixIndex, PlanDigest: request.PlanDigest,
		Tolerated: request.Tolerated, ConcurrencyGroup: request.ConcurrencyGroup, MaxParallel: request.MaxParallel,
	})
}

// ClaimCheckJob leases an external-runner job under a live repository-scoped
// runner credential. The credential role, not a caller-supplied mode, selects
// the only executor this boundary can claim.
func (s *Store) ClaimCheckJob(ctx context.Context, repositoryID, credentialID string, now time.Time, features ...string) (CheckJob, bool, error) {
	if repositoryID == "" || !validAttemptID(credentialID) || now.IsZero() {
		return CheckJob{}, false, fmt.Errorf("%w: invalid claim", ErrInvalidCheckJob)
	}
	return s.claimCheckJob(ctx, repositoryID, credentialID, RunnerRoleExternal, now, slices.Contains(features, RunnerFeatureWorkflowsV1))
}

// ClaimLocalCheckJob is the in-process claim boundary used only by serve for
// host and local-container policies. Its authority is the current local epoch
// and policy generation, neither of which is supplied by an HTTP caller.
func (s *Store) ClaimLocalCheckJob(ctx context.Context, repositoryID string, now time.Time) (CheckJob, bool, error) {
	if repositoryID == "" || now.IsZero() {
		return CheckJob{}, false, fmt.Errorf("%w: invalid local claim", ErrInvalidCheckJob)
	}
	return s.claimCheckJob(ctx, repositoryID, "", RunnerRoleServer, now, true)
}

func (s *Store) claimCheckJob(ctx context.Context, repositoryID, credentialID, role string, now time.Time, workflows bool) (CheckJob, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, false, err
	}
	defer tx.Rollback()
	var credential RunnerCredential
	if role == RunnerRoleExternal {
		var exists bool
		credential, exists, err = liveRunnerCredentialTx(ctx, tx, repositoryID, credentialID)
		if err != nil {
			return CheckJob{}, false, err
		}
		if !exists || credential.Role != RunnerRoleExternal {
			return CheckJob{}, false, ErrCheckRunnerCredential
		}
	}
	policy, exists, err := readCheckPolicyTx(ctx, tx, repositoryID)
	if err != nil {
		return CheckJob{}, false, err
	}
	if !exists {
		return CheckJob{}, false, nil
	}
	switch role {
	case RunnerRoleExternal:
		if policy.Executor != CheckExecutorExternalRunner {
			return CheckJob{}, false, nil
		}
	case RunnerRoleServer:
		if policy.Executor == CheckExecutorExternalRunner {
			return CheckJob{}, false, nil
		}
		credential = RunnerCredential{ID: policy.AuthorityEpoch, RepositoryID: repositoryID, Generation: policy.Version, Role: RunnerRoleServer}
	default:
		return CheckJob{}, false, ErrCheckRunnerCredential
	}
	if err := interruptStalePendingCheckJobsTx(ctx, tx, policy, now); err != nil {
		return CheckJob{}, false, err
	}
	if !checkConsentCurrent(policy) || policy.Execution.Legacy {
		if err := tx.Commit(); err != nil {
			return CheckJob{}, false, err
		}
		return CheckJob{}, false, nil
	}
	active, err := countActiveCheckJobsTx(ctx, tx, repositoryID)
	if err != nil {
		return CheckJob{}, false, err
	}
	if active >= policy.MaxActiveJobs {
		if err := tx.Commit(); err != nil {
			return CheckJob{}, false, err
		}
		return CheckJob{}, false, nil
	}
	if !workflows {
		if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET summary=? WHERE repository_id=? AND status='pending' AND run_id!='' AND summary=''`, runnerOldNote, repositoryID); err != nil {
			return CheckJob{}, false, err
		}
	}
	job, exists, err := oldestPendingCheckJobTx(ctx, tx, repositoryID, workflows)
	if err != nil {
		return CheckJob{}, false, err
	}
	if !exists {
		if err := tx.Commit(); err != nil {
			return CheckJob{}, false, err
		}
		return CheckJob{}, false, nil
	}
	if !checkJobAuthorityCurrent(job, policy) || job.Execution.Legacy {
		return CheckJob{}, false, errors.New("stale check job remained claimable")
	}
	leaseID, err := RandomID()
	if err != nil {
		return CheckJob{}, false, err
	}
	claimedAt := now.UTC()
	leaseExpiresAt := claimedAt.Add(time.Duration(policy.MaxLeaseMS) * time.Millisecond)
	result, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='claimed',claimed_at=?,lease_id=?,lease_expires_at=?,credential_id=?,credential_generation=?,credential_role=?
		WHERE id=? AND status='pending'`, claimedAt.UnixNano(), leaseID, leaseExpiresAt.UnixNano(), credential.ID, credential.Generation, credential.Role, job.ID)
	if err != nil {
		return CheckJob{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CheckJob{}, false, err
	}
	if affected != 1 {
		return CheckJob{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return CheckJob{}, false, err
	}
	job.Status = CheckJobClaimed
	job.ClaimedAt = &claimedAt
	job.LeaseID = leaseID
	job.LeaseExpiresAt = &leaseExpiresAt
	job.CredentialID = credential.ID
	job.CredentialGeneration = credential.Generation
	job.CredentialRole = credential.Role
	return job, true, nil
}

// CheckJobNotStartedError reports a claimed job that passed the start's
// authority checks but could not be started together with its commands and
// its attempt. The start wrote nothing: the job stays claimed under its lease,
// and the caller records it as not run. Cause is ErrCheckJobCommandsUnavailable
// when the commands could not be read, and otherwise the failed write.
type CheckJobNotStartedError struct {
	Cause error
}

func (err *CheckJobNotStartedError) Error() string { return err.Cause.Error() }
func (err *CheckJobNotStartedError) Unwrap() error { return err.Cause }

// StartCheckJob records a one-shot execution grant and its first reported
// facts, and registers the job's attempt under request.AttemptID. It returns
// the started job and the attempt, which carries the commands the job
// captured at admission. A repeated call returns ErrCheckJobStartReplay;
// callers can read the immutable historical facts through CheckJob without
// receiving a new grant.
//
// A job is started only together with its commands and its attempt, in one
// transaction after the authority checks, so a started job always has an
// attempt to complete. When a step after the authority checks fails, nothing
// is written and the error is a CheckJobNotStartedError. A failed commit is
// returned as it is, since the start may or may not have been written.
func (s *Store) StartCheckJob(ctx context.Context, request CheckJobStart, now time.Time) (CheckJob, CheckAttempt, error) {
	if request.RepositoryID == "" || !validAttemptID(request.JobID) || !validAttemptID(request.LeaseID) || !validAttemptID(request.CredentialID) || request.CredentialGeneration <= 0 || !validAttemptID(request.AttemptID) || now.IsZero() {
		return CheckJob{}, CheckAttempt{}, fmt.Errorf("%w: invalid start", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, CheckAttempt{}, err
	}
	defer tx.Rollback()
	job, exists, err := readCheckJobTx(ctx, tx, request.RepositoryID, request.JobID)
	if err != nil {
		return CheckJob{}, CheckAttempt{}, err
	}
	if !exists {
		return CheckJob{}, CheckAttempt{}, ErrCheckJobNotFound
	}
	if job.LeaseID != request.LeaseID || job.CredentialID != request.CredentialID || job.CredentialGeneration != request.CredentialGeneration {
		return CheckJob{}, CheckAttempt{}, ErrCheckJobLease
	}
	protection := request.Protection
	if protection == "" {
		protection = ProtectionUnknown
	}
	if !validProtection(protection) || !validProtectionForExecutor(job.Executor, protection) {
		return CheckJob{}, CheckAttempt{}, fmt.Errorf("%w: protection %q does not match executor %q", ErrInvalidCheckJob, protection, job.Executor)
	}
	if job.Status == CheckJobStarted || (job.Status == CheckJobAmbiguous && job.StartedAt != nil) {
		return CheckJob{}, CheckAttempt{}, ErrCheckJobStartReplay
	}
	if job.Status != CheckJobClaimed {
		return CheckJob{}, CheckAttempt{}, ErrCheckJobState
	}
	startedAt := now.UTC()
	if job.LeaseExpiresAt == nil || !startedAt.Before(*job.LeaseExpiresAt) {
		leaseLostAt := startedAt
		if job.LeaseExpiresAt != nil {
			leaseLostAt = *job.LeaseExpiresAt
		}
		if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='ambiguous',lease_lost_at=? WHERE id=? AND status='claimed'`, leaseLostAt.UnixNano(), job.ID); err != nil {
			return CheckJob{}, CheckAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return CheckJob{}, CheckAttempt{}, err
		}
		return CheckJob{}, CheckAttempt{}, ErrCheckJobLease
	}
	policy, policyExists, err := readCheckPolicyTx(ctx, tx, request.RepositoryID)
	if err != nil {
		return CheckJob{}, CheckAttempt{}, err
	}
	credentialCurrent, err := checkJobCredentialCurrentTx(ctx, tx, job)
	if err != nil {
		return CheckJob{}, CheckAttempt{}, err
	}
	if !policyExists || !checkJobAuthorityCurrent(job, policy) || !credentialCurrent {
		if err := interruptCheckJobTx(ctx, tx, job, startedAt, "Execution authority changed before start."); err != nil {
			return CheckJob{}, CheckAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return CheckJob{}, CheckAttempt{}, err
		}
		if !credentialCurrent {
			return CheckJob{}, CheckAttempt{}, ErrCheckRunnerCredential
		}
		return CheckJob{}, CheckAttempt{}, ErrCheckConsentRequired
	}
	attempt, err := s.startCheckJobTx(ctx, tx, job, request.AttemptID, protection, startedAt)
	if err != nil {
		return CheckJob{}, CheckAttempt{}, &CheckJobNotStartedError{Cause: err}
	}
	if err := tx.Commit(); err != nil {
		return CheckJob{}, CheckAttempt{}, err
	}
	job.Status = CheckJobStarted
	job.StartedAt = &startedAt
	job.Protection = protection
	job.AttemptID = attempt.ID
	return job, attempt, nil
}

// startCheckJobTx marks the claimed job started and registers its attempt
// with the commands the job captured at admission.
func (s *Store) startCheckJobTx(ctx context.Context, tx *sql.Tx, job CheckJob, attemptID, protection string, startedAt time.Time) (CheckAttempt, error) {
	configuration, exists, err := readCheckConfigurationTx(ctx, tx, job.RepositoryID, job.ConfigurationVersion)
	if err == nil && !exists {
		err = ErrCheckConfigurationMissing
	}
	if err != nil {
		return CheckAttempt{}, fmt.Errorf("%w: %w", ErrCheckJobCommandsUnavailable, err)
	}
	// The job was read as claimed in this transaction. The registration
	// requires it to be started, so an update that changed nothing fails there.
	if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='started',started_at=?,protection=? WHERE id=? AND status='claimed'`,
		startedAt.UnixNano(), protection, job.ID); err != nil {
		return CheckAttempt{}, fmt.Errorf("the job's start could not be recorded: %w", err)
	}
	attempt, err := s.registerCheckAttemptTx(ctx, tx, CheckAttempt{
		ID: attemptID, TaskID: job.TaskID, RepositoryID: job.RepositoryID, RevisionOID: job.SourceOID,
		WorktreeState: WorktreeClean, JobID: job.ID, Checks: configuration.Checks,
		StartedAt: startedAt, CreatedAt: startedAt, CredentialID: job.CredentialID,
	})
	if err != nil {
		return CheckAttempt{}, fmt.Errorf("the job's attempt could not be registered: %w", err)
	}
	return attempt, nil
}

func checkJobCredentialCurrentTx(ctx context.Context, queryer querier, job CheckJob) (bool, error) {
	switch job.CredentialRole {
	case RunnerRoleServer:
		policy, exists, err := readCheckPolicyTx(ctx, queryer, job.RepositoryID)
		if err != nil || !exists {
			return false, err
		}
		return job.CredentialID == policy.AuthorityEpoch && job.CredentialGeneration == job.PolicyVersion, nil
	case RunnerRoleExternal:
		credential, exists, err := liveRunnerCredentialTx(ctx, queryer, job.RepositoryID, job.CredentialID)
		if err != nil || !exists {
			return false, err
		}
		return credential.Role == RunnerRoleExternal && credential.Generation == job.CredentialGeneration, nil
	default:
		return false, nil
	}
}

// RenewCheckJobLease extends an unexpired claim under its original immutable
// owner. It never changes the claim identity or revives expired work.
func (s *Store) RenewCheckJobLease(ctx context.Context, authority CheckJobCompletionAuthority, now time.Time) (CheckJob, error) {
	if !validAttemptID(authority.JobID) || !validAttemptID(authority.LeaseID) || !validAttemptID(authority.CredentialID) || authority.CredentialGeneration <= 0 || now.IsZero() {
		return CheckJob{}, fmt.Errorf("%w: invalid lease renewal", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, err
	}
	defer tx.Rollback()
	job, exists, err := readCheckJobByIDTx(ctx, tx, authority.JobID)
	if err != nil {
		return CheckJob{}, err
	}
	if !exists {
		return CheckJob{}, ErrCheckJobNotFound
	}
	if job.LeaseID != authority.LeaseID || job.CredentialID != authority.CredentialID || job.CredentialGeneration != authority.CredentialGeneration {
		return CheckJob{}, ErrCheckJobLease
	}
	policy, policyExists, err := readCheckPolicyTx(ctx, tx, job.RepositoryID)
	if err != nil {
		return CheckJob{}, err
	}
	credentialCurrent, err := checkJobCredentialCurrentTx(ctx, tx, job)
	if err != nil {
		return CheckJob{}, err
	}
	when := now.UTC()
	if job.LeaseExpiresAt == nil || !when.Before(*job.LeaseExpiresAt) {
		if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='ambiguous',lease_lost_at=lease_expires_at WHERE id=? AND status IN ('claimed','started')`, job.ID); err != nil {
			return CheckJob{}, err
		}
		if err := tx.Commit(); err != nil {
			return CheckJob{}, err
		}
		return CheckJob{}, ErrCheckJobLease
	}
	if !credentialCurrent {
		return CheckJob{}, ErrCheckRunnerCredential
	}
	if !policyExists || !checkJobAuthorityCurrent(job, policy) {
		return CheckJob{}, ErrCheckConsentRequired
	}
	if job.Status != CheckJobClaimed && job.Status != CheckJobStarted {
		return CheckJob{}, ErrCheckJobState
	}
	expires := when.Add(time.Duration(policy.MaxLeaseMS) * time.Millisecond)
	result, err := tx.ExecContext(ctx, `UPDATE check_jobs SET lease_expires_at=? WHERE id=? AND status=? AND lease_id=?`, expires.UnixNano(), job.ID, job.Status, job.LeaseID)
	if err != nil {
		return CheckJob{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return CheckJob{}, ErrCheckJobState
	}
	if err := tx.Commit(); err != nil {
		return CheckJob{}, err
	}
	job.LeaseExpiresAt = &expires
	return job, nil
}

// AuthorizeCheckJobSource confines source transport to one live external
// claim. Host and container jobs never cross this HTTP authority boundary.
func (s *Store) AuthorizeCheckJobSource(ctx context.Context, repositoryID, jobID, leaseID, credentialID string, credentialGeneration int64, now time.Time) (CheckJob, error) {
	job, exists, err := s.CheckJob(ctx, repositoryID, jobID)
	if err != nil {
		return CheckJob{}, err
	}
	if !exists {
		return CheckJob{}, ErrCheckJobNotFound
	}
	if job.CredentialRole != RunnerRoleExternal || job.Executor != CheckExecutorExternalRunner || job.LeaseID != leaseID ||
		job.CredentialID != credentialID || job.CredentialGeneration != credentialGeneration {
		return CheckJob{}, ErrCheckJobLease
	}
	if job.Status != CheckJobClaimed && job.Status != CheckJobStarted {
		return CheckJob{}, ErrCheckJobState
	}
	if job.LeaseExpiresAt == nil || !now.UTC().Before(*job.LeaseExpiresAt) {
		return CheckJob{}, ErrCheckJobLease
	}
	policy, policyExists, err := s.CheckPolicy(ctx, repositoryID)
	if err != nil {
		return CheckJob{}, err
	}
	if !policyExists || !checkJobAuthorityCurrent(job, policy) {
		return CheckJob{}, ErrCheckConsentRequired
	}
	return job, nil
}

// FailCheckJobBeforeStart records a truthful terminal outcome when source or
// runtime preflight failed before the one-shot execution grant.
func (s *Store) FailCheckJobBeforeStart(ctx context.Context, authority CheckJobCompletionAuthority, status, summary string, now time.Time) (CheckJob, error) {
	if status != CheckJobUnavailable && status != CheckJobError && status != CheckJobInterrupted {
		return CheckJob{}, fmt.Errorf("%w: invalid pre-execution status", ErrInvalidCheckJob)
	}
	if !validText(summary, 500) || now.IsZero() {
		return CheckJob{}, fmt.Errorf("%w: invalid pre-execution outcome", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, err
	}
	defer tx.Rollback()
	job, exists, err := readCheckJobByIDTx(ctx, tx, authority.JobID)
	if err != nil {
		return CheckJob{}, err
	}
	if !exists {
		return CheckJob{}, ErrCheckJobNotFound
	}
	if job.Status != CheckJobClaimed || job.LeaseID != authority.LeaseID || job.CredentialID != authority.CredentialID || job.CredentialGeneration != authority.CredentialGeneration {
		return CheckJob{}, ErrCheckJobLease
	}
	credentialCurrent, err := checkJobCredentialCurrentTx(ctx, tx, job)
	if err != nil {
		return CheckJob{}, err
	}
	if !credentialCurrent {
		return CheckJob{}, ErrCheckRunnerCredential
	}
	finished := now.UTC()
	result, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status=?,finished_at=?,summary=? WHERE id=? AND status='claimed' AND lease_id=?`, status, finished.UnixNano(), summary, job.ID, job.LeaseID)
	if err != nil {
		return CheckJob{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return CheckJob{}, ErrCheckJobState
	}
	if err := tx.Commit(); err != nil {
		return CheckJob{}, err
	}
	job.Status = status
	job.FinishedAt = &finished
	job.Summary = summary
	return job, nil
}

// CancelCheckJob terminalizes pending and claimed work before execution.
// Started or ambiguous work keeps its evidence and records only cancel intent
// until a cancelled completion arrives. A finished job is returned unchanged:
// it is never reversed, and no cancel intent is recorded on it.
func (s *Store) CancelCheckJob(ctx context.Context, repositoryID, jobID string, now time.Time) (CheckJob, error) {
	if repositoryID == "" || !validAttemptID(jobID) || now.IsZero() {
		return CheckJob{}, fmt.Errorf("%w: invalid cancellation", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CheckJob{}, err
	}
	defer tx.Rollback()
	job, exists, err := readCheckJobTx(ctx, tx, repositoryID, jobID)
	if err != nil {
		return CheckJob{}, err
	}
	if !exists {
		return CheckJob{}, ErrCheckJobNotFound
	}
	if job.Status == CheckJobCancelled {
		if err := tx.Commit(); err != nil {
			return CheckJob{}, err
		}
		return job, nil
	}
	cancelledAt := now.UTC()
	if job.Status == CheckJobPending || job.Status == CheckJobClaimed {
		result, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='cancelled',cancel_requested_at=?,finished_at=? WHERE id=? AND status=?`,
			cancelledAt.UnixNano(), cancelledAt.UnixNano(), job.ID, job.Status)
		if err != nil {
			return CheckJob{}, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return CheckJob{}, err
		}
		if affected != 1 {
			return CheckJob{}, ErrCheckJobState
		}
		job.Status = CheckJobCancelled
		job.CancelRequestedAt = &cancelledAt
		job.FinishedAt = &cancelledAt
	} else if (job.Status == CheckJobStarted || job.Status == CheckJobAmbiguous) && job.CancelRequestedAt == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET cancel_requested_at=? WHERE id=? AND status=?`, cancelledAt.UnixNano(), job.ID, job.Status); err != nil {
			return CheckJob{}, err
		}
		job.CancelRequestedAt = &cancelledAt
	}
	if err := tx.Commit(); err != nil {
		return CheckJob{}, err
	}
	return job, nil
}

// ReconcileCheckJobRestart closes claims owned by a process that no longer
// exists. Unstarted claims are interrupted; started work is ambiguous because
// cleanup and execution cannot be proved after restart.
func (s *Store) ReconcileCheckJobRestart(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("%w: missing restart time", ErrInvalidCheckJob)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	when := now.UTC().UnixNano()
	claimed, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='interrupted',lease_id='',lease_expires_at=NULL,finished_at=?,interrupted_at=?,summary='The server restarted before execution began.' WHERE status='claimed'`, when, when)
	if err != nil {
		return 0, err
	}
	started, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='ambiguous',lease_lost_at=?,summary='The server restarted while execution cleanup was uncertain.' WHERE status='started'`, when)
	if err != nil {
		return 0, err
	}
	claimedCount, err := claimed.RowsAffected()
	if err != nil {
		return 0, err
	}
	startedCount, err := started.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(claimedCount + startedCount), nil
}

// ExpireCheckJobLeases marks every expired unfinished lease ambiguous. A job
// is never returned to pending, because the runner may already have executed.
func (s *Store) ExpireCheckJobLeases(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("%w: missing time", ErrInvalidCheckJob)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE check_jobs SET status='ambiguous',lease_lost_at=lease_expires_at
		WHERE status IN ('claimed','started') AND lease_expires_at IS NOT NULL AND lease_expires_at<=?`, now.UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// CheckJob reads one job.
func (s *Store) CheckJob(ctx context.Context, repositoryID, id string) (CheckJob, bool, error) {
	return readCheckJobTx(ctx, s.db, repositoryID, id)
}

// LatestCheckJobs returns a bounded newest-first owner view.
func (s *Store) LatestCheckJobs(ctx context.Context, repositoryID string, limit int) ([]CheckJob, error) {
	if repositoryID == "" || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid job list bound", ErrInvalidCheckJob)
	}
	rows, err := s.db.QueryContext(ctx, checkJobSelect+` WHERE repository_id=? ORDER BY admitted_at DESC,id DESC LIMIT ?`, repositoryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CheckJob
	for rows.Next() {
		job, err := scanCheckJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// CheckJobs lists every job of one repository in admission order.
func (s *Store) CheckJobs(ctx context.Context, repositoryID string) ([]CheckJob, error) {
	rows, err := s.db.QueryContext(ctx, checkJobSelect+` WHERE repository_id=? ORDER BY admitted_at,id`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CheckJob
	for rows.Next() {
		job, err := scanCheckJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func readCheckJobByIDTx(ctx context.Context, queryer querier, id string) (CheckJob, bool, error) {
	job, err := scanCheckJob(queryer.QueryRowContext(ctx, checkJobSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CheckJob{}, false, nil
	}
	return job, err == nil, err
}

func readCheckJobTx(ctx context.Context, queryer querier, repositoryID, id string) (CheckJob, bool, error) {
	job, err := scanCheckJob(queryer.QueryRowContext(ctx, checkJobSelect+` WHERE repository_id=? AND id=?`, repositoryID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CheckJob{}, false, nil
	}
	return job, err == nil, err
}

func readCheckJobByDedupTx(ctx context.Context, queryer querier, repositoryID, digest string) (CheckJob, bool, error) {
	job, err := scanCheckJob(queryer.QueryRowContext(ctx, checkJobSelect+` WHERE repository_id=? AND dedup_digest=?`, repositoryID, digest))
	if errors.Is(err, sql.ErrNoRows) {
		return CheckJob{}, false, nil
	}
	return job, err == nil, err
}

func oldestPendingCheckJobTx(ctx context.Context, queryer querier, repositoryID string, workflows bool) (CheckJob, bool, error) {
	job, err := scanCheckJob(queryer.QueryRowContext(ctx, checkJobSelect+` WHERE repository_id=? AND status='pending'
		AND (? OR run_id='') ORDER BY admitted_at,id LIMIT 1`, repositoryID, workflows))
	if errors.Is(err, sql.ErrNoRows) {
		return CheckJob{}, false, nil
	}
	return job, err == nil, err
}

func interruptStalePendingCheckJobsTx(ctx context.Context, tx *sql.Tx, policy CheckPolicy, now time.Time) error {
	rows, err := tx.QueryContext(ctx, checkJobSelect+` WHERE repository_id=? AND status IN ('pending','waiting','claimed') ORDER BY admitted_at,id`, policy.RepositoryID)
	if err != nil {
		return err
	}
	var stale []CheckJob
	for rows.Next() {
		job, err := scanCheckJob(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if !checkJobAuthorityCurrent(job, policy) {
			stale = append(stale, job)
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	for _, job := range stale {
		if err := interruptCheckJobTx(ctx, tx, job, now, "Execution authority changed before start."); err != nil {
			return err
		}
	}
	return nil
}

func interruptCheckJobTx(ctx context.Context, tx *sql.Tx, job CheckJob, now time.Time, summary string) error {
	when := now.UTC()
	result, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status='interrupted',lease_id='',lease_expires_at=NULL,
		finished_at=?,interrupted_at=?,summary=? WHERE id=? AND status=?`,
		when.UnixNano(), when.UnixNano(), summary, job.ID, job.Status)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrCheckJobState
	}
	return nil
}

func readUnfinishedRerunTx(ctx context.Context, queryer querier, repositoryID, root string) (CheckJob, bool, error) {
	job, err := scanCheckJob(queryer.QueryRowContext(ctx, checkJobSelect+
		` WHERE repository_id=? AND rerun_root=? AND status IN ('pending','claimed','started') ORDER BY rerun_generation DESC,id LIMIT 1`, repositoryID, root))
	if errors.Is(err, sql.ErrNoRows) {
		return CheckJob{}, false, nil
	}
	return job, err == nil, err
}

func countActiveCheckJobsTx(ctx context.Context, queryer querier, repositoryID string) (int, error) {
	var count int
	err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_jobs WHERE repository_id=? AND status IN ('claimed','started')`, repositoryID).Scan(&count)
	return count, err
}

func countUnfinishedCheckJobsTx(ctx context.Context, queryer querier, repositoryID string) (int, error) {
	var count int
	err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM check_jobs WHERE repository_id=? AND status IN ('pending','waiting','claimed','started')`, repositoryID).Scan(&count)
	return count, err
}

const checkJobSelect = `SELECT id,repository_id,task_id,trigger_kind,event_key,source_oid,base_oid,pull_request_number,trigger_ref,
	workflow_path,workflow_oid,workflow_digest,configuration_version,executor,policy_version,consent_version,limits_json,execution_json,
	dedup_digest,rerun_root,rerun_generation,status,attempt_id,lease_id,lease_expires_at,credential_id,credential_generation,credential_role,
	protection,admitted_at,claimed_at,started_at,finished_at,lease_lost_at,cancel_requested_at,interrupted_at,summary,
	run_id,job_key,matrix_index,plan_digest,tolerated,concurrency_group,max_parallel FROM check_jobs`

func scanCheckJob(scanner rowScanner) (CheckJob, error) {
	var job CheckJob
	var limitsJSON, executionJSON string
	var admittedAt int64
	var tolerated int
	var leaseExpiresAt, claimedAt, startedAt, finishedAt, leaseLostAt, cancelRequestedAt, interruptedAt sql.NullInt64
	if err := scanner.Scan(&job.ID, &job.RepositoryID, &job.TaskID, &job.Trigger, &job.EventKey, &job.SourceOID, &job.BaseOID,
		&job.PullRequestNumber, &job.TriggerRef, &job.WorkflowPath, &job.WorkflowOID, &job.WorkflowDigest,
		&job.ConfigurationVersion, &job.Executor, &job.PolicyVersion, &job.ConsentVersion, &limitsJSON, &executionJSON,
		&job.DedupDigest, &job.RerunRoot, &job.RerunGeneration, &job.Status, &job.AttemptID, &job.LeaseID,
		&leaseExpiresAt, &job.CredentialID, &job.CredentialGeneration, &job.CredentialRole, &job.Protection, &admittedAt,
		&claimedAt, &startedAt, &finishedAt, &leaseLostAt, &cancelRequestedAt, &interruptedAt, &job.Summary,
		&job.RunID, &job.JobKey, &job.MatrixIndex, &job.PlanDigest, &tolerated, &job.ConcurrencyGroup, &job.MaxParallel); err != nil {
		return CheckJob{}, err
	}
	if err := json.Unmarshal([]byte(limitsJSON), &job.Limits); err != nil {
		return CheckJob{}, err
	}
	if err := json.Unmarshal([]byte(executionJSON), &job.Execution); err != nil {
		return CheckJob{}, err
	}
	job.Tolerated = tolerated != 0
	job.AdmittedAt = unixNanoTime(admittedAt)
	job.LeaseExpiresAt = nullableNanoTime(leaseExpiresAt)
	job.ClaimedAt = nullableNanoTime(claimedAt)
	job.StartedAt = nullableNanoTime(startedAt)
	job.FinishedAt = nullableNanoTime(finishedAt)
	job.LeaseLostAt = nullableNanoTime(leaseLostAt)
	job.CancelRequestedAt = nullableNanoTime(cancelRequestedAt)
	job.InterruptedAt = nullableNanoTime(interruptedAt)
	return job, nil
}

func nullableNanoTime(value sql.NullInt64) *time.Time {
	if !value.Valid || value.Int64 == 0 {
		return nil
	}
	result := unixNanoTime(value.Int64)
	return &result
}

func terminalCheckJob(status string) bool {
	switch status {
	case CheckJobPending, CheckJobWaiting, CheckJobClaimed, CheckJobStarted:
		return false
	default:
		return true
	}
}

func validCheckJobStatus(status string) bool {
	switch status {
	case CheckJobPending, CheckJobClaimed, CheckJobStarted, CheckJobPassed, CheckJobFailed,
		CheckJobError, CheckJobCancelled, CheckJobIncomplete, CheckJobUnavailable, CheckJobAmbiguous, CheckJobInterrupted, CheckJobWaiting, CheckJobSkipped:
		return true
	default:
		return false
	}
}

// applyJobOrigin validates the immutable evidence owned by a job, then derives
// its server-owned origin fields before digest computation. A terminal job is
// accepted only for an exact registration replay of its bound attempt.
func applyJobOrigin(attempt *CheckAttempt, job CheckJob, configuration CheckConfiguration, replay bool) error {
	if job.RepositoryID != attempt.RepositoryID {
		return ErrCheckJobNotFound
	}
	if replay {
		if job.AttemptID != attempt.ID {
			return ErrCheckJobAttemptBound
		}
	} else {
		switch job.Status {
		case CheckJobStarted, CheckJobAmbiguous:
		default:
			return ErrCheckJobState
		}
	}
	if job.StartedAt == nil || job.CredentialID == "" {
		return ErrCheckJobCredential
	}
	if job.AttemptID != "" && job.AttemptID != attempt.ID {
		return ErrCheckJobAttemptBound
	}
	if attempt.TaskID == "" {
		attempt.TaskID = job.TaskID
	} else if attempt.TaskID != job.TaskID {
		return ErrCheckJobFacts
	}
	if attempt.RevisionOID != job.SourceOID || (attempt.ConfigurationVersion != 0 && attempt.ConfigurationVersion != job.ConfigurationVersion) ||
		!slices.Equal(attempt.Checks, configuration.Checks) || attempt.CycleID != "" {
		return ErrCheckJobFacts
	}
	if attempt.TimeoutMS != 0 && attempt.TimeoutMS != job.Limits.TimeoutMS {
		return ErrCheckJobFacts
	}
	if attempt.OutputLimitBytes != 0 && attempt.OutputLimitBytes != job.Limits.OutputLimitBytes {
		return ErrCheckJobFacts
	}
	if attempt.CredentialID != "" && attempt.CredentialID != job.CredentialID {
		return ErrCheckJobCredential
	}
	if !attempt.StartedAt.IsZero() && !attempt.StartedAt.Equal(*job.StartedAt) {
		return ErrCheckJobFacts
	}
	attempt.ConfigurationVersion = job.ConfigurationVersion
	attempt.TimeoutMS = job.Limits.TimeoutMS
	attempt.OutputLimitBytes = job.Limits.OutputLimitBytes
	attempt.StartedAt = *job.StartedAt
	attempt.CredentialID = job.CredentialID
	attempt.Protection = job.Protection
	attempt.ExecutionScope = executionScopeForExecutor(job.Executor)
	return nil
}

func authorizeCheckJobCompletionTx(ctx context.Context, tx *sql.Tx, attempt CheckAttempt, authority CheckJobCompletionAuthority) error {
	if authority.JobID != attempt.JobID || !validAttemptID(authority.JobID) || !validAttemptID(authority.LeaseID) ||
		!validAttemptID(authority.CredentialID) || authority.CredentialGeneration <= 0 {
		return ErrCheckJobCredential
	}
	job, exists, err := readCheckJobTx(ctx, tx, attempt.RepositoryID, attempt.JobID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrCheckJobNotFound
	}
	if job.AttemptID != attempt.ID {
		return ErrCheckJobAttemptBound
	}
	if job.LeaseID != authority.LeaseID || job.CredentialID != authority.CredentialID || job.CredentialGeneration != authority.CredentialGeneration {
		return ErrCheckJobLease
	}
	if attempt.Status != AttemptPending {
		return nil
	}
	if job.Status != CheckJobStarted && job.Status != CheckJobAmbiguous {
		return ErrCheckJobState
	}
	credentialCurrent, err := checkJobCredentialCurrentTx(ctx, tx, job)
	if err != nil {
		return err
	}
	if !credentialCurrent {
		return ErrCheckRunnerCredential
	}
	return nil
}

// finalizeCheckJobTx mirrors one authority-checked completion onto its linked
// job in the same transaction. Interrupted history is immutable. The job is
// OwnGit's record, so finished is when OwnGit received the completion, never
// a time the runner reported.
func finalizeCheckJobTx(ctx context.Context, tx *sql.Tx, attempt CheckAttempt, status string, finished time.Time, summary string, cancelled bool) error {
	job, exists, err := readCheckJobTx(ctx, tx, attempt.RepositoryID, attempt.JobID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrCheckJobNotFound
	}
	if job.AttemptID != attempt.ID {
		return ErrCheckJobAttemptBound
	}
	switch job.Status {
	case CheckJobStarted, CheckJobAmbiguous:
	default:
		return ErrCheckJobState
	}
	cancelRequestedAt := job.CancelRequestedAt
	if cancelled && cancelRequestedAt == nil {
		value := finished.UTC()
		cancelRequestedAt = &value
	}
	if _, err := tx.ExecContext(ctx, `UPDATE check_jobs SET status=?,finished_at=?,summary=?,cancel_requested_at=? WHERE id=?`,
		status, finished.UTC().UnixNano(), summary, nullableNano(cancelRequestedAt), job.ID); err != nil {
		return err
	}
	if job.RunID != "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM actions_job_plans WHERE job_id=?`, job.ID)
	}
	return err
}

func nullableNano(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().UnixNano()
}
