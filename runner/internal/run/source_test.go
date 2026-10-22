package run

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
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

// TestDetectBenchCLI checks that a commit whose bench package declares the
// bench-ingest command reads as the legacy CLI and a later one as the bench
// root CLI, per commit rather than per checkout.
func TestDetectBenchCLI(t *testing.T) {
	repo, _ := originRepo(t)
	pkg := filepath.Join(repo, "cmd", "stellar-rpc", "internal", "rpcv2", "bench")
	mustMkdir(t, pkg)
	legacy := commit(t, repo, "cmd/stellar-rpc/internal/rpcv2/bench/command.go",
		"package bench\n\nvar c = cobra.Command{\n\tUse:   \"bench-ingest\",\n}\n")
	root := commit(t, repo, "cmd/stellar-rpc/internal/rpcv2/bench/command.go",
		"package bench\n\nvar c = cobra.Command{\n\tUse:   \"bench\",\n}\n")

	for _, tc := range []struct {
		commit string
		want   plan.BenchCLI
	}{
		{legacy, plan.BenchCLILegacy},
		{root, plan.BenchCLIRoot},
	} {
		got, err := DetectBenchCLI(repo, tc.commit)
		if err != nil {
			t.Fatalf("DetectBenchCLI(%s): %v", tc.commit[:8], err)
		}
		if got != tc.want {
			t.Errorf("DetectBenchCLI(%s) = %q, want %q", tc.commit[:8], got, tc.want)
		}
	}

	if _, err := DetectBenchCLI(repo, "0000000000000000000000000000000000000000"); err == nil {
		t.Error("DetectBenchCLI of a commit the clone lacks succeeded, want an error")
	}
}

// TestDetectQueryCLI checks the query bench probe in its three shapes, and
// that a command declared only in a test file does not count.
func TestDetectQueryCLI(t *testing.T) {
	repo, _ := originRepo(t)
	mustMkdir(t, filepath.Join(repo, "cmd"))
	path := "cmd/command.go"
	ingestOnly := commit(t, repo, path, "var c = cobra.Command{Use: \"bench-ingest\"}\n")
	legacy := commit(t, repo, path, "var c = cobra.Command{Use: \"bench-ingest\"}\nvar q = cobra.Command{Use: \"bench-query\"}\n")
	rootOnly := commit(t, repo, path, "var c = cobra.Command{Use: \"bench\"}\n")
	testOnly := commit(t, repo, "cmd/command_test.go", "var q = cobra.Command{Use: \"query\"}\n"+
		"var d = newBenchCommand(\"query\", \"\")\n")
	tiered := commit(t, repo, path, "var c = cobra.Command{Use: \"bench\"}\nvar q = cobra.Command{Use:   \"query\"}\n")
	dataDir := commit(t, repo, path, "var c = cobra.Command{Use: \"bench\"}\nfunc q() { cmd = newBenchCommand(\"query\",\n\t\"Benchmark reads\") }\n")

	for _, tc := range []struct {
		name   string
		commit string
		cli    plan.BenchCLI
		want   plan.QueryCLI
		ok     bool
	}{
		{"legacy without bench-query", ingestOnly, plan.BenchCLILegacy, plan.QueryCLITiered, false},
		{"legacy with bench-query", legacy, plan.BenchCLILegacy, plan.QueryCLITiered, true},
		{"root without query", rootOnly, plan.BenchCLIRoot, plan.QueryCLITiered, false},
		{"a query command in a test file only", testOnly, plan.BenchCLIRoot, plan.QueryCLITiered, false},
		{"root with query cold|hot", tiered, plan.BenchCLIRoot, plan.QueryCLITiered, true},
		{"root with query --data-dir", dataDir, plan.BenchCLIRoot, plan.QueryCLIDataDir, true},
		{"a legacy bench-query is not the root query", legacy, plan.BenchCLIRoot, plan.QueryCLITiered, false},
	} {
		got, ok, err := DetectQueryCLI(repo, tc.commit, tc.cli)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: DetectQueryCLI = %q, %t; want %q, %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestDetectBuildTarget checks each Makefile generation: build-rpc-v2 wins
// over the rpcv1 alias that its Makefiles also call build-stellar-rpc, a
// Makefile with build-stellar-rpc alone (c48a55c6) builds shared, only
// build-native-libs makes the build static, and a Makefile with no build
// target is an error.
func TestDetectBuildTarget(t *testing.T) {
	repo, _ := originRepo(t)
	shared := commit(t, repo, "Makefile", "build-stellar-rpc: build-rpc-v1\n\nbuild-rpc-v2: build-libs\n")
	onlyStellarRPC := commit(t, repo, "Makefile", "build-stellar-rpc: build-libs\n")
	static := commit(t, repo, "Makefile",
		"build-native-libs: $(ZSTD_HOME)/lib/libzstd.a\n\nbuild-stellar-rpc: build-libs build-native-libs\n")
	neither := commit(t, repo, "Makefile", "build: build-libs\n")

	for _, tc := range []struct {
		name   string
		commit string
		want   plan.BuildTarget
		static bool
	}{
		{"build-rpc-v2 and its shared libraries", shared, plan.BuildRPCV2, false},
		{"build-stellar-rpc alone, shared (c48a55c6)", onlyStellarRPC, plan.BuildStellarRPC, false},
		{"build-native-libs, one static binary", static, plan.BuildStellarRPC, true},
	} {
		got, gotStatic, err := DetectBuildTarget(repo, tc.commit)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want || gotStatic != tc.static {
			t.Errorf("%s: DetectBuildTarget = %q, %t; want %q, %t", tc.name, got, gotStatic, tc.want, tc.static)
		}
	}
	if got, _, err := DetectBuildTarget(repo, neither); err == nil {
		t.Errorf("DetectBuildTarget of a Makefile with neither target = %q, want an error", got)
	}
}

// TestDetectRPCV2BuildFlag checks the probe for the Makefile's RPCV2 variable,
// which a mention of RPCV2 in a comment does not satisfy.
func TestDetectRPCV2BuildFlag(t *testing.T) {
	repo, _ := originRepo(t)
	before := commit(t, repo, "Makefile", "build-native-libs: $(ZSTD_HOME)/lib/libzstd.a\n")
	comment := commit(t, repo, "Makefile", "# unless built with RPCV2=true\nbuild-native-libs:\n")
	after := commit(t, repo, "Makefile", "RPCV2 ?= false\n\nbuild-native-libs:\n")
	for _, tc := range []struct {
		name   string
		commit string
		want   bool
	}{
		{"before #1136", before, false},
		{"RPCV2 in a comment only", comment, false},
		{"from #1136 on", after, true},
	} {
		got, err := DetectRPCV2BuildFlag(repo, tc.commit)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: DetectRPCV2BuildFlag = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestDetectEmptyColdOutDir checks the probe for bench ingest cold's empty
// --cold-out-dir rule, which a test file alone does not satisfy.
func TestDetectEmptyColdOutDir(t *testing.T) {
	repo, _ := originRepo(t)
	mustMkdir(t, filepath.Join(repo, "cmd"))
	before := commit(t, repo, "cmd/cold.go", "func validate() error { return nil }\n")
	testOnly := commit(t, repo, "cmd/cold_test.go", "var _ = requireEmptyDir(\"cold-out-dir\", root)\n")
	after := commit(t, repo, "cmd/cold.go", "func validate() error { return requireEmptyDir(\"cold-out-dir\", o.ColdRoot) }\n")
	for _, tc := range []struct {
		name   string
		commit string
		want   bool
	}{
		{"before #1070", before, false},
		{"the rule in a test file only", testOnly, false},
		{"from #1070 on", after, true},
	} {
		got, err := DetectEmptyColdOutDir(repo, tc.commit)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: DetectEmptyColdOutDir = %t, want %t", tc.name, got, tc.want)
		}
	}
}
