package state

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const helperActorJSON = `{"kind":"helper_credential","id":"0123456789abcdef0123456789abcdef","label":"agent"}`

// portableRecordsStore holds one of every schema 16 record: portable ones
// with values other than their defaults, and machine-local ones that a
// backup must leave behind.
func portableRecordsStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	noErr(t, store.CompleteSetup(ctx, t.TempDir(), "open", "", "synthetic-admin-hash", true))
	now := time.Unix(1_800_000_000, 0)
	for _, repository := range []Repository{{ID: "project", Name: "project", CreatedAt: now}, {ID: "other", Name: "other", CreatedAt: now}} {
		noErr(t, store.AddRepository(ctx, repository))
	}
	record, err := store.CreatePullRequest(ctx, "project", "Title", "feature", "main", strings.Repeat("1", 40), strings.Repeat("2", 40), ReviewNotRequested, now)
	noErr(t, err)
	_, err = store.ConfigureImportSource(ctx, ImportSourceInput{RepositoryID: "other", URL: "https://example.invalid/source.git", Mode: ImportModeStandalone, Now: now})
	noErr(t, err)
	for _, statement := range []string{
		`UPDATE pull_requests SET body='Line one` + "\n" + `line two',edit_revision=2,edited_at=1800000100,created_by='{"kind":"access"}',edited_by='` + helperActorJSON + `' WHERE number=1`,
		`UPDATE pull_request_reviews SET note='Looks right',actor='{"kind":"administrator"}' WHERE pull_request_number=1`,
		`INSERT INTO repository_names(name,repository_id,kind,created_at,alias_until) VALUES('renamed','project','current',1800000200,NULL),('project','project','alias',1800000200,1807776200)`,
		`INSERT INTO repository_policies(repository_id,retain_history,protect_default_branch,extra_ref_prefixes,updated_at) VALUES('project',0,1,'["refs/notes/"]',1800000300)`,
		`UPDATE import_sources SET overwrite_diverged=1,follow_upstream_deletions=1,extra_ref_prefixes='["refs/changes/"]',
			allow_private_network=1,allow_plain_http=1,redirect_policy='approved',approved_redirect_origin='https://mirror.example.invalid',allow_reserved_addresses=1,limits_json='{"pack_bytes":1}'`,
		`INSERT INTO share_links(id,repository_id,secret_hash,scope,created_at) VALUES('00000000000000000000000000000001','project',zeroblob(32),'clone',1800000400)`,
		`INSERT INTO backup_plans(name,enabled,interval_seconds,destination,keep,verify,created_at,updated_at) VALUES('default',1,43200,'/home/example/backups',7,1,1800000500,1800000500)`,
		`INSERT INTO backup_runs(id,kind,status,destination,backup_name,verification,started_at,finished_at,longest_hold_ms,longest_hold_repository) VALUES('00000000000000000000000000000002','scheduled','succeeded','/home/example/backups','owngit-1','passed',1800000600,1800000700,12,'project')`,
		`INSERT INTO push_events(repository_id,ref_name,old_oid,new_oid,refs_updated,actor,pushed_at) VALUES('project','refs/heads/main','','` + strings.Repeat("3", 40) + `',1,'{"kind":"access"}',1800000800)`,
		`INSERT INTO metadata(key,value) VALUES('admin_confirmation','never'),('retain_history','off')`,
	} {
		noErr(t, store.Exec(ctx, statement))
	}
	if record.Number != 1 {
		t.Fatalf("pull request number %d", record.Number)
	}
	return store
}

// Every portable schema 16 field survives a snapshot and restore, and every
// machine-local one starts at its default.
func TestSchema16PortableRecordsRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := portableRecordsStore(t)
	snapshot, err := source.RecoverySnapshot(ctx)
	noErr(t, err)
	edited := time.Unix(1_800_000_100, 0)
	pull := snapshot.PullRequests[0]
	if pull.Body != "Line one\nline two" || pull.EditRevision != 2 || pull.EditedAt == nil || !pull.EditedAt.Equal(edited) ||
		pull.CreatedBy != (Actor{Kind: ActorAccess}) || pull.EditedBy != (Actor{Kind: ActorHelperCredential, ID: "0123456789abcdef0123456789abcdef", Label: "agent"}) {
		t.Fatalf("snapshot pull request=%+v", pull)
	}
	if review := snapshot.PullRequestReviews[0]; review.Note != "Looks right" || review.Actor != (Actor{Kind: ActorAdministrator}) {
		t.Fatalf("snapshot review=%+v", review)
	}
	if len(snapshot.RepositoryNames) != 2 || len(snapshot.RepositoryPolicies) != 1 || snapshot.RepositoryPolicies[0].RetainHistory == nil || *snapshot.RepositoryPolicies[0].RetainHistory {
		t.Fatalf("snapshot names=%+v policies=%+v", snapshot.RepositoryNames, snapshot.RepositoryPolicies)
	}
	if importSource := snapshot.ImportSources[0]; !importSource.OverwriteDiverged || !importSource.FollowUpstreamDeletions || !reflect.DeepEqual(importSource.ExtraRefPrefixes, []string{"refs/changes/"}) || importSource.AllowPrivateNetwork {
		t.Fatalf("snapshot import source=%+v", importSource)
	}

	destination, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	noErr(t, err)
	defer destination.Close()
	noErr(t, destination.RestoreRecoveryState(ctx, t.TempDir(), snapshot))
	restored, err := destination.RecoverySnapshot(ctx)
	noErr(t, err)
	for name, pair := range map[string][2]any{
		"pull requests": {snapshot.PullRequests, restored.PullRequests},
		"reviews":       {snapshot.PullRequestReviews, restored.PullRequestReviews},
		"names":         {snapshot.RepositoryNames, restored.RepositoryNames},
		"policies":      {snapshot.RepositoryPolicies, restored.RepositoryPolicies},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("%s changed:\n%+v\n%+v", name, pair[0], pair[1])
		}
	}
	if got, want := restored.ImportSources[0], snapshot.ImportSources[0]; got.OverwriteDiverged != want.OverwriteDiverged || got.FollowUpstreamDeletions != want.FollowUpstreamDeletions || !reflect.DeepEqual(got.ExtraRefPrefixes, want.ExtraRefPrefixes) {
		t.Fatalf("import source changed: %+v", got)
	}

	var private, plain, reserved int
	var redirect, origin, limits string
	noErr(t, destination.db.QueryRowContext(ctx, `SELECT allow_private_network,allow_plain_http,redirect_policy,approved_redirect_origin,allow_reserved_addresses,limits_json FROM import_sources`).
		Scan(&private, &plain, &redirect, &origin, &reserved, &limits))
	if private != 0 || plain != 0 || redirect != "refuse" || origin != "" || reserved != 0 || limits != "{}" {
		t.Fatalf("machine-local import choices came back: %d %d %q %q %d %q", private, plain, redirect, origin, reserved, limits)
	}
	for _, table := range []string{"share_links", "backup_plans", "backup_runs", "push_events", "sessions", "helper_credentials"} {
		if count, err := destination.TableRowCount(ctx, table); err != nil || count != 0 {
			t.Fatalf("%s has %d rows after restore, err=%v", table, count, err)
		}
	}
	values, err := destination.metadataValues(ctx, "admin_confirmation", "retain_history")
	noErr(t, err)
	if len(values) != 0 {
		t.Fatalf("machine-local settings came back: %v", values)
	}
}

// A snapshot with an inconsistent schema 16 record is refused, never
// restored in part.
func TestSchema16RecordValidation(t *testing.T) {
	ctx := context.Background()
	valid, err := portableRecordsStore(t).RecoverySnapshot(ctx)
	noErr(t, err)
	until := time.Unix(1_900_000_000, 0)
	for name, change := range map[string]func(*RecoveryState){
		"description too long": func(s *RecoveryState) { s.PullRequests[0].Body = strings.Repeat("x", MaximumPullRequestTextBytes+1) },
		"edit without time":    func(s *RecoveryState) { s.PullRequests[0].EditedAt = nil },
		"editor without edit": func(s *RecoveryState) {
			s.PullRequests[0].EditRevision, s.PullRequests[0].EditedAt = 0, nil
		},
		"merger of an open pull request": func(s *RecoveryState) { s.PullRequests[0].MergedBy = Actor{Kind: ActorAccess} },
		"unknown actor kind":             func(s *RecoveryState) { s.PullRequests[0].CreatedBy = Actor{Kind: "account"} },
		"helper actor without label": func(s *RecoveryState) {
			s.PullRequestReviews[0].Actor = Actor{Kind: ActorHelperCredential, ID: strings.Repeat("a", 32)}
		},
		"note with NUL": func(s *RecoveryState) { s.PullRequestReviews[0].Note = "a\x00b" },
		"name of another repository": func(s *RecoveryState) {
			s.RepositoryNames[0].Name = "other"
		},
		"current name equal to its own ID": func(s *RecoveryState) {
			s.RepositoryNames = []RepositoryName{{Name: "project", RepositoryID: "project", Kind: RepositoryNameCurrent, CreatedAt: until}}
		},
		"two current names": func(s *RecoveryState) {
			s.RepositoryNames = append(s.RepositoryNames, RepositoryName{Name: "third", RepositoryID: "project", Kind: RepositoryNameCurrent, CreatedAt: until})
		},
		"alias without an end": func(s *RecoveryState) {
			s.RepositoryNames = append(s.RepositoryNames, RepositoryName{Name: "old", RepositoryID: "project", Kind: RepositoryNameAlias, CreatedAt: until})
		},
		"name recorded twice": func(s *RecoveryState) {
			s.RepositoryNames = append(s.RepositoryNames, RepositoryName{Name: "project", RepositoryID: "project", Kind: RepositoryNameAlias, CreatedAt: until, AliasUntil: &until})
		},
		"policy of an unknown repository": func(s *RecoveryState) { s.RepositoryPolicies[0].RepositoryID = "missing" },
		"branch namespace as extra refs":  func(s *RecoveryState) { s.RepositoryPolicies[0].ExtraRefPrefixes = []string{"refs/heads/"} },
		"extra refs without a slash":      func(s *RecoveryState) { s.ImportSources[0].ExtraRefPrefixes = []string{"refs/notes"} },
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := cloneRecoveryForTest(t, valid)
			change(&snapshot)
			destination, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
			noErr(t, err)
			defer destination.Close()
			if err := destination.RestoreRecoveryState(ctx, t.TempDir(), snapshot); err == nil {
				t.Fatal("restore accepted an invalid record")
			}
			if count, err := destination.TableRowCount(ctx, "repositories"); err != nil || count != 0 {
				t.Fatalf("a refused restore wrote %d repositories, err=%v", count, err)
			}
		})
	}
}

func cloneRecoveryForTest(t *testing.T, snapshot RecoveryState) RecoveryState {
	t.Helper()
	clone := snapshot
	clone.PullRequests = append([]PullRequest(nil), snapshot.PullRequests...)
	clone.PullRequestReviews = append([]PullRequestReview(nil), snapshot.PullRequestReviews...)
	clone.RepositoryNames = append([]RepositoryName(nil), snapshot.RepositoryNames...)
	clone.RepositoryPolicies = append([]RepositoryPolicy(nil), snapshot.RepositoryPolicies...)
	clone.ImportSources = append([]ImportSource(nil), snapshot.ImportSources...)
	return clone
}

// The schema itself refuses rows that break the frozen bounds, whatever
// code writes them.
func TestSchema16Constraints(t *testing.T) {
	ctx := context.Background()
	store := portableRecordsStore(t)
	for name, statement := range map[string]string{
		"description over 64 KiB":  `UPDATE pull_requests SET body=zeroblob(65537)`,
		"actor that is not JSON":   `UPDATE pull_requests SET merged_by='someone'`,
		"current name with an end": `INSERT INTO repository_names(name,repository_id,kind,created_at,alias_until) VALUES('x','other','current',1,2)`,
		"second current name":      `INSERT INTO repository_names(name,repository_id,kind,created_at) VALUES('again','project','current',1)`,
		"short share secret":       `INSERT INTO share_links(id,repository_id,secret_hash,scope,created_at) VALUES('00000000000000000000000000000009','project',zeroblob(16),'browse',1)`,
		"second running backup": `INSERT INTO backup_runs(id,kind,status,destination,started_at) VALUES('00000000000000000000000000000003','manual','running','/home/example/b',1);
			INSERT INTO backup_runs(id,kind,status,destination,started_at) VALUES('00000000000000000000000000000004','scheduled','running','/home/example/b',2)`,
		"backup every ten minutes":     `UPDATE backup_plans SET interval_seconds=600`,
		"negative backup hold":         `UPDATE backup_runs SET longest_hold_ms=-1`,
		"short manifest digest":        `UPDATE backup_runs SET manifest_sha256='abc'`,
		"extra refs that are not JSON": `UPDATE repository_policies SET extra_ref_prefixes='refs/notes/'`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Exec(ctx, "SAVEPOINT constraint_case; "+statement); err == nil {
				t.Fatal("the schema accepted the row")
			}
			noErr(t, store.Exec(ctx, "ROLLBACK TO constraint_case; RELEASE constraint_case"))
		})
	}
}
