package webui

import (
	"fmt"
	"html/template"
	"time"
)

// Page is one renderable screen. Every page type in this package implements it.
// page reports the template name the renderer executes, which keeps the set
// of pages closed: the backend cannot invent a page the renderer does not
// know. chrome returns the page's shared Chrome field, so a page cannot be
// added without the data the shell needs.
type Page interface {
	page() string
	chrome() Chrome
}

// ---------------------------------------------------------------------------
// Setup and bootstrap
// ---------------------------------------------------------------------------

// SetupStage selects which part of the first-run flow to show.
type SetupStage string

const (
	// SetupWelcome greets the installation owner who opened the one-time link.
	// The page holds the secret in memory only, removes it from the address
	// bar immediately, and redeems it with an explicit POST.
	SetupWelcome SetupStage = "welcome"
	// SetupWizard collects storage, general access, and administrator password.
	// It is reachable only after redemption, using the narrow setup session.
	SetupWizard SetupStage = "wizard"
	// SetupUnavailable explains why setup cannot continue: the link expired,
	// was already redeemed, or the installation is already configured.
	SetupUnavailable SetupStage = "unavailable"
	// SetupApproval is shown instead of SetupWelcome while setup runs in the
	// terminal that started OwnGit: the browser asks that terminal for
	// approval with an explicit POST.
	SetupApproval SetupStage = "approval"
	// SetupApprovalWait shows the comparison code while the terminal
	// decides. The page script asks ApprovalURL for the state and opens
	// SetupURL once the terminal has answered; without scripting the owner
	// uses the Check again link.
	SetupApprovalWait SetupStage = "approval_wait"
)

// SetupPage renders /setup.
type SetupPage struct {
	Chrome Chrome
	Stage  SetupStage

	// RedeemURL receives the POST that exchanges the one-time secret. The
	// page script reads the secret from the URL fragment, clears the fragment
	// with history.replaceState, keeps the value in memory, and submits it in
	// the "token" field only when the owner presses the start button.
	RedeemURL string
	// SubmitURL receives the completed wizard.
	SubmitURL string
	// ApprovalURL receives the POST that asks the terminal for approval.
	ApprovalURL string
	// ApprovalCode is the comparison code the terminal also shows. It helps
	// the owner recognize this browser and is not a credential.
	ApprovalCode string
	// RetryURL, when set on an unavailable stage, offers to start again.
	RetryURL string
	// SetupURL is the setup page the waiting page returns to.
	SetupURL string

	// Prerequisites are environment findings such as a missing system Git.
	Prerequisites []Prerequisite

	// Form holds the values to redisplay. Passwords are never echoed.
	Form SetupForm
	// KeepHost, when set, is the Host this browser used, which OwnGit accepts
	// for this run only. The wizard offers to keep accepting it after a
	// restart ("keep_host").
	KeepHost string
	// KeepHostSetupOnly says that OwnGit accepts KeepHost only to finish
	// setup, because it was reached by an address it was not started with.
	KeepHostSetupOnly bool

	// Reason explains an unavailable stage.
	Reason MessageCode
	// RecoveryHint is shown with an unavailable stage to point the owner at
	// installation-host reissue. It never describes an email or an external
	// recovery service.
	RecoveryHint MessageCode
}

func (SetupPage) page() string     { return "setup" }
func (p SetupPage) chrome() Chrome { return p.Chrome }

// SetupForm is the redisplayed wizard state. Password fields are deliberately
// absent: a validation failure must not echo a submitted password.
type SetupForm struct {
	// StoragePath is the "storage_path" field.
	StoragePath string
	// SuggestedPath is offered when the owner has not typed a path yet.
	SuggestedPath string
	// AccessMode is the "access_mode" field, AccessOpen or AccessPassword.
	AccessMode AccessMode
	// InsecureAck is the "insecure_ack" checkbox state.
	InsecureAck bool
	// KeepHost is the "keep_host" checkbox state.
	KeepHost bool
	// AccessPasswordSet and AdminPasswordSet report whether a value was
	// accepted on an earlier attempt, so the wizard can say "already entered"
	// without holding the secret.
	AccessPasswordSet bool
	AdminPasswordSet  bool
}

// Prerequisite is one environment requirement and its observed state.
type Prerequisite struct {
	// Name is shown verbatim, for example "git".
	Name string
	// Satisfied is true when the requirement was found.
	Satisfied bool
	// Code describes the finding, satisfied or not.
	Code MessageCode
	// Detail is an untranslated fragment such as a discovered version.
	Detail string
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// AuthScope distinguishes the two sign-in forms.
type AuthScope string

const (
	// AuthGeneral is the shared general-access password at /login,
	// submitted in the "password" field.
	AuthGeneral AuthScope = "general"
	// AuthAdmin is the administrator password at /admin/login,
	// submitted in the "admin_password" field.
	AuthAdmin AuthScope = "admin"
)

// AuthPage renders /login and /admin/login.
type AuthPage struct {
	Chrome Chrome
	Scope  AuthScope
	// SubmitURL is the POST target.
	SubmitURL string
	// Next is the path to return to after a successful sign-in. It is
	// submitted back in the "next" field and must already be validated as a
	// local path by the backend.
	Next string
	// Locked is true while attempts are blocked; the form is disabled.
	Locked bool
	// RetryAfter is when attempts resume. Zero when not locked.
	RetryAfter time.Time
	// Repo and Tabs keep the repository the administrator prompt was opened
	// from, so the sidebar and the heading stay on it. Empty elsewhere.
	Repo RepositoryHeader
	Tabs RepoTabs
}

func (AuthPage) page() string     { return "auth" }
func (p AuthPage) chrome() Chrome { return p.Chrome }

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// Settings form actions submitted in the "action" field of POST /settings.
const (
	// ActionEnableAccessPassword turns on general password protection.
	// Fields: admin_password, access_password.
	ActionEnableAccessPassword = "enable_access_password"
	// ActionChangeAccessPassword replaces the general-access password.
	// Fields: admin_password, access_password.
	ActionChangeAccessPassword = "change_access_password"
	// ActionDisableAccessPassword removes general password protection.
	// Fields: admin_password.
	ActionDisableAccessPassword = "disable_access_password"
	// ActionChangeAdminPassword replaces the administrator password.
	// Fields: admin_password (current), new_admin_password.
	ActionChangeAdminPassword = "change_admin_password"
	// ActionAcknowledgeInsecure records the informed choice to keep using
	// plain HTTP. Field: insecure_ack.
	ActionAcknowledgeInsecure = "acknowledge_insecure"
	// ActionSetUpdateCheck turns the new-release check on or off.
	// Fields: admin_password, update_check ("on", or "off" or absent for
	// off, as an unticked switch sends nothing).
	ActionSetUpdateCheck = "set_update_check"
	// ActionSetTrayIcon shows or hides the OwnGit icon on the computer
	// that runs OwnGit. Fields: admin_password, tray_icon ("on", or "off"
	// or absent for off).
	ActionSetTrayIcon = "set_tray_icon"
	// ActionSaveAccess saves the Access group: who can read and push, and
	// a new shared password. It turns the shared password on, changes it or
	// turns it off, as the fields ask. Fields: admin_password, access_mode
	// ("password" or "open"), access_password.
	ActionSaveAccess = "save_access"
	// ActionSaveConfirmation saves when the administrator password is
	// asked. Fields: admin_password, admin_confirmation (one of
	// AdminConfirmChoices), no_ask_ack (required to turn Do not ask on).
	ActionSaveConfirmation = "save_confirmation"
	// ActionSaveSession saves how long a sign-in with the shared password
	// lasts. Fields: admin_password, general_session (one of
	// SessionChoices).
	ActionSaveSession = "save_session"
	// ActionSaveInitialBranch saves the branch new repositories start on.
	// Fields: admin_password, initial_branch.
	ActionSaveInitialBranch = "save_initial_branch"
	// ActionSaveTransfers saves the Git transfer limits. Fields:
	// admin_password, transfer_per_repository, transfer_extra_slots, and
	// transfer_size, transfer_time, transfer_idle and transfer_queue, each
	// with its _unit (see LimitInput).
	ActionSaveTransfers = "save_transfers"
	// ActionSaveCheckLogs saves how long raw check logs are kept. Fields:
	// admin_password, check_logs (one of CheckLogChoices).
	ActionSaveCheckLogs = "save_check_logs"
	// ActionSaveKeptHistory saves whether repositories that follow the
	// server keep overwritten and deleted history. Fields: admin_password,
	// kept_history ("on" or "off").
	ActionSaveKeptHistory = "save_kept_history"
	// ActionSaveDeleteName saves whether deleting a repository asks for its
	// typed name. Fields: admin_password, delete_requires_name ("on" or
	// "off").
	ActionSaveDeleteName = "save_delete_name"
	// ActionSaveLoginLimits saves the login attempt limits. Fields:
	// admin_password, login_attempts, and login_window and login_pause,
	// each with its _unit (see LimitInput).
	ActionSaveLoginLimits = "save_login_limits"
	// ActionSaveCrossSite saves whether a link from another site keeps the
	// shared sign-in. Fields: admin_password, cross_site_links (one of
	// CrossSiteChoices).
	ActionSaveCrossSite = "save_cross_site"
	// ActionSaveBrowseLimits saves the browsing limits. Fields:
	// admin_password, browse_raw, browse_file, browse_commit_patch,
	// browse_file_patch, browse_commit_file, browse_compare and
	// browse_compare_time, each with its _unit (see LimitInput).
	ActionSaveBrowseLimits = "save_browse_limits"
	// ActionSaveMaintenance saves the repository maintenance choices.
	// Fields: admin_password, maintenance_enabled ("on" or "off"),
	// maintenance_window_start and maintenance_window_end (hours 0 to 23),
	// maintenance_pack_threshold, and maintenance_idle,
	// maintenance_command and maintenance_full_repack, each with its _unit.
	ActionSaveMaintenance = "save_maintenance"
	// ActionSaveCleanup saves the unused object cleanup choice. Fields:
	// admin_password, cleanup_enabled ("on" or "off"), cleanup_grace (days).
	ActionSaveCleanup = "save_cleanup"
	// ActionSaveCheckCeilings saves the check ceilings. Fields:
	// admin_password, ceiling_queue, ceiling_active and ceiling_pids
	// (counts), and ceiling_timeout, ceiling_output, ceiling_cpu,
	// ceiling_memory, ceiling_scratch and ceiling_source, each with its
	// _unit (see LimitInput).
	ActionSaveCheckCeilings = "save_check_ceilings"
)

// The Settings tabs. Each is its own address, so a tab works as an ordinary
// link and can be opened, reloaded and shared without scripting.
const (
	SettingsGeneral      = "general"
	SettingsAccess       = "access"
	SettingsNetwork      = "network"
	SettingsRepositories = "repositories"
	SettingsStorage      = "storage"
)

// SettingsTab is one entry of the Settings tab strip.
type SettingsTab struct {
	ID    string
	URL   string
	Label MessageCode
	Icon  string
}

// SettingsTabs returns the tabs in the order the strip shows them.
func SettingsTabs() []SettingsTab {
	return []SettingsTab{
		{SettingsGeneral, SettingsTabURL(SettingsGeneral), MsgSettingsTabGeneral, "settings"},
		{SettingsAccess, SettingsTabURL(SettingsAccess), MsgSettingsTabAccess, "lock"},
		{SettingsNetwork, SettingsTabURL(SettingsNetwork), MsgSettingsTabNetwork, "globe"},
		{SettingsRepositories, SettingsTabURL(SettingsRepositories), MsgSettingsTabRepositories, "repo"},
		{SettingsStorage, SettingsTabURL(SettingsStorage), MsgSettingsTabStorage, "kept"},
	}
}

// SettingsTabURL is the address of a tab. General is Settings itself.
func SettingsTabURL(tab string) string {
	if tab == SettingsGeneral || tab == "" {
		return "/settings"
	}
	return "/settings/" + tab
}

// SettingsTabOfPath returns the tab a Settings address shows.
func SettingsTabOfPath(path string) (string, bool) {
	if path == "/settings" {
		return SettingsGeneral, true
	}
	for _, tab := range SettingsTabs() {
		if tab.ID != SettingsGeneral && path == tab.URL {
			return tab.ID, true
		}
	}
	return "", false
}

// The Settings groups. A group is one form with its own Save: saving it
// never sends or resets another group's fields.
const (
	GroupUpdate      = "update"
	GroupTray        = "tray"
	GroupAccess      = "access"
	GroupAdmin       = "admin"
	GroupConfirm     = "confirm"
	GroupSession     = "session"
	GroupBranch      = "branch"
	GroupTransfer    = "transfer"
	GroupHistory     = "history"
	GroupDeleteName  = "delete_name"
	GroupLogin       = "login"
	GroupCrossSite   = "cross_site"
	GroupLogs        = "logs"
	GroupConnection  = "connection"
	GroupNetwork     = "network"
	GroupTailscale   = "tailscale"
	GroupBrowse      = "browse"
	GroupMaintenance = "maintenance"
	GroupCleanup     = "cleanup"
	GroupCeilings    = "ceilings"
)

// settingsGroupTabs names the tab of each group.
var settingsGroupTabs = map[string]string{
	GroupUpdate: SettingsGeneral, GroupTray: SettingsGeneral,
	GroupAccess: SettingsAccess, GroupAdmin: SettingsAccess, GroupConfirm: SettingsAccess, GroupSession: SettingsAccess,
	GroupCrossSite: SettingsAccess, GroupLogin: SettingsAccess,
	GroupConnection: SettingsNetwork, GroupNetwork: SettingsNetwork, GroupTailscale: SettingsNetwork,
	GroupBranch: SettingsRepositories, GroupTransfer: SettingsRepositories, GroupHistory: SettingsRepositories,
	GroupDeleteName: SettingsRepositories, GroupBrowse: SettingsRepositories, GroupCeilings: SettingsRepositories,
	GroupLogs: SettingsStorage, GroupMaintenance: SettingsStorage, GroupCleanup: SettingsStorage,
	GroupBackups: SettingsStorage, GroupBackupRuns: SettingsStorage,
}

// SettingsGroupTab returns the tab that shows group, or "" for an unknown
// group.
func SettingsGroupTab(group string) string { return settingsGroupTabs[group] }

// SettingsActionGroup returns the group of a Settings form action, or ""
// for an unknown action.
func SettingsActionGroup(action string) string {
	switch action {
	case ActionSetUpdateCheck:
		return GroupUpdate
	case ActionSetTrayIcon:
		return GroupTray
	case ActionSaveAccess, ActionEnableAccessPassword, ActionChangeAccessPassword, ActionDisableAccessPassword:
		return GroupAccess
	case ActionChangeAdminPassword:
		return GroupAdmin
	case ActionSaveConfirmation:
		return GroupConfirm
	case ActionSaveSession:
		return GroupSession
	case ActionSaveInitialBranch:
		return GroupBranch
	case ActionSaveTransfers:
		return GroupTransfer
	case ActionSaveKeptHistory:
		return GroupHistory
	case ActionSaveDeleteName:
		return GroupDeleteName
	case ActionSaveLoginLimits:
		return GroupLogin
	case ActionSaveCrossSite:
		return GroupCrossSite
	case ActionSaveCheckLogs:
		return GroupLogs
	case ActionSaveBrowseLimits:
		return GroupBrowse
	case ActionSaveMaintenance:
		return GroupMaintenance
	case ActionSaveCleanup:
		return GroupCleanup
	case ActionSaveCheckCeilings:
		return GroupCeilings
	case ActionAcknowledgeInsecure:
		return GroupConnection
	case ActionSaveNetwork:
		return GroupNetwork
	case ActionSaveTailscale, ActionTailscaleOn, ActionTailscaleOff:
		return GroupTailscale
	case ActionSaveBackupSchedule, ActionSaveBackupPlan, ActionCreateBackupPlan, ActionDeleteBackupPlan:
		return GroupBackups
	case ActionBackupNow, ActionBackupVerify, ActionBackupDownload, ActionBackupUpload:
		return GroupBackupRuns
	}
	return ""
}

// SettingsPage renders one Settings tab.
type SettingsPage struct {
	Chrome Chrome
	// Tab is the tab shown, one of the SettingsTab IDs. Empty is General.
	Tab string
	// SubmitURL is the POST target of this tab's forms.
	SubmitURL string
	// AccessMode is the currently stored mode.
	AccessMode AccessMode
	// Group is the group that Notices describe: the group whose form was
	// refused, or whose change was just saved. Every other group shows no
	// notice.
	Group string
	// Notices are the notices of Group, shown inside it.
	Notices []Notice
	// Draft holds the non-secret values a refused form sent, by field
	// name, so that form shows them again instead of the saved ones. A
	// refused Network save is in Network.Form instead. Secrets are never
	// in it.
	Draft map[string]string
	// Storage is the owner-only storage location. Only an administrator's
	// settings page shows it.
	Storage StorageInfo
	// CloneHint shows the base Git clone URL, for example
	// "http://host:port/git/". Empty when the backend cannot determine it.
	CloneHint string
	// UpdateCheck is the new-release check setting.
	UpdateCheck UpdateCheckInfo
	// Tray is the OwnGit icon of the computer that runs OwnGit.
	Tray TrayInfo
	// Policies are the server-wide policies of the tab shown.
	Policies Policies
	// Network is the network settings block. See NetworkInfo.
	Network NetworkInfo
	// Tailscale is the block for sharing on the tailnet. See TailscaleInfo.
	Tailscale TailscaleInfo
	// Checkup is what the checkup found on this computer. Only an
	// administrator's General tab shows it.
	Checkup CheckupInfo
	// Backups is the Backups group of Storage & recovery.
	Backups BackupsInfo
}

// CheckupInfo is the checkup card of the General tab.
type CheckupInfo struct {
	// Visible is true for a confirmed administrator, who alone sees the
	// findings, since they hold this computer's paths.
	Visible bool
	// Findings are the problems found, none when all is well.
	Findings []Finding
}

// Finding is one problem that the checkup found on the computer where
// OwnGit runs, with the one command that repairs it.
type Finding struct {
	// Code is the sentence, and Args fill its %s.
	Code MessageCode `json:"code"`
	Args []string    `json:"args,omitempty"`
	// Repair is the command to run there, or "" when the sentence says
	// what to change.
	Repair string `json:"repair,omitempty"`
	// Unchecked is true when OwnGit could not tell whether this is a
	// problem: a check that could not run, or settings it cannot read.
	Unchecked bool `json:"unchecked,omitempty"`
}

// Sentence is the finding in lang.
func (finding Finding) Sentence(lang Lang) string {
	args := make([]any, len(finding.Args))
	for index, arg := range finding.Args {
		args[index] = arg
	}
	return fmt.Sprintf(Text(lang, finding.Code), args...)
}

// UpdateCheckInfo describes the new-release check on the Settings page.
type UpdateCheckInfo struct {
	// Enabled is the saved setting.
	Enabled bool
	// ForcedOff is true when the server started with --no-update-check,
	// which overrides the saved setting.
	ForcedOff bool
}

// TrayInfo describes the OwnGit icon on the Settings page.
type TrayInfo struct {
	// Shown is false when the owner hid the icon.
	Shown bool
	// Desktop is true when the computer has a desktop to show it on.
	Desktop bool
	// Unreadable is true when whether it is hidden could not be read.
	Unreadable bool
	// Unavailable is true when this kind of install offers no icon.
	Unavailable bool
}

// Policies are the saved server-wide policies. Only the tab that shows a
// policy reads it; the others leave it empty.
type Policies struct {
	// Session is how long a sign-in with the shared password lasts, one
	// of SessionChoices.
	Session string
	// InitialBranch is the branch new repositories start on.
	InitialBranch string
	// TransferSize and TransferTime are the Git transfer limits: the
	// largest transfer and the longest. TransferPerRepository,
	// TransferExtraSlots, TransferIdle and TransferQueue decide how many
	// run at once and how long they wait.
	TransferSize, TransferTime                LimitInput
	TransferPerRepository, TransferExtraSlots string
	TransferIdle, TransferQueue               LimitInput
	// CheckLogs is how long raw check logs are kept, one of
	// CheckLogChoices.
	CheckLogs string
	// KeptHistory is "on" when repositories that follow the server keep
	// overwritten and deleted history, and "off" when they do not.
	KeptHistory string
	// DeleteRequiresName is "on" when deleting a repository asks for its
	// typed name, and "off" when it does not.
	DeleteRequiresName string
	// LoginAttempts, LoginWindow and LoginPause are the login attempt
	// limits: that many wrong passwords within the window pause an address.
	LoginAttempts           string
	LoginWindow, LoginPause LimitInput
	// CrossSiteLinks is one of CrossSiteChoices.
	CrossSiteLinks string
	// Browse, Maintenance and Cleanup are the browsing limits, repository
	// maintenance and unused object cleanup.
	Browse      BrowsePolicies
	Maintenance MaintenancePolicies
	Cleanup     CleanupPolicies
	// Ceilings are the check ceilings, and AboveCeilings the names of the
	// repositories whose check policy is above them.
	Ceilings      CeilingPolicies
	AboveCeilings []string
	// Visible is true for a confirmed administrator. Otherwise no saved
	// value is read or shown, and each group says where to confirm.
	Visible bool
	// Unreadable holds the groups whose saved value could not be read.
	// Such a group says so and shows the default as a change to save.
	Unreadable map[string]bool
}

// BrowsePolicies are the browsing limits as the form shows them.
type BrowsePolicies struct {
	Raw, File, CommitPatch, FilePatch, CommitFile, Compare, CompareTime LimitInput
}

// CeilingPolicies are the check ceilings as the form shows them.
type CeilingPolicies struct {
	Timeout, Output, CPU, Memory, Scratch, Source LimitInput
	Queue, Active, PIDs                           string
}

// MaintenancePolicies are the maintenance choices as the form shows them.
// Enabled is "on" or "off"; the window hours are 0 to 23.
type MaintenancePolicies struct {
	Enabled, WindowStart, WindowEnd, PackThreshold string
	Idle, Command, FullRepack                      LimitInput
}

// CleanupPolicies are the unused object cleanup choice as the form shows
// it. Enabled is "on" or "off", GraceDays a number of days.
type CleanupPolicies struct {
	Enabled, GraceDays string
}

func (SettingsPage) page() string     { return "settings" }
func (p SettingsPage) chrome() Chrome { return p.Chrome }

// ---------------------------------------------------------------------------
// Overview and activity
// ---------------------------------------------------------------------------

// OverviewPage renders "/". It is the empty dashboard right after setup, so
// every list can legitimately be empty.
type OverviewPage struct {
	Chrome Chrome
	// Activity is the annual graph across all repositories.
	Activity ActivityGraph
	// Repositories is the full list, search-filtered and in Chrome.Nav.Order.
	Repositories []RepositorySummary
	// Recent is the short "Latest activity" list under the repositories.
	Recent []ActivityEntry
	// RecentMoreURL links to the full activity page.
	RecentMoreURL string
	// TotalCount is the number of repositories before search filtering.
	TotalCount int
	// Release announces a newer OwnGit release. Nil shows nothing.
	Release *ReleaseNotice
}

func (OverviewPage) page() string     { return "overview" }
func (p OverviewPage) chrome() Chrome { return p.Chrome }

// ReleaseNotice is the dashboard notice for a newer OwnGit release. Every
// URL is set by the backend; NotesURL is built from the checked version.
type ReleaseNotice struct {
	// Version is the newer release, "X.Y.Z".
	Version string
	// Current is the running version.
	Current string
	// NotesURL is the release page.
	NotesURL string
	// GuideURL explains how to update.
	GuideURL string
	// Command updates this installation the way it was installed, for the
	// owner to copy and run. Empty shows only the guide.
	Command string
	// Start is the program to start after the command, because no service
	// runs this installation and the new program is in a new place.
	Start string
	// Restart says to restart OwnGit after the command, because no service
	// runs this installation.
	Restart bool
	// CommandHidden is true when a command exists but the viewer is not a
	// confirmed administrator, who alone sees the program's path.
	CommandHidden bool
	// DismissURL takes a POST with csrf and version.
	DismissURL string
}

// ActivityPage renders "/activity": the chronological list across every
// repository.
type ActivityPage struct {
	Chrome   Chrome
	Activity ActivityGraph
	// Days groups entries by calendar day, newest first.
	Days []ActivityDayGroup
	// Truncated is true when the year or the chosen day has more commits
	// than the list shows; ListLimit is how many it shows then.
	Truncated bool
	ListLimit int
	// OlderURL and NewerURL page through history. Empty when unavailable.
	OlderURL string
	NewerURL string
}

func (ActivityPage) page() string     { return "activity" }
func (p ActivityPage) chrome() Chrome { return p.Chrome }

// ActivityGraph is the annual daily commit graph.
//
// Counting rules are the backend's: author dates, current and retained
// historical branches, one count per commit per repository. The renderer only
// draws what it is given and reports incompleteness honestly.
type ActivityGraph struct {
	// Year is the displayed calendar year.
	Year int
	// Years are the selectable years, newest first.
	Years []ActivityYear
	// Days covers Jan 1 through Dec 31 of Year in calendar order. Days after
	// today carry Future.
	Days []ActivityDay
	// Total is the sum for the year.
	Total int
	// RepositoryCount is how many repositories were aggregated.
	RepositoryCount int
	// Scope names a single repository when the graph is scoped to one.
	Scope string
	// Complete is false when the backend could not finish counting. The
	// renderer then says the count is incomplete instead of showing a total
	// that reads as authoritative.
	Complete bool
	// IncompleteReason explains why. Used only when Complete is false.
	IncompleteReason MessageCode
	// Unreadable names the repositories left out because their Git data
	// could not be read.
	Unreadable []string
	// Available is false when activity could not be computed at all; the
	// renderer shows an explanation instead of an empty graph.
	Available bool
	// UnavailableReason explains why. Used only when Available is false.
	UnavailableReason MessageCode
	// SelectedDate highlights one day. Zero when nothing is selected.
	SelectedDate time.Time
}

// ActivityYear is one selectable year.
type ActivityYear struct {
	Year int
	URL  string
}

// ActivityDay is one calendar day of the graph.
type ActivityDay struct {
	Date   time.Time
	Count  int
	Future bool
	// URL filters the activity list to this day. Optional.
	URL string
}

// ActivityDayGroup is one day of the chronological list.
type ActivityDayGroup struct {
	Date    time.Time
	Entries []ActivityEntry
}

// ActivityEntry is one commit in an activity list.
type ActivityEntry struct {
	RepositoryID      string
	RepositoryName    string
	RepositoryAddress string
	RepositoryURL     string
	// Ref is the branch or ref this commit was observed on.
	Ref string
	// RefRetained marks a ref that no longer exists but whose history is
	// retained after a force-push or deletion.
	RefRetained bool
	Commit      CommitSummary
}

// RepositorySummary is one repository row on the overview.
type RepositorySummary struct {
	ID          string
	Name        string
	Description string
	URL         string
	// CloneURL is the Git clone address supplied by the backend.
	CloneURL  string
	CreatedAt time.Time
	// Empty is true for a repository with no commits yet.
	Empty bool
	// DefaultBranch is the branch shown for this repository. Empty when the
	// repository has no branches or the default branch is missing.
	DefaultBranch string
	// DefaultBranchMissing is true when the recorded default branch no longer
	// resolves, for example after it was deleted.
	DefaultBranchMissing bool
	// Head is the latest commit. Zero when Empty or HeadUnreadable.
	Head CommitSummary
	// HeadUnreadable is true when the default branch has a latest commit but
	// its author, date or subject could not be read. The row says so rather
	// than showing no latest commit.
	HeadUnreadable bool
	// BranchCount and TagCount are displayed when Counted is true.
	BranchCount   int
	TagCount      int
	RetainedCount int
	Counted       bool
	// Preparing is true while OwnGit prepares the repository after startup.
	// Nothing was read from Git, so only the name and description are known.
	Preparing bool
	// Unreadable is true when the repository's Git data could not be read
	// for this page. Only the name and description are known.
	Unreadable bool
	// Busy is true when another Git operation held the repository and no
	// earlier listing was available. Like Preparing, only the name and
	// description are known.
	Busy bool
	// Rank lets the script keep the order when the language changes.
	Rank NameRank
}

// Updated is the time the row shows as the repository's last update: the
// author date of its latest commit. It is zero when the row shows none,
// because the repository has no commits or was not read for this page.
func (s RepositorySummary) Updated() time.Time {
	if s.Empty || s.Preparing || s.Unreadable || s.Busy {
		return time.Time{}
	}
	return s.Head.AuthorDate
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// RepoTab selects the active repository tab.
type RepoTab string

const (
	RepoTabOverview RepoTab = "overview"
	RepoTabCode     RepoTab = "code"
	RepoTabCommits  RepoTab = "commits"
	// RepoTabPullRequests and RepoTabChecks are rendered by their own page
	// types, which share this repository's tab strip.
	RepoTabPullRequests RepoTab = "pull-requests"
	RepoTabChecks       RepoTab = "checks"
)

// RepoTabs are the addresses of a repository's sections.
//
// Every screen that belongs to one repository carries them so the strip stays
// identical wherever the reader is. A section with no URL is not rendered,
// which is how a caller that does not offer one avoids a dead tab.
type RepoTabs struct {
	OverviewURL     string
	CodeURL         string
	CommitsURL      string
	PullRequestsURL string
	TasksURL        string
	ImportsURL      string
	// SettingsURL and DeleteURL are set for every viewer. Settings sits in
	// the normal tab order; Delete is the right-aligned control that opens
	// the confirmation page. Both routes ask a viewer without an
	// administrator session to log in first.
	SettingsURL string
	DeleteURL   string
	// AdminLocked is true when the viewer has no administrator session, so
	// the administrator entries carry a lock shape and words that say the
	// link opens the administrator login first.
	AdminLocked bool
	// Active marks the current section.
	Active RepoTab
}

// RepositoryPage renders every /repositories/{id} screen. Tab selects which
// panel is filled; the other panels are links, not hidden content.
type RepositoryPage struct {
	Chrome Chrome
	Repo   RepositoryHeader
	Tab    RepoTab

	// Shared is true on a share link's pages. They show the repository's
	// code and history of its branches and tags, and link only to one
	// another: no pull requests, checks, kept history, restore or
	// downloads, and nothing outside the share.
	Shared bool
	// CloneHelp, when set, says under the clone address how Git signs in.
	CloneHelp MessageCode

	// NotFound is true when the address named a path, branch, tag, or
	// commit that this repository does not have. The page answers 404 and
	// offers the way back to the repository.
	NotFound bool

	OverviewURL string
	CodeURL     string
	CommitsURL  string
	// RestoreURL opens the restore screen for this repository. Empty means no
	// link, which is what an older caller that never sets it gets.
	RestoreURL string
	// PullRequestsURL, TasksURL, and ImportsURL add sections to the tab strip.
	// A caller that does not set one renders no dead link for it.
	PullRequestsURL string
	TasksURL        string
	ImportsURL      string
	// SettingsURL and DeleteURL are the administrator sections. They are
	// offered to every viewer; the routes ask for the administrator login
	// themselves and return to the page afterwards.
	SettingsURL string
	DeleteURL   string

	// Ref is the currently selected branch, tag, or revision.
	Ref RefSelection
	// Downloads offer the selected branch or tag on the Code tab's top
	// folder, or the open commit on its page, as archives. Empty means none.
	Downloads []ArchiveLink

	// Overview is used when Tab is RepoTabOverview.
	Overview RepositoryOverview
	// Code is used when Tab is RepoTabCode.
	Code CodeView
	// Commits is used when Tab is RepoTabCommits.
	Commits CommitsView
}

func (RepositoryPage) page() string     { return "repository" }
func (p RepositoryPage) chrome() Chrome { return p.Chrome }

// RepositoryHeader identifies the repository on every tab.
type RepositoryHeader struct {
	// ID names the repository in storage and records; Address is where it
	// answers, which links use.
	ID          string
	Address     string
	Name        string
	Description string
	URL         string
	// CloneURL is the Smart HTTP address the backend generated.
	CloneURL string
	// Empty is true when the repository has no commits. Every tab then shows
	// push instructions instead of pretending a history exists.
	Empty bool
	// Unreadable is true when the repository exists but its Git data could not
	// be read. The renderer says so instead of showing an empty history.
	Unreadable bool
	// UnreadableReason is the more specific reason, when one is known. The
	// page then adds it after saying that the data could not be read.
	UnreadableReason MessageCode
	// Preparing is true while OwnGit prepares the repository after startup.
	// Unreadable is then true as well, and the page explains the wait in
	// place of the unreadable notice.
	Preparing bool
}

// RefSelection describes the selected ref and the alternatives.
type RefSelection struct {
	// Name is the selected ref, for example "main" or "v1.2.0".
	Name string
	// Kind is "branch", "tag", or "revision".
	Kind string
	// IsDefault marks the repository's default branch.
	IsDefault bool
	// Missing is true when the requested ref does not resolve. The renderer
	// says so rather than silently substituting another ref.
	Missing bool
	// Detached is true when the view resolved to a bare revision.
	Detached bool
	// Retained is true when the ref itself is gone but its history is kept.
	Retained bool
	// Branches and Tags populate the picker.
	Branches []RefOption
	Tags     []RefOption
	// Revision is the resolved object id for the selected ref.
	Revision string
	// ShortRevision is the abbreviated form for display.
	ShortRevision string
}

// RefOption is one entry in the ref picker.
type RefOption struct {
	Name      string
	URL       string
	Selected  bool
	IsDefault bool
	// Retained marks history preserved after a force-push or deletion.
	Retained bool
}

// RepositoryOverview is the repository landing panel.
type RepositoryOverview struct {
	// Head is the latest commit on the selected ref. Zero when empty.
	Head CommitSummary
	// Recent are the newest commits on the selected ref, Head first. The
	// backend bounds the list; RecentMore says older commits exist.
	Recent     []CommitSummary
	RecentMore bool
	// DefaultBranch names the repository's default branch. Empty when the
	// repository has none that resolves.
	DefaultBranch string
	// OpenPullRequests counts open pull requests up to a bound.
	// OpenPullRequestsMore means the real count is above it, and
	// OpenPullRequestsKnown is false when the count could not be read.
	OpenPullRequests      int
	OpenPullRequestsMore  bool
	OpenPullRequestsKnown bool
	// DefaultCheck is the latest check attempt for the default branch tip.
	// DefaultCheckKnown is false when the record could not be read, and
	// HasDefaultCheck is false when no check has run for that revision.
	DefaultCheck      AttemptRecord
	HasDefaultCheck   bool
	DefaultCheckKnown bool
	DefaultCheckRev   string
	// Branches and Tags are the ref rows shown in the side column, newest
	// first. With many refs only the newest few are listed; BranchCount and
	// TagCount keep the real totals, and AllRefsURL shows every ref.
	// BranchTipsKnown and TagTipsKnown are false when the rows' latest
	// commits could not be read; the rows are then not in date order.
	Branches        []RefLine
	Tags            []RefLine
	BranchCount     int
	TagCount        int
	BranchTipsKnown bool
	TagTipsKnown    bool
	// RetainedCount counts the kept history. RetainedKnown is false when it
	// could not be read, and RetainedCount and RetainedRefs are then empty.
	RetainedCount int
	RetainedKnown bool
	// AllRefsURL lists every branch and tag. Empty when nothing is hidden.
	// FewerRefsURL returns to the short lists while every ref is shown.
	AllRefsURL   string
	FewerRefsURL string
	// RetainedRefs lists preserved history whose ref no longer exists.
	RetainedRefs []RefLine
	// Activity is the graph scoped to this repository.
	Activity ActivityGraph
	// PushCommands are the copyable first-push lines for an empty repository.
	// They contain no credentials.
	PushCommands []string
	// Readme is the README at the top of the selected ref, rendered like a
	// folder README in the code view. Nil when there is none.
	Readme *ReadmeView
	// Languages is the language make-up of the default branch.
	Languages LanguageSummary
}

// LanguageSummary is the Languages panel: up to six languages by size and
// then "Other", or a short note when no share can be shown.
type LanguageSummary struct {
	Rows []LanguageRow
	// Note explains an empty panel: nothing detected, too large, or not
	// counted. Empty when Rows are shown.
	Note MessageCode
	// AttributesNote says why .gitattributes language settings were not
	// applied: the host Git cannot read them from a commit, or they could
	// not be read. Empty when they were applied or there are none.
	AttributesNote MessageCode
}

// LanguageRow is one language, or the folded rest when Other is true.
type LanguageRow struct {
	Name string
	// Color is a "#rrggbb" value from the language table. Empty for Other,
	// which uses the neutral theme color.
	Color string
	// Share is the exact percentage, used for the bar segment width.
	Share float64
	// Percent is the shown share with one decimal, for example "93.9%".
	Percent string
	Other   bool
}

// RefLine is one branch or tag row.
type RefLine struct {
	Name string
	URL  string
	// Kind is "branch" or "tag".
	Kind string
	// IsDefault marks the default branch.
	IsDefault bool
	// Retained marks preserved history after a force-push or deletion.
	Retained bool
	// Annotated marks an annotated tag.
	Annotated bool
	// Tip is the commit this ref points at. Zero when unavailable.
	Tip CommitSummary
	// RestoreURL opens the restore screen with this ref's tip preselected as
	// the source. Empty means no link.
	RestoreURL string
}

// PageContinuation describes one bounded page, not an accumulated prefix.
type PageContinuation struct {
	First, Last, Total int
	MoreURL, FirstURL  string
	// Incomplete means Total counts only the prefix read within a byte limit.
	Incomplete bool
}

func (p PageContinuation) Shown() int { return max(0, p.Last-p.First+1) }

// CodeView is the file browser panel.
type CodeView struct {
	Continuation PageContinuation
	// ListingUnavailable keeps a failed drawer distinct from an empty folder.
	ListingUnavailable bool
	// Path is the current repository-relative path. Empty at the root.
	Path string
	// Crumbs are the navigable path segments, root first.
	Crumbs []Crumb
	// UpURL leaves the listed folder for its parent. Empty when the listed
	// folder is the root.
	UpURL string
	// Dir is the folder Entries list: Path for a folder, and the file's
	// folder when a file is open. Empty at the root.
	Dir string
	// Entries are the directory listing, already sorted.
	Entries []TreeEntry
	// File is set when Path names a file.
	File *FileView
	// Readme is the folder's rendered README. Nil when the folder has none or
	// a file is open.
	Readme *ReadmeView
	// NotFound is true when Path does not exist at the selected ref.
	NotFound bool
}

// Crumb is one breadcrumb segment.
type Crumb struct {
	Name string
	URL  string
	// Current marks the final segment, which is not a link.
	Current bool
}

// TreeEntry is one directory or file in the listing.
type TreeEntry struct {
	Name string
	Path string
	URL  string
	// Kind is "dir", "file", "symlink", or "submodule".
	Kind string
	// Size is the blob size in bytes. Valid for files only.
	Size int64
}

// FileView is one file's displayed content. The renderer escapes every line;
// repository content is never treated as markup. The one exception is
// Rendered, which is HTML the backend produced with internal/markdown.
type FileView struct {
	Path string
	Size int64
	// Lines are the text lines without trailing newlines.
	Lines        []string
	FirstLine    int
	LineURL      string
	Continuation PageContinuation
	// Binary is true when the file is not displayable text.
	Binary bool
	// Truncated is true when only the first Lines were loaded.
	Truncated bool
	// TooLarge is true when the file is above what this server can read into
	// memory. The page then shows one notice instead of lines, a kind guess
	// or a download link.
	TooLarge bool
	// TooLargeMemory is true when the read was refused not by this file's own
	// size but by the memory its stored delta needs to rebuild: a small file
	// on a much larger base costs more to read than this computer gives Git.
	// The notice names that reason instead of the size.
	TooLargeMemory bool
	// RawURL downloads the file. Empty when the backend does not offer it.
	RawURL string
	// RawCurrentRef distinguishes a moving-ref download from a pinned page.
	RawCurrentRef bool
	// RawTooLarge is true when the file is above the download limit; the
	// page then says so instead of offering RawURL.
	RawTooLarge bool
	// Image is true for a raster picture (PNG, JPEG, GIF or WebP) that the
	// page shows through RawURL. The backend sets it only when the file's
	// bytes match its type and it is within the download limit. SVG and other
	// formats that can carry script are never shown as a picture.
	// ImageWidth and ImageHeight are its pixel size, zero when unknown.
	Image       bool
	ImageWidth  int
	ImageHeight int
	// RestoreURL opens the restore screen with this file preselected. Empty
	// means no link.
	RestoreURL string

	// Document is true for a Markdown file. PreviewURL and SourceURL switch
	// between the rendered document and its source lines; ShowSource says
	// which one this page shows. Rendered is empty when the file could not
	// be rendered; the page then shows the source without the switch, with
	// NotRendered saying why.
	Document    bool
	NotRendered MessageCode
	ShowSource  bool
	PreviewURL  string
	SourceURL   string
	// Rendered is the document as HTML. The backend produces it with
	// internal/markdown, which never passes raw HTML from the file through
	// and resolves every link and image itself.
	Rendered template.HTML
}

// ReadmeView is a folder's README, rendered below the folder listing.
type ReadmeView struct {
	// Path is the README's repository path; URL opens it in the file view.
	Path string
	URL  string
	// Rendered is produced like FileView.Rendered. When it is empty, Note
	// says why and the page links to the file instead.
	Rendered template.HTML
	Note     MessageCode
}

// CommitsView is the commit history panel.
type CommitsView struct {
	// List is the history of the selected ref, newest first.
	List []CommitSummary
	// OlderURL and NewerURL page through history. Empty when unavailable.
	OlderURL string
	NewerURL string
	// Detail is the opened commit. Nil on the list-only view.
	Detail *CommitDetail
	// NotFound is true when a requested commit id does not resolve.
	NotFound bool
	// Unreadable is true when commits of this view could not be read. The
	// page's notices name them; List holds the others, and an opened commit
	// that could not be read has no Detail.
	Unreadable bool
}

// CommitSummary is one commit row.
type CommitSummary struct {
	// OID is the full object id; ShortOID is the abbreviated display form.
	OID      string
	ShortOID string
	// Subject is the first line of the message.
	Subject string
	// AuthorName is the commit author. AuthorDate drives activity counting.
	AuthorName string
	AuthorDate time.Time
	URL        string
}

// CommitDetail is one opened commit with its diff.
type CommitDetail struct {
	Commit CommitSummary
	// Body is the message after the subject line.
	Body string
	// CommitterName and CommitterDate are shown when they differ from the
	// author, so the author date is not read as the whole story. See
	// HasDistinctCommitter.
	CommitterName string
	CommitterDate time.Time
	// Parents are the parent commits.
	Parents []CommitSummary
	// Files are the changed paths.
	Files []DiffFile
	// SelectedPath is set when the commit was opened for one file: Files
	// then holds only that file, and AllFilesURL shows every file again.
	SelectedPath string
	AllFilesURL  string
	Continuation PageContinuation
	// FilePages places Files among all changed files when they span several
	// pages. Continuation counts lines of one file and never files.
	FilePages PageContinuation
	// Truncated is true when the diff was too large to load completely.
	Truncated bool
	// Unavailable is true when the diff could not be produced, for example for
	// a merge commit the backend does not expand.
	Unavailable bool
	// RestoreURL opens the restore screen with this commit as the source.
	// Empty means no link.
	RestoreURL string

	// UnavailableReason explains why. Used only when Unavailable is true.
	UnavailableReason MessageCode
}

// HasDistinctCommitter reports whether the committer identity differs from the
// author's in name or in time. Git records both, and they are ordinary
// metadata a client may set to any value, so a difference is only a difference:
// it does not prove an amend, a rebase, or any particular order of events. A
// committer date can even precede the author date. The interface therefore
// shows both identities and leaves the interpretation to the reader.
//
// Comparing only names would hide the common case where one person rewrites
// their own commit, since the name stays the same while the dates diverge.
//
// The dates are compared as instants, so the same moment recorded with a
// different UTC offset is not a difference.
func (d CommitDetail) HasDistinctCommitter() bool {
	if d.CommitterName == "" {
		// The backend has nothing to report about the committer.
		return false
	}
	if d.CommitterName != d.Commit.AuthorName {
		return true
	}
	if d.CommitterDate.IsZero() || d.Commit.AuthorDate.IsZero() {
		// Without both dates there is no difference to state.
		return false
	}
	return !d.CommitterDate.Equal(d.Commit.AuthorDate)
}

// DiffFile is one changed path in a commit.
type DiffFile struct {
	// Path is the current path; OldPath is set for renames and copies.
	Path    string
	OldPath string
	// Status is "added", "modified", "deleted", "renamed", or "copied".
	Status string
	// Additions and Deletions are the line counts.
	Additions int
	Deletions int
	// Binary is true when no text diff exists.
	Binary              bool
	TextDiffUnavailable bool
	// CountsUnknown is true when the line counts were never read, because the
	// file was left out of the read (see repository.ChangedFile.CountsRead),
	// so a zero count must not be shown as no changes.
	CountsUnknown bool
	// URL shows this file's changes alone, for example within a commit.
	URL string
	// Selected marks the file a single-file commit view was opened for.
	Selected bool
	// Hunks hold the diff body.
	Hunks []DiffHunk
	// NotLoaded is true when the file's text changes were left out because
	// the whole change was too large for one page. A non-empty URL then
	// shows them.
	NotLoaded bool
}

// DiffTotals sums the line counts of a list of changed files.
type DiffTotals struct {
	Additions int
	Deletions int
	// Text is true when at least one file has line counts, so a list of
	// binary files shows no misleading "+0 -0".
	Text bool
}

// DiffHunk is one @@ section.
type DiffHunk struct {
	// Header is the literal hunk header line.
	Header string
	Lines  []DiffLine
}

// DiffLine is one diff row. Kind is "context", "add", or "del".
type DiffLine struct {
	Kind string
	// OldLine and NewLine are 0 when the side has no number.
	OldLine int
	NewLine int
	// Text excludes the leading +/-/space marker.
	Text string
}

// ---------------------------------------------------------------------------
// Repository creation
// ---------------------------------------------------------------------------

// NewRepositoryPage renders GET /repositories/new. The form posts to
// /repositories with "name" and optional "description".
type NewRepositoryPage struct {
	Chrome    Chrome
	SubmitURL string
	// Name and Description redisplay the submitted values after a failure.
	Name        string
	Description string
	// NameRules is shown next to the field so the rule is visible before a
	// failure, not only after it.
	NameRules MessageCode
}

func (NewRepositoryPage) page() string     { return "new-repository" }
func (p NewRepositoryPage) chrome() Chrome { return p.Chrome }

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

// Restore selection values submitted in the "mode" field.
const (
	// RestoreModeAll restores the whole project tree as the source commit
	// recorded it, including deleting files the source does not have.
	RestoreModeAll = "all"
	// RestoreModeFiles restores only the paths submitted in the repeated
	// "path" field.
	RestoreModeFiles = "files"
)

// RestoreConfirm is the value the "confirm" field must carry before the apply
// request is a restore request at all. The reader ticks it after reading the
// preview, and the backend checks it again; neither side treats a rendered
// form as permission.
const RestoreConfirm = "restore"

// RestorePage renders GET /repositories/{id}/restore and the result of
// POST /repositories/{id}/restore/preview.
//
// Restoring is two deliberate steps. The first form posts the selection to
// PreviewURL and gets back the same page with Previewed set and Changes
// filled. The second form posts that reviewed selection to ApplyURL together
// with ExpectedHead and confirm=restore. Every field is redisplayed input:
// the backend revalidates the selection, recomputes the result, and refuses a
// target whose tip no longer matches ExpectedHead.
//
// Applying writes a new commit on the target branch. It never rewrites the
// branch's existing history, and it cannot reach a working copy on another
// computer.
type RestorePage struct {
	Chrome Chrome
	Repo   RepositoryHeader
	// Tabs are the repository's sections, shown in the sidebar.
	Tabs RepoTabs
	// Source is the commit the files come from, already resolved by the
	// backend. Its OID is submitted in the "source" field.
	Source CommitSummary
	// TargetBranch is the branch the restore writes to, submitted in the
	// "target" field. It may name a branch that does not exist, which the
	// restore creates at the selected commit.
	TargetBranch string
	// CreatesBranch reports that restoring would create TargetBranch rather
	// than add to an existing one. The backend observes this together with
	// ExpectedHead, so the page describes the branch from one observation
	// instead of inferring it from Branches, which is read separately and can
	// disagree with what the preview was computed against.
	CreatesBranch bool
	// Branches are existing branch names, offered only as suggestions. The
	// target is typed, because recreating a branch that was deleted is how
	// deleted work comes back and such a name is not in this list.
	Branches []RefOption
	// Mode is RestoreModeAll or RestoreModeFiles, submitted in the "mode"
	// field.
	Mode string
	// Paths are the source commit's files, each with the change restoring it
	// would make and whether it is currently selected. Selected paths are
	// submitted in the repeated "path" field.
	Paths []RestorePath
	// Changes are the previewed differences between the target branch and the
	// restored result, including deletions. Filled only after a preview.
	Changes []DiffFile
	// ExpectedHead is the target tip the preview was computed against, or the
	// zero OID when the target does not exist. It is submitted with the apply
	// request so a branch that moved in the meantime fails instead of being
	// overwritten.
	ExpectedHead string
	// Previewed is true once the page shows the result of a preview. Until
	// then there is no apply form at all.
	Previewed bool
	// CanApply is the backend's decision that this previewed selection is
	// still applicable. False disables the apply control; the reason arrives
	// as a notice.
	CanApply bool
	// DiffTruncated is true when a file's text diff was too large to show in
	// full. The changed paths are still listed completely.
	DiffTruncated bool

	// PreviewURL and ApplyURL are the two POST targets. CancelURL returns to
	// the repository.
	PreviewURL string
	ApplyURL   string
	CancelURL  string
}

func (RestorePage) page() string     { return "restore" }
func (p RestorePage) chrome() Chrome { return p.Chrome }

// RestorePath is one file of the source commit offered for selection.
//
// Path is a repository path presented as text. It is never a host path, and
// the renderer escapes it like any other repository content.
type RestorePath struct {
	Path string
	// Status is the change restoring this path would make to the target
	// branch: "added", "modified", or "deleted". A deletion is listed as its
	// own entry, because a selected source that lacks a file removes that file
	// from the branch, and nobody should have to infer that from a directory.
	Status string
	// Selected is the current checkbox state.
	Selected bool
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrorPage renders an HTTP error inside the normal shell when possible.
type ErrorPage struct {
	Chrome Chrome
	// Status is the HTTP status code.
	Status int
	// Code selects the explanation text.
	Code MessageCode
	// Detail is an untranslated fragment such as an unknown path.
	Detail string
	// RetryURL offers a way forward. Empty when there is none.
	RetryURL string
	// RetryLabel defaults to the home label when empty.
	RetryLabel MessageCode
}

func (ErrorPage) page() string     { return "error" }
func (p ErrorPage) chrome() Chrome { return p.Chrome }
