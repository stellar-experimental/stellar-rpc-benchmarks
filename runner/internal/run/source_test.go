package run

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a git command in dir and fails the test if it does not succeed.
// Identity and hooks come from the flags, not the machine: the test must behave
// the same on a devbox with a global gitconfig and on a bare CI runner.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	argv := append([]string{
		"-c", "user.name=bench", "-c", "user.email=bench@example.com",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a file and commits it, returning the new commit's sha.
func commit(t *testing.T, repo, name, body string) string {
	t.Helper()
	mustWrite(t, filepath.Join(repo, name), body)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "add "+name)
	return git(t, repo, "rev-parse", "HEAD")
}

// originRepo is a local stellar-rpc stand-in: one commit, one gitignore, on
// branch main.
func originRepo(t *testing.T) (dir, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir = filepath.Join(t.TempDir(), "origin")
	mustMkdir(t, dir)
	git(t, dir, "init", "-q")
	mustWrite(t, filepath.Join(dir, ".gitignore"), "target/\n")
	return dir, commit(t, dir, "README", "first\n")
}

func TestEnsureSrcClonesThenFetches(t *testing.T) {
	origin, first := originRepo(t)
	src := filepath.Join(t.TempDir(), "src")

	if err := EnsureSrc(src, origin, io.Discard); err != nil {
		t.Fatalf("first EnsureSrc: %v", err)
	}
	assertExists(t, filepath.Join(src, ".git"))
	got, err := ResolveRef(src, "main")
	if err != nil {
		t.Fatalf("ResolveRef after clone: %v", err)
	}
	if got != first {
		t.Errorf("main = %s, want the origin head %s", got, first)
	}

	// The origin moves on, as feature/full-history does between campaigns.
	second := commit(t, origin, "NOTES", "second\n")
	if err := EnsureSrc(src, origin, io.Discard); err != nil {
		t.Fatalf("second EnsureSrc: %v", err)
	}
	got, err = ResolveRef(src, "main")
	if err != nil {
		t.Fatalf("ResolveRef after fetch: %v", err)
	}
	// This is the stale-local-ref case: the clone's own main still points at
	// the first commit (a fetch updates only the remote-tracking refs), and
	// resolving must follow origin/main, not it.
	if local := git(t, src, "rev-parse", "refs/heads/main"); local != first {
		t.Fatalf("test premise broken: local main = %s, want the stale %s", local, first)
	}
	if got != second {
		t.Errorf("main = %s, want the fetched tip %s (a stale local ref shadowed it)", got, second)
	}
}

func TestEnsureSrcKeepsBuildCaches(t *testing.T) {
	origin, _ := originRepo(t)
	src := filepath.Join(t.TempDir(), "src")
	if err := EnsureSrc(src, origin, io.Discard); err != nil {
		t.Fatalf("EnsureSrc: %v", err)
	}

	// target/ is gitignored (a cargo build cache); scratch.go is merely
	// untracked. The reset must keep the first and drop the second, which is
	// what clean -fd without -x buys: rebuilding a nearby commit stays
	// incremental.
	mustWrite(t, filepath.Join(src, "target", "libpreflight.a"), "cached")
	mustWrite(t, filepath.Join(src, "scratch.go"), "package main")
	mustWrite(t, filepath.Join(src, "README"), "locally edited\n")

	if err := EnsureSrc(src, origin, io.Discard); err != nil {
		t.Fatalf("second EnsureSrc: %v", err)
	}
	assertExists(t, filepath.Join(src, "target", "libpreflight.a"))
	assertGone(t, filepath.Join(src, "scratch.go"))
	if b, err := os.ReadFile(filepath.Join(src, "README")); err != nil || string(b) != "first\n" {
		t.Errorf("README = %q (err %v), want the reset content", b, err)
	}
}

func TestEnsureSrcRepointsOrigin(t *testing.T) {
	first, _ := originRepo(t)
	src := filepath.Join(t.TempDir(), "src")
	if err := EnsureSrc(src, first, io.Discard); err != nil {
		t.Fatalf("EnsureSrc: %v", err)
	}

	// A campaign later points repo at a different checkout — a fork, or the
	// operator's own work in progress. The clone follows it.
	second, secondHead := originRepo(t)
	git(t, second, "checkout", "-q", "-b", "feature/full-history")
	secondHead = commit(t, second, "FEATURE", "wip\n")
	if err := EnsureSrc(src, second, io.Discard); err != nil {
		t.Fatalf("EnsureSrc onto the second origin: %v", err)
	}
	got, err := ResolveRef(src, "feature/full-history")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != secondHead {
		t.Errorf("feature/full-history = %s, want %s from the new origin", got, secondHead)
	}
}

func TestEnsureSrcPrintsItsCommands(t *testing.T) {
	origin, _ := originRepo(t)
	src := filepath.Join(t.TempDir(), "src")
	var buf strings.Builder
	if err := EnsureSrc(src, origin, &buf); err != nil {
		t.Fatalf("EnsureSrc: %v", err)
	}
	for _, want := range []string{
		"  $ git clone " + origin + " " + src,
		"  $ git -C " + src + " remote set-url origin " + origin,
		"  $ git -C " + src + " fetch -q --prune origin +refs/heads/*:refs/remotes/origin/* +refs/tags/*:refs/tags/*",
		"  $ git -C " + src + " reset -q --hard",
		"  $ git -C " + src + " clean -qfd",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q, got:\n%s", want, buf.String())
		}
	}
}

func TestEnsureSrcFailsOnAnUnreachableRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	err := EnsureSrc(filepath.Join(tmp, "src"), filepath.Join(tmp, "no-such-repo"), io.Discard)
	if err == nil {
		t.Fatal("EnsureSrc succeeded on a repo that does not exist")
	}
	if !strings.Contains(err.Error(), "clone") {
		t.Errorf("error = %v, want it to name the clone", err)
	}
}

func TestResolveRef(t *testing.T) {
	origin, head := originRepo(t)
	git(t, origin, "tag", "v1.2.3")
	src := filepath.Join(t.TempDir(), "src")
	if err := EnsureSrc(src, origin, io.Discard); err != nil {
		t.Fatalf("EnsureSrc: %v", err)
	}

	for _, tc := range []struct{ name, ref string }{
		{"branch", "main"},
		{"tag", "v1.2.3"},
		{"full sha", head},
		{"short sha", head[:8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveRef(src, tc.ref)
			if err != nil {
				t.Fatalf("ResolveRef(%s): %v", tc.ref, err)
			}
			if got != head {
				t.Errorf("ResolveRef(%s) = %s, want %s", tc.ref, got, head)
			}
		})
	}

	t.Run("unknown ref", func(t *testing.T) {
		if got, err := ResolveRef(src, "no/such/branch"); err == nil {
			t.Errorf("ResolveRef = %s, want an error", got)
		}
	})

	t.Run("no clone at all", func(t *testing.T) {
		// Not merely "git fails here": without the .git guard, git would search
		// upwards and answer out of whatever repository contains the path.
		if got, err := ResolveRef(filepath.Join(src, "cmd"), "main"); err == nil {
			t.Errorf("ResolveRef = %s, want an error naming the missing clone", got)
		}
	})
}
