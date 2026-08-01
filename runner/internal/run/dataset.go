package run

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// runDataset converges one dataset on a local cold pack root, whatever kind it
// is: the legs downstream all read <root>/ledgers and neither know nor care
// whether it was fetched, backfilled, generated, or already there.
//
// Everything a kind materializes lands in <root>.partial and is renamed onto
// <root> only once whole, so an interrupted preparation can never be mistaken
// for a finished one — the golden-present check below is exactly that
// distinction, and `rm -rf <root>` is the documented lever to force a re-fetch.
func runDataset(s plan.Step, opts Options) StepResult {
	if s.Dataset == nil {
		return failure(s, errors.New("dataset step has no dataset spec"))
	}
	if err := prepareDataset(s, opts.Output); err != nil {
		return failure(s, err)
	}
	return StepResult{ID: s.ID, Status: StatusOK}
}

func prepareDataset(s plan.Step, out io.Writer) error {
	d := s.Dataset
	partial := d.Root + ".partial"

	switch d.Kind {
	case config.KindPacksLocal:
		Notef(out, "dataset %s: local cold pack root %s", d.Name, d.Root)
		if !dirExists(filepath.Join(d.Root, "ledgers")) {
			return fmt.Errorf("dataset '%s': %s/ledgers not found — location must be a cold pack root", d.Name, d.Root)
		}

	case config.KindPacksGS:
		if goldenPresent(d.Root) {
			Notef(out, "dataset %s: golden packs already at %s — skipping fetch", d.Name, d.Root)
			break
		}
		Notef(out, "dataset %s: fetch %s", d.Name, d.Location)
		// golden_present was false, so root is absent or an empty leftover, and
		// the plan's pre_clean says to clear it — but not the partial, which
		// rsync resumes into.
		if err := preClean(s, out); err != nil {
			return err
		}
		if err := mkdirAll(out, partial); err != nil {
			return err
		}
		if err := runDatasetCommands(s, out); err != nil {
			return err
		}
		if err := rename(out, partial, d.Root); err != nil {
			return err
		}

	case config.KindBSBS3:
		if goldenPresent(d.Root) {
			Notef(out, "dataset %s: golden packs already at %s — skipping backfill", d.Name, d.Root)
			break
		}
		Notef(out, "dataset %s: golden backfill of %s from S3 (untimed)", d.Name, d.Location)
		if err := preClean(s, out); err != nil {
			return err
		}
		// One untimed cold backfill per chunk. The step's env carries
		// AWS_EC2_METADATA_DISABLED=true: without it the SDK signs requests
		// with the machine's IAM role and the public bucket 403s, but setting
		// it for the whole campaign would also hide those same instance-role
		// credentials from the publish step's `aws s3` calls.
		if err := runDatasetCommands(s, out); err != nil {
			return err
		}
		if err := rename(out, partial, d.Root); err != nil {
			return err
		}

	case config.KindFixture:
		if goldenPresent(d.Root) {
			Notef(out, "dataset %s: golden packs already at %s — skipping generation", d.Name, d.Root)
			break
		}
		if d.Stage == "" {
			// The generation commands write into the staging tree, and the
			// plan's pre_clean is derived from it; a spec without one describes
			// a preparation nobody can reason about.
			return fmt.Errorf("dataset '%s': fixture step has no staging pack dir", d.Name)
		}
		Notef(out, "dataset %s: generate a fixture pack tree", d.Name)
		if err := preClean(s, out); err != nil {
			return err
		}
		// Generate every chunk into the staging pack tree, then freeze every
		// chunk into the golden packs — both untimed.
		if err := runDatasetCommands(s, out); err != nil {
			return err
		}
		if err := rename(out, partial, d.Root); err != nil {
			return err
		}

	default:
		return fmt.Errorf("dataset '%s': unknown kind %q", d.Name, d.Kind)
	}

	if !dirExists(filepath.Join(d.Root, "ledgers")) {
		return fmt.Errorf("dataset '%s': %s/ledgers missing after preparation", d.Name, d.Root)
	}
	return nil
}

// preClean wipes what the plan says to wipe before a dataset materializes.
// Which directories a kind clears — and, for packs-gs, which it deliberately
// keeps — is a property of the kind, so the plan owns the list and a dry run
// prints exactly the wipes the run performs.
func preClean(s plan.Step, out io.Writer) error {
	if len(s.PreClean) == 0 {
		return nil
	}
	return removeAll(out, s.PreClean...)
}

// runDatasetCommands runs the step's commands in order, stopping at the first
// failure: what they build together is one pack tree, and half of one is worth
// nothing.
func runDatasetCommands(s plan.Step, out io.Writer) error {
	for _, argv := range s.Argv {
		if err := runCommand(argv, s.Env, out); err != nil {
			return err
		}
	}
	return nil
}

// goldenPresent reports whether a pack root is there and non-empty, the port of
// bash's golden_present.
func goldenPresent(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// mkdirAll and rename log themselves as the commands bash ran, so a campaign
// log shows every filesystem move the runner made, not just the ones that
// happened to be external processes.
func mkdirAll(out io.Writer, dir string) error {
	fmt.Fprintf(out, "  $ mkdir -p %s\n", dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir -p %s: %w", dir, err)
	}
	return nil
}

func rename(out io.Writer, from, to string) error {
	fmt.Fprintf(out, "  $ mv %s %s\n", from, to)
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("mv %s %s: %w", from, to, err)
	}
	return nil
}
