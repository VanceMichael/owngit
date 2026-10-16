package main

import (
	"errors"
	"strings"
	"testing"

	"owngit/internal/apiclient"
)

// helpCommands lists every command path. Leaves parse flags and must print
// their options; groups print the actions they accept.
var helpCommands = []struct {
	path string
	leaf bool
}{
	{"serve", true}, {"service", false}, {"service install", true}, {"health", true}, {"setup-link", true}, {"reset-admin", true}, {"approve-host", true},
	{"network", false}, {"network show", true}, {"network set", true}, {"network reset", true},
	{"tailscale", false}, {"tailscale status", true}, {"tailscale on", true}, {"tailscale off", true},
	{"forget-check-container", true}, {"backup", true}, {"backup verify", true}, {"backup now", true}, {"backup status", true}, {"backup runs", true}, {"backup check", true}, {"backup download", true}, {"backup upload", true},
	{"backup plan", false}, {"backup plan list", true}, {"backup plan create", true}, {"backup plan delete", true},
	{"backup schedule", false}, {"backup schedule show", true}, {"backup schedule set", true}, {"backup schedule off", true}, {"restore", true}, {"upgrade-backup", true}, {"tray", true},
	{"repo", false}, {"repo list", true}, {"repo show", true}, {"repo create", true},
	{"repo kept-history", true}, {"repo restore", false}, {"repo restore preview", true}, {"repo restore apply", true},
	{"activity", true}, {"tasks", true},
	{"repo share", false}, {"repo share list", true}, {"repo share create", true}, {"repo share revoke", true},
	{"pr", false}, {"pr create", true}, {"pr list", true}, {"pr show", true}, {"pr diff", true},
	{"pr review", false}, {"pr review request", true}, {"pr review submit", true}, {"pr review skip", true},
	{"pr mergeability", true}, {"pr merge", true},
	{"check", false}, {"check task", false}, {"check task new", true}, {"check task list", true},
	{"check cycle", false}, {"check cycle reserve", true}, {"check cycle list", true},
	{"check run", true}, {"check status", true}, {"check log", true},
	{"check config", false}, {"check config show", true},
	{"helper-credential", false}, {"helper-credential create", true},
	{"helper-credential list", true}, {"helper-credential revoke", true},
	{"check-policy", false}, {"check-policy show", true}, {"check-policy set", true},
	{"check-policy enable", true}, {"check-policy disable", true},
	{"check-job", false}, {"check-job list", true}, {"check-job show", true}, {"check-job log", true},
	{"check-job cancel", true}, {"check-job rerun", true},
	{"runner-credential", false}, {"runner-credential issue", true},
	{"runner-credential list", true}, {"runner-credential revoke", true},
	{"runner", true},
	{"import", false}, {"import add", true}, {"import refresh", true}, {"import status", true},
	{"import history", true}, {"import cancel", true}, {"import schedule", true},
	{"import credentials", true}, {"import resolve", true},
	{"settings", false}, {"settings show", true}, {"settings set", true},
	{"skill", true}, {"mcp", true}, {"update", true}, {"doctor", true},
}

func TestEveryCommandPrintsHelpAndSucceeds(t *testing.T) {
	for _, command := range helpCommands {
		for _, flag := range []string{"-h", "--help"} {
			arguments := append(strings.Fields(command.path), flag)
			output, err := captureStdout(func() error { return run(arguments) })
			if err != nil {
				t.Errorf("owngit %s %s returned an error: %v", command.path, flag, err)
				continue
			}
			if !strings.HasPrefix(output, "Usage: owngit "+command.path) {
				t.Errorf("owngit %s %s printed %q, want its usage", command.path, flag, output)
			}
			if command.leaf && !strings.Contains(output, "\nOptions:\n  --") {
				t.Errorf("owngit %s %s did not list its options: %q", command.path, flag, output)
			}
		}
	}
}

func TestHelpCommandListCoversTopLevelUsage(t *testing.T) {
	var usage strings.Builder
	printUsage(&usage)
	listed := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(usage.String())[2], "["), "]")
	// uninstall has its own help test, since running it bare uninstalls.
	covered := map[string]bool{"version": true, "uninstall": true}
	for _, command := range helpCommands {
		covered[command.path] = true
	}
	for _, name := range strings.Split(listed, "|") {
		if !covered[name] {
			t.Errorf("command %q from the top-level usage has no help test", name)
		}
	}
}

func TestFlagErrorsStillFail(t *testing.T) {
	for _, command := range helpCommands {
		if !command.leaf {
			continue
		}
		arguments := append(strings.Fields(command.path), "--no-such-flag")
		if _, err := captureStdout(func() error { return run(arguments) }); err == nil {
			t.Errorf("owngit %s --no-such-flag succeeded", command.path)
		}
	}
}

// A command group run without an action is an error, like any other missing
// argument, and the group help names every action it accepts.
func TestCommandGroupsWithoutAnActionFail(t *testing.T) {
	for _, command := range helpCommands {
		if command.leaf {
			continue
		}
		group := command.path
		usage, err := captureStderr(func() error {
			_, err := captureStdout(func() error { return run(strings.Fields(group)) })
			return err
		})
		if err == nil {
			t.Errorf("owngit %s without an action succeeded", group)
		}
		if !strings.HasPrefix(usage, "Usage: owngit "+group+" ") {
			t.Errorf("owngit %s without an action printed %q on stderr, want its usage", group, usage)
		}
	}
	output, err := captureStdout(func() error { return run([]string{"check", "--help"}) })
	if err != nil || !strings.Contains(output, "cycle list") {
		t.Errorf("owngit check --help=%q err=%v, want cycle list", output, err)
	}
}

// An action's help lists only the options that action accepts, and an option
// of another action is refused instead of being silently ignored.
func TestActionHelpListsOnlyItsOwnOptions(t *testing.T) {
	for _, test := range []struct {
		path      string
		want, not []string
	}{
		{"check-policy show", nil, []string{"--policy-file", "--enable"}},
		{"check-policy set", []string{"--policy-file", "--enable"}, nil},
		{"check-job list", nil, []string{"--job"}},
		{"check-job show", []string{"--job"}, nil},
		{"runner-credential list", []string{"--ca-file"}, []string{"--label", "--token-file", "--creation-id", "--credential"}},
		{"runner-credential revoke", []string{"--credential"}, []string{"--label", "--token-file", "--creation-id"}},
		{"runner-credential issue", []string{"--label", "--token-file", "--creation-id"}, []string{"--credential"}},
		{"import history", []string{"--limit N", "--cursor ROW"}, []string{" int"}},
		{"repo list", []string{"--server", "--password-file"}, []string{"--repository", "--name"}},
		{"repo show", []string{"--repository"}, []string{"--name"}},
		{"repo create", []string{"--name", "--description"}, []string{"--repository"}},
		{"pr review request", []string{"--number", "--source-oid", "--target-oid"}, []string{"--decision", "--reviewer", "--note-file"}},
		{"pr review submit", []string{"--number", "--source-oid", "--target-oid", "--decision", "--reviewer", "--note-file"}, nil},
		{"pr review skip", []string{"--number", "--source-oid", "--target-oid"}, []string{"--decision", "--reviewer", "--note-file"}},
		{"repo restore preview", []string{"--source OID", "--target BRANCH", "--path FILE"}, []string{"--expected-head"}},
		{"repo restore apply", []string{"--source OID", "--target BRANCH", "--path FILE", "--expected-head OID"}, nil},
	} {
		output, err := captureStdout(func() error { return run(append(strings.Fields(test.path), "--help")) })
		if err != nil {
			t.Fatalf("owngit %s --help: %v", test.path, err)
		}
		for _, want := range test.want {
			if !strings.Contains(output, "  "+want) {
				t.Errorf("owngit %s --help lacks %q: %q", test.path, want, output)
			}
		}
		for _, not := range test.not {
			if strings.Contains(output, not) {
				t.Errorf("owngit %s --help lists %q: %q", test.path, not, output)
			}
		}
	}
	var usage strings.Builder
	printImportUsage(&usage)
	if !strings.Contains(usage.String(), "[--limit N] [--cursor ROW]") {
		t.Errorf("import usage does not match the history options: %q", usage.String())
	}
	for _, arguments := range [][]string{
		{"check-policy", "show", "--policy-file", "policy.json"},
		{"pr", "review", "request", "--decision", "approved"},
		{"pr", "review", "request", "--reviewer", "synthetic-reviewer"},
		{"pr", "review", "request", "--note-file", "note.md"},
		{"pr", "review", "skip", "--decision", "approved"},
		{"pr", "review", "skip", "--reviewer", "synthetic-reviewer"},
		{"pr", "review", "skip", "--note-file", "note.md"},
	} {
		_, err := captureStdout(func() error { return run(arguments) })
		var problem *apiclient.Error
		if !errors.As(err, &problem) || problem.Code != "invalid_arguments" || !strings.Contains(problem.Message, "flag provided but not defined:") {
			t.Errorf("owngit %s err=%v, want invalid_arguments for an undefined flag", strings.Join(arguments, " "), err)
		}
	}
}

// The service actions other than install take no options. Their help says
// so, and an option is refused before anything runs.
func TestOptionlessServiceActionsPrintHelpAndRefuseOptions(t *testing.T) {
	for _, action := range []string{"uninstall", "status", "start", "stop", "restart"} {
		for _, flag := range []string{"-h", "--help"} {
			output, err := captureStdout(func() error { return run([]string{"service", action, flag}) })
			if err != nil || !strings.HasPrefix(output, "Usage: owngit service "+action) || !strings.Contains(output, "takes no options") {
				t.Errorf("owngit service %s %s = %q, %v; want its usage", action, flag, output, err)
			}
		}
		if _, err := captureStdout(func() error { return run([]string{"service", action, "--no-such-flag"}) }); err == nil {
			t.Errorf("owngit service %s --no-such-flag succeeded", action)
		}
	}
}
