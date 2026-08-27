package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planTestdata is the golden plan config from internal/plan — a two-dataset,
// four-suite campaign, which makes it the widest dry run available here.
const planTestdata = "../../internal/plan/testdata/campaign.toml"

func TestRunCmdRejectsABadConfig(t *testing.T) {
	t.Setenv("BENCH_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer
	if got := dispatch([]string{"run", filepath.Join(t.TempDir(), "nope.toml")}, &stdout, &stderr); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(stderr.String(), "config:") {
		t.Errorf("stderr = %q, want the config error", stderr.String())
	}
}

func TestRunCmdDryRunTouchesNothing(t *testing.T) {
	// A path that does not exist yet: the point of the test is that a dry run
	// does not bring it into being — no lock file, no results dir, no clone.
	benchRoot := filepath.Join(t.TempDir(), "bench")
	t.Setenv("BENCH_ROOT", benchRoot)

	var stdout, stderr bytes.Buffer
	if got := dispatch([]string{"run", planTestdata, "--dry-run"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got, stderr.String())
	}
	for _, want := range []string{
		"dry run: printing commands only",
		"using placeholder sha 'deadbeef' in paths",
		"== build\n",
		filepath.Join(benchRoot, "bin", "stellar-rpc-deadbeef"),
		// close_interval is phase 1's block time, so the query legs pace
		// themselves at the phase-1 floors without the config naming a phase.
		"query load: phase 1 floors",
		"== dataset-sac-6000\n",
		"== ingest-cold-sac-6000-c1-run1\n",
		"== query-cold-sac-6000-c1-txhash-run1\n",
		"--types=txhash --target-rps=150,300,600 --duration=60s",
		"== query-hot-soroswap-1500-c2-events-run2\n",
		"--types=events --target-rps=1.875,3.75,7.5 --duration=60s",
		"$ campaign publish " + filepath.Join(benchRoot, "results"),
		"dry run complete",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q, got:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if _, err := os.Stat(benchRoot); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(benchRoot)
		t.Errorf("%s exists after a dry run (stat err = %v), holding %v", benchRoot, err, entries)
	}
}

func TestRunCmdDryRunResumeNeedsTheClone(t *testing.T) {
	benchRoot := t.TempDir()
	t.Setenv("BENCH_ROOT", benchRoot)
	resumeDir := filepath.Join(benchRoot, "results", "golden-deadbeef-20260101T000000Z")
	if err := os.MkdirAll(resumeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if got := dispatch([]string{"run", planTestdata, "--dry-run", "--resume", resumeDir}, &stdout, &stderr); got != 1 {
		t.Errorf("exit code = %d, want 1 (stdout: %s)", got, stdout.String())
	}
	// Without the clone there is no commit to validate the bundle against, and
	// a plan printed against a placeholder sha would describe a resume that a
	// real run would refuse.
	if !strings.Contains(stderr.String(), "needs the build clone") {
		t.Errorf("stderr = %q, want it to name the missing clone", stderr.String())
	}
}
