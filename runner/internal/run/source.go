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
		if err := runCommand([]string{"git", "clone", repo, src}, nil, nil, out); err != nil {
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
		if err := runCommand(argv, nil, nil, out); err != nil {
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

// DetectQueryCLI reports the query bench shape of commit in CLI shape cli, and
// false when it has no query bench: `bench-query cold|hot` on a legacy commit,
// and under the bench root either `query cold|hot` or, from stellar-rpc#1070
// on, `query --data-dir`, whose command newBenchCommand builds directly.
func DetectQueryCLI(src, commit string, cli plan.BenchCLI) (plan.QueryCLI, bool, error) {
	probe := func(pattern string) (bool, error) {
		ok, err := grepSource(src, commit, pattern)
		if err != nil {
			return false, fmt.Errorf("probe the query bench of %s in %s: %w", commit, src, err)
		}
		return ok, nil
	}
	if cli == plan.BenchCLILegacy {
		ok, err := probe(useRegexp("bench-query"))
		return plan.QueryCLITiered, ok, err
	}
	switch ok, err := probe(`newBenchCommand\([[:space:]]*"query"`); {
	case err != nil:
		return "", false, err
	case ok:
		return plan.QueryCLIDataDir, true, nil
	}
	ok, err := probe(useRegexp("query"))
	return plan.QueryCLITiered, ok, err
}

// DetectBuildTarget reports the Makefile target that builds commit's bench
// binary, and whether that build links RocksDB and zstd statically. A
// Makefile with build-native-libs (stellar-rpc#1123 on) builds one static
// stellar-rpc binary with build-stellar-rpc. Before it, build-rpc-v2 builds the
// bench binary where the Makefile has it, and build-stellar-rpc does on the
// older refs that have only that, both against the shared libraries.
// build-native-libs is the same signal runner/bootstrap.sh reads.
func DetectBuildTarget(src, commit string) (plan.BuildTarget, bool, error) {
	has := func(target string) (bool, error) {
		ok, err := grepTree(src, commit, "^"+target+":", "Makefile")
		if err != nil {
			return false, fmt.Errorf("probe the build target of %s in %s: %w", commit, src, err)
		}
		return ok, nil
	}
	switch static, err := has("build-native-libs"); {
	case err != nil:
		return "", false, err
	case static:
		return plan.BuildStellarRPC, true, nil
	}
	for _, target := range []plan.BuildTarget{plan.BuildRPCV2, plan.BuildStellarRPC} {
		ok, err := has(string(target))
		if err != nil {
			return "", false, err
		}
		if ok {
			return target, false, nil
		}
	}
	return "", false, fmt.Errorf("commit %s has neither a %s nor a %s target in its Makefile",
		commit, plan.BuildRPCV2, plan.BuildStellarRPC)
}

// DetectEmptyColdOutDir reports whether commit's bench ingest cold refuses a
// non-empty --cold-out-dir, as it does from stellar-rpc#1070 on.
func DetectEmptyColdOutDir(src, commit string) (bool, error) {
	ok, err := grepSource(src, commit, `requireEmptyDir\([[:space:]]*"cold-out-dir"`)
	if err != nil {
		return false, fmt.Errorf("probe the bench ingest cold of %s in %s: %w", commit, src, err)
	}
	return ok, nil
}

// declaresCommand reports whether commit's non-test cmd/ sources declare a
// cobra command with Use: "<use>".
func declaresCommand(src, commit, use string) (bool, error) {
	return grepSource(src, commit, useRegexp(use))
}

func useRegexp(use string) string { return `Use:[[:space:]]*"` + use + `"` }

// grepSource reports whether commit's cmd/ sources match the extended regexp
// pattern. Test files are left out: a test may build a command the binary
// does not have.
func grepSource(src, commit, pattern string) (bool, error) {
	return grepTree(src, commit, pattern, "cmd", ":(exclude)*_test.go")
}

// grepTree reports whether the files of commit under pathspecs match the
// extended regexp pattern.
func grepTree(src, commit, pattern string, pathspecs ...string) (bool, error) {
	args := append([]string{"-C", src, "grep", "-q", "-E", pattern, commit, "--"}, pathspecs...)
	err := exec.Command("git", args...).Run()
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
