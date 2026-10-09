//go:build windows

package checkexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"owngit/internal/gitexec"
)

const (
	windowsFixtureMode            = "OWNGIT_CHECKEXEC_FIXTURE_MODE"
	windowsFixtureMarker          = "OWNGIT_CHECKEXEC_FIXTURE_MARKER"
	windowsFixtureReceipt         = "OWNGIT_CHECKEXEC_FIXTURE_RECEIPT"
	windowsFixtureIdentity        = "OWNGIT_CHECKEXEC_FIXTURE_IDENTITY"
	windowsFixtureLauncherReady   = "OWNGIT_CHECKEXEC_FIXTURE_LAUNCHER_READY"
	windowsFixtureLauncherRelease = "OWNGIT_CHECKEXEC_FIXTURE_LAUNCHER_RELEASE"
	windowsFixtureCleanupSignal   = "OWNGIT_CHECKEXEC_FIXTURE_CLEANUP_SIGNAL"
)

type windowsProcessReceipt struct {
	PID          uint32 `json:"pid"`
	CreationTime uint64 `json:"creation_time"`
	Identity     string `json:"identity"`
}

func TestWindowsDescendantFixture(t *testing.T) {
	mode := os.Getenv(windowsFixtureMode)
	if mode == "" {
		return
	}
	marker := os.Getenv(windowsFixtureMarker)
	switch mode {
	case "launcher":
		time.Sleep(300 * time.Millisecond)
		child := exec.Command(os.Args[0], "-test.run=TestWindowsDescendantFixture")
		child.Env = windowsFixtureEnvironment("marker", marker)
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		os.Exit(0)
	case "marker":
		time.Sleep(time.Second)
		if err := os.WriteFile(marker, []byte("done\n"), 0o600); err != nil {
			os.Exit(92)
		}
		os.Exit(0)
	case "ownership-launcher":
		child := exec.Command(os.Args[0], "-test.run=TestWindowsDescendantFixture")
		child.Env = windowsFixtureEnvironment("ownership-child", marker)
		if err := child.Start(); err != nil {
			os.Exit(94)
		}
		if err := writeWindowsFixtureFile(os.Getenv(windowsFixtureLauncherReady), []byte("ready\n")); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			os.Exit(95)
		}
		if !waitForWindowsMarker(os.Getenv(windowsFixtureLauncherRelease), 15*time.Second) {
			_ = child.Process.Kill()
			_ = child.Wait()
			os.Exit(96)
		}
		_ = child.Process.Release()
		os.Exit(0)
	case "ownership-child":
		var creationTime, exitTime, kernelTime, userTime windows.Filetime
		if err := windows.GetProcessTimes(windows.CurrentProcess(), &creationTime, &exitTime, &kernelTime, &userTime); err != nil {
			os.Exit(97)
		}
		receipt, err := json.Marshal(windowsProcessReceipt{
			PID:          uint32(os.Getpid()),
			CreationTime: windowsFiletimeIdentity(creationTime),
			Identity:     os.Getenv(windowsFixtureIdentity),
		})
		if err != nil || writeWindowsFixtureFile(os.Getenv(windowsFixtureReceipt), receipt) != nil {
			os.Exit(97)
		}
		if !waitForWindowsMarker(os.Getenv(windowsFixtureCleanupSignal), 15*time.Second) {
			os.Exit(98)
		}
		if err := writeWindowsFixtureFile(marker, []byte("survived cleanup\n")); err != nil {
			os.Exit(99)
		}
		os.Exit(0)
	default:
		os.Exit(93)
	}
}

func TestRunPreservesWindowsCommandShellQuoting(t *testing.T) {
	workingDirectory := filepath.Join(t.TempDir(), "working directory")
	if err := os.Mkdir(workingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		marker  string
		command func(string) string
		want    string
	}{
		{
			name:   "quoted relative path",
			marker: filepath.Join(workingDirectory, "relative marker.txt"),
			command: func(string) string {
				return `echo relative>"relative marker.txt"`
			},
			want: "relative\n",
		},
		{
			name:   "quoted absolute path with spaces",
			marker: filepath.Join(workingDirectory, "absolute marker.txt"),
			command: func(marker string) string {
				return `echo absolute>"` + marker + `"`
			},
			want: "absolute\n",
		},
		{
			name:   "control operators retain quoted redirection",
			marker: filepath.Join(workingDirectory, "control marker.txt"),
			command: func(marker string) string {
				return `echo first>"` + marker + `"&&echo second>>"` + marker + `"`
			},
			want: "first\nsecond\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results, cancelled := Run(context.Background(), []Definition{{Name: "quoted", Command: test.command(test.marker)}}, Options{
				Dir: workingDirectory, Timeout: 20 * time.Second,
			})
			if cancelled || len(results) != 1 || results[0].Status != StatusPassed || results[0].CleanupError != "" {
				t.Fatalf("cancelled=%v result_count=%d status=%s cleanup_present=%v", cancelled, len(results), windowsResultStatus(results), len(results) == 1 && results[0].CleanupError != "")
			}
			content, err := os.ReadFile(test.marker)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.ReplaceAll(string(content), "\r\n", "\n")
			if got != test.want {
				t.Fatalf("marker content=%q want=%q", got, test.want)
			}
		})
	}
}

func TestRunPreservesLeadingQuotedExecutableAndQuotedArgument(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "quoted executable directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "fixture executable.exe")
	if err := copyWindowsFixtureExecutable(executable); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "quoted command marker")
	command := `"` + executable + `" "-test.run=^TestWindowsDescendantFixture$"`
	results, cancelled := Run(context.Background(), []Definition{{Name: "quoted executable", Command: command}}, Options{
		Timeout: 20 * time.Second,
		Env:     windowsFixtureEnvironment("marker", marker),
	})
	if cancelled || len(results) != 1 || results[0].Status != StatusPassed || results[0].CleanupError != "" {
		t.Fatalf("cancelled=%v result_count=%d status=%s cleanup_present=%v", cancelled, len(results), windowsResultStatus(results), len(results) == 1 && results[0].CleanupError != "")
	}
	if !waitForWindowsMarker(marker, time.Second) {
		t.Fatal("quoted executable did not receive its quoted argument")
	}
}

func copyWindowsFixtureExecutable(destination string) (result error) {
	source, err := os.Open(os.Args[0])
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, target.Close())
	}()
	_, err = io.Copy(target, source)
	return err
}

func TestRunKeepsExplicitExecutableArgumentsOutOfTheShellPath(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "explicit marker")
	results, cancelled := Run(context.Background(), []Definition{{
		Name: "explicit", Executable: os.Args[0], Arguments: []string{"-test.run=^TestWindowsDescendantFixture$"},
	}}, Options{
		Timeout: 20 * time.Second,
		Env:     windowsFixtureEnvironment("marker", marker),
	})
	if cancelled || len(results) != 1 || results[0].Status != StatusPassed || results[0].CleanupError != "" {
		t.Fatalf("cancelled=%v result_count=%d status=%s cleanup_present=%v", cancelled, len(results), windowsResultStatus(results), len(results) == 1 && results[0].CleanupError != "")
	}
	if !waitForWindowsMarker(marker, time.Second) {
		t.Fatal("explicit executable did not receive its arguments and environment")
	}
}

func windowsResultStatus(results []Result) string {
	if len(results) != 1 {
		return ""
	}
	return results[0].Status
}

func TestRunReapsBackgroundDescendantsOnWindows(t *testing.T) {
	directory := t.TempDir()
	positiveMarker := filepath.Join(directory, "positive")
	positive := exec.Command(os.Args[0], "-test.run=TestWindowsDescendantFixture")
	positive.Env = windowsFixtureEnvironment("launcher", positiveMarker)
	if err := positive.Run(); err != nil {
		t.Fatalf("positive control did not start: %v", err)
	}
	// The marker exists as soon as the descendant creates it, while the
	// descendant may still hold it open, so wait for the complete content
	// and leave the file in place: removing it would race that handle's close.
	if !waitForWindowsMarkerContent(positiveMarker, "done\n", 4*time.Second) {
		t.Fatal("positive control descendant did not survive its launcher")
	}

	marker := filepath.Join(directory, "owned")
	command := windows.EscapeArg(os.Args[0]) + " -test.run=TestWindowsDescendantFixture"
	results, _ := Run(context.Background(), []Definition{{Name: "background", Command: command}}, Options{
		Timeout: 10 * time.Second,
		Env:     windowsFixtureEnvironment("launcher", marker),
	})
	if len(results) != 1 || results[0].Status != StatusPassed {
		t.Fatalf("result=%+v", results)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("owned descendant wrote after cleanup: %v", err)
	}
}

func TestRunOwnsChildCreatedBeforeAttachmentReturns(t *testing.T) {
	for _, testCase := range []struct {
		name                 string
		cancel               bool
		confirmAfter         time.Duration
		withholdConfirmation bool
		status               string
	}{
		{name: "completed", status: StatusPassed},
		{name: "cancelled with late confirmation", cancel: true, confirmAfter: 3 * time.Second, status: StatusCancelled},
		{name: "cancelled without confirmation", cancel: true, withholdConfirmation: true, status: StatusError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testing.Short() && testCase.cancel {
				t.Skip("waits more than two seconds for Windows exit confirmation")
			}
			directory := t.TempDir()
			marker := filepath.Join(directory, "survived")
			receiptPath := filepath.Join(directory, "child-receipt.json")
			launcherReady := filepath.Join(directory, "launcher-ready")
			launcherRelease := filepath.Join(directory, "launcher-release")
			cleanupSignal := filepath.Join(directory, "cleanup-returned")
			identity := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
			environment := windowsFixtureEnvironment("ownership-launcher", marker)
			environment = append(environment,
				windowsFixtureReceipt+"="+receiptPath,
				windowsFixtureIdentity+"="+identity,
				windowsFixtureLauncherReady+"="+launcherReady,
				windowsFixtureLauncherRelease+"="+launcherRelease,
				windowsFixtureCleanupSignal+"="+cleanupSignal,
			)

			originalObserver := ownedProcessStartedObserver
			originalAttach := attachOwnedProcess
			originalTerminate := terminateOwnedProcess
			defer func() {
				ownedProcessStartedObserver = originalObserver
				attachOwnedProcess = originalAttach
				terminateOwnedProcess = originalTerminate
			}()
			var childHandle windows.Handle
			var receipt windowsProcessReceipt
			ownedProcessStartedObserver = func() error {
				if !waitForWindowsMarker(launcherReady, 5*time.Second) {
					return fmt.Errorf("launcher did not report ready")
				}
				observed, err := readWindowsProcessReceipt(receiptPath, 5*time.Second)
				if err != nil {
					return err
				}
				if observed.PID == 0 || observed.CreationTime == 0 || observed.Identity != identity {
					return fmt.Errorf("invalid child receipt: %+v", observed)
				}
				handle, err := windows.OpenProcess(
					uint32(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE),
					false, observed.PID,
				)
				if err != nil {
					return fmt.Errorf("open fixture child %d: %w", observed.PID, err)
				}
				var creationTime, exitTime, kernelTime, userTime windows.Filetime
				if err := windows.GetProcessTimes(handle, &creationTime, &exitTime, &kernelTime, &userTime); err != nil {
					closeErr := windows.CloseHandle(handle)
					return fmt.Errorf("read fixture child %d creation time: %v (close: %v)", observed.PID, err, closeErr)
				}
				openedCreationTime := windowsFiletimeIdentity(creationTime)
				if openedCreationTime != observed.CreationTime {
					closeErr := windows.CloseHandle(handle)
					return fmt.Errorf("fixture child %d identity mismatch: receipt=%016x handle=%016x (close: %v)", observed.PID, observed.CreationTime, openedCreationTime, closeErr)
				}
				receipt = observed
				childHandle = handle
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var releaseErr error
			attachOwnedProcess = func(cmd *exec.Cmd) (*gitexec.ProcessOwner, error) {
				owner, err := originalAttach(cmd)
				if testCase.cancel {
					cancel()
				} else {
					releaseErr = writeWindowsFixtureFile(launcherRelease, []byte("release\n"))
				}
				return owner, err
			}

			attempts := 0
			if testCase.confirmAfter > 0 || testCase.withholdConfirmation {
				terminateOwnedProcess = delayedExitConfirmation(originalTerminate, testCase.confirmAfter, testCase.withholdConfirmation, &attempts)
			}

			command := windows.EscapeArg(os.Args[0]) + " -test.run=TestWindowsDescendantFixture"
			results, cancelled := Run(ctx, []Definition{{Name: "startup-child", Command: command}}, Options{
				Timeout: 20 * time.Second,
				Env:     environment,
			})

			var stateErr, signalErr, waitErr, fallbackWaitErr, terminateErr, finalObservationErr, closeErr error
			aliveAfterCleanup := false
			postCleanupState := "handle-unavailable"
			finalExitObservation := "not-observed"
			fallbackOutcome := "not-needed"
			if childHandle != 0 {
				var state uint32
				state, stateErr = windows.WaitForSingleObject(childHandle, 0)
				postCleanupState = windowsWaitState(state, stateErr)
				aliveAfterCleanup = stateErr == nil && state == uint32(windows.WAIT_TIMEOUT)
			}
			signalErr = writeWindowsFixtureFile(cleanupSignal, []byte("cleanup returned\n"))
			if childHandle != 0 {
				var state uint32
				state, waitErr = windows.WaitForSingleObject(childHandle, 5_000)
				finalExitObservation = windowsWaitState(state, waitErr)
				if waitErr == nil && state == uint32(windows.WAIT_TIMEOUT) {
					fallbackOutcome = "terminate-requested"
					terminateErr = windows.TerminateProcess(childHandle, 100)
					if terminateErr == nil {
						var fallbackState uint32
						fallbackState, fallbackWaitErr = windows.WaitForSingleObject(childHandle, 5_000)
						finalExitObservation = windowsWaitState(fallbackState, fallbackWaitErr)
						if fallbackWaitErr == nil && fallbackState == uint32(windows.WAIT_OBJECT_0) {
							fallbackOutcome = "terminated-exit-observed"
						} else if fallbackWaitErr == nil {
							fallbackOutcome = fmt.Sprintf("terminate-sent-state=0x%x", fallbackState)
							finalObservationErr = fmt.Errorf("unexpected wait state after fallback termination: 0x%x", fallbackState)
						} else {
							fallbackOutcome = "terminate-sent-wait-error"
						}
					} else {
						fallbackOutcome = "terminate-failed"
					}
				} else if waitErr == nil && state != uint32(windows.WAIT_OBJECT_0) {
					finalObservationErr = fmt.Errorf("unexpected final wait state: 0x%x", state)
				}
				closeErr = windows.CloseHandle(childHandle)
			}
			markerContent, markerErr := os.ReadFile(marker)
			markerExists := markerErr == nil
			if markerErr != nil && !os.IsNotExist(markerErr) {
				t.Errorf("read survival marker: %v", markerErr)
			}
			t.Logf("fixture receipt pid=%d creation=%016x; post-cleanup=%s; final-exit=%s; fallback=%s", receipt.PID, receipt.CreationTime, postCleanupState, finalExitObservation, fallbackOutcome)

			if releaseErr != nil || stateErr != nil || signalErr != nil || waitErr != nil || fallbackWaitErr != nil || terminateErr != nil || finalObservationErr != nil || closeErr != nil {
				t.Fatalf("fixture errors: release=%v state=%v signal=%v wait=%v fallback-wait=%v terminate=%v final=%v close=%v", releaseErr, stateErr, signalErr, waitErr, fallbackWaitErr, terminateErr, finalObservationErr, closeErr)
			}
			if cancelled != testCase.cancel || len(results) != 1 || results[0].Status != testCase.status {
				t.Fatalf("run result=%+v cancelled=%v", results, cancelled)
			}
			if testCase.withholdConfirmation {
				if !strings.Contains(results[0].CleanupError, "exit was not confirmed before the cleanup deadline") || attempts != 2 {
					t.Fatalf("unconfirmed exit: result=%+v attempts=%d", results, attempts)
				}
			} else if results[0].CleanupError != "" {
				t.Fatalf("confirmed exit reported a cleanup error: %+v", results)
			}
			if receipt.PID == 0 {
				t.Fatal("fixture child identity was not captured")
			}
			if aliveAfterCleanup && markerExists {
				t.Fatalf("fixture child %d survived Run cleanup and wrote %q", receipt.PID, markerContent)
			}
			if aliveAfterCleanup {
				t.Fatalf("fixture child %d survived Run cleanup without writing its marker", receipt.PID)
			}
			if markerExists {
				t.Fatalf("fixture child %d wrote a survival marker after cleanup", receipt.PID)
			}
		})
	}
}

func delayedExitConfirmation(terminate func(*gitexec.ProcessOwner, time.Duration) error, confirmAfter time.Duration, withholdConfirmation bool, attempts *int) func(*gitexec.ProcessOwner, time.Duration) error {
	return func(owner *gitexec.ProcessOwner, grace time.Duration) error {
		*attempts += 1
		if *attempts > 1 && !withholdConfirmation {
			return terminate(owner, grace)
		}
		deadline := time.Now().Add(grace)
		var confirmed <-chan time.Time
		if !withholdConfirmation {
			timer := time.NewTimer(confirmAfter)
			defer timer.Stop()
			confirmed = timer.C
		}
		if err := terminate(owner, time.Until(deadline)); err != nil {
			return err
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-confirmed:
			return nil
		case <-timer.C:
			return errors.New("owned process exit was not confirmed before the cleanup deadline")
		}
	}
}

func windowsFixtureEnvironment(mode, marker string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key := strings.SplitN(entry, "=", 2)[0]
		if strings.EqualFold(key, windowsFixtureMode) || strings.EqualFold(key, windowsFixtureMarker) {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, windowsFixtureMode+"="+mode, windowsFixtureMarker+"="+marker)
}

func windowsFiletimeIdentity(filetime windows.Filetime) uint64 {
	return uint64(filetime.HighDateTime)<<32 | uint64(filetime.LowDateTime)
}

func windowsWaitState(state uint32, err error) string {
	if err != nil {
		return "error=" + err.Error()
	}
	switch state {
	case uint32(windows.WAIT_OBJECT_0):
		return "exited"
	case uint32(windows.WAIT_TIMEOUT):
		return "running"
	default:
		return fmt.Sprintf("state=0x%x", state)
	}
}

func writeWindowsFixtureFile(path string, content []byte) error {
	if path == "" {
		return fmt.Errorf("fixture path is empty")
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func readWindowsProcessReceipt(path string, timeout time.Duration) (windowsProcessReceipt, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil {
			var receipt windowsProcessReceipt
			if err := json.Unmarshal(content, &receipt); err == nil {
				return receipt, nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return windowsProcessReceipt{}, fmt.Errorf("read child receipt: %w", lastErr)
}

func waitForWindowsMarkerContent(path, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if content, err := os.ReadFile(path); err == nil && string(content) == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitForWindowsMarker(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
