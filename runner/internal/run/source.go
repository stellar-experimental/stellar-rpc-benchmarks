package run

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// EnsureSrc converges the persistent build clone at src onto repo: clone once,
// then per campaign point origin at repo (it may have changed since the clone
// was made), fetch its branches and tags, and hard-reset. Gitignored build
// caches (cargo target/, Go cache) survive the reset — clean -fd, deliberately
// no -x — so rebuilding a nearby commit is incremental. repo itself is never
// modified.
func EnsureSrc(src, repo string, out io.Writer) error {
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect build clone at %s: %w", src, err)
		}
		if err := runCommand([]string{"git", "clone", repo, src}, nil, out); err != nil {
			return fmt.Errorf("clone %s into %s: %w", repo, src, err)
		}
	}
	for _, argv := range [][]string{
		{"git", "-C", src, "remote", "set-url", "origin", repo},
		{"git", "-C", src, "fetch", "-q", "--prune", "origin",
			"+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"},
		{"git", "-C", src, "reset", "-q", "--hard"},
		{"git", "-C", src, "clean", "-qfd"},
	} {
		if err := runCommand(argv, nil, out); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

// ResolveRef returns the full commit ref resolves to inside src.
// Remote-tracking branches are tried first so a stale local ref never shadows
// the fetched branch tip; the fallback covers tags and raw hashes.
func ResolveRef(src, ref string) (string, error) {
	// Without this guard git would search upwards from src and answer out of
	// whatever repository happens to contain it.
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return "", fmt.Errorf("no build clone at %s", src)
	}
	for _, rev := range []string{"refs/remotes/origin/" + ref + "^{commit}", ref + "^{commit}"} {
		out, err := exec.Command("git", "-C", src, "rev-parse", "--verify", "--quiet", rev).Output()
		if err != nil {
			continue
		}
		if sha := strings.TrimSpace(string(out)); len(sha) >= 8 {
			return sha, nil
		}
	}
	return "", fmt.Errorf("ref '%s' does not resolve to a commit in %s", ref, src)
}

// DetectBenchCLI reports the bench command shape of commit in the build clone
// at src. A commit whose cmd/ tree still declares the `bench-ingest` command
// predates stellar-rpc#1105 and gets plan.BenchCLILegacy; any other commit gets
// plan.BenchCLIRoot. Reading the source rather than probing the binary is what
// lets the plan, which is built before the binary, carry the right argv.
func DetectBenchCLI(src, commit string) (plan.BenchCLI, error) {
	legacy, err := declaresCommand(src, commit, "bench-ingest")
	switch {
	case err != nil:
		return "", fmt.Errorf("probe the bench command shape of %s in %s: %w", commit, src, err)
	case legacy:
		return plan.BenchCLILegacy, nil
	default:
		return plan.BenchCLIRoot, nil
	}
}

// HasQueryCLI reports whether commit declares the query bench in shape cli:
// `query` under the bench root, or `bench-query` on a legacy commit.
func HasQueryCLI(src, commit string, cli plan.BenchCLI) (bool, error) {
	use := "query"
	if cli == plan.BenchCLILegacy {
		use = "bench-query"
	}
	ok, err := declaresCommand(src, commit, use)
	if err != nil {
		return false, fmt.Errorf("probe the query bench of %s in %s: %w", commit, src, err)
	}
	return ok, nil
}

// declaresCommand reports whether commit's cmd/ tree declares a cobra command
// with Use: "<use>".
func declaresCommand(src, commit, use string) (bool, error) {
	err := exec.Command("git", "-C", src, "grep", "-q", "-E",
		`Use:[[:space:]]*"`+use+`"`, commit, "--", "cmd").Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return false, nil
	default:
		return false, err
	}
}
