package webui

// Text for the pull request, task, and helper credential screens.
//
// These entries live beside the rest of the catalog rather than inside it so
// one large map does not have to be edited for every new screen. They are
// merged at startup and are indistinguishable afterwards: Text and Has see one catalog.

// Shared evidence vocabulary.
const (
	MsgEvidenceUnknownState MessageCode = "evidence.unknown_state"
	MsgEvidenceAdvisory     MessageCode = "evidence.advisory"
	MsgEvidenceNotStated    MessageCode = "evidence.not_stated"

	// Which revision a result describes, as one short label per row.
	MsgRelevanceCurrent MessageCode = "evidence.relevance.current"
	MsgRelevancePrior   MessageCode = "evidence.relevance.prior"
	MsgRelevanceNone    MessageCode = "evidence.relevance.none"
	MsgRelevanceUnknown MessageCode = "evidence.relevance.unknown"

	// The pull request summary card.
	MsgPRSummaryTitle   MessageCode = "pr.summary.title"
	MsgPRSummaryDetails MessageCode = "pr.summary.details"

	MsgCheckTitle                   MessageCode = "evidence.checks.title"
	MsgCheckStateAbsent             MessageCode = "evidence.checks.absent"
	MsgCheckStatePassed             MessageCode = "evidence.checks.passed"
	MsgCheckStateFailed             MessageCode = "evidence.checks.failed"
	MsgCheckStateError              MessageCode = "evidence.checks.error"
	MsgCheckStateCancelled          MessageCode = "evidence.checks.cancelled"
	MsgCheckStateIncomplete         MessageCode = "evidence.checks.incomplete"
	MsgCheckStateUnavailable        MessageCode = "evidence.checks.unavailable"
	MsgCheckStateStale              MessageCode = "evidence.checks.stale"
	MsgCheckStatePending            MessageCode = "evidence.checks.pending"
	MsgCheckStateUnclean            MessageCode = "evidence.checks.unclean"
	MsgCheckCleanupFailed           MessageCode = "evidence.checks.cleanup_failed"
	MsgCheckCleanupRow              MessageCode = "evidence.checks.cleanup_row"
	MsgCheckRecordUnreadable        MessageCode = "evidence.checks.record_unreadable"
	MsgCheckConfigurationUnreadable MessageCode = "evidence.checks.configuration_unreadable"
	MsgCheckPendingDetail           MessageCode = "evidence.checks.pending_detail"
	MsgCheckRegisteredAt            MessageCode = "evidence.checks.registered_at"

	MsgCheckNotConfigured MessageCode = "evidence.checks.not_configured"
	MsgCheckStaleDetail   MessageCode = "evidence.checks.stale_detail"
	MsgCheckTestedCommit  MessageCode = "evidence.checks.tested_commit"
	MsgCheckNotTested     MessageCode = "evidence.checks.not_tested"
	MsgCheckExitPassed    MessageCode = "evidence.checks.exit_passed"
	MsgCheckRanAt         MessageCode = "evidence.checks.ran_at"
	MsgCheckRevision      MessageCode = "evidence.checks.revision"
	MsgCheckConfigVersion MessageCode = "evidence.checks.configuration"
	MsgCheckAttemptID     MessageCode = "evidence.checks.attempt"
	MsgCheckOpenTask      MessageCode = "evidence.checks.open_task"

	MsgCheckWorktreeClean   MessageCode = "evidence.worktree.clean"
	MsgCheckWorktreeDirty   MessageCode = "evidence.worktree.dirty"
	MsgCheckWorktreeUnknown MessageCode = "evidence.worktree.unknown"

	MsgCheckLogTitle       MessageCode = "evidence.log.title"
	MsgCheckLogAvailable   MessageCode = "evidence.log.available"
	MsgCheckLogExpired     MessageCode = "evidence.log.expired"
	MsgCheckLogNotRecorded MessageCode = "evidence.log.not_recorded"
	MsgCheckLogUnavailable MessageCode = "evidence.log.unavailable"
	MsgCheckLogUnknown     MessageCode = "evidence.log.unknown"
	MsgCheckLogExpiresAt   MessageCode = "evidence.log.expires_at"
	MsgCheckOutputCut      MessageCode = "evidence.log.output_truncated"

	MsgCheckProtectionTitle     MessageCode = "evidence.protection.title"
	MsgCheckProtectionInherited MessageCode = "evidence.protection.inherited"
	MsgCheckProtectionUnknown   MessageCode = "evidence.protection.unknown"
	MsgCheckProvenanceHelper    MessageCode = "evidence.provenance.helper"

	// An automatic job is the server's own work. It never borrows the manual
	// run's wording about the operator's environment and credential.
	MsgCheckProtectionAutoHost      MessageCode = "evidence.protection.auto_host"
	MsgCheckProtectionAutoContainer MessageCode = "evidence.protection.auto_container"
	MsgCheckProtectionRunner        MessageCode = "evidence.protection.runner"
	MsgCheckProvenanceAutomatic     MessageCode = "evidence.provenance.automatic"
	MsgCheckProvenanceRunner        MessageCode = "evidence.provenance.runner"
)

// Review vocabulary.
const (
	MsgReviewTitle            MessageCode = "evidence.review.title"
	MsgReviewStateNone        MessageCode = "evidence.review.none"
	MsgReviewStatePending     MessageCode = "evidence.review.pending"
	MsgReviewStateApproved    MessageCode = "evidence.review.approved"
	MsgReviewStateChanges     MessageCode = "evidence.review.changes_requested"
	MsgReviewStateSkipped     MessageCode = "evidence.review.skipped"
	MsgReviewStateUnavailable MessageCode = "evidence.review.unavailable"
	MsgReviewStatePartial     MessageCode = "evidence.review.partial"
	MsgReviewRecordUnreadable MessageCode = "evidence.review.record_unreadable"

	MsgReviewFromRequest MessageCode = "evidence.review.from_request"
	MsgReviewFromSkip    MessageCode = "evidence.review.from_skip"
	MsgReviewFromTool    MessageCode = "evidence.review.from_tool"
	MsgReviewFromDefault MessageCode = "evidence.review.from_default"

	MsgReviewReviewer       MessageCode = "evidence.review.reviewer"
	MsgReviewNotIndependent MessageCode = "evidence.review.not_independent"
	MsgReviewNoChecksRun    MessageCode = "evidence.review.no_checks_run"
	MsgReviewOtherRevision  MessageCode = "evidence.review.other_revision"
	MsgReviewSubmittedAt    MessageCode = "evidence.review.submitted_at"
	// The revision a review request or skip belongs to. Only a given review
	// names its revision "Tested revision" (MsgCheckRevision).
	MsgReviewRevisionRequested MessageCode = "evidence.review.revision_requested"
	MsgReviewRevisionSkipped   MessageCode = "evidence.review.revision_skipped"
	MsgReviewCoverage          MessageCode = "evidence.review.coverage"
	MsgReviewRequestIntent     MessageCode = "evidence.review.request_intent"
)

// Pull request screens.
const (
	MsgPRTitle         MessageCode = "pr.title"
	MsgPRTabLabel      MessageCode = "pr.tab"
	MsgPRStateOpen     MessageCode = "pr.state.open"
	MsgPRStateMerged   MessageCode = "pr.state.merged"
	MsgPRStateCreating MessageCode = "pr.state.creating"
	MsgPRStateClosed   MessageCode = "pr.state.closed"

	MsgPRListEmpty       MessageCode = "pr.list.empty"
	MsgPRListStart       MessageCode = "pr.list.start"
	MsgPRListUnavailable MessageCode = "pr.list.unavailable"
	MsgPRNumberLabel     MessageCode = "pr.number"
	MsgPRBranchFlow      MessageCode = "pr.branch_flow"
	MsgPRUpdatedAt       MessageCode = "pr.updated_at"

	MsgPRNewTitle          MessageCode = "pr.new.title"
	MsgPRNewIntro          MessageCode = "pr.new.intro"
	MsgPRNewSource         MessageCode = "pr.new.source"
	MsgPRNewTarget         MessageCode = "pr.new.target"
	MsgPRNewCompare        MessageCode = "pr.new.compare"
	MsgPRNewCompareHelp    MessageCode = "pr.new.compare_help"
	MsgPRNewTitleField     MessageCode = "pr.new.title_field"
	MsgPRNewTitleHelp      MessageCode = "pr.new.title_help"
	MsgPRNewReview         MessageCode = "pr.new.review"
	MsgPRNewReviewAsk      MessageCode = "pr.new.review_request"
	MsgPRNewReviewAskHelp  MessageCode = "pr.new.review_request_help"
	MsgPRNewReviewSkip     MessageCode = "pr.new.review_skip"
	MsgPRNewReviewSkipHelp MessageCode = "pr.new.review_skip_help"
	MsgPRNewReviewNone     MessageCode = "pr.new.review_none"
	MsgPRNewReviewNoneHelp MessageCode = "pr.new.review_none_help"
	MsgPRNewSubmit         MessageCode = "pr.new.submit"
	MsgPRNewChangeBranches MessageCode = "pr.new.change_branches"
	MsgPRNewObservedTips   MessageCode = "pr.new.observed_tips"
	MsgPRNewSameBranch     MessageCode = "pr.new.same_branch"
	MsgPRNewNoCommit       MessageCode = "pr.new.no_commit"
	MsgPRNewNoBranches     MessageCode = "pr.new.no_branches"

	MsgPRChangesTitle   MessageCode = "pr.changes.title"
	MsgPRChangesNone    MessageCode = "pr.changes.none"
	MsgPRChangesUnavail MessageCode = "pr.changes.unavailable"
	MsgPRChangesCut     MessageCode = "pr.changes.truncated"
	// MsgPRChangesFilesCut says the list of changed files itself was cut.
	MsgPRChangesFilesCut MessageCode = "pr.changes.files_truncated"
	// MsgPRChangesBase introduces the merge base the changes are counted from.
	MsgPRChangesBase MessageCode = "pr.changes.base"
	// MsgPRChangesNoBase and MsgPRChangesManyBases explain a comparison that
	// is not shown because the branches have no merge base, or several.
	MsgPRChangesNoBase    MessageCode = "pr.changes.no_base"
	MsgPRChangesManyBases MessageCode = "pr.changes.many_bases"
	MsgPRChangesBinary    MessageCode = "pr.changes.binary_file"
	MsgPRChangeAdded      MessageCode = "pr.changes.added"
	MsgPRChangeModified   MessageCode = "pr.changes.modified"
	MsgPRChangeDeleted    MessageCode = "pr.changes.deleted"

	MsgPRSourceLabel     MessageCode = "pr.source"
	MsgPRTargetLabel     MessageCode = "pr.target"
	MsgPRBranchGone      MessageCode = "pr.branch_missing"
	MsgPRBranchNotCommit MessageCode = "pr.branch_not_commit"

	MsgPRActionsTitle      MessageCode = "pr.actions.title"
	MsgPRRequestReview     MessageCode = "pr.actions.request_review"
	MsgPRRequestReviewHelp MessageCode = "pr.actions.request_review_help"
	MsgPRSkipReview        MessageCode = "pr.actions.skip_review"
	MsgPRSkipReviewHelp    MessageCode = "pr.actions.skip_review_help"
	MsgPRMerge             MessageCode = "pr.actions.merge"
	MsgPRMergeHelp         MessageCode = "pr.actions.merge_help"
	MsgPRMergeDespite      MessageCode = "pr.actions.merge_despite"
	MsgPRMergeRefused      MessageCode = "pr.actions.merge_refused"
	MsgPRMergedTitle       MessageCode = "pr.merged.title"
	MsgPRMergedMode        MessageCode = "pr.merged.mode"
	MsgPRMergedCommit      MessageCode = "pr.merged.commit"
	MsgPRMergedReceipt     MessageCode = "pr.merged.receipt"
	MsgPRMergedAt          MessageCode = "pr.merged.at"
	MsgPRClose             MessageCode = "pr.actions.close"
	MsgPRCloseHelp         MessageCode = "pr.actions.close_help"
	MsgPRReopen            MessageCode = "pr.actions.reopen"
	MsgPRReopenHelp        MessageCode = "pr.actions.reopen_help"
	MsgPRClosedNote        MessageCode = "pr.closed.note"
	MsgPRMergedTarget      MessageCode = "pr.merged.target_commit"
	MsgPRMergedFastForward MessageCode = "pr.merged.mode.fast_forward"
	MsgPRMergedMergeCommit MessageCode = "pr.merged.mode.merge_commit"
	MsgPRMergedUpToDate    MessageCode = "pr.merged.mode.up_to_date"
	MsgPRMergedUpToDateFor MessageCode = "pr.merged.up_to_date_note"

	MsgMergeBlockedMerged     MessageCode = "pr.blocked.already_merged"
	MsgMergeBlockedSourceGone MessageCode = "pr.blocked.source_missing"
	MsgMergeBlockedTargetGone MessageCode = "pr.blocked.target_missing"
	MsgMergeBlockedSourceKind MessageCode = "pr.blocked.source_not_commit"
	MsgMergeBlockedTargetKind MessageCode = "pr.blocked.target_not_commit"
	MsgMergeBlockedConflict   MessageCode = "pr.blocked.conflict"
	MsgMergeBlockedStale      MessageCode = "pr.blocked.stale_revision"
	MsgMergeBlockedNotOpen    MessageCode = "pr.blocked.not_open"
	MsgMergeBlockedGitFailed  MessageCode = "pr.blocked.git_failed"
	MsgMergeBlockedOther      MessageCode = "pr.blocked.other"
	MsgMergeUnexplained       MessageCode = "pr.blocked.unexplained"

	MsgPRCreated       MessageCode = "pr.result.created"
	MsgPRReviewAsked   MessageCode = "pr.result.review_requested"
	MsgPRReviewSkipped MessageCode = "pr.result.review_skipped"
	MsgPRMerged        MessageCode = "pr.result.merged"
	MsgPRStale         MessageCode = "pr.result.stale"
	MsgPRMergeBlocked  MessageCode = "pr.result.merge_blocked"
	MsgPRNotFound      MessageCode = "pr.result.not_found"
	MsgPRNotOpen       MessageCode = "pr.result.not_open"
	MsgPRInvalidTitle  MessageCode = "pr.result.invalid_title"
	MsgPRInvalidBranch MessageCode = "pr.result.invalid_branch"
	MsgPRSameBranch    MessageCode = "pr.result.same_branch"
	MsgPRFailed        MessageCode = "pr.result.failed"
	MsgPRReconciling   MessageCode = "pr.result.reconciling"
	MsgPRAlreadyOpen   MessageCode = "pr.result.already_open"
	MsgPRClosedDone    MessageCode = "pr.result.closed"
	MsgPRReopenedDone  MessageCode = "pr.result.reopened"
	MsgPRMergedFixed   MessageCode = "pr.result.merged_fixed"
	MsgPRUpToDate      MessageCode = "pr.result.up_to_date"

	// Automatic merge opt-in, cancel and waiting/blocked reasons.
	MsgAutoMergeTitle                      MessageCode = "pr.auto_merge.title"
	MsgAutoMergeEnable                     MessageCode = "pr.auto_merge.enable"
	MsgAutoMergeCancelButton               MessageCode = "pr.auto_merge.cancel_button"
	MsgAutoMergeHelp                       MessageCode = "pr.auto_merge.help"
	MsgAutoMergeListBadge                  MessageCode = "pr.auto_merge.list_badge"
	MsgAutoMergeEnabled                    MessageCode = "pr.auto_merge.enabled"
	MsgAutoMergeCanceled                   MessageCode = "pr.auto_merge.canceled"
	MsgAutoMergeNotEnabled                 MessageCode = "pr.auto_merge.not_enabled"
	MsgAutoMergeWaitingChecks              MessageCode = "pr.auto_merge.waiting_for_checks"
	MsgAutoMergeChecksRunning              MessageCode = "pr.auto_merge.checks_running"
	MsgAutoMergeEvidenceUnreadable         MessageCode = "pr.auto_merge.evidence_unreadable"
	MsgAutoMergeMergeConflict              MessageCode = "pr.auto_merge.merge_conflict"
	MsgAutoMergeRepositoryUnavailable      MessageCode = "pr.auto_merge.repository_unavailable"
	MsgAutoMergeReconciling                MessageCode = "pr.auto_merge.reconciling"
	MsgAutoMergeChecksFailed               MessageCode = "pr.auto_merge.checks_failed"
	MsgAutoMergeChecksUntrustedWorktree    MessageCode = "pr.auto_merge.untrusted_worktree"
	MsgAutoMergeChecksConfigurationChanged MessageCode = "pr.auto_merge.configuration_changed"

	// The pull request list filter and its next page.
	MsgPRFilterLabel      MessageCode = "pr.filter.label"
	MsgPRFilterAll        MessageCode = "pr.filter.all"
	MsgPRListMore         MessageCode = "pr.list.more"
	MsgPRListEmptyInState MessageCode = "pr.list.empty_state"
)

// Task and check history screens.
const (
	MsgTasksTitle      MessageCode = "tasks.title"
	MsgTasksTabLabel   MessageCode = "tasks.tab"
	MsgTasksIntro      MessageCode = "tasks.intro"
	MsgTasksEmpty      MessageCode = "tasks.empty"
	MsgTasksEmptyHelp  MessageCode = "tasks.empty_help"
	MsgTasksEmptyOlder MessageCode = "tasks.empty_older"
	MsgTasksUnavail    MessageCode = "tasks.unavailable"
	MsgTaskNotFound    MessageCode = "tasks.not_found"
	MsgTasksBackToList MessageCode = "tasks.back_to_list"
	MsgTasksMore       MessageCode = "tasks.more"

	MsgTaskStateActive    MessageCode = "tasks.state.active"
	MsgTaskStateResolved  MessageCode = "tasks.state.resolved"
	MsgTaskStateExhausted MessageCode = "tasks.state.exhausted"

	MsgTaskBudget       MessageCode = "tasks.budget"
	MsgTaskBudgetHelp   MessageCode = "tasks.budget_help"
	MsgTaskRoundsHelp   MessageCode = "tasks.rounds_help"
	MsgTaskManualRerun  MessageCode = "tasks.manual_rerun"
	MsgTaskInitialCheck MessageCode = "tasks.initial_check"
	MsgTaskInitialNone  MessageCode = "tasks.initial_check_none"
	MsgTaskExhausted    MessageCode = "tasks.exhausted_help"
	MsgTaskCreatedAt    MessageCode = "tasks.created_at"
	MsgTaskUpdatedAt    MessageCode = "tasks.updated_at"
	MsgTaskIdentifier   MessageCode = "tasks.identifier"

	MsgAttemptsTitle   MessageCode = "tasks.attempts.title"
	MsgAttemptsNone    MessageCode = "tasks.attempts.none"
	MsgAttemptsCut     MessageCode = "tasks.attempts.truncated"
	MsgAttemptDuration MessageCode = "tasks.attempts.duration"
	MsgAttemptChecks   MessageCode = "tasks.attempts.checks"
	MsgAttemptExitCode MessageCode = "tasks.attempts.exit_code"
	MsgAttemptOrder    MessageCode = "tasks.attempts.order"
	MsgAttemptOrderGap MessageCode = "tasks.attempts.order_gap"
	MsgAttemptOutput   MessageCode = "tasks.attempts.output"
	MsgAttemptNoOutput MessageCode = "tasks.attempts.no_output"

	MsgConfigTitle      MessageCode = "tasks.config.title"
	MsgConfigVersion    MessageCode = "tasks.config.version"
	MsgConfigNone       MessageCode = "tasks.config.none"
	MsgConfigNoneHelp   MessageCode = "tasks.config.none_help"
	MsgConfigRecordedAt MessageCode = "tasks.config.recorded_at"
)

// Helper credential screen.
const (
	MsgHelperTitle      MessageCode = "helper.title"
	MsgHelperIntro      MessageCode = "helper.intro"
	MsgHelperScope      MessageCode = "helper.scope"
	MsgHelperOpen       MessageCode = "helper.open"
	MsgHelperNone       MessageCode = "helper.none"
	MsgHelperListTitle  MessageCode = "helper.list.title"
	MsgHelperLabel      MessageCode = "helper.label"
	MsgHelperLabelHelp  MessageCode = "helper.label_help"
	MsgHelperCreatedAt  MessageCode = "helper.created_at"
	MsgHelperLastUsed   MessageCode = "helper.last_used"
	MsgHelperNeverUsed  MessageCode = "helper.never_used"
	MsgHelperRevokedAt  MessageCode = "helper.revoked_at"
	MsgHelperActive     MessageCode = "helper.active"
	MsgHelperRevoked    MessageCode = "helper.revoked"
	MsgHelperIssue      MessageCode = "helper.issue"
	MsgHelperIssueHelp  MessageCode = "helper.issue_help"
	MsgHelperRevoke     MessageCode = "helper.revoke"
	MsgHelperRevokeHelp MessageCode = "helper.revoke_help"

	MsgHelperTokenTitle MessageCode = "helper.token.title"
	MsgHelperTokenOnce  MessageCode = "helper.token.once"
	MsgHelperTokenStore MessageCode = "helper.token.store"
	MsgHelperTokenLabel MessageCode = "helper.token.label"

	MsgHelperIssued       MessageCode = "helper.result.issued"
	MsgHelperRevokedDone  MessageCode = "helper.result.revoked"
	MsgHelperNotFound     MessageCode = "helper.result.not_found"
	MsgHelperLabelInvalid MessageCode = "helper.result.invalid_label"
	MsgHelperFailed       MessageCode = "helper.result.failed"
	MsgHelperUnreadable   MessageCode = "helper.result.unreadable"
)

// evidenceCatalog holds the text for the screens above. It is merged into the
// shared catalog at startup.
var evidenceCatalog = map[MessageCode]message{
	// -- shared evidence vocabulary ------------------------------------
	MsgEvidenceUnknownState: {
		en: "This state is not one OwnGit recognises. It is shown as recorded and is not treated as a pass.",
		ko: "OwnGit이 아는 상태가 아닙니다. 기록된 그대로 표시하며 통과로 취급하지 않습니다.",
	},
	MsgEvidenceAdvisory: {
		en: "Checks and reviews are information. They never stop you from merging.",
		ko: "테스트, 린트, 빌드 같은 자동 체크와 코드 리뷰는 참고 정보이며 병합을 막지 않습니다.",
	},
	MsgEvidenceNotStated: {en: "Not stated", ko: "기록 없음"},

	MsgCheckTitle:            {en: "Checks", ko: "체크"},
	MsgCheckStateAbsent:      {en: "No check has run for this revision", ko: "이 커밋에서 실행된 체크가 없습니다"},
	MsgCheckStatePassed:      {en: "Checks passed", ko: "체크 통과"},
	MsgCheckStateFailed:      {en: "Checks failed", ko: "체크 실패"},
	MsgCheckStateError:       {en: "A check could not finish", ko: "체크를 끝내지 못했습니다"},
	MsgCheckStateCancelled:   {en: "The run was cancelled", ko: "실행이 취소되었습니다"},
	MsgCheckStateIncomplete:  {en: "The run is incomplete", ko: "실행이 완료되지 않았습니다"},
	MsgCheckStateUnavailable: {en: "The check environment was unavailable", ko: "체크 실행 환경을 사용할 수 없었습니다"},
	MsgCheckStateStale:       {en: "The newest result is for an earlier revision", ko: "가장 최근 결과는 이전 커밋에서 나왔습니다"},
	MsgCheckStatePending:     {en: "Registered, no result yet", ko: "등록됨, 아직 결과 없음"},

	// A non-success label. It states the one thing the record establishes, a
	// cleanup that did not confirm. It does not claim the checks passed,
	// because that reading comes from status metadata the cleanup failure
	// contradicts, and it does not claim the machine was restored. The exit
	// code stays in its own field for a reader who wants it.
	MsgCheckStateUnclean: {en: "Cleanup failed", ko: "정리 실패"},
	// Scoped to the processes this run owned. OwnGit did not confirm they
	// stopped, which is weaker than asserting they are still running and much
	// weaker than asserting the machine is clean.
	MsgCheckCleanupFailed: {
		en: "OwnGit could not confirm that the processes this run started were stopped.",
		ko: "이 실행이 시작한 프로세스가 종료되었는지 확인하지 못했습니다.",
	},
	// Shown on the row that recorded it, under the sentence above that has
	// already explained what an unconfirmed cleanup means.
	MsgCheckCleanupRow: {en: "Cleanup not confirmed", ko: "정리 미확인"},

	// Being unable to read the record is not the same as a run reporting that
	// its environment was unavailable. One is OwnGit failing, the other is a
	// result about the checks.
	MsgCheckRecordUnreadable: {
		en: "The check record could not be read",
		ko: "체크 기록을 읽지 못했습니다",
	},
	MsgCheckConfigurationUnreadable: {
		en: "The check configuration could not be read",
		ko: "체크 설정을 읽지 못했습니다",
	},

	MsgCheckPendingDetail: {
		// Only what the record says. Nothing in this package watches a live
		// process, so the screen must not promise the run is under way.
		en: "OwnGit recorded that this run was registered. No outcome has been reported for it, and this page does not track whether it is still running.",
		ko: "이 실행이 등록되었다는 기록만 있습니다. 결과는 아직 보고되지 않았고, 이 화면은 실행이 진행 중인지까지는 확인하지 않습니다.",
	},
	MsgCheckRegisteredAt: {en: "Registered", ko: "등록 시각"},

	MsgCheckNotConfigured: {
		en: "No checks are configured for this repository yet.",
		ko: "이 저장소에는 아직 설정된 체크가 없습니다.",
	},
	MsgCheckStaleDetail: {
		en: "It says nothing about the code in this pull request.",
		ko: "이 풀 리퀘스트의 코드에 대해서는 아무것도 말해 주지 않습니다.",
	},
	MsgCheckTestedCommit: {
		en: "The working copy matched this commit, so the result describes this code.",
		ko: "워킹 트리가 이 커밋과 같았으므로 이 결과는 이 코드를 체크한 것입니다.",
	},
	MsgCheckNotTested: {
		en: "The result does not prove this commit was the code that ran.",
		ko: "이 커밋이 실제로 실행된 코드였는지는 증명하지 못합니다.",
	},
	MsgCheckExitPassed: {
		en: "The commands finished successfully.",
		ko: "명령이 성공으로 끝났습니다.",
	},
	MsgCheckRanAt:         {en: "Finished", ko: "종료 시각"},
	MsgCheckRevision:      {en: "Tested revision", ko: "대상 커밋"},
	MsgCheckConfigVersion: {en: "Check configuration", ko: "체크 설정"},
	MsgCheckAttemptID:     {en: "Run", ko: "실행"},
	MsgCheckOpenTask:      {en: "Open the task", ko: "작업 열기"},

	MsgCheckWorktreeClean: {
		en: "Working copy: clean",
		ko: "워킹 트리: 변경 없음",
	},
	MsgCheckWorktreeDirty: {
		en: "Working copy: had uncommitted changes",
		ko: "워킹 트리: 커밋하지 않은 변경 있음",
	},
	MsgCheckWorktreeUnknown: {
		en: "Working copy: could not be read",
		ko: "워킹 트리: 읽지 못함",
	},

	MsgCheckLogTitle:       {en: "Raw log", ko: "원본 로그"},
	MsgCheckLogAvailable:   {en: "Kept for now", ko: "아직 보관 중"},
	MsgCheckLogExpired:     {en: "Expired. The recorded result below stays.", ko: "만료되었습니다. 아래 기록된 결과는 남아 있습니다."},
	MsgCheckLogNotRecorded: {en: "Not recorded for this run", ko: "이 실행에서는 기록하지 않았습니다"},
	MsgCheckLogUnavailable: {en: "Could not be read", ko: "읽을 수 없습니다"},
	MsgCheckLogUnknown:     {en: "Not stated", ko: "기록 없음"},
	// Not "삭제 예정": that is the change list's word for a deleted file, and
	// the two appear on the same screen.
	MsgCheckLogExpiresAt: {en: "Removed after", ko: "보관 기한"},
	MsgCheckOutputCut: {
		en: "The stored output is shortened. It is not the whole log.",
		ko: "저장된 출력은 일부만 잘라 둔 것입니다. 전체 로그가 아닙니다.",
	},

	MsgCheckProtectionTitle: {en: "How it ran", ko: "실행 환경"},
	MsgCheckProtectionInherited: {
		en: "In your own development environment, with your permissions. It is not a sandbox.",
		ko: "본인의 개발 환경에서 본인 권한으로 실행되었습니다. 샌드박스가 아닙니다.",
	},
	MsgCheckProtectionUnknown: {
		en: "The protection around this run was not recorded, so nothing about it is claimed.",
		ko: "이 실행의 보호 수준은 기록되지 않았으므로 보호에 관해서는 아무것도 주장하지 않습니다.",
	},
	// An automatic job runs on the server, not on the operator's machine. It
	// is still not a sandbox, and saying so stays as plain here as it is for a
	// manual run.
	MsgCheckProtectionAutoHost: {
		en: "On the OwnGit server host, with the server's permissions. It is not a sandbox.",
		ko: "OwnGit 서버 호스트에서 서버 권한으로 실행되었습니다. 샌드박스가 아닙니다.",
	},
	MsgCheckProtectionAutoContainer: {
		en: "In a container the server started, with the limits in the saved policy.",
		ko: "서버가 시작한 컨테이너에서 저장된 정책의 제한을 적용해 실행되었습니다.",
	},
	MsgCheckProtectionRunner: {
		en: "On an external runner. The protection is what that runner reported, and OwnGit did not verify it.",
		ko: "외부 러너에서 실행되었습니다. 보호 수준은 해당 러너가 보고한 내용이며 OwnGit이 확인한 것이 아닙니다.",
	},
	MsgCheckProvenanceHelper: {
		en: "Reported by the check helper using its own repository credential.",
		ko: "체크 에이전트가 저장소 전용 토큰으로 보고했습니다.",
	},
	MsgCheckProvenanceAutomatic: {
		en: "Run by OwnGit for a job it admitted under the saved policy.",
		ko: "OwnGit이 저장된 정책에 따라 받아들인 작업을 직접 실행했습니다.",
	},
	MsgCheckProvenanceRunner: {
		en: "Claimed and reported by an external runner using a repository runner token.",
		ko: "외부 러너가 저장소 러너 토큰으로 작업을 가져가 보고했습니다.",
	},

	// -- review --------------------------------------------------------
	MsgReviewTitle:            {en: "Review", ko: "리뷰"},
	MsgReviewStateNone:        {en: "No review for this revision", ko: "이 커밋의 리뷰가 없습니다"},
	MsgReviewStatePending:     {en: "Review requested", ko: "리뷰 요청됨"},
	MsgReviewStateApproved:    {en: "Approved", ko: "승인됨"},
	MsgReviewStateChanges:     {en: "Changes requested", ko: "변경 요청됨"},
	MsgReviewStateSkipped:     {en: "Review skipped", ko: "리뷰 건너뜀"},
	MsgReviewStateUnavailable: {en: "The review could not run", ko: "리뷰를 실행하지 못했습니다"},
	MsgReviewStatePartial:     {en: "The review covered only part of the change", ko: "리뷰가 변경의 일부만 다루었습니다"},
	MsgReviewRecordUnreadable: {en: "The review record could not be read", ko: "리뷰 기록을 읽지 못했습니다"},

	MsgReviewFromRequest: {en: "Recorded when a review was requested", ko: "리뷰를 요청할 때 기록되었습니다"},
	MsgReviewFromSkip:    {en: "Recorded as an explicit skip", ko: "명시적으로 건너뛴다고 기록되었습니다"},
	MsgReviewFromTool:    {en: "Supplied by an external coding tool", ko: "외부 코딩 도구가 제출했습니다"},
	MsgReviewFromDefault: {en: "Recorded without a stated source", ko: "출처 없이 기록되었습니다"},

	MsgReviewReviewer: {en: "Reviewer", ko: "리뷰어"},
	MsgReviewNotIndependent: {
		en: "OwnGit did not verify this reviewer independently.",
		ko: "OwnGit이 이 리뷰어의 독립성을 따로 확인하지는 않았습니다.",
	},
	MsgReviewNoChecksRun: {
		en: "The reviewer did not run the project checks.",
		ko: "리뷰어는 프로젝트 체크를 실행하지 않았습니다.",
	},
	MsgReviewOtherRevision: {
		en: "This review belongs to an earlier revision, not to the code shown here.",
		ko: "이 리뷰는 이전 커밋을 리뷰한 것이며 여기 표시된 코드를 리뷰한 것이 아닙니다.",
	},
	MsgReviewSubmittedAt:       {en: "Recorded", ko: "기록 시각"},
	MsgReviewRevisionRequested: {en: "Requested for revision", ko: "요청한 커밋"},
	MsgReviewRevisionSkipped:   {en: "Skipped for revision", ko: "건너뛴 커밋"},
	MsgReviewCoverage:          {en: "Covered", ko: "다룬 범위"},
	MsgReviewRequestIntent: {
		en: "Requesting or skipping records your decision. It does not call a review model.",
		ko: "요청과 건너뛰기는 결정을 기록할 뿐, 리뷰 모델을 호출하지는 않습니다.",
	},

	// -- pull requests -------------------------------------------------
	MsgPRTitle:         {en: "Pull requests", ko: "풀 리퀘스트"},
	MsgPRTabLabel:      {en: "Pull requests", ko: "풀 리퀘스트"},
	MsgPRStateOpen:     {en: "Open", ko: "열림"},
	MsgPRStateMerged:   {en: "Merged", ko: "병합됨"},
	MsgPRStateCreating: {en: "Still being created", ko: "생성 중"},
	MsgPRStateClosed:   {en: "Closed", ko: "닫힘"},

	MsgPRListEmpty: {en: "No pull requests yet.", ko: "아직 풀 리퀘스트가 없습니다."},
	MsgPRListStart: {
		en: "Open one to compare two branches and merge when you are ready.",
		ko: "두 브랜치를 비교하고 원할 때 병합하려면 풀 리퀘스트를 여세요.",
	},
	MsgPRListUnavailable: {
		en: "Pull request records could not be read, so this list is not complete.",
		ko: "풀 리퀘스트 기록을 읽지 못해 목록이 완전하지 않습니다.",
	},
	MsgPRNumberLabel: {en: "Number", ko: "번호"},
	MsgPRBranchFlow:  {en: "into", ko: "→"},
	MsgPRUpdatedAt:   {en: "Updated", ko: "업데이트"},

	MsgPRNewTitle: {en: "New pull request", ko: "새 풀 리퀘스트"},
	MsgPRNewIntro: {
		en: "Choose the branch with your work and the branch it should go into.",
		ko: "작업이 있는 브랜치와 그 작업이 들어갈 브랜치를 고르세요.",
	},
	MsgPRNewSource:  {en: "Branch with your work", ko: "작업이 있는 브랜치"},
	MsgPRNewTarget:  {en: "Branch it goes into", ko: "작업이 들어갈 브랜치"},
	MsgPRNewCompare: {en: "Compare these branches", ko: "두 브랜치 비교"},
	MsgPRNewCompareHelp: {
		en: "OwnGit reads both branch tips and shows the difference before anything is created.",
		ko: "만들기 전에 두 브랜치의 최신 커밋을 읽어 차이를 보여 줍니다.",
	},
	MsgPRNewTitleField: {en: "Title", ko: "제목"},
	MsgPRNewTitleHelp:  {en: "Say what this change does.", ko: "이 변경이 무엇을 하는지 적어 주세요."},
	MsgPRNewReview:     {en: "Review", ko: "리뷰"},
	MsgPRNewReviewAsk:  {en: "Request a review", ko: "리뷰 요청"},
	MsgPRNewReviewAskHelp: {
		en: "Records that you want a review of this revision.",
		ko: "이 커밋에 리뷰가 필요하다고 기록합니다.",
	},
	MsgPRNewReviewSkip: {en: "Skip review", ko: "리뷰 건너뛰기"},
	MsgPRNewReviewSkipHelp: {
		en: "Records that you deliberately went without one.",
		ko: "의도적으로 리뷰 없이 진행했다고 기록합니다.",
	},
	MsgPRNewReviewNone: {en: "Decide later", ko: "나중에 결정"},
	MsgPRNewReviewNoneHelp: {
		en: "Leaves review unrequested. Nothing waits on it.",
		ko: "리뷰를 요청하지 않은 채로 둡니다. 리뷰를 기다리는 일은 없습니다.",
	},
	MsgPRNewSubmit:         {en: "Create pull request", ko: "풀 리퀘스트 만들기"},
	MsgPRNewChangeBranches: {en: "Choose different branches", ko: "다른 브랜치 고르기"},
	MsgPRNewObservedTips: {
		en: "These are the commits OwnGit read just now. If either branch moves before you submit, creating fails instead of using the wrong commit.",
		ko: "방금 읽은 커밋입니다. 제출 전에 브랜치가 움직이면 잘못된 커밋을 쓰지 않고 실패합니다.",
	},
	MsgPRNewSameBranch: {
		en: "Pick two different branches.",
		ko: "서로 다른 브랜치를 골라 주세요.",
	},
	MsgPRNewNoCommit: {
		en: "One of these branches does not point at a commit, so there is nothing to compare.",
		ko: "두 브랜치 중 하나가 커밋을 가리키지 않아 비교할 내용이 없습니다.",
	},
	MsgPRNewNoBranches: {
		en: "This repository needs at least two branches before you can open a pull request.",
		ko: "풀 리퀘스트를 열려면 브랜치가 둘 이상 있어야 합니다.",
	},

	MsgPRChangesTitle: {en: "Changes", ko: "변경 내용"},
	MsgPRChangesNone: {
		en: "The source branch has no changes that the target does not already have.",
		ko: "원본 브랜치에 대상 브랜치로 가져올 새 변경 내용이 없습니다.",
	},
	MsgPRChangesUnavail: {
		en: "The comparison could not be produced, so the change list is missing rather than empty.",
		ko: "비교를 만들지 못했습니다. 바뀐 내용이 없는 것이 아니라 변경 목록을 만들지 못한 것입니다.",
	},
	MsgPRChangesCut: {
		en: "This comparison is incomplete: some files' changes were too large to show. Every changed file is still listed.",
		ko: "이 비교는 일부만 보여 줍니다. 일부 파일은 변경 내용이 너무 커서 표시하지 않았고, 변경된 파일 목록은 모두 표시됩니다.",
	},
	MsgPRChangesFilesCut: {
		en: "This comparison is incomplete: it was too large to read in full, so some changed files are missing from the list.",
		ko: "이 비교는 일부만 보여 줍니다. 비교할 내용이 너무 커서 끝까지 읽지 못했기 때문에 목록에서 빠진 변경 파일이 있습니다.",
	},
	MsgPRChangesBase: {
		en: "Changes the source branch made since it branched off the target. Merge base:",
		ko: "원본 브랜치가 대상 브랜치에서 갈라진 뒤에 바꾼 내용입니다. 병합 기준 커밋:",
	},
	MsgPRChangesNoBase: {
		en: "The source and target branches have no commit in common, so their changes cannot be compared.",
		ko: "원본 브랜치와 대상 브랜치에 공통 커밋이 없어 변경 내용을 비교할 수 없습니다.",
	},
	MsgPRChangesManyBases: {
		en: "The branches have more than one merge base, so where the source branched off is ambiguous. OwnGit does not pick one, so no change list is shown.",
		ko: "두 브랜치의 병합 기준 커밋이 여러 개라서 원본 브랜치가 어디서 갈라졌는지 하나로 정할 수 없습니다. OwnGit은 그중 하나를 임의로 고르지 않으므로 변경 목록을 표시하지 않습니다.",
	},
	MsgPRChangesBinary: {en: "Binary file", ko: "바이너리 파일"},

	// A pull request already contains its change, so these read in the present
	// tense. The restore screen's "will be added" describes an action the
	// reader has not taken yet and would be wrong here.
	MsgPRChangeAdded:    {en: "Added", ko: "추가됨"},
	MsgPRChangeModified: {en: "Changed", ko: "변경됨"},
	MsgPRChangeDeleted:  {en: "Deleted", ko: "삭제됨"},

	MsgPRSourceLabel:     {en: "From", ko: "가져올 브랜치"},
	MsgPRTargetLabel:     {en: "Into", ko: "대상 브랜치"},
	MsgPRBranchGone:      {en: "This branch no longer exists", ko: "이 브랜치가 더 이상 없습니다"},
	MsgPRBranchNotCommit: {en: "This branch does not point at a commit", ko: "이 브랜치가 커밋을 가리키지 않습니다"},

	MsgPRActionsTitle:  {en: "What you can do", ko: "할 수 있는 일"},
	MsgPRRequestReview: {en: "Request a review", ko: "리뷰 요청"},
	MsgPRRequestReviewHelp: {
		en: "Records the request against the current revision.",
		ko: "현재 커밋의 리뷰 요청으로 기록합니다.",
	},
	MsgPRSkipReview: {en: "Skip review", ko: "리뷰 건너뛰기"},
	MsgPRSkipReviewHelp: {
		en: "Records that you went ahead without one.",
		ko: "리뷰 없이 진행했다고 기록합니다.",
	},
	MsgPRMerge: {en: "Merge", ko: "병합"},
	MsgPRMergeHelp: {
		en: "Merges the current revision into the target branch. A branch that moved in the meantime fails instead of being overwritten.",
		ko: "현재 커밋을 대상 브랜치에 병합합니다. 그 사이 브랜치가 움직였다면 덮어쓰지 않고 실패합니다.",
	},
	MsgPRMergeDespite: {
		en: "You can merge with checks or a review still failing or pending. The result above stays as it is; merging does not turn it into a pass.",
		ko: "체크나 리뷰가 실패했거나 대기 중이어도 병합할 수 있습니다. 위의 결과는 그대로 남으며 병합해도 통과로 바뀌지 않습니다.",
	},
	MsgPRMergeRefused: {
		en: "Merging is refused for the reasons below. These are Git and repository reasons, not check or review results.",
		ko: "아래 이유로 병합이 거부됩니다. 체크나 리뷰 결과가 아니라 Git과 저장소 상태의 문제입니다.",
	},
	MsgPRMergedTitle:   {en: "Merged", ko: "병합됨"},
	MsgPRMergedMode:    {en: "Method", ko: "방식"},
	MsgPRMergedCommit:  {en: "Merge commit", ko: "병합 커밋"},
	MsgPRMergedReceipt: {en: "Receipt", ko: "병합 기록 ref"},
	MsgPRMergedAt:      {en: "Merged", ko: "병합 시각"},
	MsgPRMergedTarget:  {en: "Target branch commit", ko: "대상 브랜치 커밋"},
	MsgPRClose:         {en: "Close pull request", ko: "풀 리퀘스트 닫기"},
	MsgPRCloseHelp: {
		en: "Closes it without merging. Nothing changes in either branch, the history stays, and you can reopen it later.",
		ko: "병합하지 않고 닫습니다. 두 브랜치는 그대로이고 기록도 남으며 나중에 다시 열 수 있습니다.",
	},
	MsgPRReopen: {en: "Reopen pull request", ko: "풀 리퀘스트 다시 열기"},
	MsgPRReopenHelp: {
		en: "Opens it again for review and merging. This is refused while another pull request is open for the same two branches.",
		ko: "다시 열어 리뷰하고 병합할 수 있게 합니다. 같은 두 브랜치로 열린 다른 풀 리퀘스트가 있으면 다시 열 수 없습니다.",
	},
	MsgPRClosedNote: {
		en: "This pull request was closed without merging.",
		ko: "이 풀 리퀘스트는 병합하지 않고 닫혔습니다.",
	},
	// Fast-forward is established Git terminology and stays in English.
	MsgPRMergedFastForward: {en: "Fast-forward", ko: "fast-forward"},
	MsgPRMergedMergeCommit: {en: "Merge commit", ko: "병합 커밋"},
	MsgPRMergedUpToDate:    {en: "Already up to date, no new commit", ko: "이미 최신 상태, 새 커밋 없음"},
	MsgPRMergedUpToDateFor: {
		en: "The target branch already contained this work, so OwnGit left it unchanged and recorded the pull request as merged.",
		ko: "대상 브랜치에 이 작업이 이미 들어 있어 브랜치는 그대로 두고 풀 리퀘스트를 병합됨으로 기록했습니다.",
	},

	MsgMergeBlockedMerged:     {en: "This pull request is already merged.", ko: "이미 병합된 풀 리퀘스트입니다."},
	MsgMergeBlockedSourceGone: {en: "The branch with your work no longer exists.", ko: "작업이 있던 브랜치가 더 이상 없습니다."},
	MsgMergeBlockedTargetGone: {en: "The target branch no longer exists.", ko: "대상 브랜치가 더 이상 없습니다."},
	MsgMergeBlockedSourceKind: {en: "The branch with your work does not point at a commit.", ko: "작업이 있는 브랜치가 커밋을 가리키지 않습니다."},
	MsgMergeBlockedTargetKind: {en: "The target branch does not point at a commit.", ko: "대상 브랜치가 커밋을 가리키지 않습니다."},
	MsgMergeBlockedConflict: {
		en: "The same lines changed on both branches, so Git cannot combine them automatically.",
		ko: "두 브랜치에서 같은 부분이 바뀌어 Git이 자동으로 합칠 수 없습니다.",
	},
	MsgMergeBlockedStale: {
		en: "A branch moved after this page was loaded, so nothing was changed.",
		ko: "페이지를 연 뒤 브랜치가 움직여 아무것도 바뀌지 않았습니다.",
	},
	MsgMergeBlockedNotOpen: {en: "This pull request is no longer open.", ko: "이 풀 리퀘스트는 더 이상 열려 있지 않습니다."},
	MsgMergeBlockedGitFailed: {
		en: "Git refused the update, so the target branch is unchanged.",
		ko: "Git이 브랜치 업데이트를 거부해 대상 브랜치는 그대로입니다.",
	},
	MsgMergeBlockedOther: {en: "OwnGit refused this merge for the reason recorded below.", ko: "아래 기록된 이유로 병합이 거부되었습니다."},
	MsgMergeUnexplained: {
		// No reason was recorded, so none is invented. Reloading is the one
		// thing that can produce a current answer.
		en: "No reason was recorded with this refusal. Reload to check the current state of both branches.",
		ko: "이 거부에는 이유가 기록되지 않았습니다. 페이지를 새로 고쳐 두 브랜치의 현재 상태를 확인하세요.",
	},

	MsgPRCreated:       {en: "Pull request created.", ko: "풀 리퀘스트를 만들었습니다."},
	MsgPRReviewAsked:   {en: "Review requested.", ko: "리뷰를 요청했습니다."},
	MsgPRReviewSkipped: {en: "Recorded as skipped.", ko: "건너뛴 것으로 기록했습니다."},
	MsgPRMerged:        {en: "Merged.", ko: "병합했습니다."},
	MsgPRStale: {
		en: "A branch moved since this page was loaded, so nothing was changed. Reload and check the new commits first.",
		ko: "페이지를 연 뒤 브랜치가 움직여 아무것도 바뀌지 않았습니다. 페이지를 새로 고쳐 새 커밋부터 확인하세요.",
	},
	MsgPRMergeBlocked:     {en: "The merge was refused.", ko: "병합이 거부되었습니다."},
	MsgPRNotFound:         {en: "That pull request does not exist.", ko: "해당 풀 리퀘스트가 없습니다."},
	MsgPRNotOpen:          {en: "That pull request is not open.", ko: "해당 풀 리퀘스트는 열려 있지 않습니다."},
	MsgPRInvalidTitle:     {en: "Enter a title on one line, in 500 bytes or fewer: up to 500 characters in English, or about 160 in Korean.", ko: "제목은 줄바꿈 없이 500바이트 이내로 입력하세요. 영문은 500자, 한글은 약 160자입니다."},
	MsgPRInvalidBranch:    {en: "That branch name cannot be used here.", ko: "여기서는 사용할 수 없는 브랜치 이름입니다."},
	MsgPRSameBranch:       {en: "Pick two different branches.", ko: "서로 다른 브랜치를 골라 주세요."},
	MsgPRFailed:           {en: "The pull request operation did not complete.", ko: "풀 리퀘스트 작업을 끝내지 못했습니다."},
	MsgPRClosedDone:       {en: "Pull request closed.", ko: "풀 리퀘스트를 닫았습니다."},
	MsgPRReopenedDone:     {en: "Pull request reopened.", ko: "풀 리퀘스트를 다시 열었습니다."},
	MsgPRFilterLabel:      {en: "Pull request state", ko: "풀 리퀘스트 상태"},
	MsgPRFilterAll:        {en: "All", ko: "전체"},
	MsgPRListMore:         {en: "Show older pull requests", ko: "이전 풀 리퀘스트 더 보기"},
	MsgPRListEmptyInState: {en: "No pull requests in this state.", ko: "이 상태의 풀 리퀘스트가 없습니다."},
	MsgPRMergedFixed: {
		en: "A merged pull request cannot be closed or reopened.",
		ko: "병합된 풀 리퀘스트는 닫거나 다시 열 수 없습니다.",
	},
	MsgPRAlreadyOpen: {
		en: "A pull request is already open for these two branches:",
		ko: "이 두 브랜치로 이미 열려 있는 풀 리퀘스트가 있습니다:",
	},
	MsgPRUpToDate: {
		en: "Already up to date. The target branch already contained this work, so no commit was made.",
		ko: "이미 최신 상태입니다. 대상 브랜치에 이 작업이 이미 들어 있어 커밋을 만들지 않았습니다.",
	},
	MsgAutoMergeTitle: {
		en: "Automatic merge",
		ko: "자동 병합",
	},
	MsgAutoMergeEnable: {
		en: "Merge automatically when checks pass",
		ko: "검사 통과 시 자동으로 병합",
	},
	MsgAutoMergeCancelButton: {
		en: "Cancel automatic merge",
		ko: "자동 병합 취소",
	},
	MsgAutoMergeHelp: {
		en: "OwnGit merges this request itself when the current source commit's checks passed on a clean worktree and the branches merge without a conflict. You can still merge manually. Canceling publishes nothing.",
		ko: "현재 원본 커밋의 검사가 깨끗한 작업 트리에서 통과하고 브랜치가 충돌 없이 병합되면 OwnGit이 이 요청을 직접 병합합니다. 수동으로 병합할 수도 있습니다. 취소해도 병합이 게시되지는 않습니다.",
	},
	MsgAutoMergeListBadge: {
		en: "Automatic merge",
		ko: "자동 병합",
	},
	MsgAutoMergeEnabled: {
		en: "Automatic merging enabled. OwnGit merges this request itself once the checks of the current source commit pass.",
		ko: "자동 병합을 켰습니다. 현재 원본 커밋의 검사를 통과하면 OwnGit이 이 요청을 직접 병합합니다.",
	},
	MsgAutoMergeCanceled: {
		en: "Automatic merging canceled.",
		ko: "자동 병합을 취소했습니다.",
	},
	MsgAutoMergeNotEnabled: {
		en: "Automatic merging is not enabled for this pull request.",
		ko: "이 풀 리퀘스트에는 자동 병합이 켜져 있지 않습니다.",
	},
	MsgAutoMergeWaitingChecks: {
		en: "Waiting for passing checks of the current source commit.",
		ko: "현재 원본 커밋의 통과 검사를 기다리고 있습니다.",
	},
	MsgAutoMergeChecksRunning: {
		en: "Checks of the current source commit are still running.",
		ko: "현재 원본 커밋의 검사가 아직 실행 중입니다.",
	},
	MsgAutoMergeEvidenceUnreadable: {
		en: "The check records could not be read, so the automatic merge waits and retries.",
		ko: "검사 기록을 읽을 수 없어 자동 병합이 기다리며 다시 시도합니다.",
	},
	MsgAutoMergeMergeConflict: {
		en: "The current revisions conflict. The automatic merge waits until a branch change resolves them.",
		ko: "현재 리비전이 충돌합니다. 브랜치 변경으로 해결될 때까지 자동 병합이 기다립니다.",
	},
	MsgAutoMergeRepositoryUnavailable: {
		en: "The repository is temporarily unavailable, so the automatic merge waits and retries.",
		ko: "저장소를 일시적으로 사용할 수 없어 자동 병합이 기다리며 다시 시도합니다.",
	},
	MsgAutoMergeReconciling: {
		en: "The current source commit passed its checks and merges cleanly. OwnGit is publishing the merge.",
		ko: "현재 원본 커밋이 검사를 통과했고 깨끗이 병합됩니다. OwnGit이 병합을 게시하는 중입니다.",
	},
	MsgAutoMergeChecksFailed: {
		en: "The latest checks of the current source commit did not pass.",
		ko: "현재 원본 커밋의 최근 검사가 통과하지 못했습니다.",
	},
	MsgAutoMergeChecksUntrustedWorktree: {
		en: "The passing checks were not recorded from a clean worktree of the current source commit, so they cannot publish an automatic merge.",
		ko: "통과한 검사가 현재 원본 커밋의 깨끗한 작업 트리에서 기록되지 않아 자동 병합을 게시할 수 없습니다.",
	},
	MsgAutoMergeChecksConfigurationChanged: {
		en: "The passing checks used a different check configuration than the workflow committed at the current source commit.",
		ko: "통과한 검사가 현재 원본 커밋에 커밋된 워크플로와 다른 검사 구성을 사용했습니다.",
	},
	MsgPRReconciling: {
		en: "Your work was kept, but OwnGit still has to finish recording it. Reload shortly.",
		ko: "작업은 그대로 남아 있지만 OwnGit이 아직 기록을 마무리해야 합니다. 잠시 뒤 페이지를 새로 고치세요.",
	},

	// -- tasks ---------------------------------------------------------
	MsgTasksTitle:    {en: "Checks and tasks", ko: "체크와 작업"},
	MsgTasksTabLabel: {en: "Checks", ko: "체크"},
	MsgTasksIntro: {
		en: "What the check helper actually ran, and which commit each result belongs to.",
		ko: "체크 에이전트가 실행한 명령과 각 결과가 어느 커밋에 해당하는지 보여 줍니다.",
	},
	MsgTasksEmpty: {en: "No task has been recorded yet.", ko: "아직 기록된 작업이 없습니다."},
	MsgTasksEmptyHelp: {
		en: "Tasks appear here once the check helper reports a run for this repository.",
		ko: "체크 에이전트가 이 저장소의 실행 결과를 보고하면 여기에 나타납니다.",
	},
	MsgTasksEmptyOlder: {en: "No older tasks remain.", ko: "더 이전 작업이 없습니다."},
	MsgTasksUnavail: {
		en: "Task records could not be read, so this list is not complete.",
		ko: "작업 기록을 읽지 못해 목록이 완전하지 않습니다.",
	},
	MsgTaskNotFound:    {en: "That task does not exist.", ko: "해당 작업이 없습니다."},
	MsgTasksBackToList: {en: "All tasks", ko: "작업 목록"},
	MsgTasksMore:       {en: "Show older tasks", ko: "이전 작업 더 보기"},

	MsgTaskStateActive:    {en: "Active", ko: "진행 중"},
	MsgTaskStateResolved:  {en: "Resolved", ko: "해결됨"},
	MsgTaskStateExhausted: {en: "Correction attempts used up", ko: "수정 시도 소진"},

	MsgTaskBudget: {en: "Correction rounds", ko: "수정 라운드"},
	MsgTaskRoundsHelp: {
		// A round is reserved before it runs, and a round that succeeds still
		// counts. Saying so stops the reader expecting the number to move only
		// on failure.
		en: "A round is counted when it is reserved, including a round that fixes the problem.",
		ko: "라운드는 예약될 때 집계되며 문제를 해결한 라운드도 포함됩니다.",
	},
	MsgTaskManualRerun: {
		en: "Running the checks yourself does not use a round.",
		ko: "직접 체크를 실행해도 라운드를 소모하지 않습니다.",
	},
	MsgTaskInitialCheck: {en: "First check", ko: "첫 체크"},
	MsgTaskInitialNone: {
		en: "The first check for this task has not been recorded yet.",
		ko: "이 작업의 첫 체크는 아직 기록되지 않았습니다.",
	},
	MsgTaskBudgetHelp: {
		en: "The budget belongs to the task. Moving to a new commit does not give it more rounds.",
		ko: "라운드 한도는 작업마다 정해집니다. 새 커밋으로 옮겨도 라운드가 늘어나지 않습니다.",
	},
	MsgTaskExhausted: {
		en: "Automatic correction has stopped for this task. Your work and its history are untouched, and Git operations are not restricted.",
		ko: "이 작업의 자동 수정은 멈췄습니다. 작업물과 기록은 그대로이며 Git 작업도 제한되지 않습니다.",
	},
	MsgTaskCreatedAt:  {en: "Started", ko: "시작"},
	MsgTaskUpdatedAt:  {en: "Last change", ko: "마지막 변경"},
	MsgTaskIdentifier: {en: "Task", ko: "작업"},

	MsgAttemptsTitle: {en: "Runs", ko: "실행 기록"},
	MsgAttemptsNone: {
		en: "This task has no recorded run yet.",
		ko: "이 작업에는 아직 기록된 실행이 없습니다.",
	},
	MsgAttemptsCut: {
		en: "Only the most recent runs are shown.",
		ko: "가장 최근 실행만 표시합니다.",
	},
	MsgAttemptDuration: {en: "Took", ko: "소요"},
	MsgAttemptChecks:   {en: "Commands", ko: "명령"},
	MsgAttemptExitCode: {en: "Exit code", ko: "종료 코드"},
	MsgAttemptOrder:    {en: "Registration order", ko: "등록 순번"},
	// The number counts registrations across the whole repository, so gaps
	// within one task are expected and do not mean an attempt is missing.
	MsgAttemptOrderGap: {
		en: "Counted across the whole repository, so numbers within one task can skip.",
		ko: "저장소 전체 기준 순번이라 한 작업 안에서는 번호가 건너뛸 수 있습니다.",
	},
	MsgAttemptOutput:   {en: "Output", ko: "출력"},
	MsgAttemptNoOutput: {en: "No output was kept for this command.", ko: "이 명령의 출력은 보관되지 않았습니다."},

	MsgConfigTitle:   {en: "Latest recorded check configuration", ko: "가장 최근에 기록된 체크 구성"},
	MsgConfigVersion: {en: "Version", ko: "버전"},
	MsgConfigNone:    {en: "No check configuration has been recorded yet.", ko: "아직 기록된 체크 구성이 없습니다."},
	MsgConfigNoneHelp: {
		en: "Until then, nothing here reports on this repository's code.",
		ko: "그때까지 여기에는 이 저장소 코드에 관한 보고가 없습니다.",
	},
	MsgConfigRecordedAt: {en: "Recorded", ko: "기록 시각"},

	// -- helper credentials --------------------------------------------
	MsgHelperTitle: {en: "Check helper credentials", ko: "체크 에이전트 토큰"},
	MsgHelperIntro: {
		en: "The check helper reports results with its own credential, separate from repository access and from the administrator password.",
		ko: "체크 에이전트가 결과를 보고할 때 쓰는 전용 토큰입니다. 저장소 접근 권한이나 관리자 비밀번호와는 별개입니다.",
	},
	MsgHelperScope: {
		en: "A credential works only for this repository and only for reporting check evidence. You can revoke it at any time.",
		ko: "이 토큰은 이 저장소의 체크 결과를 보고할 때만 쓸 수 있습니다. 언제든 폐기할 수 있습니다.",
	},
	MsgHelperOpen:      {en: "Helper credentials", ko: "체크 에이전트 토큰"},
	MsgHelperNone:      {en: "No credential has been issued for this repository.", ko: "이 저장소에 발급된 토큰이 없습니다."},
	MsgHelperListTitle: {en: "Issued credentials", ko: "발급된 토큰"},
	MsgHelperLabel:     {en: "Label", ko: "이름"},
	MsgHelperLabelHelp: {
		en: "Name the machine or tool that will use it, so you know what you are revoking later.",
		ko: "나중에 무엇을 폐기하는지 알 수 있도록 사용할 기기나 도구 이름을 적어 두세요.",
	},
	MsgHelperCreatedAt: {en: "Issued", ko: "발급"},
	MsgHelperLastUsed:  {en: "Last used", ko: "마지막 사용"},
	MsgHelperNeverUsed: {en: "Never used", ko: "사용한 적 없음"},
	MsgHelperRevokedAt: {en: "Revoked", ko: "폐기한 때"},
	MsgHelperActive:    {en: "Active", ko: "사용 중"},
	MsgHelperRevoked:   {en: "Revoked", ko: "폐기됨"},
	MsgHelperIssue:     {en: "Issue a credential", ko: "토큰 발급"},
	MsgHelperIssueHelp: {
		en: "The secret is shown once, right after it is issued.",
		ko: "토큰은 발급 직후 한 번만 표시됩니다.",
	},
	MsgHelperRevoke: {en: "Revoke", ko: "폐기"},
	MsgHelperRevokeHelp: {
		en: "The helper stops being able to report with it immediately. Results it already reported stay.",
		ko: "체크 에이전트는 이 토큰으로 즉시 보고할 수 없게 됩니다. 이미 보고된 결과는 남습니다.",
	},

	MsgHelperTokenTitle: {en: "Copy this now", ko: "지금 토큰을 복사하세요"},
	MsgHelperTokenOnce: {
		en: "This is the only time OwnGit shows this secret. It keeps only a hash, so leaving this page loses the value for good.",
		ko: "이 토큰은 지금 한 번만 볼 수 있습니다. OwnGit은 토큰의 해시만 보관하므로 이 페이지를 떠나면 다시 확인할 수 없습니다.",
	},
	MsgHelperTokenStore: {
		en: "Put it straight into the helper's configuration or your password manager. Do not paste it into a shared chat or a file in the repository.",
		ko: "체크 에이전트 설정이나 비밀번호 관리자에 바로 저장하세요. 공유 채팅이나 저장소 파일에는 붙여 넣지 마세요.",
	},
	MsgHelperTokenLabel: {en: "Credential secret", ko: "토큰"},

	MsgHelperIssued:      {en: "Credential issued.", ko: "토큰을 발급했습니다."},
	MsgHelperRevokedDone: {en: "Credential revoked.", ko: "토큰을 폐기했습니다."},
	MsgHelperNotFound:    {en: "That credential was not found or was already revoked.", ko: "해당 토큰이 없거나 이미 폐기되었습니다."},
	MsgHelperLabelInvalid: {
		en: "Enter a single-line label between 1 and 100 UTF-8 bytes.",
		ko: "한 줄 이름을 UTF-8 기준 100바이트 이내로 입력하세요. 한글만 쓰면 최대 33자입니다.",
	},
	MsgHelperFailed: {en: "The credential operation did not complete.", ko: "토큰 작업을 끝내지 못했습니다."},
	MsgHelperUnreadable: {
		en: "The helper credential records could not be read, so this screen could not be shown. Reload the page to try again.",
		ko: "체크 에이전트 토큰 기록을 읽지 못해 이 화면을 열 수 없습니다. 페이지를 다시 불러오세요.",
	},

	// -- summary relevance ---------------------------------------------
	// Four distinct answers to one question: which code does this result
	// describe. "Not recorded" and "could not be determined" are separate
	// because an absent record and an unreadable one are different facts.
	MsgRelevanceCurrent: {en: "This revision", ko: "현재 변경 기준"},
	MsgRelevancePrior:   {en: "An earlier revision", ko: "이전 변경 결과"},
	MsgRelevanceNone:    {en: "Nothing recorded", ko: "기록 없음"},
	MsgRelevanceUnknown: {en: "Could not be determined", ko: "확인 못함"},

	MsgPRSummaryTitle:   {en: "What is known about this change", ko: "이 변경에서 확인된 내용"},
	MsgPRSummaryDetails: {en: "Details and findings", ko: "자세한 내용과 지적 보기"},
}

// init merges the entries above into the shared catalog.
//
// A duplicate code would mean two definitions of one sentence, with whichever
// file loaded last silently winning. That is a mistake in this package, so it
// fails at startup the same way an unparsable template does.
func init() {
	registerMessages(evidenceCatalog)
}
