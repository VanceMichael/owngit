package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"owngit/internal/checkoutput"
)

const maximumStepOutput = 256 << 10

type ScriptResult struct {
	Status       string
	ExitCode     *int
	Duration     time.Duration
	Output       string
	OutputGap    checkoutput.Gap
	Truncated    bool
	CleanupError string
}

// RunIdentity comes from the admitted run, never from a persisted plan.
type RunIdentity struct {
	ID      string
	Number  int64
	Attempt int64
}

// Script is one prepared step. An adapter must join its output writers and
// release its owned processes before returning. Output is the masking boundary.
// Container adapters translate the paths and payload environment, not control.
type Script struct {
	Path             string
	Shell            string
	RunsOn           []string
	Workspace        string
	Directory        string
	ScriptsDirectory string
	WorkDirectory    string
	FilesDirectory   string
	Environment      []string
	// BaseEnvironment is the trusted payload base before workflow layers.
	BaseEnvironment []string
	// ActionsRoot stays open for the adapter call. The adapter must not close it.
	ActionsRoot *os.Root
	Timeout     time.Duration
	OutputLimit int64
	Output      io.Writer
}

type ScriptRunner func(context.Context, Script) ScriptResult

type RunOptions struct {
	Workspace string
	// Directory is the private actions folder beside the source workspace.
	// The caller owns the envelope and removes it after execution.
	Directory string
	Identity  RunIdentity
	Secrets   map[string]string
	Evaluator Evaluator
	RunScript ScriptRunner
	// BaseEnvironment is a complete trusted payload base. Nil uses Environment.
	BaseEnvironment []string
	// Environment builds the defined payload base using the job's temp folder.
	// An adapter supplies this when BaseEnvironment is nil.
	Environment func(temporary string) ([]string, string)
	RunnerName  string
	// RunnerOS and RunnerArch describe the payload executor, including containers.
	// Empty values use the host. RunnerOS also selects environment name semantics.
	RunnerOS    string
	RunnerArch  string
	MaxTimeout  time.Duration
	OutputLimit int64
}

// StepResult keeps unevaluated identity and display commands. DisplayName,
// output, excerpts, outputs, notes and errors are masked before return.
type StepResult struct {
	ScriptResult
	Name        string
	Command     string
	Role        string
	Index       int
	ID          string
	DisplayName string
	Excerpt     string
	Outputs     map[string]string
	Notes       []Message
}

func (step StepResult) Evidence() StepEvidence {
	return StepEvidence{Status: step.Status, Role: step.Role, CleanupError: step.CleanupError}
}

type JobResult struct {
	Steps     []StepResult
	Status    string
	Tolerated bool
	Cancelled bool
	Error     string
	Notes     []Message
}

// Evidence preserves a job-level incomplete outcome when no step was counted.
// It leaves the raw step results unchanged.
func (job JobResult) Evidence() []StepEvidence {
	facts := make([]StepEvidence, len(job.Steps))
	for index, step := range job.Steps {
		facts[index] = step.Evidence()
	}
	if job.Status == StatusIncomplete && len(facts) != 0 && AggregateAttemptStatus(facts, job.Cancelled) == StatusSkipped {
		facts[0].Status, facts[0].Role = StatusIncomplete, RoleRun
	}
	return facts
}

// RunJob executes an already admitted job. It never reads secrets or plans from
// storage, chooses a machine, or grants execution authority.
func RunJob(ctx context.Context, plan JobPlan, options RunOptions) JobResult {
	mask := newMasker(options.Secrets)
	job := JobResult{Status: StatusError}
	fail := func(err error) JobResult { job.Error = mask.text(err.Error()); return job }
	if options.Identity.ID == "" || options.Identity.Number < 1 || options.Identity.Attempt < 1 {
		return fail(fmt.Errorf("workflow.context: run identity is required"))
	}
	if len(plan.Steps) > 50 || len(options.Secrets) > 100 {
		return fail(fmt.Errorf("workflow.limit: too many steps or secrets"))
	}
	for _, value := range options.Secrets {
		if len(value) > maxEnvironmentValue || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return fail(fmt.Errorf("workflow.secrets: invalid delivered secret"))
		}
	}
	workspace, err := filepath.Abs(options.Workspace)
	if err == nil {
		workspace, err = filepath.EvalSymlinks(workspace)
	}
	if err != nil || options.Workspace == "" {
		return fail(fmt.Errorf("workflow.workspace: workspace is unavailable"))
	}
	if options.Directory == "" {
		options.Directory = filepath.Join(filepath.Dir(workspace), "actions")
	}
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return fail(err)
	}
	if relative, err := filepath.Rel(workspace, directory); err != nil || filepath.IsLocal(relative) {
		return fail(fmt.Errorf("workflow.workspace: actions folder must be outside the workspace"))
	}
	root, err := prepareActionsDirectory(directory)
	if err != nil {
		return fail(fmt.Errorf("workflow.workspace: %w", err))
	}
	defer root.Close()
	temporary := filepath.Join(directory, "work", "temp")
	osName, arch := runnerPlatform()
	if options.RunnerOS != "" {
		osName = options.RunnerOS
	}
	if options.RunnerArch != "" {
		arch = options.RunnerArch
	}
	windows := osName == "Windows"
	contexts, err := runtimeContexts(plan, options.Identity, workspace, temporary, osName, arch, options.RunnerName)
	if err != nil {
		return fail(err)
	}
	secrets := map[string]any{}
	for _, name := range plan.SecretNames {
		value := ""
		for delivered, candidate := range options.Secrets {
			if strings.EqualFold(delivered, name) {
				value = candidate
				break
			}
		}
		secrets[name] = value
		if value == "" {
			job.Notes = append(job.Notes, Message{Code: "note.secret_unset", Detail: mask.text("Secret " + name + " is not set.")})
		}
	}
	contexts["secrets"] = secrets
	job.Tolerated, err = evaluateBool(options.Evaluator, "job.continue-on-error", plan.ContinueOnError, contexts)
	if err != nil {
		return fail(err)
	}
	jobTimeout, err := evaluateTimeout(options.Evaluator, "job.timeout-minutes", plan.TimeoutMinutes, contexts, 360*time.Minute)
	if err != nil {
		return fail(err)
	}
	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	event, err := json.Marshal(plan.Context.GitHub.Event)
	if err != nil || len(event) > 64<<10 {
		return fail(fmt.Errorf("workflow.context: invalid or oversized event"))
	}
	if err := writePrivate(root, "scripts/event.json", event); err != nil {
		return fail(err)
	}
	base, environmentNote := options.BaseEnvironment, ""
	if base == nil {
		if options.Environment == nil {
			return fail(fmt.Errorf("workflow.environment: defined payload environment is required"))
		}
		base, environmentNote = options.Environment(temporary)
	}
	options.BaseEnvironment = base
	if options.MaxTimeout <= 0 {
		options.MaxTimeout = 10 * time.Minute
	}
	if options.OutputLimit <= 0 {
		options.OutputLimit = 64 << 10
	}
	if options.RunScript == nil {
		return fail(fmt.Errorf("workflow.executor: script adapter is required"))
	}
	owned := actionsEnvironment(plan, options.Identity, workspace, temporary, filepath.Join(directory, "scripts", "event.json"), osName, arch)
	updates, paths := map[string]string{}, []string{}
	for _, note := range plan.Notes {
		note.Detail, note.Path = mask.text(note.Detail), mask.text(note.Path)
		job.Notes = append(job.Notes, note)
	}
	failed, stopped := false, false
	for index, step := range plan.Steps {
		name, command := StepDisplay(step)
		result := StepResult{Index: index, ID: mask.text(step.ID), Name: mask.text(name), Command: mask.text(command), ScriptResult: ScriptResult{Status: StatusSkipped}, Role: RoleRun}
		if step.Uses != "" {
			result.Role = RoleBuiltin
		}
		if jobCtx.Err() != nil || stopped {
			job.Steps = append(job.Steps, result)
			continue
		}
		setEffectiveStepStatus(contexts, failed)
		values := environmentMap(base, windows)
		for name, value := range owned {
			setEnvironment(values, name, value, windows)
		}
		contexts["env"] = maps.Clone(values)
		err = applyEnvironment(values, plan.WorkflowEnv, options.Evaluator, "workflow.env", contexts, windows)
		if err == nil {
			contexts["env"] = maps.Clone(values)
			err = applyEnvironment(values, plan.Env, options.Evaluator, "job.env", contexts, windows)
		}
		if err == nil {
			for name, value := range updates {
				setEnvironment(values, name, value, windows)
			}
			prependPaths(values, paths, windows)
			contexts["env"] = maps.Clone(values)
			err = applyEnvironment(values, step.Env, options.Evaluator, "step.env", contexts, windows)
		}
		contexts["env"] = maps.Clone(values)
		var run bool
		if err == nil {
			run, err = stepCondition(step.If, !failed, options.Evaluator, contexts)
		}
		if err == nil && run {
			err = executeStep(jobCtx, root, plan, step, options, values, contexts, &result, mask)
		}
		if err != nil {
			result.Status = StatusError
			if result.Role == RoleBuiltin {
				result.Role = RoleRun
			}
			result.Output += mask.text(err.Error())
		} else if changes, ok := contexts["step_changes"].(commandChanges); ok {
			for name, value := range changes.env {
				setEnvironment(updates, name, value, windows)
			}
			paths = append(paths, changes.paths...)
		}
		delete(contexts, "step_changes")
		retained := checkoutput.LogBuffer{Limit: maximumStepOutput}
		if environmentNote != "" && (result.Status == StatusFailed || result.Status == StatusError || result.Status == StatusUnavailable) {
			retained.Add(mask.text(environmentNote))
		}
		retained.AddClipped(result.Output, result.OutputGap)
		result.Output, result.OutputGap, _ = retained.ResultWithGap()
		if jobCtx.Err() != nil {
			stopped = true
			if ctx.Err() == nil && result.Status == StatusCancelled {
				result.Status = StatusIncomplete
			}
		}
		if mask.stopped || result.CleanupError != "" {
			stopped = true
			result.Status, result.Role = StatusError, RoleRun
		}
		result.CleanupError = mask.text(result.CleanupError)
		result.Excerpt, _ = checkoutput.ClipLog(result.Output, 8<<10, result.OutputGap)
		if StepConclusion(result.Evidence()) == ResultFailure {
			failed = true
		}
		if step.ID != "" {
			outputs := result.Outputs
			if raw, ok := contexts["step_outputs"].(map[string]string); ok && run && err == nil {
				outputs = raw
			}
			contexts["steps"].(map[string]any)[step.ID] = map[string]any{"outputs": outputs, "outcome": NeedsResult(result.Status, false), "conclusion": StepConclusion(result.Evidence())}
		}
		delete(contexts, "step_outputs")
		job.Steps = append(job.Steps, result)
	}
	facts := make([]StepEvidence, len(job.Steps))
	for index := range job.Steps {
		step := &job.Steps[index]
		step.maskMetadata(mask)
		facts[index] = step.Evidence()
	}
	job.Cancelled = ctx.Err() != nil
	job.Status = AggregateAttemptStatus(facts, job.Cancelled)
	if jobCtx.Err() != nil || stopped {
		job.Notes = append(job.Notes, Message{Code: "note.stopped", Detail: "OwnGit stopped this job. Later steps, including always() steps, did not run."})
		if !job.Cancelled && jobCtx.Err() != nil && job.Status == StatusSkipped {
			job.Status = StatusIncomplete
		}
	}
	for index := range job.Notes {
		job.Notes[index].Detail, job.Notes[index].Path = mask.text(job.Notes[index].Detail), mask.text(job.Notes[index].Path)
	}
	return job
}

func (step *StepResult) maskMetadata(mask *masker) {
	step.Name, step.Command = mask.text(step.Name), mask.text(step.Command)
	step.ID, step.DisplayName = mask.text(step.ID), mask.text(step.DisplayName)
	maskedOutputs := make(map[string]string, len(step.Outputs))
	for name, value := range step.Outputs {
		maskedOutputs[mask.text(name)] = mask.text(value)
	}
	step.Outputs = maskedOutputs
	for index := range step.Notes {
		step.Notes[index].Detail = mask.text(step.Notes[index].Detail)
		step.Notes[index].Path = mask.text(step.Notes[index].Path)
	}
}

func setEffectiveStepStatus(contexts map[string]any, failed bool) {
	status := ResultSuccess
	if failed {
		status = ResultFailure
	}
	contexts["job"] = map[string]any{"status": status}
	contexts["success"], contexts["failure"], contexts["cancelled"] = !failed, failed, false
}

func executeStep(ctx context.Context, root *os.Root, plan JobPlan, step Step, options RunOptions, values map[string]string, contexts map[string]any, result *StepResult, mask *masker) error {
	tolerated, err := evaluateBool(options.Evaluator, "step.continue-on-error", step.ContinueOnError, contexts)
	if err != nil {
		return err
	}
	if tolerated && step.Uses == "" {
		result.Role = RoleTolerated
	}
	name, err := evaluateText(options.Evaluator, "step.name", step.Name, contexts)
	if err != nil {
		return err
	}
	result.DisplayName = mask.text(name)
	if step.Uses != "" {
		return runBuiltin(step, options.Evaluator, contexts, result, mask)
	}
	files, err := prepareCommandFiles(root, result.Index+1)
	if err != nil {
		return err
	}
	defer files.root.Close()
	windows := contexts["runner"].(map[string]any)["os"] == "Windows"
	for name, value := range files.paths {
		setEnvironment(values, name, value, windows)
	}
	contexts["env"] = maps.Clone(values)
	environment, err := environmentEntries(values, windows)
	if err != nil {
		return err
	}
	code, err := evaluateText(options.Evaluator, "step.run", step.Run, contexts)
	if err != nil {
		return err
	}
	shell := step.Shell
	if shell == "" {
		shell = plan.Defaults.Shell
		if shell == "" {
			shell = plan.WorkflowDefaults.Shell
		}
		shell, err = evaluateText(options.Evaluator, "job.defaults.run.shell", shell, contexts)
	}
	working := step.WorkingDirectory
	workingKey := "step.working-directory"
	if working == "" {
		working, workingKey = plan.Defaults.WorkingDirectory, "job.defaults.run.working-directory"
		if working == "" {
			working = plan.WorkflowDefaults.WorkingDirectory
		}
	}
	if err != nil {
		return err
	}
	working, err = evaluateText(options.Evaluator, workingKey, working, contexts)
	if err != nil {
		return err
	}
	workspace := contexts["github"].(map[string]any)["workspace"].(string)
	directory, err := workingDirectory(workspace, working)
	if err != nil {
		return err
	}
	timeout, err := evaluateTimeout(options.Evaluator, "step.timeout-minutes", step.TimeoutMinutes, contexts, options.MaxTimeout)
	if err != nil {
		return err
	}
	timeout = min(timeout, options.MaxTimeout)
	scriptName := filepath.Join("scripts", fmt.Sprintf("step-%d.script", result.Index+1))
	if err := writePrivate(root, scriptName, []byte(code)); err != nil {
		return err
	}
	stream := newCommandMaskingWriter(mask)
	native := options.RunScript(ctx, Script{Path: filepath.Join(root.Name(), scriptName), Shell: shell, RunsOn: plan.RunsOn,
		Workspace: workspace, Directory: directory, ScriptsDirectory: filepath.Join(root.Name(), "scripts"), WorkDirectory: filepath.Join(root.Name(), "work"),
		FilesDirectory: files.root.Name(), Environment: environment, BaseEnvironment: options.BaseEnvironment, ActionsRoot: root,
		Timeout: timeout, OutputLimit: options.OutputLimit, Output: stream})
	output, gap, streamErr := stream.finish()
	result.Status, result.ExitCode, result.Duration, result.Truncated, result.CleanupError = native.Status, native.ExitCode, native.Duration, native.Truncated, native.CleanupError
	log := checkoutput.LogBuffer{Limit: maximumStepOutput}
	log.AddClipped(output, gap)
	log.Add(mask.text(native.Output))
	result.Output, result.OutputGap, _ = log.ResultWithGap()
	if streamErr != nil {
		return streamErr
	}
	if result.Status == StatusIncomplete && !result.Truncated && result.CleanupError == "" {
		detail := "Step reached timeout-minutes. Raise the step's timeout-minutes if more time is needed."
		if timeout == options.MaxTimeout {
			detail = fmt.Sprintf("Step reached max_timeout_ms (%d ms). Raise it under Automatic checks, Execution limits (suggested: 600000 for 10 minutes).", options.MaxTimeout.Milliseconds())
		}
		result.Notes = append(result.Notes, Message{Code: "workflow.step_timeout", Detail: detail})
	}
	changes, err := files.read(windows)
	if err != nil {
		return err
	}
	contexts["step_changes"] = changes
	contexts["step_outputs"] = changes.outputs
	result.Outputs = map[string]string{}
	for name, value := range changes.outputs {
		result.Outputs[mask.text(name)] = mask.text(value)
	}
	for _, note := range changes.notes {
		note.Detail = mask.text(note.Detail)
		result.Notes = append(result.Notes, note)
	}
	return nil
}

func prepareActionsDirectory(directory string) (*os.Root, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("actions folder must be a directory, not a link")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"scripts", "work", "files", filepath.Join("work", "home"), filepath.Join("work", "temp"), filepath.Join("work", "cache"), filepath.Join("work", "cache", "go-build")} {
		if err := root.Mkdir(name, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

func writePrivate(root *os.Root, name string, value []byte) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(value)
	return errors.Join(writeErr, file.Close())
}

func workingDirectory(workspace, relative string) (string, error) {
	if relative == "" {
		relative = "."
	}
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("workflow.working_directory: %s must stay inside the workspace", relative)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer root.Close()
	directory, err := root.OpenRoot(relative)
	if err != nil {
		return "", fmt.Errorf("workflow.working_directory: %w", err)
	}
	defer directory.Close()
	return filepath.Join(workspace, relative), nil
}
