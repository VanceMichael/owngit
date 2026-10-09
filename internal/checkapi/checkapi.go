// Package checkapi defines the JSON wire contract shared by the check helper
// CLI and the OwnGit server. It carries no storage or HTTP behavior so both
// sides can depend on it without a cycle.
package checkapi

import (
	"time"

	"owngit/internal/checkoutput"
	"owngit/internal/state"
)

// MaximumUploadBytes bounds one check evidence upload. The server and the
// helper share the value so a large but valid attempt is not rejected by the
// client before it reaches the server.
const MaximumUploadBytes = 2 << 20

// DefaultLeaseRenewDelay and MinimumLeaseRenewDelay bound how long a lease
// holder waits before extending its claim. A wait longer than the lease itself
// loses work that is still running, and the minimum one keeps a short lease
// from renewing in a busy loop.
const (
	DefaultLeaseRenewDelay = time.Second
	MinimumLeaseRenewDelay = 100 * time.Millisecond
)

// LeaseRenewDelay returns how long to wait, at now, before renewing a lease
// that expires at expiresAt: a third of the remaining time, capped at the
// default. With less than three minimum waits left, that third is too small to
// use and the default would arrive after the lease had ended, so the minimum
// wait is returned instead. The server still decides whether a renewal arrived
// in time. A nil expiry means the lease has no known deadline: the default.
func LeaseRenewDelay(now time.Time, expiresAt *time.Time) time.Duration {
	if expiresAt == nil {
		return DefaultLeaseRenewDelay
	}
	if candidate := expiresAt.Sub(now) / 3; candidate > MinimumLeaseRenewDelay {
		return min(candidate, DefaultLeaseRenewDelay)
	}
	return MinimumLeaseRenewDelay
}

type Gap = checkoutput.Gap
type LogBuffer = checkoutput.LogBuffer

func ClipText(value string, limit int) (string, bool) {
	return checkoutput.ClipText(value, limit)
}

func ClipLog(value string, limit int, gap Gap) (string, bool) {
	return checkoutput.ClipLog(value, limit, gap)
}

func SummaryText(value string, limit int) string {
	return checkoutput.SummaryText(value, limit)
}

type CheckDefinition struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type Task struct {
	ID                        string `json:"id"`
	RepositoryID              string `json:"repository_id"`
	Title                     string `json:"title"`
	Status                    string `json:"status"`
	CorrectionCyclesUsed      int    `json:"correction_cycles_used"`
	CorrectionCyclesRemaining int    `json:"correction_cycles_remaining"`
	// CorrectionCycleLimit is the task-scoped budget, so a client does not
	// maintain its own copy of the value.
	CorrectionCycleLimit int       `json:"correction_cycle_limit"`
	InitialCheckDone     bool      `json:"initial_check_done"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	// LastRegisteredSequence and LastAppliedSequence expose the server-issued
	// order. PendingAttemptID is the newest registration without a completion.
	LastRegisteredSequence int64  `json:"last_registered_sequence"`
	LastAppliedSequence    int64  `json:"last_applied_sequence"`
	PendingAttemptID       string `json:"pending_attempt_id,omitempty"`
	LastAppliedAttemptID   string `json:"last_applied_attempt_id,omitempty"`
}

type Result struct {
	Name          string `json:"name"`
	Command       string `json:"command"`
	Status        string `json:"status"`
	Role          string `json:"role,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	OutputExcerpt string `json:"output_excerpt,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	// CleanupError reports that the owned process group could not be confirmed
	// released. The status is error while ExitCode stays the command code.
	CleanupError string `json:"cleanup_error,omitempty"`
}

type Attempt struct {
	ID                   string    `json:"id"`
	TaskID               string    `json:"task_id"`
	RepositoryID         string    `json:"repository_id"`
	RevisionOID          string    `json:"revision_oid"`
	WorktreeState        string    `json:"worktree_state"`
	ConfigurationVersion int64     `json:"configuration_version"`
	Status               string    `json:"status"`
	ExitCode             *int      `json:"exit_code,omitempty"`
	StartedAt            time.Time `json:"started_at"`
	FinishedAt           time.Time `json:"finished_at,omitempty"`
	DurationMS           int64     `json:"duration_ms"`
	Summary              string    `json:"summary"`
	// Sequence is the server-issued repository-wide order. It is the authority for
	// task progression, not the client finish time.
	Sequence int64 `json:"sequence"`
	// CycleID binds an automatic correction round. Empty means the initial
	// check or a manual rerun.
	CycleID string `json:"cycle_id,omitempty"`
	// JobID links a server-owned automatic job. Empty means the historical
	// helper path, where the helper cannot assert a job origin.
	JobID string `json:"job_id,omitempty"`
	// Protection and ExecutionScope state what was actually established. The
	// helper runs with the user's account and files, so protection is unknown.
	Protection     string `json:"protection"`
	ExecutionScope string `json:"execution_scope"`
	// CredentialID is the server-authenticated helper credential that
	// submitted the attempt.
	CredentialID string `json:"credential_id,omitempty"`
	// TimeoutMS and OutputLimitBytes are the execution limits that applied.
	TimeoutMS        int64      `json:"timeout_ms"`
	OutputLimitBytes int64      `json:"output_limit_bytes"`
	LogID            string     `json:"log_id,omitempty"`
	LogExpiresAt     *time.Time `json:"log_expires_at,omitempty"`
	LogTruncated     bool       `json:"log_truncated"`
	LogError         string     `json:"log_error,omitempty"`
	// CleanupFailed is the aggregate of the per-result cleanup errors, so a
	// renderer that omits result rows still sees the failure.
	CleanupFailed bool     `json:"cleanup_failed,omitempty"`
	Results       []Result `json:"results,omitempty"`
}

// AttemptRegistration is the first half of the two-phase protocol. It is sent
// before execution, so the server can issue a monotonic repository-wide sequence
// and a retransmit stays idempotent.
type AttemptRegistration struct {
	// AttemptID is generated before execution.
	AttemptID     string            `json:"attempt_id"`
	RevisionOID   string            `json:"revision_oid"`
	WorktreeState string            `json:"worktree_state"`
	Checks        []CheckDefinition `json:"checks"`
	CycleID       string            `json:"cycle_id,omitempty"`
	// JobID links a server-owned automatic job. The server derives scope and
	// protection from that job, so the helper payload cannot forge them.
	JobID            string    `json:"job_id,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	TimeoutMS        int64     `json:"timeout_ms"`
	OutputLimitBytes int64     `json:"output_limit_bytes"`
}

// AttemptCompletion is the second half. The server recomputes the aggregate
// status from the results instead of trusting a helper-supplied value.
type AttemptCompletion struct {
	Results       []Result  `json:"results"`
	Cancelled     bool      `json:"cancelled,omitempty"`
	FinishedAt    time.Time `json:"finished_at"`
	WorktreeState string    `json:"worktree_state"`
	Log           string    `json:"log,omitempty"`
	LogTruncated  bool      `json:"log_truncated,omitempty"`
}

// Cycle is one reserved automatic correction round.
type Cycle struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"task_id"`
	Sequence   int64     `json:"sequence"`
	ReservedAt time.Time `json:"reserved_at"`
	AttemptID  string    `json:"attempt_id,omitempty"`
}

// CreateCycleInput reserves one correction round. The identity is generated by
// the caller, so a retransmit returns the same round.
type CreateCycleInput struct {
	CycleID string `json:"cycle_id"`
}

type Configuration struct {
	Version   int64             `json:"version"`
	Checks    []CheckDefinition `json:"checks"`
	CreatedAt time.Time         `json:"created_at"`
}

type Credential struct {
	ID           string `json:"id"`
	RepositoryID string `json:"repository_id"`
	// RepositoryAddress is where the credential's repository answers now,
	// given by the list across repositories.
	RepositoryAddress string     `json:"repository_address,omitempty"`
	Label             string     `json:"label"`
	CreationID        string     `json:"creation_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
}

type CreateTaskInput struct {
	Title string `json:"title,omitempty"`
}

type CreateCredentialInput struct {
	Label string `json:"label,omitempty"`
	// CreationID is generated before the request, so a lost or malformed
	// response can be compensated with a scoped revoke.
	CreationID string `json:"creation_id,omitempty"`
}

type TaskResponse struct {
	OK      bool     `json:"ok"`
	Task    *Task    `json:"task,omitempty"`
	Attempt *Attempt `json:"attempt,omitempty"`
}

type TaskListResponse struct {
	OK    bool    `json:"ok"`
	Tasks []*Task `json:"tasks"`
	// Next continues the list below the last task shown, and is absent on
	// the last page.
	Next string `json:"next,omitempty"`
}

type ConfigurationResponse struct {
	OK            bool           `json:"ok"`
	Configuration *Configuration `json:"configuration,omitempty"`
}

type CredentialResponse struct {
	OK         bool        `json:"ok"`
	Credential *Credential `json:"credential,omitempty"`
	// RepositoryAddress is where the credential's repository answers now:
	// its current name, or its ID.
	RepositoryAddress string `json:"repository_address,omitempty"`
	// Token is returned only when the credential is created. The server keeps
	// only its hash, so a retransmit returns the credential without a token.
	Token string `json:"token,omitempty"`
}

// CycleResponse carries one reserved correction round.
type CycleResponse struct {
	OK    bool   `json:"ok"`
	Task  *Task  `json:"task,omitempty"`
	Cycle *Cycle `json:"cycle,omitempty"`
}

// CycleListResponse lists the reserved correction rounds of one task.
type CycleListResponse struct {
	OK     bool     `json:"ok"`
	Cycles []*Cycle `json:"cycles"`
}

type CredentialListResponse struct {
	OK          bool          `json:"ok"`
	Credentials []*Credential `json:"credentials"`
}

type OKResponse struct {
	OK bool `json:"ok"`
}

// PolicyInput is the owner-selected configured-check policy. Generation,
// consent and authority are always server-owned.
type PolicyInput struct {
	Executor            string                       `json:"executor"`
	AllowedEvents       []string                     `json:"allowed_events"`
	MaxTimeoutMS        int64                        `json:"max_timeout_ms"`
	MaxOutputLimitBytes int64                        `json:"max_output_limit_bytes"`
	QueueLimit          int                          `json:"queue_limit"`
	MaxActiveJobs       int                          `json:"max_active_jobs"`
	MaxLeaseMS          int64                        `json:"max_lease_ms"`
	Execution           state.CheckExecutionSettings `json:"execution"`
}

// SaveAndEnableInput saves a policy and turns checks on for exactly that
// policy in one step.
type SaveAndEnableInput struct {
	Policy PolicyInput `json:"policy"`
	// Expected, when present, is the stored policy the change was reviewed
	// against; version 0 with an empty digest means none existed. The step is
	// refused if the stored policy is another one.
	Expected *ExpectedPolicy `json:"expected,omitempty"`
}

// ExpectedPolicy names one stored policy generation.
type ExpectedPolicy struct {
	Version int64  `json:"version"`
	Digest  string `json:"digest"`
}

type Policy struct {
	RepositoryID        string                       `json:"repository_id"`
	Version             int64                        `json:"version"`
	Digest              string                       `json:"digest"`
	Executor            string                       `json:"executor"`
	AllowedEvents       []string                     `json:"allowed_events"`
	MaxTimeoutMS        int64                        `json:"max_timeout_ms"`
	MaxOutputLimitBytes int64                        `json:"max_output_limit_bytes"`
	QueueLimit          int                          `json:"queue_limit"`
	MaxActiveJobs       int                          `json:"max_active_jobs"`
	MaxLeaseMS          int64                        `json:"max_lease_ms"`
	Execution           state.CheckExecutionSettings `json:"execution"`
	ConsentVersion      int64                        `json:"consent_version"`
	ConsentActive       bool                         `json:"consent_active"`
	RunnerGeneration    int64                        `json:"runner_generation"`
	CreatedAt           time.Time                    `json:"created_at"`
	UpdatedAt           time.Time                    `json:"updated_at"`
}

type RuntimeStatus struct {
	Available         bool   `json:"available"`
	UnavailableCode   string `json:"unavailable_code,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type PolicyResponse struct {
	OK      bool          `json:"ok"`
	Policy  *Policy       `json:"policy,omitempty"`
	Runtime RuntimeStatus `json:"runtime"`
}

type Job struct {
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
	Limits               state.CheckJobLimits         `json:"limits"`
	Execution            state.CheckExecutionSettings `json:"execution"`
	Status               string                       `json:"status"`
	AttemptID            string                       `json:"attempt_id,omitempty"`
	LeaseID              string                       `json:"lease_id,omitempty"`
	LeaseExpiresAt       *time.Time                   `json:"lease_expires_at,omitempty"`
	Protection           string                       `json:"protection"`
	CancelRequested      bool                         `json:"cancel_requested"`
	Summary              string                       `json:"summary,omitempty"`
	AdmittedAt           time.Time                    `json:"admitted_at"`
	StartedAt            *time.Time                   `json:"started_at,omitempty"`
	FinishedAt           *time.Time                   `json:"finished_at,omitempty"`
	Checks               []CheckDefinition            `json:"checks,omitempty"`
	RunID                string                       `json:"run_id,omitempty"`
	JobKey               string                       `json:"job_key,omitempty"`
	MatrixIndex          int                          `json:"matrix_index,omitempty"`
	PlanDigest           string                       `json:"plan_digest,omitempty"`
}

type JobResponse struct {
	OK      bool     `json:"ok"`
	Job     *Job     `json:"job,omitempty"`
	Attempt *Attempt `json:"attempt,omitempty"`
}

type JobListResponse struct {
	OK   bool   `json:"ok"`
	Jobs []*Job `json:"jobs"`
}

type RunnerCredential struct {
	ID           string     `json:"id"`
	RepositoryID string     `json:"repository_id"`
	Label        string     `json:"label"`
	CreationID   string     `json:"creation_id,omitempty"`
	Generation   int64      `json:"generation"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

type RunnerCredentialResponse struct {
	OK         bool              `json:"ok"`
	Credential *RunnerCredential `json:"credential,omitempty"`
	// RepositoryAddress is where the credential's repository answers now:
	// its current name, or its ID.
	RepositoryAddress string `json:"repository_address,omitempty"`
	Token             string `json:"token,omitempty"`
}

type RunnerCredentialListResponse struct {
	OK          bool                `json:"ok"`
	Credentials []*RunnerCredential `json:"credentials"`
}

type RunnerLeaseInput struct {
	LeaseID string `json:"lease_id"`
}

type RunnerClaimInput struct {
	Features []string `json:"features,omitempty"`
}

// MaximumRunnerStartBytes covers secrets at six JSON bytes per input byte.
// A canonical plan string and its selected names use at most three times the
// plan bound (including bidi escaping). The envelope covers captured displays.
// Only the one-shot start answer carries secrets.
const MaximumRunnerStartBytes = state.MaxWorkflowSecrets*state.MaxWorkflowSecretValueBytes*6 + state.MaximumActionsPlanBytes*3 + (1 << 20)

type RunnerStartResponse struct {
	JobResponse
	Actions *RunnerActionsGrant `json:"actions,omitempty"`
}

type RunnerActionsGrant struct {
	// Plan is a string so JSON encoding preserves its exact digested bytes.
	Plan       string            `json:"plan"`
	RunID      string            `json:"run_id"`
	RunNumber  int64             `json:"run_number"`
	RunAttempt int64             `json:"run_attempt"`
	Secrets    map[string]string `json:"secrets"`
}

type RunnerStartInput struct {
	LeaseID   string `json:"lease_id"`
	AttemptID string `json:"attempt_id"`
}

// ClaimedJob names a job and lease that a claim committed to the runner. When
// the server cannot hand the job over, on the claim or on the start, it sends
// ClaimedJob as the details of the error, so the runner can still end the job
// it holds.
type ClaimedJob struct {
	JobID   string `json:"job_id"`
	LeaseID string `json:"lease_id"`
}

type RunnerUnavailableInput struct {
	LeaseID string `json:"lease_id"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

type RunnerCompletionInput struct {
	LeaseID    string            `json:"lease_id"`
	Completion AttemptCompletion `json:"completion"`
}

type SourceEntry struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type SourceManifest struct {
	OK           bool                    `json:"ok"`
	JobID        string                  `json:"job_id"`
	CommitOID    string                  `json:"commit_oid"`
	ObjectFormat string                  `json:"object_format"`
	Limits       state.CheckSourceLimits `json:"limits"`
	Entries      []SourceEntry           `json:"entries"`
}

// LogResponse carries one disposable raw log. The durable attempt record
// stays readable after the log expires.
type LogResponse struct {
	OK        bool       `json:"ok"`
	LogID     string     `json:"log_id"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Truncated bool       `json:"truncated"`
	Content   string     `json:"content"`
}
