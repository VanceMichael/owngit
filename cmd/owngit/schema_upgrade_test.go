package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"owngit/internal/auth"
	"owngit/internal/pullrequest"
	"owngit/internal/testfixture"
)

const baselineUpgradeLine = "state database upgraded from the committed baseline (no schema version) to schema 18"

// createBaselineStateForTest writes a committed baseline state whose
// repository exists with the pull request revision it records, so the
// backup before the upgrade can be made. It returns the state directory.
func createBaselineStateForTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	repositories := filepath.Join(root, "repositories")
	if err := os.Mkdir(repositories, 0o700); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(repositories, testfixture.BaselineRepositoryID+".git")
	runPRGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	work := filepath.Join(root, "work")
	runPRGit(t, root, "init", "--initial-branch=main", work)
	runPRGit(t, work, "config", "user.name", "Baseline Test")
	runPRGit(t, work, "config", "user.email", "baseline@example.invalid")
	runPRGit(t, work, "commit", "--allow-empty", "-m", "target")
	target := prGitOutput(t, work, "rev-parse", "HEAD")
	runPRGit(t, work, "commit", "--allow-empty", "-m", "source")
	source := prGitOutput(t, work, "rev-parse", "HEAD")
	runPRGit(t, work, "push", remote, target+":refs/heads/main", source+":refs/heads/feature")
	sourceRef, targetRef := pullrequest.RevisionRefNames(testfixture.BaselinePullRequestNumber, source, target)
	runPRGit(t, remote, "update-ref", sourceRef, source)
	runPRGit(t, remote, "update-ref", targetRef, target)
	adminHash, err := auth.HashPassword("synthetic-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := testfixture.CreateCommittedBaselineState(context.Background(), stateDir, testfixture.BaselineStateOptions{
		RepositoryRoot: repositories, AdminPasswordHash: adminHash, SourceOID: source, TargetOID: target,
	}); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

// The upgrade is reported once, after the backup; a new database reports
// nothing.
func TestOpenStateReportsTheSchemaUpgradeOnce(t *testing.T) {
	stateDir := createBaselineStateForTest(t)
	var lines []string
	for range 2 {
		reported, err := openStateForTest(t, stateDir)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, reported...)
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "backed up the state to ") || !strings.HasPrefix(lines[1], "to go back to the earlier OwnGit") || lines[2] != baselineUpgradeLine {
		t.Fatalf("reported lines %q, want the backup, how to go back and %q, once", lines, baselineUpgradeLine)
	}
	if fresh, err := openStateForTest(t, filepath.Join(t.TempDir(), "state")); err != nil || len(fresh) != 0 {
		t.Fatalf("a new database reported %q err=%v", fresh, err)
	}
}

// An offline command writes the backup and the upgrade to standard error.
func TestOfflineCommandWritesTheSchemaUpgradeToStderr(t *testing.T) {
	stateDir := createBaselineStateForTest(t)
	run := func() string {
		t.Helper()
		var stderr string
		_, err := captureStdout(func() error {
			var runErr error
			stderr, runErr = captureStderr(func() error {
				return runCommand("backup", []string{"--state-dir", stateDir, "--output", filepath.Join(t.TempDir(), "backup")})
			})
			return runErr
		})
		if err != nil {
			t.Fatal(err)
		}
		return stderr
	}
	if got := run(); !strings.HasPrefix(got, "backed up the state to ") || !strings.HasSuffix(got, "\n"+baselineUpgradeLine+"\n") {
		t.Fatalf("first run wrote %q to stderr", got)
	}
	if got := run(); strings.TrimSpace(got) != "" {
		t.Fatalf("second run wrote %q to stderr", got)
	}
}
