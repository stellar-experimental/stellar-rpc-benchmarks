package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// LegSchemaVersion is the version of the leg.json contract. Like plan.json,
// additive changes keep the version.
const LegSchemaVersion = 1

// Status is what became of one step.
type Status string

const (
	StatusOK      Status = "ok"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped" // a need (transitively) failed
	StatusResumed Status = "resumed" // already complete in an earlier session
)

// StepResult is one line of the campaign's outcome.
type StepResult struct {
	ID     string
	Status Status
	Err    error // nil unless failed
}

// Options configures a walk.
type Options struct {
	// Output receives the runner's notes, the command lines, and the child
	// processes' stdout and stderr. The caller composes the tee (terminal plus
	// campaign.log); the executor just writes. Defaults to os.Stdout, which is
	// where bash sent everything.
	Output io.Writer
	// Resume inspects each leg's existing --out directory and skips the ones an
	// earlier session finished. Off, nothing existing is read or wiped by the
	// resume path.
	Resume bool
	// FailFast stops the walk at the first failed step. Default (keep going): a
	// failure only skips the steps that need it.
	FailFast bool
	// OnStepDone is called after every step the walk executed — including the
	// ones a resume or an existing binary made a no-op, excluding only the ones
	// skipped because a need failed. The run wiring writes binary.txt from it
	// the moment the build succeeds, so a campaign that dies during its legs
	// still leaves the binary's identity in the bundle.
	OnStepDone func(s plan.Step, res StepResult)
}

// legSentinel is leg.json: the runner's own record that a leg ran to
// completion, written whether it succeeded or not. The bench subcommands'
// invocation.json cannot play this role — it is written by the process being
// measured, so a process killed before it got there leaves no trace at all.
type legSentinel struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Argv          []string `json:"argv"`
	ExitCode      int      `json:"exit_code"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	DurationNS    int64    `json:"duration_ns"`
	Error         string   `json:"error,omitempty"`
}

// Execute walks the plan in order — sequentially, always: these are benchmarks,
// and two of them sharing the machine measure the sharing. It returns one
// result per executed step and a non-nil error when any step failed; the
// failure summary has already been printed to Output, so a caller that exits
// nonzero needs to print nothing more.
//
// Under FailFast the walk stops at the first failure, so the returned slice is
// short: steps after the failed one have no result at all, rather than a
// skipped one.
func Execute(p *plan.Plan, opts Options) ([]StepResult, error) {
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	results := make([]StepResult, 0, len(p.Steps))
	// bad holds every step that failed or was skipped. Skipped steps being bad
	// is what makes the propagation transitive: the dependent of a skipped step
	// is skipped too, without walking the graph.
	bad := map[string]bool{}
	skipNeed := map[string]string{}

	for _, step := range p.Steps {
		// The tarball and the publish are the wiring's epilogue, not the walk's:
		// tar must run after the final provenance writes so the bundle it
		// preserves contains them, and a publish failure is not a benchmark
		// failure. They are in the plan because they are part of the campaign;
		// they are skipped here because they belong after every step of it.
		if step.Kind == plan.KindTarball || step.Kind == plan.KindPublish {
			continue
		}
		if need := firstBadNeed(step, bad); need != "" {
			Notef(opts.Output, "skipping %s: needs %s, which failed or was skipped", step.ID, need)
			bad[step.ID] = true
			skipNeed[step.ID] = need
			results = append(results, StepResult{ID: step.ID, Status: StatusSkipped})
			continue
		}
		Notef(opts.Output, "%s", step.ID)
		res := executeStep(p, step, opts)
		results = append(results, res)
		if opts.OnStepDone != nil {
			opts.OnStepDone(step, res)
		}
		if res.Status == StatusFailed {
			Notef(opts.Output, "%s failed: %v", step.ID, res.Err)
			bad[step.ID] = true
			if opts.FailFast {
				break
			}
		}
	}
	return results, summarize(results, skipNeed, opts.Output)
}

// firstBadNeed returns the first of a step's needs that failed or was skipped,
// or "" when the step is clear to run.
func firstBadNeed(s plan.Step, bad map[string]bool) string {
	for _, need := range s.Needs {
		if bad[need] {
			return need
		}
	}
	return ""
}

// summarize prints the end-of-campaign failure block and returns the error
// Execute hands back. An all-ok campaign prints nothing and returns nil.
func summarize(results []StepResult, skipNeed map[string]string, w io.Writer) error {
	var failed, skipped []StepResult
	for _, r := range results {
		switch r.Status {
		case StatusFailed:
			failed = append(failed, r)
		case StatusSkipped:
			skipped = append(skipped, r)
		}
	}
	if len(failed) == 0 && len(skipped) == 0 {
		return nil
	}
	fmt.Fprintf(w, "== campaign summary: %d failed, %d skipped\n", len(failed), len(skipped))
	for _, r := range failed {
		fmt.Fprintf(w, "==   failed:  %s (%v)\n", r.ID, r.Err)
	}
	for _, r := range skipped {
		fmt.Fprintf(w, "==   skipped: %s (needs %s)\n", r.ID, skipNeed[r.ID])
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d step(s) failed", len(failed))
}

func executeStep(p *plan.Plan, s plan.Step, opts Options) StepResult {
	switch s.Kind {
	case plan.KindLeg:
		return runLeg(s, opts)
	case plan.KindBuild:
		return runBuild(p, s, opts)
	case plan.KindDataset:
		return runDataset(s, opts)
	default:
		return failure(s, fmt.Errorf("unknown step kind %q", s.Kind))
	}
}

// runLeg runs one timed benchmark leg: the whole point of the campaign, and the
// only step kind with a completion sentinel.
func runLeg(s plan.Step, opts Options) StepResult {
	if len(s.Argv) != 1 {
		return failure(s, fmt.Errorf("leg has %d commands, want exactly 1 (the measurement is the process)", len(s.Argv)))
	}
	if opts.Resume {
		base := filepath.Base(s.OutDir)
		state := classifyLegDir(s.OutDir)
		switch state.kind {
		case legComplete:
			Notef(opts.Output, "resume: %s already complete — skipping", base)
			return StepResult{ID: s.ID, Status: StatusResumed}
		case legFailedEarlier:
			Notef(opts.Output, "resume: %s failed in an earlier session (%s) — wiping and re-running", base, state.reason)
		case legPartial:
			Notef(opts.Output, "resume: %s is a partial leg — wiping and re-running", base)
		}
		if state.kind != legAbsent {
			if err := removeAll(opts.Output, s.OutDir); err != nil {
				return failure(s, err)
			}
		}
	}
	for _, dir := range s.PreClean {
		if err := removeAll(opts.Output, dir); err != nil {
			return failure(s, err)
		}
	}

	// The bench subcommand creates its own --out dir, but creating it here too
	// means the sentinel has somewhere to land even when the binary dies
	// instantly — which is exactly the case resume most needs to classify.
	started := time.Now()
	runErr := os.MkdirAll(s.OutDir, 0o755)
	if runErr == nil {
		runErr = runCommand(s.Argv[0], s.Env, opts.Output)
	}
	finished := time.Now()

	if err := writeLegSentinel(s, started, finished, runErr); err != nil {
		if runErr == nil {
			// A leg whose completion cannot be recorded is not complete: a
			// resume would re-run it anyway, so call it failed now.
			runErr = err
		} else {
			Notef(opts.Output, "warning: %s: %v", s.ID, err)
		}
	}
	if runErr != nil {
		return failure(s, runErr)
	}
	// Post-cleaning only on success keeps a failed leg's scratch around for
	// diagnosis. A failure to clean is not a failure of the measurement.
	for _, dir := range s.PostClean {
		if err := removeAll(opts.Output, dir); err != nil {
			Notef(opts.Output, "warning: %s: %v", s.ID, err)
		}
	}
	return StepResult{ID: s.ID, Status: StatusOK}
}

// writeLegSentinel records how the leg went, success or failure, into its --out
// directory.
func writeLegSentinel(s plan.Step, started, finished time.Time, runErr error) error {
	sentinel := legSentinel{
		SchemaVersion: LegSchemaVersion,
		ID:            s.ID,
		Argv:          s.Argv[0],
		StartedAt:     started.UTC().Format(time.RFC3339),
		FinishedAt:    finished.UTC().Format(time.RFC3339),
		DurationNS:    finished.Sub(started).Nanoseconds(),
	}
	if runErr != nil {
		sentinel.ExitCode = exitCode(runErr)
		sentinel.Error = runErr.Error()
	}
	b, err := json.MarshalIndent(sentinel, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", legSentinelName, err)
	}
	if err := os.WriteFile(filepath.Join(s.OutDir, legSentinelName), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", legSentinelName, err)
	}
	return nil
}

// exitCode is the child's status, or -1 when it never got far enough to have
// one (binary missing, permission denied, signal).
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// runBuild builds the binary under test unless it is already there. The binary
// at its versioned path is its own completion marker — the path contains the
// commit's short sha, so a stale binary cannot be mistaken for this one.
func runBuild(p *plan.Plan, s plan.Step, opts Options) StepResult {
	if executableExists(p.Bin) {
		Notef(opts.Output, "binary %s already built — skipping build", p.Bin)
		return StepResult{ID: s.ID, Status: StatusOK}
	}
	return runCommands(s, opts)
}

// executableExists reports whether path is a regular file anyone may execute.
func executableExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// runCommands runs a step's commands in order, stopping at the first failure.
// They belong to one step precisely because stopping between them would leave
// something half-made.
func runCommands(s plan.Step, opts Options) StepResult {
	for _, argv := range s.Argv {
		if err := runCommand(argv, s.Env, opts.Output); err != nil {
			return failure(s, err)
		}
	}
	return StepResult{ID: s.ID, Status: StatusOK}
}

// RunStep runs one step's commands outside the walk, printed and plumbed
// exactly as Execute would. It exists for the steps Execute deliberately leaves
// to the wiring's epilogue — the tarball, which must be made after the final
// provenance writes.
func RunStep(s plan.Step, out io.Writer) error {
	if res := runCommands(s, Options{Output: out}); res.Status != StatusOK {
		return res.Err
	}
	return nil
}

// runCommand prints the command the way the plan printer and bash's run() do,
// then executes it with its output going wherever the notes go.
func runCommand(argv []string, env map[string]string, out io.Writer) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	fmt.Fprintf(out, "  $ %s%s\n", envPrefix(env), strings.Join(argv, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = os.Environ()
	for _, k := range sortedKeys(env) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	return cmd.Run()
}

// removeAll wipes directories, logging them as the single rm -rf bash ran.
func removeAll(out io.Writer, dirs ...string) error {
	fmt.Fprintf(out, "  $ rm -rf %s\n", strings.Join(dirs, " "))
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("rm -rf %s: %w", dir, err)
		}
	}
	return nil
}

// envPrefix renders a step's extra environment as an `env K=V ` command prefix,
// matching plan.Plan.Print so a log line and a plan line for the same command
// read identically.
func envPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("env ")
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&b, "%s=%s ", k, env[k])
	}
	return b.String()
}

func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func failure(s plan.Step, err error) StepResult {
	return StepResult{ID: s.ID, Status: StatusFailed, Err: err}
}
