// Package checkrun admits and executes repository-configured advisory checks.
package checkrun

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"owngit/internal/checkapi"
	"owngit/internal/checkexec"
	"owngit/internal/checksource"
	"owngit/internal/checkworkflow"
	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
	"owngit/internal/statepath"
)

const (
	RuntimeUnavailableWorkspace = "workspace_unavailable"
	RuntimeUnavailableRecovery  = "restart_reconciliation_unavailable"

	maximumObservedRefs     = 64
	maximumObservedPRs      = 64
	maximumAutomaticLogSize = checkexec.KeptOutputBytes

	// maximumPendingPushes bounds the branch updates one repository retains
	// between passes, like the bounded observation set. A repository that
	// overflows drops its oldest update, named once in the log.
	maximumPendingPushes = 64

	// maximumPendingPushTotal bounds the retained branch updates across all
	// repositories, so many repositories cannot multiply the per-repository
	// bound. An update holds a ref of at most 500 bytes (the observation ref
	// bound), a 40- or 64-character object ID and two string headers, about
	// 600 bytes at most, so the retained set stays under 2.5 MB.
	maximumPendingPushTotal = 4096

	// repositoryBusyWait bounds how long a job waits for its exact source
	// while pushes or other repository writes hold the repository.
	repositoryBusyWait = 10 * time.Minute

	// admissionBusyWait bounds how long one admission drain waits, in total,
	// for repositories that writers hold. The writer is usually the push that
	// reported the events, which releases the repository milliseconds after its
	// handler returns. A repository still held after that keeps its events, in
	// order, for the next push or the retry after admissionRetryWait, so a long
	// write cannot hold back the events of other repositories.
	admissionBusyWait = 5 * time.Second

	// admissionRetryWait is how long a retained push event a repository could
	// not decide waits before the admission goroutine tries it again, at the
	// cadence of the default reconciliation pass.
	admissionRetryWait = 30 * time.Second
)

var ErrRuntimeUnavailable = errors.New("configured check runtime is unavailable")

// errRevisionRejected marks an admission failure that the revision itself
// determines, such as an unparsable or oversized workflow or a branch name the
// job record cannot carry. Retrying the same revision cannot succeed, so the
// reconciler records it as that revision's outcome and continues with the next
// one instead of blocking the rest of the repository.
var errRevisionRejected = errors.New("configured check workflow was rejected")

// PushUpdate is one ref a push updated, as the Git handler reports it. New is
// empty when the push deleted the ref.
type PushUpdate struct {
	Ref string
	New string
}

// pushUpdate is one branch update of an accepted push that no pass has decided
// yet.
type pushUpdate struct {
	ref string
	oid string
}

// RuntimeUnavailableError identifies a check-specific startup boundary that
// must disable automatic and external-runner authority without blocking Git.
type RuntimeUnavailableError struct {
	Code string
	Err  error
}

func (err *RuntimeUnavailableError) Error() string {
	return fmt.Sprintf("%s: %v", err.Code, err.Err)
}

func (err *RuntimeUnavailableError) Unwrap() error { return err.Err }

func (err *RuntimeUnavailableError) Is(target error) bool { return target == ErrRuntimeUnavailable }

// Coordinator owns bounded reconciliation, the in-process host/container
// worker and the admission of push events. External-runner jobs are admitted
// here but claimed only over the separate runner protocol. Admission runs on
// its own goroutine, so a push made while a job runs does not wait for it.
//
// Start requires Store, Repositories and PullRequests, which the serving
// process always sets. The command and the Checks page that forget a
// foreign container build a Coordinator with only Store, because
// ForgetForeignContainer reads nothing else.
type Coordinator struct {
	Store         *state.Store
	Repositories  *repository.Manager
	PullRequests  *pullrequest.Service
	WorkspaceRoot string
	DockerPath    string
	Interval      time.Duration
	Logf          func(string, ...any)
	// OnCheckCompletion signals the auto-merge convergence after a local
	// worker wrote a terminal check attempt. It must not block or run Git.
	OnCheckCompletion func(repositoryID string)

	wake              chan struct{}
	admits            chan struct{}
	cancel            context.CancelFunc
	done              chan struct{}
	admitDone         chan struct{}
	workspace         *checksource.WorkspaceRoot
	pushCursor        map[string]string
	skippedRefs       map[string]map[string]string
	pushChecked       map[string]bool
	pushScans         map[string]pushScan
	pullRequestCursor map[string]int64
	// pendingPushes holds the branch updates of accepted pushes that no
	// attempt has admitted or refused yet, oldest first per repository. The
	// Git handler writes it and the admission goroutine drains it, so mu
	// guards it. It is memory only: a stop drops what no attempt admitted yet.
	pendingPushes map[string][]pushUpdate
	// draining counts the updates the running drain took out of pendingPushes,
	// so the bound across repositories still counts them while they are
	// decided. An update the drain keeps is counted twice until the drain ends,
	// which only refuses an arrival early, never retains more than the bound.
	draining int
	mu       sync.Mutex
}

func (coordinator *Coordinator) Start(parent context.Context) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.cancel != nil {
		return errors.New("configured check coordinator is already running")
	}
	if coordinator.WorkspaceRoot == "" {
		coordinator.WorkspaceRoot = filepath.Join(coordinator.Store.Dir(), statepath.Runtime, "check-jobs")
	}
	if _, err := coordinator.Store.ReconcileCheckJobRestart(parent, time.Now().UTC()); err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return &RuntimeUnavailableError{Code: RuntimeUnavailableRecovery, Err: fmt.Errorf("reconcile configured checks after restart: %w", err)}
	}
	workspace, err := checksource.AcquireWorkspaceRoot(coordinator.WorkspaceRoot)
	if err != nil {
		return &RuntimeUnavailableError{Code: RuntimeUnavailableWorkspace, Err: fmt.Errorf("acquire check workspace root: %w", err)}
	}
	keepWorkspace := false
	defer func() {
		if !keepWorkspace {
			workspace.Close()
		}
	}()
	containerCleanupErr := coordinator.reconcileContainers(parent)
	if containerCleanupErr != nil {
		coordinator.log("configured check startup container cleanup: %v", containerCleanupErr)
	} else {
		removed, more, cleanupErr := workspace.Cleanup(1000)
		if cleanupErr != nil || more {
			coordinator.log("configured check startup workspace cleanup removed=%d more=%v error=%v", removed, more, cleanupErr)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	wake := make(chan struct{}, 1)
	admits := make(chan struct{}, 1)
	done := make(chan struct{})
	admitDone := make(chan struct{})
	coordinator.cancel = cancel
	coordinator.wake = wake
	coordinator.admits = admits
	coordinator.done = done
	coordinator.admitDone = admitDone
	coordinator.workspace = workspace
	if coordinator.pushCursor == nil {
		coordinator.pushCursor = make(map[string]string)
	}
	if coordinator.pullRequestCursor == nil {
		coordinator.pullRequestCursor = make(map[string]int64)
	}
	keepWorkspace = true
	go coordinator.loop(ctx, wake, done)
	go coordinator.admissionLoop(ctx, admits, admitDone)
	wake <- struct{}{}
	admits <- struct{}{}
	return nil
}

func (coordinator *Coordinator) Stop(ctx context.Context) error {
	coordinator.mu.Lock()
	cancel, done, admitDone := coordinator.cancel, coordinator.done, coordinator.admitDone
	coordinator.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	// Both loops must end within the caller's bound. The retained push events
	// are memory only: a stop drops what no attempt admitted yet, and the next
	// start reconciles each branch head as before.
	for _, end := range []<-chan struct{}{done, admitDone} {
		select {
		case <-end:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	coordinator.mu.Lock()
	var workspace *checksource.WorkspaceRoot
	if coordinator.done == done {
		coordinator.cancel = nil
		coordinator.wake = nil
		coordinator.admits = nil
		coordinator.done = nil
		coordinator.admitDone = nil
		coordinator.pendingPushes = nil
		coordinator.draining = 0
		workspace = coordinator.workspace
		coordinator.workspace = nil
	}
	coordinator.mu.Unlock()
	workspace.Close()
	return nil
}

// Wake schedules a bounded reconciliation. repositoryID is accepted by write
// seams for future narrowing; reconciliation remains global so repeated wakes
// coalesce without an unbounded repository queue. A push's own ref updates are
// retained by NotePush instead, which wakes the coordinator the same way.
func (coordinator *Coordinator) Wake(repositoryID string) {
	coordinator.mu.Lock()
	wake := coordinator.wake
	coordinator.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// wakeAdmission asks the admission goroutine to decide the retained push
// events. A wake is coalesced: one drain covers everything retained before it
// starts, and a signal that arrives during a drain wakes the next one.
func (coordinator *Coordinator) wakeAdmission() {
	coordinator.mu.Lock()
	admits := coordinator.admits
	coordinator.mu.Unlock()
	if admits == nil {
		return
	}
	select {
	case admits <- struct{}{}:
	default:
	}
}

// NotePush retains the branch updates of one accepted push, so each of them is
// admitted as its own job even while another job runs. It only adds to memory
// and never waits for the state, the repository or the push: the Git response
// is already complete and cannot be delayed or failed by check admission. A
// deletion, a ref outside refs/heads and a branch name the check records
// cannot hold carry no event, and the admission goroutine is woken here, so an
// event is decided while a local job and the reconciliation pass continue. A
// coordinator that has not started keeps nothing; the next start reconciles
// each branch head as before, and a commit that is no longer a head keeps no
// job.
func (coordinator *Coordinator) NotePush(repositoryID string, updates []PushUpdate) {
	if repositoryID == "" || len(updates) == 0 {
		return
	}
	facts := make([]pushUpdate, 0, len(updates))
	for _, update := range updates {
		if update.New == "" || !strings.HasPrefix(update.Ref, "refs/heads/") {
			continue
		}
		facts = append(facts, pushUpdate{ref: update.Ref, oid: update.New})
	}
	if len(facts) == 0 {
		return
	}
	coordinator.keepPushes(repositoryID, facts, false)
	coordinator.Wake(repositoryID)
	coordinator.wakeAdmission()
}

// keepPushes retains accepted branch updates for a later attempt and names in
// one summary line what that call dropped. An arrival goes after what is
// already waiting; an update an attempt could not decide goes in front of it,
// so admission stays oldest first. A repository at its bound drops the oldest
// arrival, or the newest update when an older one goes back in front, so a
// retry never loses the event it was retrying. When all repositories together
// retain maximumPendingPushTotal updates, an arrival that would grow the set
// is dropped instead. An update no observation or job can name is not
// retained; the head pass names such a branch once.
func (coordinator *Coordinator) keepPushes(repositoryID string, updates []pushUpdate, front bool) {
	if len(updates) == 0 {
		return
	}
	coordinator.mu.Lock()
	// Nothing would admit the updates while the coordinator is not running, and
	// the next start reconciles each branch head anyway.
	if coordinator.wake == nil {
		coordinator.mu.Unlock()
		return
	}
	if coordinator.pendingPushes == nil {
		coordinator.pendingPushes = make(map[string][]pushUpdate)
	}
	kept := coordinator.pendingPushes[repositoryID]
	var dropped []pushUpdate
	acrossRepositories := false
	if front {
		var put []pushUpdate
		for _, update := range updates {
			if !state.ValidCheckObservationRef(update.ref) || containsPushUpdate(kept, update) {
				continue
			}
			put = append(put, update)
		}
		kept = append(put, kept...)
		for len(kept) > maximumPendingPushes {
			dropped = append(dropped, kept[len(kept)-1])
			kept = kept[:len(kept)-1]
		}
	} else {
		// total counts what every other repository keeps and what the running
		// drain took, so the bound leaves room for the retries it puts back in
		// front.
		total := coordinator.draining - len(kept)
		for _, waiting := range coordinator.pendingPushes {
			total += len(waiting)
		}
		for _, update := range updates {
			if !state.ValidCheckObservationRef(update.ref) || containsPushUpdate(kept, update) {
				continue
			}
			if len(kept) < maximumPendingPushes && total+len(kept) >= maximumPendingPushTotal {
				acrossRepositories = true
				dropped = append(dropped, update)
				continue
			}
			kept = append(kept, update)
			if len(kept) > maximumPendingPushes {
				dropped = append(dropped, kept[0])
				kept = kept[1:]
			}
		}
	}
	if len(kept) == 0 {
		delete(coordinator.pendingPushes, repositoryID)
	} else {
		coordinator.pendingPushes[repositoryID] = kept
	}
	coordinator.mu.Unlock()
	if len(dropped) != 0 {
		// One line per call, so a push that updates many branches cannot flood
		// the log or delay the Git response that already finished.
		// A call drops by one bound only: the total refuses growth only while the
		// repository is below its own bound, and it cannot grow to it then.
		bound := fmt.Sprintf("%d updates per repository are already waiting", maximumPendingPushes)
		if acrossRepositories {
			bound = fmt.Sprintf("%d updates across repositories are already waiting", maximumPendingPushTotal)
		}
		coordinator.log("configured check push %s dropped %d of %d updates: %s (first dropped %s at %s, last dropped %s at %s)",
			repositoryID, len(dropped), len(updates), bound,
			branchName(dropped[0].ref), dropped[0].oid, branchName(dropped[len(dropped)-1].ref), dropped[len(dropped)-1].oid)
	}
}

// containsPushUpdate reports whether an update is already waiting.
func containsPushUpdate(updates []pushUpdate, update pushUpdate) bool {
	for _, pending := range updates {
		if pending == update {
			return true
		}
	}
	return false
}

// admitPendingPushes admits the branch updates of pushes that no attempt has
// decided yet, each as its own job, so a push made while a local job runs
// keeps its own event instead of only the head it left. It runs on the
// admission goroutine, independently of the reconciliation pass and of job
// execution. Admission applies the same policy, consent, legacy, ceiling,
// branch, dedup and queue rules the head pass uses and leaves the repository
// exactly as the head pass leaves it. An event a repository cannot decide yet,
// or one a transient Git or state failure stopped, waits for a later attempt
// and is named once for the repository; a deterministic refusal is dropped
// like a head the rules refuse. It reports whether an event waits.
//
// Each repository is tried first without waiting for a writer, so a repository
// another write holds never delays the events of the others. The drain then
// waits for the held repositories, at most admissionBusyWait in total.
//
// An event is decided against the policy as it is at admission time, not as it
// was at push time. Immediate admission keeps that window to one attempt, and
// the admitting transaction checks consent again, so the event path cannot
// widen what a saved policy allows.
func (coordinator *Coordinator) admitPendingPushes(ctx context.Context) bool {
	coordinator.mu.Lock()
	pending := coordinator.pendingPushes
	coordinator.pendingPushes = nil
	for _, facts := range pending {
		coordinator.draining += len(facts)
	}
	coordinator.mu.Unlock()
	if len(pending) == 0 {
		return false
	}
	defer func() {
		coordinator.mu.Lock()
		coordinator.draining = 0
		coordinator.mu.Unlock()
	}()
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if ctx.Err() != nil {
		// The coordinator is stopping. Retained events are memory only, so what
		// no attempt admitted yet goes away with the process, and the next start
		// reconciles each branch head as before.
		return false
	}
	ceilings, err := coordinator.Store.CheckCeilings(ctx)
	if err != nil {
		if ctx.Err() == nil {
			coordinator.log("configured check push admission: %v", err)
		}
		for _, id := range ids {
			coordinator.keepPushes(id, pending[id], true)
		}
		return ctx.Err() == nil
	}
	type heldRepository struct {
		id     string
		policy state.CheckPolicy
		facts  []pushUpdate
	}
	var held []heldRepository
	waiting := false
	for _, repositoryID := range ids {
		if ctx.Err() != nil {
			return false
		}
		facts := pending[repositoryID]
		if coordinator.Repositories.Preparing(repositoryID) {
			coordinator.keepPushes(repositoryID, facts, true)
			waiting = true
			continue
		}
		policy, ready, err := coordinator.admissiblePushPolicy(ctx, repositoryID, ceilings)
		if err != nil {
			if ctx.Err() == nil {
				coordinator.log("configured check push admission for %s: %v", repositoryID, err)
			}
			coordinator.keepPushes(repositoryID, facts, true)
			waiting = true
			continue
		}
		if !ready || !contains(policy.AllowedEvents, checkworkflow.EventPush) {
			// Nothing admits these pushes now: checks are not configured or
			// consented, the execution is legacy, the policy is above this
			// computer's ceilings, or the push event is off. The head pass leaves
			// those branches unobserved, so a later change admits what arrived
			// meanwhile, and an event type the policy does not allow is not
			// recorded at all.
			continue
		}
		undecided, busy, failure := coordinator.admitPendingEvents(ctx, repositoryID, policy, facts, time.Time{})
		if busy {
			held = append(held, heldRepository{id: repositoryID, policy: policy, facts: undecided})
			continue
		}
		if coordinator.keepUndecided(repositoryID, undecided, failure) {
			waiting = true
		}
	}
	deadline := time.Now().Add(admissionBusyWait)
	for _, entry := range held {
		undecided, _, failure := coordinator.admitPendingEvents(ctx, entry.id, entry.policy, entry.facts, deadline)
		if coordinator.keepUndecided(entry.id, undecided, failure) {
			waiting = true
		}
	}
	return waiting
}

// keepUndecided puts a repository's undecided events back in front of newer
// arrivals, names the failure that stopped them once, and reports whether any
// event waits.
func (coordinator *Coordinator) keepUndecided(repositoryID string, undecided []pushUpdate, failure error) bool {
	if len(undecided) == 0 {
		return false
	}
	if failure != nil {
		coordinator.log("configured check push %s keeps %d updates for a later attempt: %v", repositoryID, len(undecided), failure)
	}
	coordinator.keepPushes(repositoryID, undecided, true)
	return true
}

// admissionLoop decides retained push events while the reconciliation loop
// executes jobs, so a push made during a local job becomes a job during that
// run instead of after it. It is the only drainer of the retained set. An
// event a repository could not decide is attempted again at the reconciliation
// cadence, never in a busy loop, and the loop ends with the coordinator.
func (coordinator *Coordinator) admissionLoop(ctx context.Context, admits <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-admits:
		}
		for ctx.Err() == nil && coordinator.admitPendingPushes(ctx) {
			timer := time.NewTimer(admissionRetryWait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-admits:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}

// admitPendingEvents admits one repository's retained push events, oldest
// first, and returns the events that wait for a later attempt with the failure
// that stopped them. An event that already has a job, or that the rules
// refuse, is left alone and named in the log; an event a Git or state failure
// stopped waits, like a head the head pass could not decide. A writer that
// still holds the repository at deadline stops the repository: that event and
// every later one wait, in order, and busy is true. A zero deadline tries once
// without waiting.
func (coordinator *Coordinator) admitPendingEvents(ctx context.Context, repositoryID string, policy state.CheckPolicy, facts []pushUpdate, deadline time.Time) (undecided []pushUpdate, busy bool, failure error) {
	events := make([]string, 0, len(facts))
	for _, fact := range facts {
		events = append(events, fact.ref+"@"+fact.oid)
	}
	seen, err := coordinator.Store.CheckEventsWithJobs(ctx, repositoryID, checkworkflow.EventPush, events)
	if err != nil {
		if ctx.Err() != nil {
			return facts, false, nil
		}
		return facts, false, err
	}
	for index, fact := range facts {
		if seen[fact.ref+"@"+fact.oid] {
			continue
		}
		admit := func() error {
			_, admitErr := coordinator.admit(ctx, policy, state.CheckJobRequest{
				RepositoryID: repositoryID, Trigger: checkworkflow.EventPush,
				EventKey: fact.ref + "@" + fact.oid, SourceOID: fact.oid, TriggerRef: branchName(fact.ref),
			})
			return admitErr
		}
		// The push that reported this update usually still holds the repository
		// write lock while its handler returns, so a short wait is expected.
		err := checksource.RetryWhileRepositoryBusy(ctx, max(time.Until(deadline), 0), admit)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			// The coordinator is stopping: the event stays in memory only.
			undecided = append(undecided, fact)
		case errors.Is(err, repository.ErrPinnedRepositoryBusy):
			return append(undecided, facts[index:]...), true, repository.ErrPinnedRepositoryBusy
		case decidedPushRefusal(err):
			coordinator.log("configured check push %s %s at %s was not admitted: %v",
				repositoryID, branchName(fact.ref), fact.oid, err)
		default:
			undecided = append(undecided, fact)
			failure = err
		}
	}
	return undecided, false, failure
}

// decidedPushRefusal reports whether the admission rules decided an event, so
// a later attempt would refuse it the same way. Everything else, such as a Git
// process or state write failure, may pass on a later attempt.
func decidedPushRefusal(err error) bool {
	return errors.Is(err, errRevisionRejected) ||
		errors.Is(err, state.ErrCheckQueueFull) ||
		errors.Is(err, state.ErrCheckPolicyMissing) ||
		errors.Is(err, state.ErrCheckConsentRequired) ||
		errors.Is(err, state.ErrCheckEventNotAllowed) ||
		errors.Is(err, state.ErrCheckCeilingExceeded)
}

// admissiblePushPolicy returns the policy a repository's push checks may be
// admitted under now, and false while it may not: when checks are not
// configured, consent is not current, the execution settings are legacy, or
// the policy is above this computer's check ceilings. The head pass leaves
// those branches unobserved, so a later pass admits what arrived meanwhile.
func (coordinator *Coordinator) admissiblePushPolicy(ctx context.Context, repositoryID string, ceilings state.CheckCeilings) (state.CheckPolicy, bool, error) {
	policy, exists, err := coordinator.Store.CheckPolicy(ctx, repositoryID)
	if err != nil {
		return state.CheckPolicy{}, false, err
	}
	if !exists || !policy.ConsentActive || policy.ConsentDigest != policy.Digest || policy.Execution.Legacy {
		return state.CheckPolicy{}, false, nil
	}
	if len(ceilings.Exceeded(policy.CeilingValues())) != 0 {
		return state.CheckPolicy{}, false, nil
	}
	return policy, true, nil
}

// noteSkippedRef names once per head a branch whose name the check records
// cannot hold: no observation and no job can carry it, so the head pass skips
// it and a push to it retains no event.
func (coordinator *Coordinator) noteSkippedRef(repositoryID, refName, oid string) {
	if coordinator.skippedRefs == nil {
		coordinator.skippedRefs = make(map[string]map[string]string)
	}
	skipped := coordinator.skippedRefs[repositoryID]
	if skipped[refName] == oid {
		return
	}
	if skipped == nil {
		skipped = make(map[string]string)
		coordinator.skippedRefs[repositoryID] = skipped
	}
	skipped[refName] = oid
	coordinator.log("configured check push %s %s is skipped: the branch name cannot be recorded", repositoryID, branchName(refName))
}

// branchName names a branch ref the way the jobs and the messages name it.
func branchName(refName string) string { return strings.TrimPrefix(refName, "refs/heads/") }

func (coordinator *Coordinator) loop(ctx context.Context, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	interval := coordinator.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
		if err := coordinator.reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
			coordinator.log("configured check reconciliation: %v", err)
		}
		if err := coordinator.runOneLocal(ctx); err != nil && !errors.Is(err, context.Canceled) {
			coordinator.log("configured check execution: %v", err)
		}
	}
}

func (coordinator *Coordinator) reconcile(ctx context.Context) error {
	if _, err := coordinator.Store.ExpireCheckJobLeases(ctx, time.Now().UTC()); err != nil {
		return err
	}
	ids, err := coordinator.Store.CheckPolicyRepositories(ctx)
	if err != nil {
		return err
	}
	ceilings, err := coordinator.Store.CheckCeilings(ctx)
	if err != nil {
		return err
	}
	// Retained push events are decided by the admission goroutine, which does
	// not wait for this pass or for a running job; the head pass below then
	// finds their jobs and queues nothing twice.
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A repository still being prepared after startup is reconciled once
		// it is ready; its preparation wakes the coordinator.
		if coordinator.Repositories.Preparing(id) {
			continue
		}
		policy, ready, err := coordinator.admissiblePushPolicy(ctx, id, ceilings)
		if err != nil {
			return err
		}
		// A policy that is not admissible now waits, like one without consent:
		// its branches stay unobserved, so raising the ceiling, giving consent
		// or lowering the policy admits what arrived meanwhile.
		if !ready {
			continue
		}
		if err := coordinator.reconcilePushes(ctx, id, policy); err != nil {
			coordinator.log("reconcile push checks for %s: %v", id, err)
		}
		if err := coordinator.reconcilePullRequests(ctx, id, policy); err != nil {
			coordinator.log("reconcile pull request checks for %s: %v", id, err)
		}
	}
	return nil
}

// fullScanInterval is the longest a repository's branch heads go unread. A
// repository whose refs OwnGit did not write since its last complete scan is
// skipped until then, which also catches a writer outside OwnGit.
const fullScanInterval = 10 * time.Minute

// pushScan tracks the sweep of one repository's branch heads at one RefState
// and policy. A repository with more branches than one pass handles needs
// several passes; handled counts the branches of the consecutive passes since
// the sweep began at endCursor-contiguous positions, and complete is set once
// they cover every head. at is when the sweep read its first batch.
type pushScan struct {
	refs          repository.RefState
	policyVersion int64
	policyDigest  string
	at            time.Time
	handled       int
	endCursor     string
	complete      bool
}

func (coordinator *Coordinator) reconcilePushes(ctx context.Context, repositoryID string, policy state.CheckPolicy) error {
	if coordinator.pushCursor == nil {
		coordinator.pushCursor = make(map[string]string)
	}
	if coordinator.pushChecked == nil {
		coordinator.pushChecked = make(map[string]bool)
	}
	// A disabled event observes nothing, so enabling it later admits the
	// heads that arrived meanwhile (the policy change clears older records).
	if !contains(policy.AllowedEvents, checkworkflow.EventPush) {
		return nil
	}
	// Every OwnGit ref write advances the lock generation, so a repository
	// with the same RefState and policy as its last complete scan needs no Git
	// read until fullScanInterval has passed. BranchHeads compares the state
	// after it has waited for a writer that is still pushing, so a push that
	// woke this pass is never skipped. A full read takes one Git process, not
	// the tag and default branch reads of Summary.
	var lastRefs *repository.RefState
	now := time.Now()
	if last, ok := coordinator.pushScans[repositoryID]; ok && last.complete && coordinator.pushChecked[repositoryID] &&
		last.policyVersion == policy.Version && last.policyDigest == policy.Digest && now.Sub(last.at) < fullScanInterval {
		lastRefs = &last.refs
	}
	heads, refs, unchanged, err := coordinator.Repositories.BranchHeads(ctx, repositoryID, lastRefs)
	if err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	cursorAtStart := coordinator.pushCursor[repositoryID]
	sort.Slice(heads, func(i, j int) bool { return heads[i].Name < heads[j].Name })
	// Verifying pages run toward the last branch without wrapping, so each
	// head is evaluated once; later pages rotate through all branches.
	verifying := !coordinator.pushChecked[repositoryID]
	var branches []repository.Ref
	if verifying {
		branches = branchesAfter(heads, coordinator.pushCursor[repositoryID], maximumObservedRefs)
	} else {
		branches = boundedBranchesAfter(heads, coordinator.pushCursor[repositoryID], maximumObservedRefs)
	}
	if len(heads) > maximumObservedRefs {
		coordinator.log("configured check ref reconciliation for %s is processing a fair batch of %d/%d branches", repositoryID, len(branches), len(heads))
	}
	observations, err := coordinator.Store.CheckObservations(ctx, repositoryID)
	if err != nil {
		return err
	}
	previous := make(map[string]string, len(observations))
	for _, observation := range observations {
		previous[observation.RefName] = observation.OID
	}
	live := make(map[string]bool, len(heads))
	for _, branch := range heads {
		live["refs/heads/"+branch.Name] = true
	}
	// A skipped branch whose ref is gone needs no further reporting.
	for refName := range coordinator.skippedRefs[repositoryID] {
		if !live[refName] {
			delete(coordinator.skippedRefs[repositoryID], refName)
		}
	}
	// The observation set is bounded, so in a repository with more branches
	// a head can lose its observation without moving. A head that already has
	// a job for this exact event, under any policy version, is not queued
	// again; otherwise every policy change would requeue those heads.
	//
	// Until this coordinator has gone through every branch of the repository
	// once, a head whose observation equals its current object is checked
	// against the job history too, since an observation can outlive the job
	// it stood for (left by an earlier version or lost across a restart).
	var unobserved []string
	for _, branch := range branches {
		if refName := "refs/heads/" + branch.Name; previous[refName] == "" || verifying {
			unobserved = append(unobserved, refName+"@"+branch.OID)
		}
	}
	seen, err := coordinator.Store.CheckEventsWithJobs(ctx, repositoryID, checkworkflow.EventPush, unobserved)
	if err != nil {
		return err
	}
	for _, branch := range branches {
		refName := "refs/heads/" + branch.Name
		if !state.ValidCheckObservationRef(refName) {
			// No observation or job can name this branch, so it is skipped
			// once per head and the rest of the repository continues.
			coordinator.noteSkippedRef(repositoryID, refName, branch.OID)
			coordinator.pushCursor[repositoryID] = branch.Name
			continue
		}
		if (previous[refName] != branch.OID || verifying) && !seen[refName+"@"+branch.OID] {
			admitted, err := coordinator.admit(ctx, policy, state.CheckJobRequest{
				RepositoryID: repositoryID, Trigger: checkworkflow.EventPush,
				EventKey: refName + "@" + branch.OID, SourceOID: branch.OID, TriggerRef: branch.Name,
			})
			switch {
			case errors.Is(err, errRevisionRejected):
				// Recording the observation below means this revision is
				// reported once and admitted again only after the branch moves.
				coordinator.log("configured check push %s %s at %s was not admitted: %v", repositoryID, branch.Name, branch.OID, err)
			case err != nil:
				return err
			case admitted || (previous[refName] != "" && previous[refName] != branch.OID):
				coordinator.log("observed configured check push %s %s", repositoryID, branch.Name)
			}
		}
		if err := coordinator.Store.RecordCheckObservation(ctx, repositoryID, refName, branch.OID, time.Now().UTC()); err != nil {
			return err
		}
		coordinator.pushCursor[repositoryID] = branch.Name
	}
	if len(heads) == 0 {
		delete(coordinator.pushCursor, repositoryID)
	}
	// The sweep is complete once a page reaches the last branch.
	if verifying && (len(branches) == 0 || branches[len(branches)-1].Name == heads[len(heads)-1].Name) {
		coordinator.pushChecked[repositoryID] = true
	}
	for refName := range previous {
		if !live[refName] {
			if err := coordinator.Store.DeleteCheckObservation(ctx, repositoryID, refName); err != nil {
				return err
			}
		}
	}
	// A sweep continues only while the refs, the policy and the cursor are
	// the ones the previous successful pass left; a failed pass moves the
	// cursor without recording, so the next pass starts a new sweep. Once the
	// consecutive passes have covered every head at one RefState, later passes
	// skip the Git read.
	sweep, ok := coordinator.pushScans[repositoryID]
	if !ok || sweep.refs != refs || sweep.policyVersion != policy.Version || sweep.policyDigest != policy.Digest ||
		sweep.endCursor != cursorAtStart || sweep.complete {
		sweep = pushScan{refs: refs, policyVersion: policy.Version, policyDigest: policy.Digest, at: now}
	}
	sweep.handled += len(branches)
	sweep.endCursor = coordinator.pushCursor[repositoryID]
	sweep.complete = sweep.handled >= len(heads) && coordinator.pushChecked[repositoryID]
	if coordinator.pushScans == nil {
		coordinator.pushScans = make(map[string]pushScan)
	}
	coordinator.pushScans[repositoryID] = sweep
	return nil
}

func (coordinator *Coordinator) reconcilePullRequests(ctx context.Context, repositoryID string, policy state.CheckPolicy) error {
	if coordinator.pullRequestCursor == nil {
		coordinator.pullRequestCursor = make(map[string]int64)
	}
	if !contains(policy.AllowedEvents, checkworkflow.EventPullRequest) {
		return nil
	}
	revisions, more, err := coordinator.PullRequests.ObserveCurrentRevisionsAfter(ctx, repositoryID, coordinator.pullRequestCursor[repositoryID], maximumObservedPRs)
	if err != nil {
		return err
	}
	eventKey := func(revision pullrequest.CurrentRevision) string {
		return fmt.Sprintf("pr/%d/%s/%s", revision.PullRequest.Number, revision.SourceOID, revision.TargetOID)
	}
	keys := make([]string, 0, len(revisions))
	for _, revision := range revisions {
		keys = append(keys, eventKey(revision))
	}
	// A revision that already has a job, under any policy version, is not
	// queued again, so a policy change does not requeue every open request.
	seen, err := coordinator.Store.CheckEventsWithJobs(ctx, repositoryID, checkworkflow.EventPullRequest, keys)
	if err != nil {
		return err
	}
	observed := 0
	for _, revision := range revisions {
		coordinator.pullRequestCursor[repositoryID] = revision.PullRequest.Number
		if revision.NewlyObserved {
			observed++
		}
		if revision.SourceOID == "" || revision.TargetOID == "" || seen[eventKey(revision)] {
			continue
		}
		_, err := coordinator.admit(ctx, policy, state.CheckJobRequest{
			RepositoryID: repositoryID, Trigger: checkworkflow.EventPullRequest,
			EventKey:  eventKey(revision),
			SourceOID: revision.SourceOID, BaseOID: revision.TargetOID, PullRequestNumber: revision.PullRequest.Number,
			TriggerRef: revision.PullRequest.TargetBranch,
		})
		if errors.Is(err, errRevisionRejected) {
			coordinator.log("configured check pull request %s #%d at %s was not admitted: %v", repositoryID, revision.PullRequest.Number, revision.SourceOID, err)
		} else if err != nil {
			return err
		}
	}
	if observed > 0 {
		coordinator.log("observed %d current pull request revisions for %s", observed, repositoryID)
	}
	if more {
		coordinator.log("configured check pull request reconciliation for %s is processing fair batches of at most %d open requests", repositoryID, maximumObservedPRs)
	}
	if len(revisions) == 0 {
		delete(coordinator.pullRequestCursor, repositoryID)
	}
	return nil
}

func (coordinator *Coordinator) admit(ctx context.Context, policy state.CheckPolicy, request state.CheckJobRequest) (bool, error) {
	if !contains(policy.AllowedEvents, request.Trigger) {
		return false, nil
	}
	pinned, err := coordinator.Repositories.PinRepository(ctx, request.RepositoryID, request.SourceOID, request.SourceOID)
	if err != nil {
		return false, fmt.Errorf("pin configured check source %s: %w", request.SourceOID, err)
	}
	blob, document, err := ReadPinnedWorkflow(ctx, pinned, policy.Execution.Source.MetadataLimit)
	if errors.Is(err, repository.ErrPinnedPathNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	effective, err := checkworkflow.Tighten(document, checkworkflow.OperatorPolicy{
		AllowedEvents: policy.AllowedEvents, MaxTimeoutMS: policy.MaxTimeoutMS,
		MaxOutputLimitBytes: policy.MaxOutputLimitBytes,
	})
	if err != nil {
		return false, fmt.Errorf("%w: %w", errRevisionRejected, err)
	}
	if !effective.MatchesBranch(request.Trigger, request.TriggerRef) {
		return false, nil
	}
	request.WorkflowPath = checkworkflow.Path
	request.WorkflowOID = blob.OID
	digest := sha256.Sum256(blob.Content)
	request.WorkflowDigest = fmt.Sprintf("%x", digest[:])
	request.TimeoutMS = document.Limits.TimeoutMS
	request.OutputLimitBytes = document.Limits.OutputLimitBytes
	request.Checks = make([]state.CheckDefinition, 0, len(document.Checks))
	for _, check := range document.Checks {
		request.Checks = append(request.Checks, state.CheckDefinition{Name: check.Name, Command: check.Command})
	}
	var deduped bool
	err = pinned.WhilePresent(ctx, func() (err error) {
		_, deduped, err = coordinator.Store.AdmitCheckJob(ctx, request, time.Now().UTC())
		return err
	})
	if errors.Is(err, state.ErrInvalidCheckJob) {
		return false, fmt.Errorf("%w: %w", errRevisionRejected, err)
	}
	return !deduped && err == nil, err
}

// ReadPinnedWorkflow reads and parses the workflow file of a pinned revision.
// A revision without the file reports repository.ErrPinnedPathNotFound. A file
// that cannot be a workflow wraps errRevisionRejected.
func ReadPinnedWorkflow(ctx context.Context, pinned *repository.PinnedRepository, metadataLimit int64) (repository.PinnedBlobChunk, checkworkflow.Document, error) {
	blob, err := pinned.ReadBlob(ctx, repository.PinnedHead, checkworkflow.Path, 0, metadataLimit,
		checkworkflow.MaximumBytes+1, checkworkflow.MaximumBytes+1)
	if errors.Is(err, repository.ErrPinnedPathNotFound) {
		return blob, checkworkflow.Document{}, err
	}
	if errors.Is(err, repository.ErrPinnedUnsupportedObject) || errors.Is(err, repository.ErrPinnedOutputLimit) ||
		errors.Is(err, repository.ErrPinnedBlobTooLarge) {
		return blob, checkworkflow.Document{}, fmt.Errorf("%w: read configured check workflow: %w", errRevisionRejected, err)
	}
	if err != nil {
		return blob, checkworkflow.Document{}, fmt.Errorf("read configured check workflow: %w", err)
	}
	if blob.Symlink || (blob.Mode != "100644" && blob.Mode != "100755") || blob.HasMore || blob.Size != int64(len(blob.Content)) || len(blob.Content) > checkworkflow.MaximumBytes {
		return blob, checkworkflow.Document{}, fmt.Errorf("%w: configured check workflow exceeds its source bound", errRevisionRejected)
	}
	document, err := checkworkflow.Parse(blob.Content)
	if err != nil {
		return blob, checkworkflow.Document{}, fmt.Errorf("%w: parse configured check workflow: %w", errRevisionRejected, err)
	}
	return blob, document, nil
}

func (coordinator *Coordinator) runOneLocal(ctx context.Context) error {
	// The repository holding the oldest pending job goes first, so a busy
	// repository never keeps another repository's older job waiting.
	ids, err := coordinator.Store.CheckPendingRepositories(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if coordinator.Repositories.Preparing(id) {
			continue
		}
		job, claimed, err := coordinator.Store.ClaimLocalCheckJob(ctx, id, time.Now().UTC())
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		if err := coordinator.executeLocal(ctx, job); err != nil {
			return err
		}
		if coordinator.OnCheckCompletion != nil {
			coordinator.OnCheckCompletion(id)
		}
		coordinator.Wake(id)
		return nil
	}
	return nil
}

func (coordinator *Coordinator) executeLocal(parent context.Context, job state.CheckJob) error {
	authority := state.CheckJobCompletionAuthority{
		JobID: job.ID, LeaseID: job.LeaseID, CredentialID: job.CredentialID, CredentialGeneration: job.CredentialGeneration,
	}
	jobContext, cancelJob := context.WithCancel(parent)
	watchDone := make(chan error, 1)
	go coordinator.watchLease(jobContext, cancelJob, job, authority, watchDone)
	watchStopped := false
	stopWatcher := func() error {
		if watchStopped {
			return nil
		}
		watchStopped = true
		cancelJob()
		return <-watchDone
	}
	defer stopWatcher()

	workspace, materialization, err := coordinator.materialize(jobContext, job)
	if err != nil {
		_ = stopWatcher()
		return coordinator.recordNotRun(parent, job, authority, "Exact configured-check source is unavailable or workspace ownership is uncertain: ", err)
	}

	cleanupWorkspace := func(results []checkexec.Result) []checkexec.Result {
		if err := coordinator.workspace.RemoveJob(job.ID); err != nil && len(results) != 0 {
			results[len(results)-1].Status = checkexec.StatusError
			results[len(results)-1].CleanupError = boundedSummary("remove private source workspace: " + err.Error())
		}
		return results
	}

	var protection string
	var container preparedContainer
	switch job.Executor {
	case state.CheckExecutorHost:
		protection = state.ProtectionHost
	case state.CheckExecutorContainer:
		if container, err = coordinator.containerPreflight(jobContext, job, workspace); err != nil {
			_ = stopWatcher()
			return coordinator.recordNotRun(parent, job, authority, "Configured container runtime is unavailable: ", err)
		}
		protection = state.ProtectionContainer
	default:
		_ = coordinator.workspace.RemoveJob(job.ID)
		return errors.New("local worker claimed a nonlocal configured check")
	}

	attemptID, err := state.RandomID()
	if err != nil {
		_ = coordinator.workspace.RemoveJob(job.ID)
		return err
	}
	_, attempt, err := coordinator.Store.StartCheckJob(jobContext, state.CheckJobStart{
		RepositoryID: job.RepositoryID, JobID: job.ID, LeaseID: job.LeaseID,
		CredentialID: job.CredentialID, CredentialGeneration: job.CredentialGeneration, Protection: protection,
		AttemptID: attemptID,
	}, time.Now().UTC())
	var notStarted *state.CheckJobNotStartedError
	if errors.As(err, &notStarted) {
		// The job was not started and is still claimed.
		_ = stopWatcher()
		return coordinator.recordNotRun(parent, job, authority, "The job was not started: ", err)
	}
	if err != nil {
		_ = coordinator.workspace.RemoveJob(job.ID)
		return err
	}

	definitions := make([]checkexec.Definition, 0, len(attempt.Checks))
	for _, check := range attempt.Checks {
		definitions = append(definitions, checkexec.Definition{Name: check.Name, Command: check.Command})
	}
	var results []checkexec.Result
	var cancelled bool
	if job.Executor == state.CheckExecutorContainer {
		results, cancelled = coordinator.runContainerChecks(jobContext, job, container, workspace, definitions)
	} else {
		results, cancelled = checkexec.Run(jobContext, definitions, checkexec.Options{
			Dir: workspace, TempRoot: filepath.Dir(workspace), Timeout: time.Duration(job.Limits.TimeoutMS) * time.Millisecond,
			OutputLimit: job.Limits.OutputLimitBytes,
		})
	}
	submittedWorktree := state.WorktreeClean
	verifyContext, cancelVerify := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	clean, verifyErr := checksource.VerifyResult(verifyContext, materialization)
	cancelVerify()
	if verifyErr != nil {
		submittedWorktree = state.WorktreeUnknown
		if len(results) != 0 {
			results[len(results)-1].Status = checkexec.StatusError
			results[len(results)-1].CleanupError = boundedSummary("verify private source workspace: " + verifyErr.Error())
		}
	} else if !clean {
		submittedWorktree = state.WorktreeDirty
		if len(results) != 0 && results[len(results)-1].Status == checkexec.StatusPassed {
			results[len(results)-1].Status = checkexec.StatusIncomplete
		}
	}
	watchErr := stopWatcher()
	results = cleanupWorkspace(results)
	if watchErr != nil && !errors.Is(watchErr, context.Canceled) && len(results) != 0 {
		results[len(results)-1].Status = checkexec.StatusError
		results[len(results)-1].CleanupError = boundedSummary("lease or cancellation observation failed: " + watchErr.Error())
	}
	completion := state.CheckCompletion{
		AttemptID: attemptID, RepositoryID: job.RepositoryID, TaskID: job.TaskID,
		Results: stateResults(results), Cancelled: cancelled, FinishedAt: time.Now().UTC(),
		WorktreeState: submittedWorktree,
	}
	completion.Log, completion.LogTruncated = buildLog(results)
	_, _, completeErr := coordinator.Store.CompleteCheckJobAttempt(context.WithoutCancel(parent), completion, authority, time.Now().UTC())
	return completeErr
}

// recordNotRun ends a claimed job that ran nothing, with the reason. The job
// is interrupted when the coordinator is stopping, since the stop may be the
// cause, and unavailable otherwise. The record is written even while the
// coordinator stops.
func (coordinator *Coordinator) recordNotRun(parent context.Context, job state.CheckJob, authority state.CheckJobCompletionAuthority, reason string, cause error) error {
	_ = coordinator.workspace.RemoveJob(job.ID)
	status := state.CheckJobUnavailable
	if parent.Err() != nil {
		status = state.CheckJobInterrupted
	}
	_, err := coordinator.Store.FailCheckJobBeforeStart(context.WithoutCancel(parent), authority, status, boundedSummary(reason+cause.Error()), time.Now().UTC())
	if err == nil {
		coordinator.log("reported configured-check job %s %s: %v", job.ID, status, cause)
	}
	return err
}

// materialize copies the job's exact source into a new private workspace. A
// push or another repository write makes pinned reads refuse for a moment; the
// copy then starts again in a fresh workspace instead of ending the job.
func (coordinator *Coordinator) materialize(ctx context.Context, job state.CheckJob) (string, *checksource.Result, error) {
	var workspace string
	var materialization *checksource.Result
	err := checksource.RetryWhileRepositoryBusy(ctx, repositoryBusyWait, func() error {
		if workspace != "" {
			if err := coordinator.workspace.RemoveJob(job.ID); err != nil {
				return err
			}
		}
		var err error
		if _, workspace, err = coordinator.workspace.PrepareJob(job.ID); err != nil {
			return err
		}
		pinned, err := coordinator.Repositories.PinRepository(ctx, job.RepositoryID, job.SourceOID, job.SourceOID)
		if err != nil {
			return err
		}
		materialization, err = checksource.MaterializePinned(ctx, pinned, repository.PinnedHead, workspace, checksource.Options{
			Limits: checksource.Limits{
				MaxEntries: job.Execution.Source.MaxEntries, MaxFileBytes: job.Execution.Source.MaxFileBytes,
				MaxTotalBytes: job.Execution.Source.MaxTotalBytes, MaxPathDepth: job.Execution.Source.MaxPathDepth,
				MaxPathBytes: job.Execution.Source.MaxPathBytes, MaxNameBytes: job.Execution.Source.MaxNameBytes,
				MetadataLimit: job.Execution.Source.MetadataLimit,
			},
			Timeout: time.Duration(job.Limits.TimeoutMS) * time.Millisecond,
		})
		return err
	})
	return workspace, materialization, err
}

func (coordinator *Coordinator) watchLease(ctx context.Context, cancel context.CancelFunc, job state.CheckJob, authority state.CheckJobCompletionAuthority, done chan<- error) {
	ticker := time.NewTicker(checkapi.LeaseRenewDelay(time.Now(), job.LeaseExpiresAt))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- ctx.Err()
			return
		case <-ticker.C:
			current, exists, err := coordinator.Store.CheckJob(ctx, job.RepositoryID, job.ID)
			if err != nil || !exists {
				cancel()
				if err == nil {
					err = state.ErrCheckJobNotFound
				}
				done <- err
				return
			}
			if current.CancelRequestedAt != nil {
				cancel()
				done <- nil
				return
			}
			if _, err := coordinator.Store.RenewCheckJobLease(ctx, authority, time.Now().UTC()); err != nil {
				cancel()
				done <- err
				return
			}
		}
	}
}

func stateResults(results []checkexec.Result) []state.CheckResult {
	converted := make([]state.CheckResult, 0, len(results))
	for index, result := range results {
		excerpt, cut := checkapi.ClipLog(result.Output, state.MaximumCheckExcerptBytes, result.OutputGap)
		truncated := result.Truncated || cut
		cleanupError := result.CleanupError
		if cleanupError != "" {
			cleanupError = boundedSummary(cleanupError)
		}
		converted = append(converted, state.CheckResult{
			Position: index, Name: result.Name, Command: result.Command, Status: result.Status,
			ExitCode: result.ExitCode, DurationMS: result.Duration.Milliseconds(), OutputExcerpt: excerpt,
			Truncated: truncated, CleanupError: cleanupError,
		})
	}
	return converted
}

func buildLog(results []checkexec.Result) (string, bool) {
	log := checkapi.LogBuffer{Limit: maximumAutomaticLogSize}
	for _, result := range results {
		// Output may be far larger than the log, so it is added as its own part
		// rather than copied whole into one string before the cut.
		log.Add("[" + result.Status + "] " + result.Command + "\n")
		log.AddClipped(result.Output, result.OutputGap)
		log.Add("\n")
	}
	return log.Result()
}

// branchesAfter returns up to limit branches that sort after the given name,
// without wrapping to the first branch.
func branchesAfter(branches []repository.Ref, after string, limit int) []repository.Ref {
	start := sort.Search(len(branches), func(index int) bool { return branches[index].Name > after })
	return branches[start:min(start+limit, len(branches))]
}

func boundedBranchesAfter(branches []repository.Ref, after string, limit int) []repository.Ref {
	if len(branches) == 0 || limit < 1 {
		return nil
	}
	if limit > len(branches) {
		limit = len(branches)
	}
	start := sort.Search(len(branches), func(index int) bool { return branches[index].Name > after })
	if start == len(branches) {
		start = 0
	}
	selected := make([]repository.Ref, 0, limit)
	for offset := 0; offset < limit; offset++ {
		selected = append(selected, branches[(start+offset)%len(branches)])
	}
	return selected
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func boundedSummary(value string) string {
	value = checkapi.SummaryText(value, 500)
	if value == "" {
		return "Configured check execution failed before a command started."
	}
	return value
}

func (coordinator *Coordinator) log(format string, arguments ...any) {
	if coordinator.Logf != nil {
		coordinator.Logf(format, arguments...)
	}
}
