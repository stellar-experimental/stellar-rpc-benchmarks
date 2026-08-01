// Package bundle owns the bundle root's manifest: reading metadata.json, the
// status vocabulary it carries, and the checks a --resume must pass before any
// step of a resumed campaign runs.
//
// metadata.json is a cross-repo contract with converter/convert.py — see
// SCHEMA.md § Inputs and runner/README.md § Campaign bundle layout. Only the
// fields the runner itself consumes are modelled here; the rest are carried
// through untouched by readers.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// Status values metadata.json carries in its (additive) status field.
// "running" is written up front; the end-of-campaign rewrite makes it
// "finished", or "failed" when any leg failed or the campaign aborted.
// Bash-era bundles have no status; readers treat absent as unknown.
const (
	StatusRunning  = "running"
	StatusFinished = "finished"
	StatusFailed   = "failed"
)

// MetadataName is the bundle-root manifest's filename.
const MetadataName = "metadata.json"

// Metadata is the read-side of the bundle manifest — only the fields the
// runner consumes; the converter reads more.
type Metadata struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	Campaign      struct {
		Name        string `json:"name"`
		ConfigFile  string `json:"config_file"`
		Ref         string `json:"ref"`
		BuiltCommit string `json:"built_commit"`
	} `json:"campaign"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Status     string `json:"status"`
}

// ReadMetadata reads and parses bundleDir/metadata.json. Fields the runner does
// not model (datasets, hardware, and the rest of the campaign block) are
// ignored, not an error: this reader must keep working on bundles written by a
// newer writer, and on the bash runner's.
func ReadMetadata(bundleDir string) (*Metadata, error) {
	path := filepath.Join(bundleDir, MetadataName)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bundle: read %s: %w", path, err)
	}
	var m Metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("bundle: parse %s: %w", path, err)
	}
	return &m, nil
}

// Resume is what a validated --resume hands the run wiring.
type Resume struct {
	Dir       string
	RunID     string
	Sha8      string // parsed from RunID's fixed tail
	Stamp     string // parsed from RunID's fixed tail
	StartedAt string // recovered; "" for pre-crash-safe bundles (caller
	// records this session's start, like bash)
}

// reRunID matches a run id. NAME may contain '-', so the sha and stamp are
// matched as the fixed tail.
var reRunID = regexp.MustCompile(`^(.+)-([0-9a-f]{8})-([0-9]{8}T[0-9]{6}Z)$`)

// ValidateResume refuses to resume dir unless it is provably the same
// campaign: same name, same binary, byte-identical config. Identity comes from
// the bundle's metadata.json, not from parsing the directory basename, so a
// renamed or copied bundle cannot pass itself off as another campaign.
// diffOut receives the unified diff when the config guard fires.
//
// Because every one of these checks runs before the first step, a resumed
// campaign never needs to re-copy the config into the bundle: the stored copy
// is already known to be the config being run. The run wiring copies it only
// when creating a fresh bundle.
func ValidateResume(dir, cfgPath string, cfg *config.Config,
	builtCommit, benchRoot string, diffOut io.Writer) (*Resume, error) {

	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("--resume: cannot read '%s': %w", dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("--resume: '%s' is not a directory", dir)
	}

	meta, err := ReadMetadata(dir)
	if err != nil {
		return nil, fmt.Errorf("--resume: no readable %s in %s — this bundle predates "+
			"crash-safe metadata (or is not a campaign bundle); resume it with the bash "+
			"runner or start a fresh campaign", MetadataName, dir)
	}

	m := reRunID.FindStringSubmatch(meta.RunID)
	if m == nil {
		return nil, fmt.Errorf("--resume: '%s' is not a <NAME>-<sha>-<stamp> run id", meta.RunID)
	}
	sha8, stamp := m[2], m[3]

	if meta.Campaign.Name != cfg.Name {
		return nil, fmt.Errorf("--resume: '%s' belongs to campaign '%s', but this config's name is '%s'",
			meta.RunID, meta.Campaign.Name, cfg.Name)
	}
	if meta.Campaign.BuiltCommit != builtCommit {
		return nil, fmt.Errorf("--resume: '%s' was benchmarked with commit %s, but ref '%s' now resolves "+
			"to %s — resuming would mix two binaries in one bundle; check out the same ref or "+
			"start a fresh campaign", meta.RunID, meta.Campaign.BuiltCommit, cfg.Ref, builtCommit)
	}
	if expected := filepath.Join(benchRoot, "results", meta.RunID); filepath.Clean(dir) != filepath.Clean(expected) {
		return nil, fmt.Errorf("--resume: '%s' is not this BENCH_ROOT's results directory (expected %s) — "+
			"set BENCH_ROOT to the original campaign's root", dir, expected)
	}

	if err := checkStoredConfig(dir, cfgPath, meta, diffOut); err != nil {
		return nil, err
	}

	// An empty started_at is not an error: bundles written before metadata.json
	// was written up front have none, and the caller records this session's
	// start instead, as bash did.
	return &Resume{Dir: dir, RunID: meta.RunID, Sha8: sha8, Stamp: stamp, StartedAt: meta.StartedAt}, nil
}

// checkStoredConfig is the config-diff guard: it proves the config about to be
// run is byte-identical to the one this campaign started with. Bash checked
// nothing here and overwrote the bundle's stored copy with the current config,
// so a resume with edited knobs produced mixed data under a manifest that
// uniformly claimed the new knobs.
func checkStoredConfig(dir, cfgPath string, meta *Metadata, diffOut io.Writer) error {
	if meta.Campaign.ConfigFile == "" {
		return fmt.Errorf("--resume: '%s' records no config_file — cannot verify what this campaign "+
			"was started with; start a fresh campaign", meta.RunID)
	}
	// metadata.json travels with the bundle and may be hand-edited; the guard
	// must only ever compare against the copy stored in the bundle root.
	if meta.Campaign.ConfigFile != filepath.Base(meta.Campaign.ConfigFile) {
		return fmt.Errorf("--resume: '%s' records a config_file that is not a bundle-root filename "+
			"('%s') — refusing to compare against a path outside the bundle",
			meta.RunID, meta.Campaign.ConfigFile)
	}
	stored := filepath.Join(dir, meta.Campaign.ConfigFile)
	storedBytes, err := os.ReadFile(stored)
	if err != nil {
		return fmt.Errorf("--resume: cannot read the config this campaign started with (%s): %w — "+
			"start a fresh campaign", stored, err)
	}
	currentBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("--resume: cannot read %s: %w", cfgPath, err)
	}
	if string(storedBytes) == string(currentBytes) {
		return nil
	}
	writeDiff(diffOut, stored, cfgPath)
	return fmt.Errorf("--resume: the config differs from the one this campaign started with (%s) — "+
		"a resumed campaign must run the exact config it began with; restore it or start a "+
		"fresh campaign", stored)
}

// writeDiff shows the operator what changed. diff(1) is universally present and
// exits 1 for "files differ", which is the expected case here; anything else it
// says is reported in place of the diff, never in place of the refusal.
func writeDiff(diffOut io.Writer, stored, current string) {
	if diffOut == nil {
		return
	}
	out, err := exec.Command("diff", "-u", stored, current).Output()
	if len(out) > 0 {
		_, _ = diffOut.Write(out)
	}
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
		_, _ = fmt.Fprintf(diffOut, "(cannot show the diff: diff -u %s %s: %v)\n", stored, current, err)
	}
}
