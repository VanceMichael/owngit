package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"owngit/internal/apiclient"
	"owngit/internal/auth"
	"owngit/internal/gitexec"
	"owngit/internal/repository"
	"owngit/internal/service"
	"owngit/internal/state"
)

// The backup commands of a running server print its JSON, and the MCP
// backup_status tool, with general access, returns the summary without the
// folder.
func TestBackupCommandsTalkToTheServer(t *testing.T) {
	serverURL, _ := startRepositoryCLIServer(t, "")
	adminFile := filepath.Join(t.TempDir(), "admin-password")
	noErr(t, os.WriteFile(adminFile, []byte("admin-password\n"), 0o600))
	noErr(t, state.ProtectPrivatePath(adminFile, false))
	remote := []string{"--server", serverURL, "--accept-insecure-http", "--password-file", adminFile}
	command := func(arguments ...string) []string { return append(arguments, remote...) }

	status := cliOutput(t, backupState, command("status")...)
	if !strings.Contains(status, `"state":"not_configured"`) || !strings.Contains(status, `"last_run":null`) {
		t.Fatalf("status: %s", status)
	}
	session := startMCPSession(t, mcpOptions{server: serverURL, acceptInsecureHTTP: true})
	if text, isError := session.call("backup_status", nil); isError || text != `{"ok":true,"schedule":"not_configured","last_run":null,"last_verified_at":null,"next_run":null}` {
		t.Fatalf("backup_status isError=%v: %s", isError, text)
	}
	for _, refused := range []struct {
		arguments []string
		code      string
	}{
		{command("now"), "backup_not_configured"},
		{command("schedule", "set", "--keep", "3"), "invalid_backup_schedule"},
		{command("schedule", "set", "--destination", "relative"), "invalid_backup_schedule"},
	} {
		_, err := captureStdout(func() error { return backupState(refused.arguments) })
		var problem *apiclient.Error
		if !errors.As(err, &problem) || problem.Code != refused.code {
			t.Errorf("%v: %v", refused.arguments, err)
		}
	}
	if _, err := captureStdout(func() error { return backupState(command("schedule", "set", "--keep", "many")) }); err == nil {
		t.Error("--keep many was accepted")
	}

	destination := filepath.Join(t.TempDir(), "backups")
	var saved struct {
		Schedule struct {
			State       string `json:"state"`
			Destination string `json:"destination"`
			Interval    string `json:"interval"`
			Keep        int    `json:"keep"`
			Verify      bool   `json:"verify"`
		} `json:"schedule"`
		Warnings []string `json:"warnings"`
	}
	output := cliOutput(t, backupState, command("schedule", "set", "--destination", destination, "--interval", "12h", "--keep", "2", "--verify", "off")...)
	noErr(t, json.Unmarshal([]byte(output), &saved))
	if saved.Schedule.State != "on" || saved.Schedule.Destination != destination || saved.Schedule.Interval != "12h" || saved.Schedule.Keep != 2 ||
		saved.Schedule.Verify || len(saved.Warnings) != 1 {
		t.Fatalf("schedule set: %s", output)
	}
	if text, isError := session.call("backup_status", nil); isError || strings.Contains(text, "backups") || !strings.Contains(text, `"schedule":"on"`) {
		t.Fatalf("backup_status after set isError=%v: %s", isError, text)
	}
	output = cliOutput(t, backupState, command("schedule", "off")...)
	noErr(t, json.Unmarshal([]byte(output), &saved))
	if saved.Schedule.State != "off" || saved.Schedule.Keep != 2 || len(saved.Warnings) != 1 {
		t.Fatalf("schedule off: %s", output)
	}
	if shown := cliOutput(t, backupState, command("schedule", "show")...); !strings.Contains(shown, `"state":"off"`) {
		t.Fatalf("schedule show: %s", shown)
	}
	if runs := cliOutput(t, backupState, command("runs")...); !strings.HasPrefix(runs, `{"ok":true,"runs":[`) {
		t.Fatalf("runs: %s", runs)
	}
}

// The backup plan commands manage named plans, and the existing commands
// address one plan with --plan; the calls without --plan keep using the
// default plan.
func TestBackupPlanCommands(t *testing.T) {
	serverURL, _ := startRepositoryCLIServer(t, "")
	adminFile := filepath.Join(t.TempDir(), "admin-password")
	noErr(t, os.WriteFile(adminFile, []byte("admin-password\n"), 0o600))
	noErr(t, state.ProtectPrivatePath(adminFile, false))
	remote := []string{"--server", serverURL, "--accept-insecure-http", "--password-file", adminFile}
	command := func(arguments ...string) []string { return append(arguments, remote...) }

	if output := cliOutput(t, backupState, command("plan", "list")...); !strings.Contains(output, `"plans":[]`) {
		t.Fatalf("plan list before creating: %s", output)
	}
	// Local argument validation never reaches the server.
	if _, err := captureStdout(func() error { return backupState(command("plan", "create", "--destination", t.TempDir())) }); err == nil {
		t.Fatal("plan create without --name was accepted")
	}
	if _, err := captureStdout(func() error { return backupState(command("plan", "create", "--name", "off site", "--destination", t.TempDir())) }); err == nil {
		t.Fatal("plan create with a bad name was accepted")
	}
	destination := filepath.Join(t.TempDir(), "offline-backups")
	output := cliOutput(t, backupState, command("plan", "create", "--name", "offsite", "--destination", destination, "--keep", "3", "--verify", "off", "--scheduled", "off")...)
	if !strings.Contains(output, `"plan":"offsite"`) || !strings.Contains(output, `"state":"off"`) || !strings.Contains(output, `"keep":3`) {
		t.Fatalf("plan create: %s", output)
	}
	_, err := captureStdout(func() error { return backupState(command("plan", "create", "--name", "offsite", "--destination", destination)) })
	var problem *apiclient.Error
	if !errors.As(err, &problem) || problem.Code != "backup_plan_exists" {
		t.Fatalf("a duplicate plan: %v", err)
	}
	// Change and read the plan through the schedule commands with --plan.
	// "set" turns scheduling on, so turn it straight back off and wait for
	// any run it could start before deleting the plan.
	if output := cliOutput(t, backupState, command("schedule", "set", "--plan", "offsite", "--keep", "4")...); !strings.Contains(output, `"keep":4`) {
		t.Fatalf("schedule set --plan: %s", output)
	}
	cliOutput(t, backupState, command("schedule", "off", "--plan", "offsite")...)
	waitForPlanRunsIdle(t, backupState, command)
	if output := cliOutput(t, backupState, command("schedule", "show", "--plan", "offsite")...); !strings.Contains(output, `"plan":"offsite"`) || !strings.Contains(output, `"state":"off"`) || !strings.Contains(output, `"keep":4`) {
		t.Fatalf("schedule show --plan: %s", output)
	}
	if output := cliOutput(t, backupState, command("status", "--plan", "offsite")...); !strings.Contains(output, `"plan":"offsite"`) {
		t.Fatalf("status --plan: %s", output)
	}
	if output := cliOutput(t, backupState, command("runs", "--plan", "offsite")...); !strings.HasPrefix(output, `{"ok":true,"runs":[`) {
		t.Fatalf("runs --plan: %s", output)
	}
	// Starting a plan that does not exist names it; a bad query is refused.
	_, err = captureStdout(func() error { return backupState(command("now", "--plan", "missing")) })
	if !errors.As(err, &problem) || problem.Code != "backup_plan_not_found" {
		t.Fatalf("backing up a missing plan: %v", err)
	}
	// The default plan is still absent, so a call without --plan keeps the
	// old answer.
	_, err = captureStdout(func() error { return backupState(command("now")) })
	if !errors.As(err, &problem) || problem.Code != "backup_not_configured" {
		t.Fatalf("back up now without a plan: %v", err)
	}
	// The idle plan deletes; deleting it again says it is missing.
	if output := cliOutput(t, backupState, command("plan", "delete", "--name", "offsite")...); !strings.Contains(output, `"plan":"offsite"`) {
		t.Fatalf("plan delete: %s", output)
	}
	_, err = captureStdout(func() error { return backupState(command("plan", "delete", "--name", "offsite")) })
	if !errors.As(err, &problem) || problem.Code != "backup_plan_not_found" {
		t.Fatalf("deleting the plan again: %v", err)
	}
}

// waitForPlanRunsIdle polls the offsite plan's runs through the CLI until
// no run of it is running.
func waitForPlanRunsIdle(t *testing.T, state func([]string) error, command func(...string) []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		output, err := captureStdout(func() error { return state(command("runs", "--plan", "offsite")) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output, `"status":"running"`) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the offsite run did not finish")
}

// A serving OwnGit records a backup that a stopped process left running as
// interrupted, does not repeat its scheduled slot, and makes a verified
// backup when asked.
func TestServeRunsBackups(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	repositoriesRoot := filepath.Join(root, "repositories")
	noErr(t, os.Mkdir(repositoriesRoot, 0o700))
	store, err := state.Open(ctx, stateDir)
	noErr(t, err)
	adminHash, err := auth.HashPassword("admin-password")
	noErr(t, err)
	noErr(t, store.CompleteSetup(ctx, repositoriesRoot, "open", "", adminHash, true))
	runner, err := gitexec.New("", filepath.Join(stateDir, "runtime"))
	noErr(t, err)
	manager := &repository.Manager{Store: store, Git: runner, Locks: gitexec.NewLocks(), Root: repositoriesRoot}
	_, err = manager.Create(ctx, "project", "")
	noErr(t, err)
	destination := filepath.Join(root, "backups")
	noErr(t, store.SaveBackupSchedule(ctx, state.BackupSchedule{Enabled: true, Interval: 24 * time.Hour, Destination: destination, Keep: 7, Verify: true, UpdatedAt: time.Now()}))
	left := state.BackupRun{ID: strings.Repeat("d", 32), Kind: state.BackupRunScheduled, Destination: destination, BackupName: "owngit-backup-left", StartedAt: time.Now().Add(-time.Hour)}
	noErr(t, store.StartBackupRun(ctx, left))
	noErr(t, store.Close())

	instance := startServed(t, stateDir)
	instance.waitForLog("recorded as interrupted", time.Minute)
	adminFile := filepath.Join(root, "admin-password")
	noErr(t, os.WriteFile(adminFile, []byte("admin-password\n"), 0o600))
	noErr(t, state.ProtectPrivatePath(adminFile, false))
	remote := []string{"--server", instance.url, "--accept-insecure-http", "--password-file", adminFile}
	var started struct {
		Run struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"run"`
	}
	noErr(t, json.Unmarshal([]byte(cliOutput(t, backupState, append([]string{"now"}, remote...)...)), &started))
	instance.waitForLog("backup "+started.Run.Path+" ", 5*time.Minute)

	var status struct {
		LastRun struct {
			ID, Status, Verification string
			LongestHoldMS            *int64 `json:"longest_hold_ms"`
		} `json:"last_run"`
		LastVerified *struct{ ID string } `json:"last_verified"`
		NextRun      time.Time            `json:"next_run"`
	}
	output := cliOutput(t, backupState, append([]string{"status"}, remote...)...)
	noErr(t, json.Unmarshal([]byte(output), &status))
	if status.LastRun.ID != started.Run.ID || status.LastRun.Status != "succeeded" || status.LastRun.Verification != "passed" ||
		status.LastRun.LongestHoldMS == nil || status.LastVerified == nil || status.LastVerified.ID != started.Run.ID ||
		!status.NextRun.Equal(left.StartedAt.Truncate(time.Second).Add(24*time.Hour)) {
		t.Fatalf("status: %s", output)
	}
	var runs struct {
		Runs []struct{ ID, Status, Message string } `json:"runs"`
	}
	noErr(t, json.Unmarshal([]byte(cliOutput(t, backupState, append([]string{"runs"}, remote...)...)), &runs))
	if len(runs.Runs) != 2 || runs.Runs[1].ID != left.ID || runs.Runs[1].Status != "interrupted" {
		t.Fatalf("runs: %+v", runs)
	}

	// The backup is verified again on request.
	if output := cliOutput(t, backupState, append([]string{"check", "--run", started.Run.ID}, remote...)...); !strings.Contains(output, `"status":"running"`) {
		t.Fatalf("check: %s", output)
	}
	instance.waitForLog("verification of backup "+started.Run.Path+" passed", 5*time.Minute)

	// A downloaded backup, unpacked with tar, passes owngit backup verify
	// on this computer, and uploaded again it is verified and comes with
	// the command that restores it.
	archive := filepath.Join(root, "backup.tar")
	var downloaded struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}
	noErr(t, json.Unmarshal([]byte(cliOutput(t, backupState, append([]string{"download", "--run", started.Run.ID, "--output", archive}, remote...)...)), &downloaded))
	if downloaded.Path != archive || downloaded.Bytes == 0 {
		t.Fatalf("download: %+v", downloaded)
	}
	if _, err := captureStdout(func() error {
		return backupState(append([]string{"download", "--run", started.Run.ID, "--output", archive}, remote...))
	}); err == nil {
		t.Fatal("a download replaced a file that exists")
	}
	unpacked := filepath.Join(root, "unpacked")
	noErr(t, os.Mkdir(unpacked, 0o700))
	unpackTar(t, archive, unpacked)
	if output := cliOutput(t, backupState, "verify", filepath.Join(unpacked, filepath.Base(started.Run.Path))); !strings.Contains(output, "passed") {
		t.Fatalf("backup verify of the unpacked download: %s", output)
	}
	if output := cliOutput(t, backupState, append([]string{"upload", "--input", archive}, remote...)...); !strings.Contains(output, `"status":"verifying"`) {
		t.Fatalf("upload: %s", output)
	}
	instance.waitForLog("the uploaded backup "+filepath.Base(started.Run.Path)+" passed verification", 5*time.Minute)
	var uploaded struct {
		Upload struct {
			Status, Path string
		} `json:"upload"`
		Command string `json:"upload_restore_command"`
	}
	output = cliOutput(t, backupState, append([]string{"status"}, remote...)...)
	noErr(t, json.Unmarshal([]byte(output), &uploaded))
	moved := filepath.Join(stateDir+".before-restore", "backup-uploads", filepath.Base(started.Run.Path))
	if uploaded.Upload.Status != "passed" || !strings.Contains(uploaded.Command, programWord()+" restore --input "+commandWord(moved)+" --state-dir "+commandWord(stateDir)) ||
		!strings.HasSuffix(uploaded.Command, " --verify") {
		t.Fatalf("status after the upload: %s", output)
	}
}

// The restore steps name the folders that move aside, a restore command
// that reads an uploaded backup where it is after that move, and the
// service commands only for a service.
func TestRestoreGuideMovesTheCurrentFoldersAside(t *testing.T) {
	stateDir, repositories := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "repositories")
	elsewhere := filepath.Join(t.TempDir(), "backups", "owngit-backup-1")
	noErr(t, os.Mkdir(stateDir, 0o700))
	noErr(t, os.MkdirAll(elsewhere, 0o700))
	guide := restoreGuide(stateDir, false)(elsewhere, repositories)
	if guide.Stop != "" || guide.Start != "" || guide.MovedState != stateDir+".before-restore" || guide.MovedRepositories != repositories+".before-restore" ||
		guide.Command != programWord()+" restore --input "+commandWord(elsewhere)+" --state-dir "+commandWord(stateDir)+" --repository-root "+commandWord(repositories)+" --verify" {
		t.Fatalf("guide: %+v", guide)
	}
	upload := filepath.Join(stateDir, "backup-uploads", "b")
	noErr(t, os.MkdirAll(upload, 0o700))
	uploaded := restoreGuide(stateDir, true)(upload, repositories)
	if uploaded.Stop != programWord()+" service stop" || uploaded.Start != programWord()+" service start" ||
		!strings.Contains(uploaded.Command, "--input "+commandWord(filepath.Join(stateDir+".before-restore", "backup-uploads", "b"))) {
		t.Fatalf("uploaded guide: %+v", uploaded)
	}

	// The test binary is not "owngit" on PATH, so the printed commands name its
	// absolute path.
	if self, err := os.Executable(); err != nil || !strings.Contains(guide.Command, commandWord(self)+" restore ") || !strings.HasSuffix(uploaded.Stop, commandWord(self)+" service stop") ||
		!strings.HasPrefix(strings.TrimPrefix(guide.Network, "& "), commandWord(self)+" network set --state-dir "+commandWord(stateDir)+" --base-url ") {
		t.Fatalf("printed commands do not start with the executable path %q: %s / %s", self, guide.Command, uploaded.Stop)
	}

	// The state folder named through a link, and the upload by its real
	// path, as the state store names it: the upload still moves with the
	// state folder.
	if runtime.GOOS == "windows" {
		return
	}
	link := filepath.Join(t.TempDir(), "state-link")
	noErr(t, os.Symlink(stateDir, link))
	for _, folders := range [][2]string{{link, upload}, {stateDir, filepath.Join(link, "backup-uploads", "b")}} {
		guide := restoreGuide(folders[0], false)(folders[1], repositories)
		if want := "--input " + commandWord(filepath.Join(folders[0]+".before-restore", "backup-uploads", "b")); !strings.Contains(guide.Command, want) {
			t.Fatalf("guide for %s with the upload %s: %s, want %s", folders[0], folders[1], guide.Command, want)
		}
	}
}

// When a folder on the way to an uploaded backup cannot be inspected, the
// restore steps give no command, since the backup may move with the state
// folder, and say which backup could not be checked.
func TestRestoreGuideGivesNoCommandWhereItCannotCheck(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("folder permissions that keep this account out need a regular account on macOS or Linux")
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	noErr(t, os.MkdirAll(filepath.Join(stateDir, "backup-uploads", "b"), 0o700))
	// The upload is named through a link in a folder this account cannot
	// search.
	closed := filepath.Join(root, "closed")
	noErr(t, os.Mkdir(closed, 0o700))
	noErr(t, os.Symlink(stateDir, filepath.Join(closed, "state")))
	upload := filepath.Join(closed, "state", "backup-uploads", "b")
	noErr(t, os.Chmod(closed, 0))
	defer os.Chmod(closed, 0o700)

	guide := restoreGuide(stateDir, false)(upload, filepath.Join(root, "repositories"))
	if guide.Command != "" || guide.Unchecked != upload || !strings.Contains(guide.Problem, "permission denied") {
		t.Fatalf("guide for an upload that cannot be checked: %+v", guide)
	}
}

// Every path in a restore command stays one literal argument: nothing in
// it is expanded by PowerShell on Windows or by a POSIX shell elsewhere.
func TestRestoreCommandWordsAreLiteral(t *testing.T) {
	path := `D:\Data\o'wner\$(Invoke-Evil) ` + "`" + `%TEMP% & x`
	if got := shellWord("windows", path); got != `'D:\Data\o''wner\$(Invoke-Evil) `+"`"+`%TEMP% & x'` {
		t.Fatalf("PowerShell word: %s", got)
	}
	// PowerShell also ends a literal string at the single quotation marks.
	for _, mark := range []string{"\u2018", "\u2019", "\u201a", "\u201b"} {
		path := `D:\Data\a` + mark + `; Invoke-Evil; ` + mark + `b`
		want := `'D:\Data\a` + mark + mark + `; Invoke-Evil; ` + mark + mark + `b'`
		if got := shellWord("windows", path); got != want {
			t.Fatalf("PowerShell word with %U: %s", []rune(mark)[0], got)
		}
	}
	if got := shellWord("linux", "/srv/o'wner/$(evil) `x`"); got != `'/srv/o'\''wner/$(evil) `+"`x`'" {
		t.Fatalf("POSIX word: %s", got)
	}
	if commandShell("windows") != "PowerShell" || commandShell("darwin") != "" {
		t.Fatal("the shell is not named for Windows only")
	}
}

// unpackTar unpacks the tar archive of folders and regular files into dir,
// as any tar program would, without depending on the one this computer has.
func unpackTar(t *testing.T, archive, dir string) {
	t.Helper()
	file, err := os.Open(archive)
	noErr(t, err)
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		noErr(t, err)
		target := filepath.Join(dir, filepath.FromSlash(header.Name))
		if !filepath.IsLocal(filepath.FromSlash(header.Name)) {
			t.Fatalf("archive entry outside the folder: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			noErr(t, os.MkdirAll(target, 0o700))
		case tar.TypeReg:
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			noErr(t, err)
			_, err = io.Copy(out, reader)
			noErr(t, errors.Join(err, out.Close()))
		default:
			t.Fatalf("archive entry %s of type %c", header.Name, header.Typeflag)
		}
	}
}

// A Linux system service runs as the owngit account, and the restore runs as
// it too. The account switch is runuser, since a Proxmox container has no
// sudo. It needs the account, so it runs only as that account (the lab runs
// the compiled test as owngit).
func TestRestoreGuideSwitchesAccountWithRunuser(t *testing.T) {
	if account, err := user.Current(); runtime.GOOS != "linux" || err != nil || account.Username != service.AccountName {
		t.Skip("runs only as the owngit account on Linux")
	}
	guide := restoreGuide(t.TempDir(), true)(t.TempDir(), t.TempDir())
	if !strings.HasPrefix(guide.Command, "runuser -u ") || strings.Contains(guide.Command, "sudo") {
		t.Fatalf("restore command: %s", guide.Command)
	}
}
