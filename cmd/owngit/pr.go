package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"owngit/internal/apiclient"
	"owngit/internal/bidi"
	"owngit/internal/pullrequest"
	"owngit/internal/state"
)

// generalRemoteFlags are the connection flags of commands that use general
// access: the shared password, or no credential when access is open.
type generalRemoteFlags struct {
	server             string
	repository         string
	passwordFile       string
	acceptInsecureHTTP bool
	withRepository     bool
}

func prCommand(arguments []string) error {
	if len(arguments) == 0 {
		printPRUsage(os.Stderr)
		return cliProblem("invalid_arguments", "pr requires create, list, show, edit, diff, review, mergeability, merge, close, or reopen.")
	}
	if isHelpArgument(arguments[0]) {
		printPRUsage(os.Stdout)
		return nil
	}
	switch arguments[0] {
	case "create":
		return prCreate(arguments[1:])
	case "list":
		return prList(arguments[1:])
	case "show":
		return prShow(arguments[1:])
	case "edit":
		return prEdit(arguments[1:])
	case "diff":
		return prDiff(arguments[1:])
	case "review":
		return prReview(arguments[1:])
	case "mergeability":
		return prMergeability(arguments[1:])
	case "merge":
		return prMerge(arguments[1:])
	case "close", "reopen":
		return prSetClosed(arguments[0], arguments[1:])
	case "auto-merge":
		return prAutoMerge(arguments[1:])
	default:
		return cliProblem("invalid_arguments", "Unknown pr command: "+arguments[0])
	}
}

// prAutoMerge enables automatic merging for an open pull request, or cancels
// the intent with --cancel. OwnGit then merges the request itself once the
// checks of its current source commit pass and the current pair merges.
func prAutoMerge(arguments []string) error {
	flags := newCommandFlagSet("pr auto-merge")
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	cancel := flags.Bool("cancel", false, "cancel an enabled automatic merge")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *number <= 0 {
		return cliProblem("invalid_arguments", "pr auto-merge requires --number.")
	}
	connection, err := remote.connection()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if *cancel {
		return writeResult(cancelPullRequestAutoMerge(ctx, connection, *number))
	}
	return writeResult(enablePullRequestAutoMerge(ctx, connection, *number))
}

func prCreate(arguments []string) error {
	flags := newCommandFlagSet("pr create")
	remote := addGeneralRemoteFlags(flags, true)
	title := flags.String("title", "", "pull request title")
	source := flags.String("source", "", "source branch")
	targetBranch := flags.String("target", "", "target branch")
	review := flags.String("review", "", "optional review choice: request or skip")
	bodyFile := flags.String("body-file", "", "file with the Markdown description, or - for standard input")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *title == "" || *source == "" || *targetBranch == "" {
		return cliProblem("invalid_arguments", "pr create requires --title, --source, and --target. --review and --body-file are optional.")
	}
	body, err := readPullRequestText(*bodyFile, "--body-file")
	if err != nil {
		return err
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(createPullRequest(context.Background(), target, pullrequest.CreateInput{
		Title: *title, Body: body, SourceBranch: *source, TargetBranch: *targetBranch, ReviewChoice: *review,
	}))
}

// prEdit replaces the title, the description, or both. --edit-revision is
// the edit_revision that pr show printed; the edit is refused with stale_edit
// when the pull request was edited since.
func prEdit(arguments []string) error {
	flags := newCommandFlagSet("pr edit")
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	revision := flags.Int64("edit-revision", -1, "edit_revision from pr show")
	title := flags.String("title", "", "new title")
	bodyFile := flags.String("body-file", "", "file with the new Markdown description, or - for standard input; an empty file clears it")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	given := map[string]bool{}
	flags.Visit(func(set *flag.Flag) { given[set.Name] = true })
	if *number <= 0 || *revision < 0 {
		return cliProblem("invalid_arguments", "pr edit requires --number and --edit-revision (edit_revision from pr show).")
	}
	if !given["title"] && !given["body-file"] {
		return cliProblem("invalid_arguments", "pr edit requires --title, --body-file, or both.")
	}
	input := pullrequest.EditInput{EditRevision: revision}
	if given["title"] {
		input.Title = title
	}
	if given["body-file"] {
		// An empty path is more likely an unset variable than a wish to
		// clear the description; an empty file clears it.
		if *bodyFile == "" {
			return cliProblem("invalid_arguments", "--body-file needs a file, or - for standard input.")
		}
		body, err := readPullRequestText(*bodyFile, "--body-file")
		if err != nil {
			return err
		}
		input.Body = &body
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(editPullRequest(context.Background(), target, *number, input))
}

// readPullRequestText reads a description or review note from path, or from
// standard input when path is "-". No path is no text. The bytes are kept as
// read, so the text check before sending refuses one that is not UTF-8. The
// 64 KiB limit applies after CRLF line ends become LF, so reading stops at
// twice the limit: a text that long is over the limit however its line ends
// convert, and the check refuses it like any other text over the limit.
func readPullRequestText(path, flagName string) (string, error) {
	if path == "" {
		return "", nil
	}
	reader := io.Reader(os.Stdin)
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return "", &apiclient.Error{Code: "invalid_arguments", Message: flagName + " could not be opened.", Cause: err}
		}
		defer file.Close()
		reader = file
	}
	limit := int64(2 * state.MaximumPullRequestTextBytes)
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return "", &apiclient.Error{Code: "invalid_arguments", Message: flagName + " could not be read.", Cause: err}
	}
	return string(content), nil
}

func prList(arguments []string) error {
	flags := newCommandFlagSet("pr list")
	remote := addGeneralRemoteFlags(flags, true)
	var query pullRequestListQuery
	flags.StringVar(&query.State, "state", "", "list only open, closed, or merged pull requests (default: all states)")
	flags.IntVar(&query.Limit, "limit", 0, "pull requests per page, 1 to 100 (default 50)")
	flags.Int64Var(&query.Before, "before", 0, "continue below this pull request number: the \"next\" value of the previous page")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(listPullRequests(context.Background(), target, query))
}

func prShow(arguments []string) error {
	flags := newCommandFlagSet("pr show")
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *number <= 0 {
		return cliProblem("invalid_arguments", "pr show requires a positive --number.")
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(showPullRequest(context.Background(), target, *number))
}

func prDiff(arguments []string) error {
	flags := newCommandFlagSet("pr diff")
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	sourceOID := flags.String("source-oid", "", "pin this exact source commit object ID (with --target-oid)")
	targetOID := flags.String("target-oid", "", "pin this exact target commit object ID (with --source-oid)")
	stat := flags.Bool("stat", false, "print the JSON result without the patch")
	patch := flags.Bool("patch", false, "print only the patch text; the revisions and any cut are noted on standard error")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *number <= 0 {
		return cliProblem("invalid_arguments", "pr diff requires a positive --number.")
	}
	if (*sourceOID == "") != (*targetOID == "") {
		return cliProblem("invalid_arguments", "pr diff requires both --source-oid and --target-oid, or neither.")
	}
	if *stat && *patch {
		return cliProblem("invalid_arguments", "Use --stat or --patch, not both.")
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	content, err := pullRequestDiff(context.Background(), target, *number, pullrequest.RevisionInput{SourceOID: *sourceOID, TargetOID: *targetOID})
	switch {
	case err != nil:
		return err
	case *stat:
		return writeResult(diffStat(content))
	case *patch:
		return writeDiffPatch(content, os.Stdout, os.Stderr)
	}
	return writeJSON(content)
}

// diffStat returns a diff result without its patch.
func diffStat(content []byte) ([]byte, error) {
	diff, err := decodeDiff(content)
	if err != nil {
		return nil, err
	}
	return encodeDiffStat(diff)
}

func decodeDiff(content []byte) (pullrequest.Diff, error) {
	var diff pullrequest.Diff
	if err := json.Unmarshal(content, &diff); err != nil {
		return pullrequest.Diff{}, &apiclient.Error{Code: "invalid_response", Message: "The OwnGit API returned an invalid diff.", Cause: err}
	}
	return diff, nil
}

// encodeDiffStat encodes diff without its patch.
func encodeDiffStat(diff pullrequest.Diff) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		pullrequest.Diff
		// A field at a shallower depth hides the embedded one of the same
		// JSON name, and a nil pointer with omitempty is left out.
		Patch *struct{} `json:"patch,omitempty"`
	}{Diff: diff})
	if err != nil {
		return nil, &apiclient.Error{Code: "output_failed", Message: "The JSON result could not be encoded.", Cause: err}
	}
	return encoded, nil
}

// writeDiffPatch prints only the patch text on output, and on notes one line
// with the diffed revisions plus a line for each condition a reader of the
// bare patch would otherwise miss.
func writeDiffPatch(content []byte, output, notes io.Writer) error {
	var diff pullrequest.Diff
	if err := json.Unmarshal(content, &diff); err != nil {
		return &apiclient.Error{Code: "invalid_response", Message: "The OwnGit API returned an invalid diff.", Cause: err}
	}
	lines := []string{fmt.Sprintf("owngit: source %s, target %s, merge base %s\n", diff.Source.OID, diff.Target.OID, valueOr(diff.MergeBase, "none"))}
	if diff.Moved && diff.Current != nil {
		lines = append(lines, fmt.Sprintf("owngit: the branches moved; the current source is %s and the current target is %s\n",
			valueOr(diff.Current.Source.OID, diff.Current.Source.Status), valueOr(diff.Current.Target.OID, diff.Current.Target.Status)))
	}
	if diff.Unavailable != "" {
		lines = append(lines, fmt.Sprintf("owngit: no patch: %s\n", diff.Unavailable))
	}
	if diff.Truncated {
		scope := "the patch leaves out some files"
		if diff.Incomplete {
			scope = "the patch and the file list leave out some files"
		}
		lines = append(lines, fmt.Sprintf("owngit: %s (%s)\n", scope, diff.Reason))
	}
	if note := tooLargeNote(diff.Files); note != "" {
		lines = append(lines, fmt.Sprintf("owngit: %s\n", note))
	}
	if _, err := io.WriteString(notes, strings.Join(lines, "")); err != nil {
		return &apiclient.Error{Code: "output_failed", Message: "The patch notes could not be written.", Cause: err}
	}
	if _, err := io.WriteString(output, diff.Patch); err != nil {
		return &apiclient.Error{Code: "output_failed", Message: "The patch could not be written.", Cause: err}
	}
	return nil
}

// noteFileNames is how many file names one note names; a longer list is
// counted instead, so the note stays one readable line.
const noteFileNames = 10

// tooLargeNote names the files the patch of diff leaves out because this
// computer cannot compare them as text, so a reader of the bare patch does not
// take it for a complete change. Each such file is in the file list with
// too_large set; the patch itself cannot show that they are missing.
func tooLargeNote(files []pullrequest.DiffFile) string {
	var paths []string
	for _, file := range files {
		if file.TooLarge {
			paths = append(paths, file.Path)
		}
	}
	switch {
	case len(paths) == 0:
		return ""
	case len(paths) == 1:
		return "the patch leaves out " + paths[0] + ", which is too large to compare here"
	case len(paths) <= noteFileNames:
		return "the patch leaves out these files, which are too large to compare here: " + strings.Join(paths, ", ")
	default:
		return fmt.Sprintf("the patch leaves out %d files too large to compare here, including %s",
			len(paths), strings.Join(paths[:3], ", "))
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func prReview(arguments []string) error {
	if len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: owngit pr review <request|submit|skip> [options]")
		return cliProblem("invalid_arguments", "pr review requires request, submit, or skip.")
	}
	if isHelpArgument(arguments[0]) {
		fmt.Fprintln(os.Stdout, "Usage: owngit pr review <request|submit|skip> [options]")
		return nil
	}
	action := arguments[0]
	if action != "request" && action != "submit" && action != "skip" {
		return cliProblem("invalid_arguments", "pr review requires request, submit, or skip.")
	}
	flags := newCommandFlagSet("pr review " + action)
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	sourceOID := flags.String("source-oid", "", "exact source commit object ID")
	targetOID := flags.String("target-oid", "", "exact target commit object ID")
	var decision, reviewer, noteFile string
	if action == "submit" {
		flags.StringVar(&decision, "decision", "", "review result: approved or changes_requested")
		flags.StringVar(&reviewer, "reviewer", "", "supplied reviewer label")
		flags.StringVar(&noteFile, "note-file", "", "file with a Markdown review note, or - for standard input")
	}
	if err := parseFlagsWithoutOperands(flags, arguments[1:]); err != nil {
		return err
	}
	if *number <= 0 || *sourceOID == "" || *targetOID == "" {
		return cliProblem("invalid_arguments", "pr review requires --number, --source-oid, and --target-oid.")
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	if action == "submit" {
		if decision == "" || reviewer == "" {
			return cliProblem("invalid_arguments", "pr review submit requires --decision and --reviewer.")
		}
		note, err := readPullRequestText(noteFile, "--note-file")
		if err != nil {
			return err
		}
		return writeResult(submitPullRequestReview(context.Background(), target, *number, pullrequest.ReviewSubmitInput{
			SourceOID: *sourceOID, TargetOID: *targetOID, Decision: decision, ReviewerLabel: reviewer, Note: note,
		}))
	}
	return writeResult(markPullRequestReview(context.Background(), target, *number, action, pullrequest.RevisionInput{SourceOID: *sourceOID, TargetOID: *targetOID}))
}

// prMergeability prints whether the pull request can merge now. With
// --source-oid and --target-oid the answer is stale unless that pair is still
// the current one.
func prMergeability(arguments []string) error {
	flags := newCommandFlagSet("pr mergeability")
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	sourceOID := flags.String("source-oid", "", "answer stale unless the source is still this commit (with --target-oid)")
	targetOID := flags.String("target-oid", "", "answer stale unless the target is still this commit (with --source-oid)")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *number <= 0 {
		return cliProblem("invalid_arguments", "pr mergeability requires a positive --number.")
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(pullRequestMergeability(context.Background(), target, *number, pullrequest.RevisionInput{SourceOID: *sourceOID, TargetOID: *targetOID}))
}

func prMerge(arguments []string) error {
	flags := newCommandFlagSet("pr merge")
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	sourceOID := flags.String("source-oid", "", "exact source commit object ID")
	targetOID := flags.String("target-oid", "", "exact target commit object ID")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *number <= 0 || *sourceOID == "" || *targetOID == "" {
		return cliProblem("invalid_arguments", "pr merge requires --number, --source-oid, and --target-oid.")
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(mergePullRequest(context.Background(), target, *number, pullrequest.RevisionInput{SourceOID: *sourceOID, TargetOID: *targetOID}))
}

// prSetClosed closes or reopens a pull request. Neither changes a branch, so
// no object IDs are needed.
func prSetClosed(action string, arguments []string) error {
	flags := newCommandFlagSet("pr " + action)
	remote := addGeneralRemoteFlags(flags, true)
	number := flags.Int64("number", 0, "pull request number")
	if err := parseFlagsWithoutOperands(flags, arguments); err != nil {
		return err
	}
	if *number <= 0 {
		return cliProblem("invalid_arguments", "pr "+action+" requires a positive --number.")
	}
	target, err := remote.connection()
	if err != nil {
		return err
	}
	return writeResult(setPullRequestClosed(context.Background(), target, *number, action == "close"))
}

// newCommandFlagSet returns an empty flag set for a client command. The flag
// package's own error output is discarded because the command reports a
// structured error instead.
func newCommandFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

// addGeneralRemoteFlags adds the connection flags. withRepository adds
// --repository for commands that address one repository.
func addGeneralRemoteFlags(flags *flag.FlagSet, withRepository bool) *generalRemoteFlags {
	remote := &generalRemoteFlags{withRepository: withRepository}
	flags.StringVar(&remote.server, "server", "", "OwnGit HTTP(S) origin")
	if withRepository {
		flags.StringVar(&remote.repository, "repository", "", "repository identifier")
	}
	flags.StringVar(&remote.passwordFile, "password-file", "", "owner-readable file containing the shared general-access password")
	flags.BoolVar(&remote.acceptInsecureHTTP, "accept-insecure-http", false, "accept unencrypted HTTP for this request")
	return remote
}

// parseFlagsWithoutOperands parses a client command that takes options only.
// A flag error or any positional argument becomes an invalid_arguments
// problem, and -h or --help still ends the command through errUsageShown.
func parseFlagsWithoutOperands(flags *flag.FlagSet, arguments []string) error {
	if err := parseFlags(flags, arguments); err != nil {
		if errors.Is(err, errUsageShown) {
			return err
		}
		return cliProblem("invalid_arguments", err.Error())
	}
	if flags.NArg() != 0 {
		return cliProblem("invalid_arguments", "Unexpected positional arguments were supplied.")
	}
	return nil
}

// connection resolves the server and repository from the flags or, inside a
// clone, from its origin remote, then reads the shared password if a file is
// given.
func (remote *generalRemoteFlags) connection() (connection, error) {
	resolved, err := resolveTarget(context.Background(), remote.server, remote.repository, remote.withRepository, remote.acceptInsecureHTTP, ".")
	if err != nil {
		return connection{}, err
	}
	target := connection{server: resolved.server, repository: resolved.repository, credential: credential{kind: credentialNone}}
	if remote.passwordFile != "" {
		password, err := readServerPassword(remote.passwordFile, resolved.server, resolved.inferredServer, "The shared password file")
		if err != nil {
			return connection{}, err
		}
		target.credential = credential{kind: credentialSharedPassword, secret: password}
	}
	noteInference(resolved)
	return target, nil
}

func cliProblem(code, message string) error {
	return &apiclient.Error{Code: code, Message: message}
}

func writeStructuredCommandError(writer io.Writer, err error) bool {
	var coded interface {
		error
		ErrorCode() string
		ErrorDetails() json.RawMessage
	}
	if !errors.As(err, &coded) {
		return false
	}
	// The message is the whole error, so context that wraps the coded
	// error, such as how to run the command as another account, stays.
	description := pullrequest.ErrorDescription{Code: coded.ErrorCode(), Message: err.Error()}
	description.Details = coded.ErrorDetails()
	encoded, _ := bidi.MarshalJSON(pullrequest.ErrorEnvelope{OK: false, Error: description})
	_, _ = writer.Write(append(encoded, '\n'))
	return true
}

func printPRUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage: owngit pr <create|list|show|edit|diff|review|mergeability|merge|close|reopen|auto-merge> [options]")
	fmt.Fprintln(writer, "Inside a clone of an OwnGit repository, --server and --repository default to its origin remote. HTTP also requires --accept-insecure-http.")
	fmt.Fprintln(writer, "pr list shows the newest pull requests first, one page at a time. When more remain, the result has \"next\"; pass it as --before for the following page.")
	fmt.Fprintln(writer, "pr auto-merge --number N enables automatic merging once the current source commit's checks pass; --cancel cancels it.")
}
