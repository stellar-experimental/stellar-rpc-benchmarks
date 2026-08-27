package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// stubBinary writes an executable that answers `version` with four lines, so
// the writers' head -3 equivalent has something to cut.
func stubBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stellar-rpc-v2")
	script := "#!/bin/sh\n" +
		"echo 'stellar-rpc-v2 v23.0.0'\n" +
		"echo 'commit: deadbeef'\n" +
		"echo 'build: 2026-01-01'\n" +
		"echo 'fourth line should be cut'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return path
}

// readFile is the written provenance file's contents.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestWriteBinaryInfo(t *testing.T) {
	dir := t.TempDir()
	bin := stubBinary(t)
	err := WriteBinaryInfo(dir, bin, "https://github.com/stellar/stellar-rpc.git",
		"feature/full-history", "deadbeefcafebabe")
	if err != nil {
		t.Fatalf("WriteBinaryInfo: %v", err)
	}
	got := readFile(t, filepath.Join(dir, BinaryInfoName))
	for _, want := range []string{
		"binary: " + bin,
		"commit: deadbeefcafebabe",
		"ref:    feature/full-history",
		"repo:   https://github.com/stellar/stellar-rpc.git",
		"stellar-rpc-v2 v23.0.0",
		"build: 2026-01-01",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("binary.txt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fourth line") {
		t.Errorf("binary.txt should keep only the first 3 version lines:\n%s", got)
	}
}

// TestWriteBinaryInfoMissingBinary covers the best-effort contract: a binary
// that cannot run costs the file its version lines, not the campaign its
// bundle.
func TestWriteBinaryInfoMissingBinary(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-a-binary")
	if err := WriteBinaryInfo(dir, missing, "repo", "ref", "commit"); err != nil {
		t.Fatalf("WriteBinaryInfo: %v", err)
	}
	got := readFile(t, filepath.Join(dir, BinaryInfoName))
	if !strings.Contains(got, "binary: "+missing) {
		t.Errorf("binary.txt should still record the identity lines:\n%s", got)
	}
	if !strings.Contains(got, "version:") {
		t.Errorf("binary.txt should report why the version is missing:\n%s", got)
	}
}

func machineInput(t *testing.T, benchRoot, bin string) MachineInput {
	t.Helper()
	cfg, err := config.Load("testdata/campaign.toml")
	if err != nil {
		t.Fatalf("config.Load(testdata/campaign.toml): %v", err)
	}
	return MachineInput{
		Cfg:         cfg,
		Repo:        "https://github.com/stellar/stellar-rpc.git",
		Ref:         "feature/full-history",
		BuiltCommit: "deadbeefcafebabe",
		BinPath:     bin,
		BenchRoot:   benchRoot,
		QueryPhase:  1,
		Hardware: Hardware{
			InstanceType: "i4i.4xlarge",
			InstanceID:   "i-0123456789abcdef0",
			Uname:        "Linux 6.8.0-1029-aws x86_64",
		},
	}
}

func TestWriteMachineMetadata(t *testing.T) {
	dir := t.TempDir()
	benchRoot := t.TempDir()
	in := machineInput(t, benchRoot, stubBinary(t))
	if err := WriteMachineMetadata(dir, in); err != nil {
		t.Fatalf("WriteMachineMetadata: %v", err)
	}
	got := readFile(t, filepath.Join(dir, MachineMetadataName))
	for _, want := range []string{
		"instance-type: i4i.4xlarge",
		"instance-id:   i-0123456789abcdef0",
		"repo: https://github.com/stellar/stellar-rpc.git",
		"ref: feature/full-history (deadbeefcafebabe)",
		"binary: " + in.BinPath + " (commit deadbeefcafebabe)",
		"stellar-rpc-v2 v23.0.0",
		"campaign: golden · ingest: both · query: yes · runs: 5 · query-duration: 60s · query-phase: 1",
		"close-interval: 2s · workers: 1 · hot-num-ledgers: 50000",
		"fsync probe: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("machine-metadata.txt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fsync probe: unavailable") {
		t.Errorf("the probe should have run against a writable BENCH_ROOT:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(benchRoot, ".fsync-probe")); !os.IsNotExist(err) {
		t.Errorf("the probe file should be removed, stat err = %v", err)
	}
	// Every fact is best-effort, so the file must never carry a blank line
	// where an absent one would have been.
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Errorf("machine-metadata.txt line %d is blank — absent facts should leave no gap", i+1)
		}
	}
}

// TestWriteMachineMetadataNoHardware covers a machine off EC2: the instance
// lines are absent rather than empty.
func TestWriteMachineMetadataNoHardware(t *testing.T) {
	dir := t.TempDir()
	in := machineInput(t, t.TempDir(), stubBinary(t))
	in.Hardware = Hardware{}
	if err := WriteMachineMetadata(dir, in); err != nil {
		t.Fatalf("WriteMachineMetadata: %v", err)
	}
	got := readFile(t, filepath.Join(dir, MachineMetadataName))
	if strings.Contains(got, "instance-type") || strings.Contains(got, "instance-id") {
		t.Errorf("off EC2 the instance lines should be absent:\n%s", got)
	}
}

// TestWriteMachineMetadataUnwritableProbe covers a BENCH_ROOT the probe cannot
// write: the file still gets written, with the probe reporting why.
func TestWriteMachineMetadataUnwritableProbe(t *testing.T) {
	dir := t.TempDir()
	in := machineInput(t, filepath.Join(t.TempDir(), "missing"), stubBinary(t))
	if err := WriteMachineMetadata(dir, in); err != nil {
		t.Fatalf("WriteMachineMetadata: %v", err)
	}
	got := readFile(t, filepath.Join(dir, MachineMetadataName))
	if !strings.Contains(got, "fsync probe: unavailable") {
		t.Errorf("an unwritable BENCH_ROOT should report an unavailable probe:\n%s", got)
	}
}
