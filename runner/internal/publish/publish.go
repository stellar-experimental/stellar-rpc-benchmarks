// Package publish safeguards a finished benchmark campaign bundle to object
// storage. It uploads a campaign results directory to <dest-root>/<run_id>/,
// where run_id is the bundle's basename (the same run_id recorded in the
// bundle's metadata.json). The uploader is idempotent, but published runs are
// immutable: it refuses to write into a destination that already holds objects
// unless Force is given.
//
// It is the port of runner/publish.sh, and a leaf package on purpose: preflight
// borrows List to prove, before a campaign starts, that the credentials it will
// need seventeen hours from now exist today.
package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// listTimeout bounds a destination listing. An unreachable endpoint should cost
// seconds, not the TCP stack's idea of patience.
const listTimeout = 30 * time.Second

// metadataName is the bundle manifest whose absence marks a pre-manifest
// bundle. The file itself is internal/bundle's contract; publish only looks.
const metadataName = "metadata.json"

// State is what a destination listing found.
type State int

const (
	Empty      State = iota // the prefix holds no objects
	HasObjects              // the prefix holds at least one object
)

// ListError is a listing that failed for a reason other than emptiness: bad
// credentials, no network, no such bucket. It carries the tool's exit status
// and the first line of its complaint, which is what an operator needs.
type ListError struct {
	ExitCode int
	Detail   string
}

func (e *ListError) Error() string { return fmt.Sprintf("exit %d: %s", e.ExitCode, e.Detail) }

// tool is one object-storage CLI: how it lists a prefix, how it syncs a
// directory into one, and how it says "no objects here".
type tool struct {
	name string
	ls   func(uri string) []string
	sync func(dir, dest string) []string
	// empty reports whether a nonzero exit was this tool's way of saying the
	// prefix holds no objects.
	empty func(stdout, stderr string) bool
}

// toolFor dispatches on the URI's scheme. gs:// uploads with `gcloud storage
// rsync -r`, s3:// with `aws s3 sync`. No other scheme is supported.
func toolFor(uri string) (tool, error) {
	switch {
	case strings.HasPrefix(uri, "gs://"):
		return tool{
			name: "gcloud",
			ls:   func(uri string) []string { return []string{"gcloud", "storage", "ls", uri} },
			sync: func(dir, dest string) []string {
				return []string{"gcloud", "storage", "rsync", "-r", dir, dest}
			},
			empty: func(_, stderr string) bool { return strings.Contains(stderr, "matched no objects") },
		}, nil
	case strings.HasPrefix(uri, "s3://"):
		return tool{
			name:  "aws",
			ls:    func(uri string) []string { return []string{"aws", "s3", "ls", uri} },
			sync:  func(dir, dest string) []string { return []string{"aws", "s3", "sync", dir, dest} },
			empty: func(stdout, stderr string) bool { return stdout == "" && stderr == "" },
		}, nil
	}
	return tool{}, fmt.Errorf("unsupported scheme (supported: gs://, s3://)")
}

// List reports whether uri already holds objects.
//
// Both CLIs report an empty prefix through a nonzero exit, but each has its own
// signature: aws s3 ls says nothing at all, gcloud storage ls says the URL
// matched no objects. runner/publish.sh accepts either signature from either
// CLI, out of shell expedience; here each tool is held to its own signature
// only, because the false-pass direction is the dangerous one — reading a
// credential failure as "empty prefix" would skip the immutability check that
// exists to keep a published run immutable, and would pass a preflight
// credential check that never actually ran.
func List(uri string) (State, error) {
	t, err := toolFor(uri)
	if err != nil {
		return Empty, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	argv := t.ls(uri)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	out, errOut := strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())

	if runErr == nil {
		if out == "" {
			return Empty, nil
		}
		return HasObjects, nil
	}
	if ctx.Err() != nil {
		return Empty, fmt.Errorf("%s took longer than %s", t.name, listTimeout)
	}
	// The empty-prefix signature only means anything when the CLI actually ran
	// and exited on its own: a binary that never started, or one killed by a
	// signal, is silent for reasons that are not emptiness.
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return Empty, &ListError{ExitCode: -1, Detail: runErr.Error()}
	}
	if exitErr.Exited() && t.empty(out, errOut) {
		return Empty, nil
	}
	detail := diagnosis(errOut)
	if detail == "" {
		detail = diagnosis(out)
	}
	if detail == "" {
		detail = "no output"
	}
	return Empty, &ListError{ExitCode: exitCode(runErr), Detail: detail}
}

// Run publishes resultsDir to <destRoot>/<run_id>/, where run_id is the
// bundle's basename.
//
// force skips the immutability check, and a forced publish is a MERGE, not a
// replace: the sync writes every object the bundle has, but objects already at
// the destination that this bundle does not contain survive it. Re-publishing a
// differently-shaped bundle over an old one therefore leaves the old one's
// extra files in place, and the destination is the union of the two.
//
// dryRun prints every cloud command and executes none of them. On success the
// final line written to out is `published: <dest>` (machine-greppable; scripts
// depend on it).
func Run(resultsDir, destRoot string, dryRun, force bool, out io.Writer) error {
	resultsDir = strings.TrimSuffix(resultsDir, "/")
	if fi, err := os.Stat(resultsDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("results dir not found: %s", resultsDir)
	}
	if destRoot == "" {
		return errors.New("no destination: pass <dest-root-uri> or set PUBLISH_URI")
	}

	runID := filepath.Base(resultsDir)
	if _, err := os.Stat(filepath.Join(resultsDir, metadataName)); err != nil {
		notef(out, "warning: %s/%s missing — pre-manifest bundle", resultsDir, metadataName)
	}
	dest := strings.TrimSuffix(destRoot, "/") + "/" + runID + "/"

	t, err := toolFor(dest)
	if err != nil {
		return fmt.Errorf("unsupported destination scheme: %s (supported: gs://, s3://)", dest)
	}

	// Published runs are immutable: a destination that already holds objects is
	// only written to with force. The listing command is printed even under a
	// dry run — it is part of what a real publish would do.
	if !force {
		printCmd(out, t.ls(dest))
		if !dryRun {
			state, err := List(dest)
			if err != nil {
				var le *ListError
				if errors.As(err, &le) {
					return fmt.Errorf("cannot list destination %s (exit %d): %s", dest, le.ExitCode, le.Detail)
				}
				return fmt.Errorf("cannot list destination %s: %s", dest, err)
			}
			if state == HasObjects {
				return fmt.Errorf("destination already has objects: %s — published runs are immutable; pass --force to overwrite", dest)
			}
		}
	}

	notef(out, "publish %s → %s", runID, dest)
	argv := t.sync(resultsDir, dest)
	printCmd(out, argv)
	if dryRun {
		notef(out, "dry run complete")
		return nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	fmt.Fprintf(out, "published: %s\n", dest)
	return nil
}

// diagnosis reduces a CLI's output stream to its first non-empty line. Both
// tools put the actual error on stderr ("ERROR: (gcloud.storage.ls) …", "fatal
// error: Unable to locate credentials") and follow it with several lines of
// remediation prose, which would bury the failure in the report.
func diagnosis(stream string) string {
	for _, line := range strings.Split(stream, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// exitCode is the tool's status, or -1 when it never got far enough to have one
// (binary missing, permission denied, signal).
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// printCmd echoes a command the way bash's run() and the plan printer do.
func printCmd(w io.Writer, argv []string) {
	fmt.Fprintf(w, "  $ %s\n", strings.Join(argv, " "))
}

// notef is run.Notef's line format, duplicated rather than imported: preflight
// imports this package, and a leaf package keeps that dependency honest.
func notef(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "== [%s] %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}
