package main

import (
	"errors"
	"strconv"
	"testing"

	"owngit/internal/apiclient"
)

func TestPRCLIAutoMergeEnableAndCancel(t *testing.T) {
	fixture := newPRCLIFixture(t)
	created := runPRCommandJSON(t, append([]string{"create", "--title", "Auto", "--source", "feature", "--target", "main"}, fixture.remoteFlags...))
	number := strconv.FormatInt(created.PullRequest.Number, 10)

	enabled := runPRCommandJSON(t, fixture.autoMergeFlags(number))
	if enabled.PullRequest == nil || enabled.PullRequest.AutoMerge == nil || !enabled.PullRequest.AutoMerge.Enabled {
		t.Fatalf("enable response=%+v", enabled.PullRequest)
	}
	if enabled.PullRequest.AutoMerge.Reason != "waiting_for_checks" ||
		enabled.PullRequest.AutoMerge.SourceOID != fixture.sourceOID || enabled.PullRequest.AutoMerge.TargetOID != fixture.targetOID {
		t.Fatalf("enable auto-merge state=%+v", enabled.PullRequest.AutoMerge)
	}

	shown := runPRCommandJSON(t, append([]string{"show", "--number", number}, fixture.remoteFlags...))
	if shown.PullRequest == nil || shown.PullRequest.AutoMerge == nil || !shown.PullRequest.AutoMerge.Enabled {
		t.Fatalf("show after enable=%+v", shown.PullRequest)
	}

	canceled := runPRCommandJSON(t, fixture.autoMergeFlags(number, "--cancel"))
	if canceled.PullRequest == nil || canceled.PullRequest.AutoMerge != nil {
		t.Fatalf("cancel response=%+v", canceled.PullRequest)
	}

	_, err := captureStdout(func() error { return prCommand(fixture.autoMergeFlags(number, "--cancel")) })
	if err == nil {
		t.Fatal("a second cancel succeeded")
	}
	if got := commandErrorCode(err); got != "auto_merge_not_enabled" {
		t.Fatalf("second cancel code=%q", got)
	}
}

func TestPRCLIAutoMergeRequiresNumber(t *testing.T) {
	fixture := newPRCLIFixture(t)
	_, err := captureStdout(func() error { return prCommand(append(fixture.remoteFlags, "auto-merge")) })
	if err == nil {
		t.Fatal("auto-merge without --number succeeded")
	}
	var apierror *apiclient.Error
	if !errors.As(err, &apierror) || apierror.Code != "invalid_arguments" {
		t.Fatalf("auto-merge validation error=%v", err)
	}

	// Positional operands are refused.
	_, err = captureStdout(func() error { return prCommand(append(fixture.remoteFlags, "auto-merge", "--number", "1", "extra")) })
	if err == nil {
		t.Fatal("auto-merge accepted a positional operand")
	}
}

func (f prCLIFixture) autoMergeFlags(number string, extra ...string) []string {
	arguments := append([]string{"auto-merge"}, extra...)
	arguments = append(arguments, "--number", number)
	return append(arguments, f.remoteFlags...)
}
