package recovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"owngit/internal/pullrequest"
	"owngit/internal/repository"
	"owngit/internal/state"
)

// newRecordsBackupStore is a backup store with a pull request, an import
// source, and one row of every schema 16 record: portable ones with values
// only format 11 holds, and machine-local ones that no backup carries.
func newRecordsBackupStore(t *testing.T, root string) (*state.Store, *repository.Manager) {
	t.Helper()
	ctx := context.Background()
	store, manager := newBackupStore(t, root)
	work := filepath.Join(root, "backup-work")
	remote, err := manager.Path("project")
	noErr(t, err)
	runGit(t, work, "checkout", "-b", "feature")
	noErr(t, os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "feature")
	runGit(t, work, "push", remote, "HEAD:refs/heads/feature")
	service := &pullrequest.Service{Store: store, Repositories: manager}
	_, err = service.Create(ctx, pullrequest.CreateInput{Repository: "project", Title: "Described", SourceBranch: "feature", TargetBranch: "main"})
	noErr(t, err)
	_, err = store.ConfigureImportSource(ctx, state.ImportSourceInput{RepositoryID: "project", URL: "https://example.invalid/source.git", Mode: state.ImportModeCoexistence, Now: time.Now()})
	noErr(t, err)
	for _, statement := range []string{
		`UPDATE pull_requests SET body='What and why',edit_revision=1,edited_at=created_at+60,created_by='{"kind":"access"}',
			edited_by='{"kind":"helper_credential","id":"0123456789abcdef0123456789abcdef","label":"agent"}'`,
		`UPDATE pull_request_reviews SET note='Checked both revisions',actor='{"kind":"administrator"}'`,
		`INSERT INTO repository_names(name,repository_id,kind,created_at,alias_until) VALUES('renamed','project','current',1800000000,NULL),('project','project','alias',1800000000,1807776000)`,
		`INSERT INTO repository_policies(repository_id,retain_history,protect_default_branch,extra_ref_prefixes,updated_at) VALUES('project',0,1,'["refs/notes/"]',1800000000)`,
		`UPDATE import_sources SET overwrite_diverged=1,extra_ref_prefixes='["refs/changes/"]',allow_private_network=1,allow_plain_http=1,redirect_policy='same_origin'`,
		`INSERT INTO share_links(id,repository_id,secret_hash,scope,created_at) VALUES('00000000000000000000000000000001','project',zeroblob(32),'clone',1800000000)`,
		`INSERT INTO backup_plans(name,enabled,interval_seconds,destination,keep,verify,created_at,updated_at) VALUES('default',1,86400,'/home/example/backups',7,1,1800000000,1800000000)`,
		`INSERT INTO backup_runs(id,kind,status,destination,started_at) VALUES('00000000000000000000000000000002','manual','succeeded','/home/example/backups',1800000000)`,
		`INSERT INTO push_events(repository_id,ref_name,old_oid,new_oid,refs_updated,pushed_at) VALUES('project','refs/heads/main','','` + strings.Repeat("a", 40) + `',1,1800000000)`,
		`INSERT INTO metadata(key,value) VALUES('admin_confirmation','never')`,
	} {
		noErr(t, store.Exec(ctx, statement))
	}
	return store, manager
}

// A backup with records that only format 11 holds is written in format 11,
// restores them all, and brings back no machine-local item: no share link,
// schedule, recent push, connection choice or remembered confirmation.
func TestFormat11RecordsSurviveBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, manager := newRecordsBackupStore(t, root)
	before, err := store.RecoverySnapshot(ctx)
	noErr(t, err)

	backup := filepath.Join(root, "backup")
	_, err = CreateWithReport(ctx, store, manager, backup)
	noErr(t, err)
	manifest, err := readManifest(filepath.Join(backup, manifestName))
	noErr(t, err)
	if manifest.Version != recordsBackupVersion || format11Content(manifest) == "" {
		t.Fatalf("backup version=%d content=%q, want %d", manifest.Version, format11Content(manifest), recordsBackupVersion)
	}

	restoredState := canonicalTestTarget(t, filepath.Join(root, "restored-state"))
	if _, err := RestoreWithReport(ctx, backup, restoredState, canonicalTestTarget(t, filepath.Join(root, "restored-repositories")), ""); err != nil {
		t.Fatal(err)
	}
	restored, err := state.Open(ctx, restoredState)
	noErr(t, err)
	defer restored.Close()
	after, err := restored.RecoverySnapshot(ctx)
	noErr(t, err)
	for name, pair := range map[string][2]any{
		"pull requests": {before.PullRequests, after.PullRequests},
		"reviews":       {before.PullRequestReviews, after.PullRequestReviews},
		"names":         {before.RepositoryNames, after.RepositoryNames},
		"policies":      {before.RepositoryPolicies, after.RepositoryPolicies},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("%s changed through the backup:\n%+v\n%+v", name, pair[0], pair[1])
		}
	}
	// The restored names answer as they did: the current name, and the old
	// one until its alias ends.
	for _, check := range []struct {
		name    string
		at      int64
		current string
	}{{"renamed", 1800000001, "renamed"}, {"project", 1800000001, "renamed"}, {"project", 1807776000, ""}} {
		address, found, err := restored.ResolveRepositoryName(ctx, check.name, time.Unix(check.at, 0))
		noErr(t, err)
		if found != (check.current != "") || (found && (address.RepositoryID != "project" || address.Current != check.current)) {
			t.Fatalf("restored %q at %d reached %+v found=%v", check.name, check.at, address, found)
		}
	}
	if got := after.ImportSources[0]; !got.OverwriteDiverged || got.FollowUpstreamDeletions || !reflect.DeepEqual(got.ExtraRefPrefixes, []string{"refs/changes/"}) || got.AllowPrivateNetwork {
		t.Fatalf("restored import source=%+v", got)
	}
	for _, table := range []string{"share_links", "backup_plans", "backup_runs", "push_events", "sessions", "helper_credentials"} {
		if count, err := restored.TableRowCount(ctx, table); err != nil || count != 0 {
			t.Fatalf("%s has %d rows after restore, err=%v", table, count, err)
		}
	}
	if count, err := restored.TableRowCount(ctx, "import_sources"); err != nil || count != 1 {
		t.Fatalf("import sources=%d err=%v", count, err)
	}
	for _, condition := range []string{
		`EXISTS(SELECT 1 FROM import_sources WHERE allow_private_network=0 AND allow_plain_http=0 AND redirect_policy='refuse')`,
		`NOT EXISTS(SELECT 1 FROM metadata WHERE key='admin_confirmation')`,
	} {
		if !holds(t, restored, condition) {
			t.Fatalf("machine-local state came back: %s is false", condition)
		}
	}

	// A version 10 manifest that holds any of these records is refused,
	// never restored without them.
	for name, edit := range map[string]func(*Manifest){
		"description": func(m *Manifest) {
			m.Repositories[0].Names, m.Repositories[0].Policy, m.PullRequestReviews = nil, nil, clearReviews(m.PullRequestReviews)
		},
		"name": func(m *Manifest) { m.PullRequests, m.Repositories[0].Policy = clearPullRequests(m.PullRequests), nil },
	} {
		older := manifest
		older.Repositories = append([]RepositoryManifest(nil), manifest.Repositories...)
		older.PullRequests = append([]PullRequestManifest(nil), manifest.PullRequests...)
		older.PullRequestReviews = append([]PullRequestReviewManifest(nil), manifest.PullRequestReviews...)
		older.ImportSources = nil
		older.Version = closedPullRequestBackupVersion
		edit(&older)
		if err := validateManifest(older); err == nil || !strings.Contains(err.Error(), "version 10 backup contains") {
			t.Fatalf("version 10 manifest with a %s: err=%v", name, err)
		}
	}
}

// holds evaluates an SQL condition in store, which has no accessor for the
// machine-local columns a restore resets.
func holds(t *testing.T, store *state.Store, condition string) bool {
	t.Helper()
	ctx := context.Background()
	noErr(t, store.Exec(ctx, "CREATE TEMP TABLE condition_probe AS SELECT 1 AS one WHERE "+condition))
	defer func() { noErr(t, store.Exec(ctx, "DROP TABLE condition_probe")) }()
	count, err := store.TableRowCount(ctx, "condition_probe")
	noErr(t, err)
	return count == 1
}

func clearPullRequests(records []PullRequestManifest) []PullRequestManifest {
	cleared := append([]PullRequestManifest(nil), records...)
	for index := range cleared {
		cleared[index].Body, cleared[index].EditRevision, cleared[index].EditedAt = "", 0, nil
		cleared[index].CreatedBy, cleared[index].EditedBy = state.Actor{}, state.Actor{}
	}
	return cleared
}

func clearReviews(records []PullRequestReviewManifest) []PullRequestReviewManifest {
	cleared := append([]PullRequestReviewManifest(nil), records...)
	for index := range cleared {
		cleared[index].Note, cleared[index].Actor = "", state.Actor{}
	}
	return cleared
}

// A backup whose records format 10 can hold stays format 10, which the
// release before this one restores, even when a repository has a policy
// row that keeps every default, or an import source has a machine-local
// sign-in revision. A restore counts as a sign-in change: the sign-in
// revision is the restored source's new authority revision.
func TestBackupWithoutFormat11RecordsStaysFormat10(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, manager := newBackupStore(t, root)
	noErr(t, store.Exec(ctx, `INSERT INTO repository_policies(repository_id,updated_at) VALUES('project',1800000000)`))
	noErr(t, store.Exec(ctx, `INSERT INTO share_links(id,repository_id,secret_hash,scope,created_at) VALUES('00000000000000000000000000000001','project',zeroblob(32),'browse',1800000000)`))
	_, err := store.ConfigureImportSource(ctx, state.ImportSourceInput{RepositoryID: "project", URL: "https://example.invalid/source.git", Mode: state.ImportModeCoexistence, Now: time.Now()})
	noErr(t, err)
	noErr(t, store.Exec(ctx, `UPDATE import_sources SET authority_revision=5,sign_in_revision=4`))
	backup := filepath.Join(root, "backup")
	_, err = CreateWithReport(ctx, store, manager, backup)
	noErr(t, err)
	manifest, err := readManifest(filepath.Join(backup, manifestName))
	noErr(t, err)
	if manifest.Version != closedPullRequestBackupVersion {
		t.Fatalf("backup version=%d, want %d", manifest.Version, closedPullRequestBackupVersion)
	}
	restoredState := canonicalTestTarget(t, filepath.Join(root, "restored-state"))
	if _, err := RestoreWithReport(ctx, backup, restoredState, canonicalTestTarget(t, filepath.Join(root, "restored-repositories")), ""); err != nil {
		t.Fatal(err)
	}
	restored, err := state.Open(ctx, restoredState)
	noErr(t, err)
	defer restored.Close()
	for _, table := range []string{"repository_policies", "share_links"} {
		if count, err := restored.TableRowCount(ctx, table); err != nil || count != 0 {
			t.Fatalf("%s has %d rows after restore, err=%v", table, count, err)
		}
	}
	source, exists, err := restored.ImportSource(ctx, "project")
	if err != nil || !exists || source.AuthorityRevision != 6 || source.SignInRevision != 6 {
		t.Fatalf("restored import source = %+v exists=%v err=%v", source, exists, err)
	}
}

// Any state the product accepts backs up and restores: many pull requests
// and review notes at the largest allowed text, made of the characters HTML
// escaping would grow sixfold, give a manifest past the 64 MiB that version
// 10 readers accept, which version 11 holds without escaping them. One
// description and one note are control characters, which JSON must escape
// sixfold, so a text at its longest encoded form restores too.
func TestBackupAtTheProductTextLimits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, manager := newBackupStore(t, root)
	remote, err := manager.Path("project")
	noErr(t, err)
	oid := gitOutput(t, remote, "rev-parse", "refs/heads/main")
	text := strings.Repeat("<&>", state.MaximumPullRequestTextBytes/3) + strings.Repeat("<", state.MaximumPullRequestTextBytes%3)
	escaped := strings.Repeat("\x01", state.MaximumPullRequestTextBytes)
	textOf := func(number, sequence int) string {
		if number == 1 && sequence <= 1 {
			return escaped
		}
		return text
	}
	const pullRequests, reviewsEach = 8, 130
	var refs strings.Builder
	noErr(t, store.Exec(ctx, "BEGIN"))
	for number := 1; number <= pullRequests; number++ {
		noErr(t, store.Exec(ctx, `INSERT INTO pull_requests(repository_id,number,title,source_branch,target_branch,status,created_at,updated_at,body) VALUES('project',?,'Large',?,'main','open',1800000000,1800000000,?)`,
			number, "feature-"+strconv.Itoa(number), textOf(number, 0)))
		noErr(t, store.Exec(ctx, `INSERT INTO pull_request_revisions(repository_id,pull_request_number,source_oid,target_oid,recorded_at) VALUES('project',?,?,?,1800000000)`, number, oid, oid))
		for sequence := 1; sequence <= reviewsEach; sequence++ {
			noErr(t, store.Exec(ctx, `INSERT INTO pull_request_reviews(repository_id,pull_request_number,sequence,source_oid,target_oid,status,reviewer_label,provenance,created_at,note) VALUES('project',?,?,?,?,'approved','tool','supplied_external_tool',1800000000,?)`,
				number, sequence, oid, oid, textOf(number, sequence)))
		}
		sourceRef, targetRef := pullrequest.RevisionRefNames(int64(number), oid, oid)
		fmt.Fprintf(&refs, "create %s %s\ncreate %s %s\n", sourceRef, oid, targetRef, oid)
	}
	noErr(t, store.Exec(ctx, "COMMIT"))
	command := exec.Command("git", "--git-dir", remote, "update-ref", "--stdin")
	command.Stdin = strings.NewReader(refs.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create revision refs: %v %s", err, output)
	}

	backup := filepath.Join(root, "backup")
	_, err = CreateWithReport(ctx, store, manager, backup)
	noErr(t, err)
	info, err := os.Stat(filepath.Join(backup, manifestName))
	noErr(t, err)
	texts := int64(pullRequests * (1 + reviewsEach) * state.MaximumPullRequestTextBytes)
	if info.Size() <= format10ManifestLimit || info.Size() > texts+texts/10 {
		t.Fatalf("manifest is %d bytes for %d bytes of text", info.Size(), texts)
	}
	restoredState := canonicalTestTarget(t, filepath.Join(root, "restored-state"))
	if _, err := RestoreWithReport(ctx, backup, restoredState, canonicalTestTarget(t, filepath.Join(root, "restored-repositories")), ""); err != nil {
		t.Fatal(err)
	}
	restored, err := state.Open(ctx, restoredState)
	noErr(t, err)
	defer restored.Close()
	snapshot, err := restored.RecoverySnapshot(ctx)
	noErr(t, err)
	if len(snapshot.PullRequests) != pullRequests || len(snapshot.PullRequestReviews) != pullRequests*reviewsEach {
		t.Fatalf("restored %d pull requests and %d reviews", len(snapshot.PullRequests), len(snapshot.PullRequestReviews))
	}
	for _, record := range snapshot.PullRequests {
		if record.Body != textOf(int(record.Number), 0) {
			t.Fatalf("pull request %d description changed", record.Number)
		}
	}
	for _, review := range snapshot.PullRequestReviews {
		if review.Note != textOf(int(review.PullRequestNumber), int(review.Sequence)) {
			t.Fatalf("review %d/%d note changed", review.PullRequestNumber, review.Sequence)
		}
	}
}

// Every field that format 11 added makes a backup format 11. A field added
// to a manifest record later changes the counts pinned here, so its author
// decides whether it needs a new format instead of it silently travelling
// in format 10.
func TestEveryFormat11FieldNeedsFormat11(t *testing.T) {
	blank := func() Manifest {
		return Manifest{Repositories: []RepositoryManifest{{}}, PullRequests: []PullRequestManifest{{}}, PullRequestReviews: []PullRequestReviewManifest{{}}, ImportSources: []ImportSourceManifest{{}},
			CheckPolicies: []CheckPolicyManifest{{Execution: state.CheckExecutionSettings{ContainerNetwork: state.ContainerNetworkBridge}}}}
	}
	if content := format11Content(blank()); content != "" {
		t.Fatalf("empty records need format 11: %s", content)
	}
	// Import history of HEAD, branches and tags, with kept-history refs, is
	// what format 10 readers accept.
	oid := strings.Repeat("a", 40)
	history := blank()
	history.ImportObservations = []ImportObservationManifest{{RefName: state.ImportHeadRef}, {RefName: "refs/heads/main"}, {RefName: "refs/tags/v1"}}
	history.ImportIntents = []ImportIntentManifest{{
		Expected: map[string]string{"refs/heads/main": oid, "refs/owngit/retained/heads/" + oid: ""},
		Desired:  map[string]string{"refs/heads/main": strings.Repeat("b", 40), "refs/owngit/retained/heads/" + oid: oid},
		Observed: map[string]string{"refs/heads/main": strings.Repeat("b", 40)},
		Retained: map[string]string{"refs/owngit/retained/heads/" + oid: oid},
	}}
	if content := format11Content(history); content != "" {
		t.Fatalf("branch and tag import history needs format 11: %s", content)
	}
	until := time.Unix(1_800_000_000, 0)
	helper := state.Actor{Kind: state.ActorAccess}
	for name, edit := range map[string]func(*Manifest){
		"repository names":          func(m *Manifest) { m.Repositories[0].Names = []RepositoryNameManifest{{Name: "renamed"}} },
		"repository policy":         func(m *Manifest) { m.Repositories[0].Policy = &RepositoryPolicyManifest{} },
		"empty SHA-256 repository":  func(m *Manifest) { m.Repositories[0].ObjectFormat = "sha256" },
		"description":               func(m *Manifest) { m.PullRequests[0].Body = "x" },
		"edit revision":             func(m *Manifest) { m.PullRequests[0].EditRevision = 1 },
		"edited at":                 func(m *Manifest) { m.PullRequests[0].EditedAt = &until },
		"created by":                func(m *Manifest) { m.PullRequests[0].CreatedBy = helper },
		"edited by":                 func(m *Manifest) { m.PullRequests[0].EditedBy = helper },
		"merged by":                 func(m *Manifest) { m.PullRequests[0].MergedBy = helper },
		"review note":               func(m *Manifest) { m.PullRequestReviews[0].Note = "x" },
		"review actor":              func(m *Manifest) { m.PullRequestReviews[0].Actor = helper },
		"overwrite diverged":        func(m *Manifest) { m.ImportSources[0].OverwriteDiverged = true },
		"follow upstream deletions": func(m *Manifest) { m.ImportSources[0].FollowUpstreamDeletions = true },
		"import extra refs":         func(m *Manifest) { m.ImportSources[0].ExtraRefPrefixes = []string{"refs/notes/"} },
		"extra namespace observation": func(m *Manifest) {
			m.ImportObservations = []ImportObservationManifest{{RefName: "refs/notes/commits"}}
		},
		"extra namespace intent": func(m *Manifest) {
			m.ImportIntents = []ImportIntentManifest{{Observed: map[string]string{"refs/notes/commits": strings.Repeat("a", 40)}}}
		},
		"container option of a policy": func(m *Manifest) {
			m.CheckPolicies = []CheckPolicyManifest{{Execution: state.CheckExecutionSettings{ContainerWritableRoot: true}}}
		},
		"named network of a job": func(m *Manifest) {
			m.CheckJobs = []CheckJobManifest{{Execution: state.CheckExecutionSettings{ContainerNetwork: "checks"}}}
		},
		"policy time above the earlier bound":  func(m *Manifest) { m.CheckPolicies[0].MaxTimeoutMS = 24*60*60*1000 + 1 },
		"policy queue above the earlier bound": func(m *Manifest) { m.CheckPolicies[0].QueueLimit = 1001 },
		"policy memory above the earlier bound": func(m *Manifest) {
			m.CheckPolicies[0].Execution.ContainerMemoryBytes = 64<<30 + 1
		},
		"job output above the earlier bound": func(m *Manifest) {
			m.CheckJobs = []CheckJobManifest{{Limits: CheckJobLimitsManifest{OutputLimitBytes: 64<<20 + 1}}}
		},
		"job source total above the earlier bound": func(m *Manifest) {
			m.CheckJobs = []CheckJobManifest{{Execution: state.CheckExecutionSettings{Source: state.CheckSourceLimits{MaxTotalBytes: 4<<30 + 1}}}}
		},
		"deletion intent": func(m *Manifest) {
			m.ImportIntents = []ImportIntentManifest{{Expected: map[string]string{"refs/heads/gone": strings.Repeat("a", 40)}, Desired: map[string]string{"refs/heads/gone": ""}}}
		},
	} {
		manifest := blank()
		edit(&manifest)
		if format11Content(manifest) == "" {
			t.Errorf("%s does not make a backup format 11", name)
		}
	}
	for recordType, fields := range map[reflect.Type]int{
		reflect.TypeFor[Manifest](): 25, reflect.TypeFor[RepositoryManifest](): 13, reflect.TypeFor[RepositoryNameManifest](): 4, reflect.TypeFor[RepositoryPolicyManifest](): 4,
		reflect.TypeFor[Head](): 2, reflect.TypeFor[Ref](): 2, reflect.TypeFor[PullRequestManifest](): 19, reflect.TypeFor[PullRequestRevisionManifest](): 5,
		reflect.TypeFor[PullRequestReviewManifest](): 12, reflect.TypeFor[PullRequestMergeManifest](): 11, reflect.TypeFor[TaskManifest](): 5, reflect.TypeFor[CheckConfigurationManifest](): 5,
		reflect.TypeFor[CheckDefinitionManifest](): 2, reflect.TypeFor[CheckCycleManifest](): 6, reflect.TypeFor[CheckAttemptManifest](): 32, reflect.TypeFor[CheckResultManifest](): 11,
		reflect.TypeFor[CheckPolicyManifest](): 17, reflect.TypeFor[CheckJobManifest](): 44, reflect.TypeFor[CheckJobLimitsManifest](): 2, reflect.TypeFor[state.CheckExecutionSettings](): 14,
		reflect.TypeFor[ImportSourceManifest](): 11, reflect.TypeFor[ImportRunManifest](): 30, reflect.TypeFor[ImportObservationManifest](): 7, reflect.TypeFor[ImportIntentManifest](): 18,
		reflect.TypeFor[state.Actor](): 3,
	} {
		if recordType.NumField() != fields {
			t.Errorf("%s has %d fields, not %d: decide whether the new field needs format 11 (format11Content), then update this count", recordType.Name(), recordType.NumField(), fields)
		}
	}
}
