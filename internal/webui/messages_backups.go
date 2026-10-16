package webui

// Backups that OwnGit makes while it serves: the warnings shown when an
// owner turns a part of them off.
const (
	MsgBackupScheduleOffWarning MessageCode = "backup.schedule_off_warning"
	MsgBackupVerifyOffWarning   MessageCode = "backup.verify_off_warning"
)

// The Backups group of Storage & recovery.
const (
	MsgBackupTitle                MessageCode = "backup.title"
	MsgBackupScope                MessageCode = "backup.scope"
	MsgBackupNotConfigured        MessageCode = "backup.not_configured"
	MsgBackupDestination          MessageCode = "backup.destination"
	MsgBackupDestinationHelp      MessageCode = "backup.destination_help"
	MsgBackupScheduled            MessageCode = "backup.scheduled"
	MsgBackupScheduledHelp        MessageCode = "backup.scheduled_help"
	MsgBackupInterval             MessageCode = "backup.interval"
	MsgBackupEvery12h             MessageCode = "backup.every_12h"
	MsgBackupEveryDay             MessageCode = "backup.every_day"
	MsgBackupEvery7d              MessageCode = "backup.every_7d"
	MsgBackupIntervalHelp         MessageCode = "backup.interval_help"
	MsgBackupKeep                 MessageCode = "backup.keep"
	MsgBackupKeepHelp             MessageCode = "backup.keep_help"
	MsgBackupKeepInvalid          MessageCode = "backup.keep_invalid"
	MsgBackupVerify               MessageCode = "backup.verify"
	MsgBackupVerifyHelp           MessageCode = "backup.verify_help"
	MsgBackupScheduleRefused      MessageCode = "backup.schedule_refused"
	MsgBackupSaved                MessageCode = "backup.saved"
	MsgBackupPlansTitle           MessageCode = "backup.plans_title"
	MsgBackupPlan                 MessageCode = "backup.plan"
	MsgBackupPlanName             MessageCode = "backup.plan_name"
	MsgBackupPlanNameHelp         MessageCode = "backup.plan_name_help"
	MsgBackupNewPlan              MessageCode = "backup.new_plan"
	MsgBackupCreatePlan           MessageCode = "backup.create_plan"
	MsgBackupDeletePlan           MessageCode = "backup.delete_plan"
	MsgBackupDeletePlanHelp       MessageCode = "backup.delete_plan_help"
	MsgBackupPlanCreated          MessageCode = "backup.plan_created"
	MsgBackupPlanDeleted          MessageCode = "backup.plan_deleted"
	MsgBackupPlanChoose           MessageCode = "backup.plan_choose"
	MsgBackupActiveRefused        MessageCode = "backup.plan_active_refused"
	MsgBackupPlanExistsRefused    MessageCode = "backup.plan_exists_refused"
	MsgBackupPlanMissingRefused   MessageCode = "backup.plan_missing_refused"
	MsgBackupPlanLimitRefused     MessageCode = "backup.plan_limit_refused"
	MsgBackupStateTitle           MessageCode = "backup.state_title"
	MsgBackupStateOn              MessageCode = "backup.state_on"
	MsgBackupStateOff             MessageCode = "backup.state_off"
	MsgBackupStateNotConfigured   MessageCode = "backup.state_not_configured"
	MsgBackupRunningNow           MessageCode = "backup.running_now"
	MsgBackupRunningSince         MessageCode = "backup.running_since"
	MsgBackupNone                 MessageCode = "backup.none"
	MsgBackupLastRun              MessageCode = "backup.last_run"
	MsgBackupLastVerified         MessageCode = "backup.last_verified"
	MsgBackupNextRun              MessageCode = "backup.next_run"
	MsgBackupNextRunSoon          MessageCode = "backup.next_run_soon"
	MsgBackupNow                  MessageCode = "backup.now"
	MsgBackupStarted              MessageCode = "backup.started"
	MsgBackupBusy                 MessageCode = "backup.busy"
	MsgBackupWorkEnded            MessageCode = "backup.work_ended"
	MsgBackupRunningRefused       MessageCode = "backup.running_refused"
	MsgBackupChooseFolder         MessageCode = "backup.choose_folder"
	MsgBackupRestoreLimit         MessageCode = "backup.restore_limit"
	MsgBackupRunsTitle            MessageCode = "backup.runs_title"
	MsgBackupRunsEmpty            MessageCode = "backup.runs_empty"
	MsgBackupStartedAt            MessageCode = "backup.started_at"
	MsgBackupKind                 MessageCode = "backup.kind"
	MsgBackupStatus               MessageCode = "backup.status"
	MsgBackupVerification         MessageCode = "backup.verification"
	MsgBackupHold                 MessageCode = "backup.hold"
	MsgBackupHoldValue            MessageCode = "backup.hold_value"
	MsgBackupHoldNone             MessageCode = "backup.hold_none"
	MsgBackupMessage              MessageCode = "backup.message"
	MsgBackupNotKept              MessageCode = "backup.not_kept"
	MsgBackupKindScheduled        MessageCode = "backup.kind_scheduled"
	MsgBackupKindManual           MessageCode = "backup.kind_manual"
	MsgBackupStatusRunning        MessageCode = "backup.status_running"
	MsgBackupStatusSucceeded      MessageCode = "backup.status_succeeded"
	MsgBackupStatusFailed         MessageCode = "backup.status_failed"
	MsgBackupStatusInterrupted    MessageCode = "backup.status_interrupted"
	MsgBackupVerifyPassed         MessageCode = "backup.verify_passed"
	MsgBackupVerifyFailed         MessageCode = "backup.verify_failed"
	MsgBackupVerifyNotRun         MessageCode = "backup.verify_not_run"
	MsgBackupActions              MessageCode = "backup.actions"
	MsgBackupVerifyAgain          MessageCode = "backup.verify_again"
	MsgBackupCheckStarted         MessageCode = "backup.check_started"
	MsgBackupCheckTitle           MessageCode = "backup.check_title"
	MsgBackupCheckRunning         MessageCode = "backup.check_running"
	MsgBackupCheckPassed          MessageCode = "backup.check_passed"
	MsgBackupCheckFailed          MessageCode = "backup.check_failed"
	MsgBackupGone                 MessageCode = "backup.gone"
	MsgBackupNoBackup             MessageCode = "backup.no_backup"
	MsgBackupDownload             MessageCode = "backup.download"
	MsgBackupDownloadHelp         MessageCode = "backup.download_help"
	MsgBackupRestoreTitle         MessageCode = "backup.restore_title"
	MsgBackupRestoreLead          MessageCode = "backup.restore_lead"
	MsgBackupRestoreStopService   MessageCode = "backup.restore_stop_service"
	MsgBackupRestoreStopProcess   MessageCode = "backup.restore_stop_process"
	MsgBackupRestoreMove          MessageCode = "backup.restore_move"
	MsgBackupRestoreRun           MessageCode = "backup.restore_run"
	MsgBackupRestoreUnchecked     MessageCode = "backup.restore_unchecked"
	MsgBackupRestoreCommand       MessageCode = "backup.restore_command"
	MsgBackupRestoreNetwork       MessageCode = "backup.restore_network"
	MsgBackupRestoreStartService  MessageCode = "backup.restore_start_service"
	MsgBackupRestoreStartProcess  MessageCode = "backup.restore_start_process"
	MsgBackupRestoreAfter         MessageCode = "backup.restore_after"
	MsgBackupUploadTitle          MessageCode = "backup.upload_title"
	MsgBackupUploadHelp           MessageCode = "backup.upload_help"
	MsgBackupUploadFile           MessageCode = "backup.upload_file"
	MsgBackupUploadSend           MessageCode = "backup.upload_send"
	MsgBackupUploadReceived       MessageCode = "backup.upload_received"
	MsgBackupUploadRefused        MessageCode = "backup.upload_refused"
	MsgBackupUploadMissing        MessageCode = "backup.upload_missing"
	MsgBackupUploadNoSize         MessageCode = "backup.upload_no_size"
	MsgBackupUploadName           MessageCode = "backup.upload_name"
	MsgBackupUploadRemovesAt      MessageCode = "backup.upload_removes_at"
	MsgBackupUploadVerifying      MessageCode = "backup.upload_verifying"
	MsgBackupUploadPassed         MessageCode = "backup.upload_passed"
	MsgBackupUploadFailed         MessageCode = "backup.upload_failed"
	MsgBackupFailed               MessageCode = "backup.failed"
	MsgBackupFolderOverlaps       MessageCode = "backup.folder_overlaps"
	MsgBackupUploadEndsEarly      MessageCode = "backup.upload_ends_early"
	MsgBackupUploadNoManifest     MessageCode = "backup.upload_no_manifest"
	MsgBackupUploadDeclaredNoSize MessageCode = "backup.upload_declared_no_size"
	MsgBackupUploadStopped        MessageCode = "backup.upload_stopped"
	MsgBackupUploadUnsafePath     MessageCode = "backup.upload_unsafe_path"
	MsgBackupUploadFolderName     MessageCode = "backup.upload_folder_name"
	MsgBackupUploadManyFolders    MessageCode = "backup.upload_many_folders"
	MsgBackupUploadDuplicate      MessageCode = "backup.upload_duplicate"
	MsgBackupUploadUnknownEntry   MessageCode = "backup.upload_unknown_entry"
	MsgBackupUploadBeforeFolder   MessageCode = "backup.upload_before_folder"
	MsgBackupFolderNotAbsolute    MessageCode = "backup.folder_not_absolute"
	MsgBackupFolderUnusable       MessageCode = "backup.folder_unusable"
	MsgBackupTextInterrupted      MessageCode = "backup.text_interrupted"
	MsgBackupTextComplete         MessageCode = "backup.text_complete"
	MsgBackupTextAlso             MessageCode = "backup.text_also"
	MsgBackupTextNotVerified      MessageCode = "backup.text_not_verified"
	MsgBackupTextUploadFailed     MessageCode = "backup.text_upload_failed"
	MsgBackupTextNotRemoved       MessageCode = "backup.text_not_removed"
	MsgBackupTextVerifyStopped    MessageCode = "backup.text_verify_stopped"
	MsgBackupTextFolderChanged    MessageCode = "backup.text_folder_changed"
	MsgBackupTextNotRecorded      MessageCode = "backup.text_not_recorded"
)

var backupsCatalog = map[MessageCode]message{
	MsgBackupTitle: {
		en: "Backups",
		ko: "백업",
	},
	MsgBackupScope: {
		en: "While OwnGit runs, it backs up every repository and its records into a folder on this computer: on a schedule, or when you ask. Only the administrator sees and changes backups.",
		ko: "OwnGit이 실행되는 동안 모든 저장소와 기록을 이 컴퓨터의 폴더에 백업합니다. 정해 둔 간격마다, 또는 요청할 때 백업합니다. 백업은 관리자만 보고 바꿀 수 있습니다.",
	},
	MsgBackupNotConfigured: {
		en: "No backup folder is set yet, so OwnGit makes no backups. Choose a folder and save.",
		ko: "아직 백업 폴더를 정하지 않아 OwnGit이 백업을 만들지 않습니다. 폴더를 정하고 저장하세요.",
	},
	MsgBackupDestination: {
		en: "Backup folder",
		ko: "백업 폴더",
	},
	MsgBackupDestinationHelp: {
		en: "An absolute path on the computer that runs OwnGit, outside OwnGit's state and repository folders. OwnGit creates it, private to its account, when it is missing.",
		ko: "OwnGit이 실행되는 컴퓨터의 절대 경로이며, OwnGit의 상태 폴더와 저장소 폴더 밖이어야 합니다. 폴더가 없으면 OwnGit 계정만 쓸 수 있게 만듭니다.",
	},
	MsgBackupScheduled: {
		en: "Scheduled backups",
		ko: "예약 백업",
	},
	MsgBackupScheduledHelp: {
		en: "Off stops scheduled backups. Back up now still works, and backups already made stay.",
		ko: "끄면 예약 백업을 멈춥니다. 지금 백업은 계속 쓸 수 있고, 이미 만든 백업은 그대로 남습니다.",
	},
	MsgBackupInterval: {
		en: "Back up every",
		ko: "백업 간격",
	},
	MsgBackupEvery12h: {
		en: "12 hours",
		ko: "12시간",
	},
	MsgBackupEveryDay: {
		en: "Day",
		ko: "하루",
	},
	MsgBackupEvery7d: {
		en: "7 days",
		ko: "7일",
	},
	MsgBackupIntervalHelp: {
		en: "The next scheduled backup starts one interval after the last scheduled one started.",
		ko: "다음 예약 백업은 마지막 예약 백업이 시작된 때부터 이 간격이 지나면 시작합니다.",
	},
	MsgBackupKeep: {
		en: "Backups to keep",
		ko: "보관할 백업 수",
	},
	MsgBackupKeepHelp: {
		en: "From 1 to 1000. OwnGit keeps this many of the backups it made in the folder, plus the newest verified one, and removes older ones it made. A backup that fails its verification does not count: OwnGit keeps only the newest one of those, until a verified backup replaces it. It never touches other files there.",
		ko: "1에서 1000 사이입니다. OwnGit은 폴더에 자기가 만든 백업을 이 개수만큼, 그리고 가장 최근에 검사를 통과한 백업을 남기고 더 오래된 자기 백업을 지웁니다. 검사에 실패한 백업은 개수에 넣지 않으며, 가장 최근 것 하나만 남기고 검사를 통과한 백업이 생기면 그것도 지웁니다. 폴더의 다른 파일은 건드리지 않습니다.",
	},
	MsgBackupKeepInvalid: {
		en: "Enter a whole number from 1 to 1000.",
		ko: "1에서 1000 사이의 정수를 입력하세요.",
	},
	MsgBackupVerify: {
		en: "Verify each new backup",
		ko: "새 백업마다 검사",
	},
	MsgBackupVerifyHelp: {
		en: "OwnGit rehearses a restore of each new backup in the system's temporary folder, which needs room for the repositories. A verification that takes longer than 2 hours fails.",
		ko: "새 백업마다 시스템 임시 폴더에서 복원을 연습해 봅니다. 임시 폴더에 저장소가 들어갈 공간이 있어야 합니다. 2시간 안에 끝나지 않은 검사는 실패로 처리합니다.",
	},
	MsgBackupScheduleRefused: {
		en: "The backup settings were not saved:",
		ko: "백업 설정을 저장하지 못했습니다:",
	},
	MsgBackupSaved: {
		en: "The backup settings are saved.",
		ko: "백업 설정을 저장했습니다.",
	},
	MsgBackupPlansTitle: {
		en: "Backup plans",
		ko: "백업 계획",
	},
	MsgBackupPlan: {
		en: "Plan",
		ko: "계획",
	},
	MsgBackupPlanName: {
		en: "Plan name",
		ko: "계획 이름",
	},
	MsgBackupPlanNameHelp: {
		en: "Letters, digits, underscores and dashes, up to 64 characters. Each run and backup is named with its plan.",
		ko: "문자, 숫자, 밑줄, 하이픈만 사용하고 최대 64자까지 가능합니다. 각 실행과 백업에 계획 이름이 기록됩니다.",
	},
	MsgBackupNewPlan: {
		en: "New plan",
		ko: "새 계획",
	},
	MsgBackupCreatePlan: {
		en: "Create plan",
		ko: "계획 만들기",
	},
	MsgBackupDeletePlan: {
		en: "Delete plan",
		ko: "계획 삭제",
	},
	MsgBackupDeletePlanHelp: {
		en: "Deletes only the plan. Backups it already made and their records stay listed and on disk, and OwnGit removes nothing for a deleted plan.",
		ko: "계획만 삭제합니다. 이미 만든 백업과 그 기록은 목록과 디스크에 그대로 남으며, 삭제한 계획의 백업을 OwnGit이 자동으로 지우지는 않습니다.",
	},
	MsgBackupPlanCreated: {
		en: "Created the backup plan.",
		ko: "백업 계획을 만들었습니다.",
	},
	MsgBackupPlanDeleted: {
		en: "Deleted the backup plan. Its backups and their records stay listed and on disk; delete them by hand when they are no longer needed.",
		ko: "백업 계획을 삭제했습니다. 백업과 기록은 목록과 디스크에 그대로 남으니, 필요 없어지면 직접 삭제하세요.",
	},
	MsgBackupPlanChoose: {
		en: "Choose the plan to view or change",
		ko: "보거나 변경할 계획을 선택하세요",
	},
	MsgBackupActiveRefused: {
		en: "The plan is still making a backup. Wait for it to finish, then delete it.",
		ko: "계획의 백업이 아직 진행 중입니다. 끝날 때까지 기다린 다음 삭제하세요.",
	},
	MsgBackupPlanExistsRefused: {
		en: "A backup plan with this name already exists.",
		ko: "같은 이름의 백업 계획이 이미 있습니다.",
	},
	MsgBackupPlanMissingRefused: {
		en: "The backup plan does not exist.",
		ko: "백업 계획이 존재하지 않습니다.",
	},
	MsgBackupPlanLimitRefused: {
		en: "There are already as many backup plans as OwnGit keeps.",
		ko: "저장 가능한 백업 계획 수가 이미 가득 찼습니다.",
	},
	MsgBackupStateTitle: {
		en: "Current state",
		ko: "현재 상태",
	},
	MsgBackupStateOn: {
		en: "On",
		ko: "켜짐",
	},
	MsgBackupStateOff: {
		en: "Off",
		ko: "꺼짐",
	},
	MsgBackupStateNotConfigured: {
		en: "Not configured",
		ko: "설정 안 됨",
	},
	MsgBackupRunningNow: {
		en: "Running now",
		ko: "지금 진행 중",
	},
	MsgBackupRunningSince: {
		en: "A backup started at %s.",
		ko: "%s에 시작한 백업이 진행 중입니다.",
	},
	MsgBackupNone: {
		en: "None",
		ko: "없음",
	},
	MsgBackupLastRun: {
		en: "Last backup",
		ko: "마지막 백업",
	},
	MsgBackupLastVerified: {
		en: "Last verified backup",
		ko: "마지막으로 검사를 통과한 백업",
	},
	MsgBackupNextRun: {
		en: "Next scheduled backup",
		ko: "다음 예약 백업",
	},
	MsgBackupNextRunSoon: {
		en: "As soon as the running backup ends",
		ko: "진행 중인 백업이 끝나는 대로",
	},
	MsgBackupNow: {
		en: "Back up now",
		ko: "지금 백업",
	},
	MsgBackupStarted: {
		en: "A backup has started. Current state updates when it ends; if it does not, reload this page.",
		ko: "백업을 시작했습니다. 끝나면 현재 상태가 바뀝니다. 바뀌지 않으면 이 페이지를 새로 고치세요.",
	},
	MsgBackupBusy: {
		en: "A backup, a verification or an upload is running. Wait for it to finish.",
		ko: "백업, 검사, 올리기 중 하나가 진행 중입니다. 끝날 때까지 기다리세요.",
	},
	MsgBackupWorkEnded: {
		en: "The backup, verification or upload has ended. Current state shows the result.",
		ko: "백업, 검사, 올리기 중 하나가 끝났습니다. 결과는 현재 상태에 나옵니다.",
	},
	MsgBackupRunningRefused: {
		en: "A backup is already running. Wait for it to finish.",
		ko: "이미 백업이 진행 중입니다. 끝날 때까지 기다리세요.",
	},
	MsgBackupChooseFolder: {
		en: "Choose a backup folder first.",
		ko: "먼저 백업 폴더를 정하세요.",
	},
	MsgBackupRestoreLimit: {
		en: "The backup folder is on a file system (%s) that repositories cannot be restored to. Backups work there, but restore the repositories to a folder on another disk.",
		ko: "백업 폴더가 있는 파일 시스템(%s)으로는 저장소를 복원할 수 없습니다. 이곳에 백업하는 것은 문제없지만, 저장소는 다른 디스크의 폴더로 복원하세요.",
	},
	MsgBackupRunsTitle: {
		en: "Recorded backups",
		ko: "백업 기록",
	},
	MsgBackupRunsEmpty: {
		en: "No backups yet.",
		ko: "아직 백업이 없습니다.",
	},
	MsgBackupStartedAt: {
		en: "Started",
		ko: "시작",
	},
	MsgBackupKind: {
		en: "Kind",
		ko: "종류",
	},
	MsgBackupStatus: {
		en: "Result",
		ko: "결과",
	},
	MsgBackupVerification: {
		en: "Verification",
		ko: "검사",
	},
	MsgBackupHold: {
		en: "Longest Git write wait",
		ko: "Git 쓰기 최대 대기",
	},
	MsgBackupHoldValue: {
		en: "%s ms (%s)",
		ko: "%s ms (%s)",
	},
	MsgBackupHoldNone: {
		en: "No repositories",
		ko: "저장소 없음",
	},
	MsgBackupMessage: {
		en: "Message",
		ko: "메시지",
	},
	MsgBackupNotKept: {
		en: "OwnGit keeps no backup of this run.",
		ko: "이 기록의 백업은 남아 있지 않습니다.",
	},
	MsgBackupKindScheduled: {
		en: "Scheduled",
		ko: "예약",
	},
	MsgBackupKindManual: {
		en: "Back up now",
		ko: "지금 백업",
	},
	MsgBackupStatusRunning: {
		en: "Running",
		ko: "진행 중",
	},
	MsgBackupStatusSucceeded: {
		en: "Succeeded",
		ko: "성공",
	},
	MsgBackupStatusFailed: {
		en: "Failed",
		ko: "실패",
	},
	MsgBackupStatusInterrupted: {
		en: "Interrupted",
		ko: "중단됨",
	},
	MsgBackupVerifyPassed: {
		en: "Passed",
		ko: "통과",
	},
	MsgBackupVerifyFailed: {
		en: "Failed",
		ko: "실패",
	},
	MsgBackupVerifyNotRun: {
		en: "Not verified",
		ko: "검사 안 함",
	},
	MsgBackupActions: {
		en: "Use this backup",
		ko: "이 백업 사용",
	},
	MsgBackupVerifyAgain: {
		en: "Verify again",
		ko: "다시 검사",
	},
	MsgBackupCheckStarted: {
		en: "The verification has started. Reload this page to see its result.",
		ko: "검사를 시작했습니다. 결과를 보려면 이 페이지를 새로 고치세요.",
	},
	MsgBackupCheckTitle: {
		en: "Verification of %s",
		ko: "%s 검사",
	},
	MsgBackupCheckRunning: {
		en: "Running",
		ko: "진행 중",
	},
	MsgBackupCheckPassed: {
		en: "Passed",
		ko: "통과",
	},
	MsgBackupCheckFailed: {
		en: "Failed",
		ko: "실패",
	},
	MsgBackupGone: {
		en: "This backup is no longer in its folder, so nothing was done:",
		ko: "이 백업이 폴더에 더 이상 없어 아무것도 하지 않았습니다:",
	},
	MsgBackupNoBackup: {
		en: "OwnGit keeps no backup of this run any more.",
		ko: "이 기록의 백업은 더 이상 남아 있지 않습니다.",
	},
	MsgBackupDownload: {
		en: "Download",
		ko: "내려받기",
	},
	MsgBackupDownloadHelp: {
		en: "Downloads the backup as one .tar file with its manifest. It holds every repository and OwnGit's records, including password hashes, so keep it as private as the backup folder. The download stops when your browser receives nothing for 1 minute.",
		ko: "백업을 매니페스트와 함께 .tar 파일 하나로 내려받습니다. 모든 저장소와 OwnGit 기록, 비밀번호 해시까지 들어 있으니 백업 폴더처럼 남이 볼 수 없게 보관하세요. 브라우저가 1분 동안 아무것도 받지 못하면 내려받기를 멈춥니다.",
	},
	MsgBackupRestoreTitle: {
		en: "Restore this backup",
		ko: "이 백업으로 복원",
	},
	MsgBackupRestoreLead: {
		en: "OwnGit never restores from the browser. A restore runs on this computer while OwnGit is stopped, and it never replaces anything:",
		ko: "브라우저에서는 복원하지 않습니다. 복원은 OwnGit을 멈춘 상태에서 이 컴퓨터에서 실행하며, 무엇도 덮어쓰지 않습니다:",
	},
	MsgBackupRestoreStopService: {
		en: "Stop OwnGit:",
		ko: "OwnGit을 멈춥니다:",
	},
	MsgBackupRestoreStopProcess: {
		en: "Stop OwnGit: end the owngit serve process that runs it now, for example with Ctrl+C in its terminal.",
		ko: "OwnGit을 멈춥니다. 지금 실행 중인 owngit serve 프로세스를 끝내세요. 예를 들어 그 터미널에서 Ctrl+C를 누르면 됩니다.",
	},
	MsgBackupRestoreMove: {
		en: "The restore refuses folders that exist, so rename the current ones first: %s to %s, and %s to %s.",
		ko: "복원은 이미 있는 폴더에는 쓰지 않으므로, 먼저 지금 폴더의 이름을 바꾸세요. %s 폴더를 %s 폴더로, %s 폴더를 %s 폴더로 바꿉니다.",
	},
	MsgBackupRestoreUnchecked: {
		en: "OwnGit could not check where %s will be once the state folder is renamed, so it gives no restore command. Check that OwnGit can read every folder on the way to it:",
		ko: "상태 폴더 이름을 바꾼 뒤 %s이(가) 어디에 있게 될지 OwnGit이 확인하지 못해 복원 명령을 보여 주지 않습니다. 그곳까지 가는 모든 폴더를 OwnGit이 읽을 수 있는지 확인하세요:",
	},
	MsgBackupRestoreRun: {
		en: "Run this command. It verifies the backup first and restores it only when it passes:",
		ko: "이 명령을 실행하세요. 먼저 백업을 검사하고, 통과한 경우에만 복원합니다:",
	},
	MsgBackupRestoreCommand: {
		en: "Restore command",
		ko: "복원 명령",
	},
	MsgBackupRestoreNetwork: {
		en: "Behind a proxy, save the public address and the proxy again before you start OwnGit, because a restore resets network settings and they apply at the next start:",
		ko: "프록시 뒤에서 쓴다면 OwnGit을 시작하기 전에 공개 주소와 프록시를 다시 저장하세요. 복원은 네트워크 설정을 초기화하고, 저장한 설정은 다음 시작부터 적용됩니다:",
	},
	MsgBackupRestoreStartService: {
		en: "Start OwnGit again:",
		ko: "OwnGit을 다시 시작합니다:",
	},
	MsgBackupRestoreStartProcess: {
		en: "Start OwnGit again the way you started it before.",
		ko: "전에 시작하던 방법으로 OwnGit을 다시 시작하세요.",
	},
	MsgBackupRestoreAfter: {
		en: "When it finishes, the command lists what a backup does not bring back and how to set it up again, such as sessions, tokens and credentials, check and import settings, network settings and the backup schedule. The renamed folders stay until you remove them.",
		ko: "복원이 끝나면 세션, 토큰과 인증 정보, 체크와 가져오기 설정, 네트워크 설정, 백업 일정처럼 백업에 들어 있지 않은 것과 다시 설정하는 방법을 명령이 알려 줍니다. 이름을 바꾼 폴더는 직접 지울 때까지 남습니다.",
	},
	MsgBackupUploadTitle: {
		en: "Restore from a backup file",
		ko: "백업 파일로 복원",
	},
	MsgBackupUploadHelp: {
		en: "Upload a .tar file that OwnGit downloaded. OwnGit keeps one uploaded backup in its state folder: a new upload replaces it, and it is removed 24 hours after it arrived or when OwnGit starts again. The disk of the state folder needs the file's size plus 1 GiB free, and an upload that receives nothing for 1 minute stops. OwnGit then verifies it and removes one that fails. Nothing is replaced until you run the restore command.",
		ko: "OwnGit에서 내려받은 .tar 파일을 올리세요. 올린 백업은 상태 폴더에 하나만 보관합니다. 새로 올리면 바뀌고, 올린 지 24시간이 지나거나 OwnGit을 다시 시작하면 지워집니다. 상태 폴더가 있는 디스크에 파일 크기보다 1 GiB 넘게 여유가 있어야 하며, 1분 동안 아무것도 받지 못하면 올리기를 멈춥니다. 올린 뒤에는 검사하고, 통과하지 못하면 지웁니다. 복원 명령을 실행하기 전에는 아무것도 바뀌지 않습니다.",
	},
	MsgBackupUploadFile: {
		en: "Backup file",
		ko: "백업 파일",
	},
	MsgBackupUploadSend: {
		en: "Upload and verify",
		ko: "올리고 검사",
	},
	MsgBackupUploadReceived: {
		en: "The backup file was uploaded and its verification has started. Reload this page to see the result.",
		ko: "백업 파일을 올렸고 검사를 시작했습니다. 결과를 보려면 이 페이지를 새로 고치세요.",
	},
	MsgBackupUploadRefused: {
		en: "The backup file was refused, and nothing of it was kept:",
		ko: "백업 파일을 받지 않았고, 아무것도 남기지 않았습니다:",
	},
	MsgBackupUploadMissing: {
		en: "Choose a backup file to upload.",
		ko: "올릴 백업 파일을 고르세요.",
	},
	MsgBackupUploadNoSize: {
		en: "The upload did not say how large it is, so it was refused.",
		ko: "올린 파일의 크기를 알 수 없어 받지 않았습니다.",
	},
	MsgBackupUploadName: {
		en: "Uploaded backup %s (%s)",
		ko: "올린 백업 %s (%s)",
	},
	MsgBackupUploadRemovesAt: {
		en: "OwnGit removes it at %s.",
		ko: "%s에 지웁니다.",
	},
	MsgBackupUploadVerifying: {
		en: "Verifying",
		ko: "검사 중",
	},
	MsgBackupUploadPassed: {
		en: "Passed",
		ko: "통과",
	},
	MsgBackupUploadFailed: {
		en: "Failed",
		ko: "실패",
	},
	MsgBackupFolderNotAbsolute: {
		en: "The backup folder must be an absolute path.",
		ko: "백업 폴더는 절대 경로여야 합니다.",
	},
	MsgBackupFolderUnusable: {
		en: "The backup folder cannot be used:",
		ko: "이 백업 폴더는 쓸 수 없습니다:",
	},
	MsgBackupTextInterrupted: {
		en: "OwnGit stopped before this backup finished, so it is not a finished backup.",
		ko: "백업이 끝나기 전에 OwnGit이 멈춰서 이 백업은 완성되지 않았습니다.",
	},
	MsgBackupTextComplete: {
		en: "The backup is complete, but",
		ko: "백업은 끝났지만 다음 문제가 있습니다:",
	},
	MsgBackupTextAlso: {
		en: "Also,",
		ko: "이와 함께 다음 문제가 있습니다:",
	},
	MsgBackupTextNotVerified: {
		en: "The backup was written but did not pass verification:",
		ko: "백업을 만들었지만 검사를 통과하지 못했습니다:",
	},
	MsgBackupTextUploadFailed: {
		en: "The uploaded backup did not pass verification, so it was removed:",
		ko: "올린 백업이 검사를 통과하지 못해 지웠습니다:",
	},
	MsgBackupTextNotRemoved: {
		en: "It could not be removed:",
		ko: "지우지 못했습니다:",
	},
	MsgBackupTextVerifyStopped: {
		en: "OwnGit stopped before the verification finished",
		ko: "검사가 끝나기 전에 OwnGit이 멈췄습니다",
	},
	MsgBackupTextFolderChanged: {
		en: "The backup's folder no longer holds this backup (it was moved, removed, changed or replaced by another backup), so nothing was recorded for it.",
		ko: "백업 폴더에 이 백업이 더 이상 없어서(옮겨졌거나 지워졌거나 바뀌었거나 다른 백업으로 교체됨) 아무것도 기록하지 않았습니다.",
	},
	MsgBackupTextNotRecorded: {
		en: "The result could not be recorded:",
		ko: "결과를 기록하지 못했습니다:",
	},
	MsgBackupFolderOverlaps: {
		en: "The backup folder must be outside OwnGit's state and repository folders.",
		ko: "백업 폴더는 OwnGit의 상태 폴더와 저장소 폴더 밖에 있어야 합니다.",
	},
	MsgBackupUploadEndsEarly: {
		en: "The backup file ends early, so nothing of it was kept.",
		ko: "백업 파일이 중간에 끝나서 아무것도 남기지 않았습니다.",
	},
	MsgBackupUploadNoManifest: {
		en: "The backup file holds no manifest.json, so nothing of it was kept.",
		ko: "백업 파일에 manifest.json이 없어 아무것도 남기지 않았습니다.",
	},
	MsgBackupUploadDeclaredNoSize: {
		en: "The upload declared no size, so nothing of it was kept.",
		ko: "업로드가 크기를 알리지 않아 아무것도 남기지 않았습니다.",
	},
	MsgBackupUploadStopped: {
		en: "The upload stopped before it ended, so nothing of it was kept.",
		ko: "업로드가 끝나기 전에 멈춰서 아무것도 남기지 않았습니다.",
	},
	MsgBackupUploadUnsafePath: {
		en: "The backup file holds a path that is not allowed, and nothing of it was kept:",
		ko: "백업 파일에 허용되지 않는 경로가 있어 아무것도 남기지 않았습니다:",
	},
	MsgBackupUploadFolderName: {
		en: "The backup file's folder name is not allowed (only letters, digits, '.', '_' and '-'), and nothing of it was kept:",
		ko: "백업 파일의 폴더 이름을 쓸 수 없습니다(영문자, 숫자, '.', '_', '-'만 가능). 아무것도 남기지 않았습니다:",
	},
	MsgBackupUploadManyFolders: {
		en: "The backup file holds more than one top folder, and nothing of it was kept:",
		ko: "백업 파일에 최상위 폴더가 둘 이상 있어 아무것도 남기지 않았습니다:",
	},
	MsgBackupUploadDuplicate: {
		en: "The backup file holds the same entry twice, and nothing of it was kept:",
		ko: "백업 파일에 같은 항목이 두 번 들어 있어 아무것도 남기지 않았습니다:",
	},
	MsgBackupUploadUnknownEntry: {
		en: "The backup file holds an entry that a backup does not have, and nothing of it was kept:",
		ko: "백업 파일에 백업에 없는 항목이 있어 아무것도 남기지 않았습니다:",
	},
	MsgBackupUploadBeforeFolder: {
		en: "The backup file lists a file before its folder, and nothing of it was kept:",
		ko: "백업 파일에서 파일이 자기 폴더보다 먼저 나와 아무것도 남기지 않았습니다:",
	},
	MsgBackupFailed: {
		en: "This did not work. Try again later.",
		ko: "처리하지 못했습니다. 나중에 다시 시도하세요.",
	},
	MsgBackupScheduleOffWarning: {
		en: "OwnGit will stop creating scheduled recovery copies. Back up now still works, and the backups already made stay.",
		ko: "OwnGit이 예약된 복구용 백업을 더 이상 만들지 않습니다. 지금 백업하기는 계속 쓸 수 있고, 이미 만든 백업은 그대로 남습니다.",
	},
	MsgBackupVerifyOffWarning: {
		en: "New backups will not be checked by rehearsing a restore, so a damaged backup may go unnoticed until you need it. OwnGit never removes the last verified backup.",
		ko: "새 백업을 복원 연습으로 검사하지 않으므로, 손상된 백업을 실제로 필요할 때까지 알아차리지 못할 수 있습니다. 마지막으로 검사를 통과한 백업은 지우지 않습니다.",
	},
}

func init() {
	registerMessages(backupsCatalog)
}
