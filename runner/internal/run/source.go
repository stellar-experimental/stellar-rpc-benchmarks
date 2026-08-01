package run

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureSrc converges the persistent build clone at src onto repo: clone once,
// then per campaign point origin at repo (it may have changed since the clone
// was made), fetch its branches and tags, and hard-reset. Gitignored build
// caches (cargo target/, Go cache) survive the reset — clean -fd, deliberately
// no -x — so rebuilding a nearby commit is incremental. repo itself is never
// modified.
func EnsureSrc(src, repo string, out io.Writer) error {
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
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
