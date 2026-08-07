// Package preflight answers one question before a campaign starts: does this
// machine have what this config needs? A missing gcloud credential is cheap to
// fix and catastrophic to discover seventeen hours in, when the campaign
// finally reaches its publish step.
//
// Every check is derived from the config — a campaign that publishes nowhere
// is never asked about gcloud — and every environment probe goes through Deps,
// so a test can trigger exactly one failure at a time.
package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/publish"
)

// The bench devbox's NVMe layout. The bash campaign runner this package
// replaces only verified the mount when BENCH_ROOT was left at its default, on
// the reasoning that an operator who pointed BENCH_ROOT elsewhere knows what
// they mounted; this keeps that.
const (
	defaultBenchRoot = "/mnt/nvme/bench"
	nvmeMount        = "/mnt/nvme"
)

const gib = 1 << 30

// minFree is the free-space line below which preflight warns. It is a warning,
// not a failure: how much a campaign really needs depends on its datasets and
// rep count, and a small fixture campaign runs happily under it.
const minFree = 100 * gib

// Deps are the environment probes, injectable so tests can stub each check.
type Deps struct {
	LookPath func(file string) (string, error) // default exec.LookPath
	// ListRoot lists an object-storage root to prove the current credentials
	// can reach it. An empty root is success; auth/network errors are not.
	ListRoot   func(uri string) error           // default ListRoot
	Mountpoint func(dir string) error           // default: exec `mountpoint -q <dir>`
	DiskFree   func(dir string) (uint64, error) // free bytes; default syscall.Statfs
}

// Result is what preflight found. Failures are things the campaign will need
// and does not have; warnings are things an operator should see before
// committing the machine for a day.
type Result struct {
	Failures []string // each names the missing thing AND the config key that needs it
	Warnings []string
}

func (r *Result) failf(format string, a ...any) {
	r.Failures = append(r.Failures, fmt.Sprintf(format, a...))
}
func (r *Result) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// requireTool records a failure when tool is not on PATH, saying which part of
// this config wants it. It reports whether the tool is there.
func (r *Result) requireTool(d Deps, tool, why string) bool {
	if _, err := d.LookPath(tool); err != nil {
		r.failf("%s not found in PATH — %s", tool, why)
		return false
	}
	return true
}

// Run performs every check cfg needs. benchRoot is the storage root the
// campaign would use; binPath is the versioned binary it would run, or "" when
// the ref is not resolvable yet (a standalone preflight on a fresh machine).
// Toolchain checks assume a build will happen unless binPath already exists
// and is executable.
func Run(cfg *config.Config, benchRoot, binPath string, d Deps) Result {
	d = withDefaults(d)
	var res Result

	// git and make drive every campaign: the build clone is a git checkout,
	// and the binary comes out of the repo's Makefile.
	res.requireTool(d, "git", fmt.Sprintf("the campaign clones and builds repo '%s'", cfg.Repo))
	res.requireTool(d, "make", fmt.Sprintf("the campaign builds repo '%s'", cfg.Repo))

	if willBuild(binPath) {
		res.requireTool(d, "go", fmt.Sprintf("building ref '%s' needs the Go toolchain", cfg.Ref))
		// rustup installs outside PATH on the devbox, so a cargo that
		// LookPath cannot see may still be the one the build uses — the same
		// fallback the bash campaign runner does when recording the rustc
		// version.
		if _, err := d.LookPath("cargo"); err != nil && !isExecutable(cargoBin()) {
			res.failf("cargo not found in PATH or %s — building ref '%s' needs the Rust toolchain", cargoBin(), cfg.Ref)
		}
	}

	// Each cloud CLI is wanted by the datasets that fetch through it and by a
	// publish_uri in its scheme. bsb-s3 datasets deliberately want neither: the
	// bench binary's own SDK reads that public bucket, with no CLI and no
	// credentials.
	var wantsGcloud, wantsAWS []string
	for _, ds := range cfg.Datasets {
		switch ds.Kind {
		case config.KindPacksGS:
			wantsGcloud = append(wantsGcloud, fmt.Sprintf("dataset '%s' fetches its packs from '%s'", ds.Name, ds.Location))
		case config.KindPacksS3:
			wantsAWS = append(wantsAWS, fmt.Sprintf("dataset '%s' fetches its packs from '%s'", ds.Name, ds.Location))
		}
	}
	if strings.HasPrefix(cfg.PublishURI, "gs://") {
		wantsGcloud = append(wantsGcloud, fmt.Sprintf("publish_uri '%s'", cfg.PublishURI))
	}
	if strings.HasPrefix(cfg.PublishURI, "s3://") {
		wantsAWS = append(wantsAWS, fmt.Sprintf("publish_uri '%s'", cfg.PublishURI))
	}
	havePublishCLI := true
	if len(wantsGcloud) > 0 {
		ok := res.requireTool(d, "gcloud", "needed by "+strings.Join(wantsGcloud, ", "))
		// The lists also collect datasets, and a CLI needed only by a dataset
		// must not suppress a listing in the other scheme.
		if strings.HasPrefix(cfg.PublishURI, "gs://") {
			havePublishCLI = ok
		}
	}
	if len(wantsAWS) > 0 {
		ok := res.requireTool(d, "aws", "needed by "+strings.Join(wantsAWS, ", "))
		if strings.HasPrefix(cfg.PublishURI, "s3://") {
			havePublishCLI = ok
		}
	}

	// A listing cannot succeed without its CLI, and a missing CLI is already a
	// failure above; running it anyway reports one root cause twice.
	if cfg.PublishURI != "" && havePublishCLI {
		if err := d.ListRoot(cfg.PublishURI); err != nil {
			res.failf("cannot list publish_uri '%s': %s — the campaign would only discover this at its publish step, hours from now", cfg.PublishURI, err)
		}
	}

	// Ported from the bash campaign runner: the mount is only checked for the
	// default root, and only where a mountpoint command exists (macOS has none).
	if benchRoot == defaultBenchRoot {
		if _, err := d.LookPath("mountpoint"); err == nil {
			if err := d.Mountpoint(nvmeMount); err != nil {
				res.failf("%s not mounted — run bootstrap.sh first, or set BENCH_ROOT", nvmeMount)
			}
		}
	}

	free, err := d.DiskFree(benchRoot)
	switch {
	case err != nil:
		res.warnf("could not measure free disk under %s: %s", benchRoot, err)
	case free < minFree:
		res.warnf("only %d GiB free under %s — a full campaign may need more", free/gib, benchRoot)
	}

	return res
}

func withDefaults(d Deps) Deps {
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.ListRoot == nil {
		d.ListRoot = ListRoot
	}
	if d.Mountpoint == nil {
		d.Mountpoint = mountpoint
	}
	if d.DiskFree == nil {
		d.DiskFree = diskFree
	}
	return d
}

// willBuild reports whether the campaign will compile the ref, which is what
// makes the Go and Rust toolchains a hard requirement. An unresolved ref ("")
// counts as a build: assuming otherwise would skip the check on exactly the
// fresh machine that most likely lacks a toolchain.
func willBuild(binPath string) bool {
	return binPath == "" || !isExecutable(binPath)
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func cargoBin() string { return filepath.Join(os.Getenv("HOME"), ".cargo", "bin", "cargo") }

// ListRoot reports whether the current credentials can list the
// object-storage root at uri. An empty root is a listable root; an auth,
// network, or missing-bucket error is not.
//
// The listing itself is publish.List — one implementation shared with the
// publish step, because both are asking the same question of the same two CLIs
// and a listing wrongly read as "empty" fails the same way in both places.
func ListRoot(uri string) error {
	// Ask for the directory-style prefix: `aws s3 ls s3://bucket/results` sends
	// prefix "results", which a bucket policy conditioned on s3:prefix
	// "results/*" rejects even though "results/" is allowed. The publish step
	// already lists with the trailing slash; match it so preflight exercises
	// the same permission.
	_, err := publish.List(strings.TrimSuffix(uri, "/") + "/")
	return err
}

func mountpoint(dir string) error { return exec.Command("mountpoint", "-q", dir).Run() }

// diskFree returns the free bytes on the filesystem holding dir. BENCH_ROOT
// often does not exist yet, so it walks up to the nearest existing parent —
// that is the filesystem the root will land on.
func diskFree(dir string) (uint64, error) {
	target, err := nearestExisting(dir)
	if err != nil {
		return 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(target, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", target, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func nearestExisting(dir string) (string, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("no existing directory above %s", dir)
		}
		path = parent
	}
}
