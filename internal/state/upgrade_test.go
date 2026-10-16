package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// beforeUpgrade runs after the inspection and before anything in the state
// directory changes. It gets a copy that opens upgraded while the state
// keeps its bytes, and its error leaves the state at the older schema.
func TestBeforeUpgradeSeesTheStateUnchangedAndCanStopTheUpgrade(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	createReleasedSchemaWithPullRequest(t, directory, 15)
	before := captureSchemaDirectory(t, directory)

	stop := errors.New("backup failed")
	calls := 0
	_, err := openWithBeforeUpgrade(t, directory, func(ctx context.Context, upgrade *Upgrade) error {
		calls++
		if upgrade.From != 15 || upgrade.To != 18 || upgrade.Describe() != "from schema 15 to 18" {
			t.Fatalf("upgrade=%d to %d (%s)", upgrade.From, upgrade.To, upgrade.Describe())
		}
		assertSchemaDirectoryUnchanged(t, directory, before)
		copied, err := upgrade.OpenCopy(ctx, t.TempDir())
		if err != nil {
			t.Fatalf("open copy: %v", err)
		}
		defer copied.Close()
		if version, _, err := readSchemaVersion(ctx, copied.db); err != nil || version != 18 {
			t.Fatalf("copy schema=%d err=%v", version, err)
		}
		assertPullRequestHistoryKept(t, copied)
		assertSchemaDirectoryUnchanged(t, directory, before)
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err=%v after %d calls, want the backup error after one", err, calls)
	}
	assertSchemaDirectoryUnchanged(t, directory, before)

	// Once beforeUpgrade succeeds, the state is upgraded, and a current
	// state has no upgrade.
	store, err := openWithBeforeUpgrade(t, directory, func(context.Context, *Upgrade) error { calls++; return nil })
	noErr(t, err)
	if version, _, err := readSchemaVersion(ctx, store.db); err != nil || version != 18 || calls != 2 {
		t.Fatalf("schema=%d err=%v calls=%d", version, err, calls)
	}
	noErr(t, store.Close())
	store, err = openWithBeforeUpgrade(t, directory, func(context.Context, *Upgrade) error { calls++; return nil })
	noErr(t, err)
	noErr(t, store.Close())
	if calls != 2 {
		t.Fatalf("a current state called beforeUpgrade (%d calls)", calls)
	}
}

// A state that a stopped process left with a write-ahead log is copied with
// it, so the copy holds what only the log holds.
func TestCopyIncludesTheWriteAheadLog(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	createReleasedSchemaWithPullRequest(t, source, 15)
	db := openSchemaDatabase(t, filepath.Join(source, databaseName))
	defer db.Close()
	for _, statement := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`, `INSERT INTO metadata(key,value) VALUES('only_in_log','yes')`} {
		_, err := db.Exec(statement)
		noErr(t, err)
	}
	// The files as a process that stopped without closing leaves them.
	directory := filepath.Join(t.TempDir(), "state")
	noErr(t, os.Mkdir(directory, 0o700))
	for _, name := range []string{databaseName, databaseName + walSuffix} {
		content, err := os.ReadFile(filepath.Join(source, name))
		noErr(t, err)
		noErr(t, os.WriteFile(filepath.Join(directory, name), content, 0o600))
	}
	before := captureSchemaDirectory(t, directory)
	_, err := openWithBeforeUpgrade(t, directory, func(ctx context.Context, upgrade *Upgrade) error {
		copied, err := upgrade.OpenCopy(ctx, t.TempDir())
		noErr(t, err)
		defer copied.Close()
		values, err := copied.metadataValues(ctx, "only_in_log")
		noErr(t, err)
		if values["only_in_log"] != "yes" {
			t.Fatalf("the copy lost the write-ahead log: %v", values)
		}
		return errors.New("stop")
	})
	if err == nil || err.Error() != "stop" {
		t.Fatalf("err=%v", err)
	}
	assertSchemaDirectoryUnchanged(t, directory, before)
}

// A new state and an empty database have nothing to back up.
func TestNewStateHasNoUpgrade(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	noErr(t, os.MkdirAll(directory, 0o700))
	noErr(t, os.WriteFile(filepath.Join(directory, databaseName), nil, 0o600))
	for range 2 {
		store, err := openWithBeforeUpgrade(t, directory, func(context.Context, *Upgrade) error {
			t.Fatal("beforeUpgrade ran for a new database")
			return nil
		})
		noErr(t, err)
		noErr(t, store.Close())
	}
}

// The copy is the database as inspected: one that changed since is a
// change during inspection, not a backup of other bytes.
func TestCopyOfAChangedStateIsRefused(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	createReleasedSchemaWithPullRequest(t, directory, 15)
	_, err := openWithBeforeUpgrade(t, directory, func(ctx context.Context, upgrade *Upgrade) error {
		file, err := os.OpenFile(filepath.Join(directory, databaseName), os.O_WRONLY|os.O_APPEND, 0)
		noErr(t, err)
		_, err = file.Write(make([]byte, 4096))
		noErr(t, errors.Join(err, file.Close()))
		store, err := upgrade.OpenCopy(ctx, t.TempDir())
		if err == nil {
			store.Close()
		}
		return err
	})
	if !errors.Is(err, ErrInspectionUnstable) {
		t.Fatalf("err=%v, want a change during inspection", err)
	}
}

func TestUpgradeBackupSetting(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	held, err := CreateDirectory(directory)
	noErr(t, err)
	defer held.Close()
	enabled := func() bool {
		t.Helper()
		on, err := UpgradeBackupEnabled(held)
		noErr(t, err)
		return on
	}
	if !enabled() {
		t.Fatal("the upgrade backup is off by default")
	}
	noErr(t, SetUpgradeBackup(held, false))
	noErr(t, SetUpgradeBackup(held, false))
	if enabled() {
		t.Fatal("the upgrade backup stayed on")
	}
	noErr(t, SetUpgradeBackup(held, true))
	noErr(t, SetUpgradeBackup(held, true))
	if !enabled() {
		t.Fatal("the upgrade backup stayed off")
	}
	// Something at the name that is not this account's own file is an
	// error, not a setting.
	noErr(t, os.Mkdir(filepath.Join(directory, upgradeBackupOffName), 0o700))
	if _, err := UpgradeBackupEnabled(held); err == nil {
		t.Fatal("a folder at the setting's name was read as a setting")
	}
	if got, want := UpgradeBackupFolder(filepath.Join("base", "owngit")), filepath.Join("base", "owngit-backups"); got != want {
		t.Fatalf("folder=%q, want %q", got, want)
	}
}

func openWithBeforeUpgrade(t *testing.T, directory string, beforeUpgrade BeforeUpgrade) (*Store, error) {
	t.Helper()
	held, err := CreateDirectory(directory)
	noErr(t, err)
	defer held.Close()
	return OpenIn(context.Background(), held, beforeUpgrade)
}
