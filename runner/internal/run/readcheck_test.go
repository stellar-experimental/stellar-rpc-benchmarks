package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// fakeReader writes a stand-in stellar-rpc binary at dir/bin/stellar-rpc-4d30c258
// that records its argv and GOMEMLIMIT in dir/calls, writes into its
// --hot-dir, and fails with a packfile error when dir/fail exists.
func fakeReader(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin", "stellar-rpc-4d30c258")
	mustWrite(t, bin, `#!/bin/sh
echo "GOMEMLIMIT=$GOMEMLIMIT $*" >> "`+filepath.Join(dir, "calls")+`"
for a in "$@"; do case $a in --hot-dir=*) mkdir -p "${a#--hot-dir=}" && : > "${a#--hot-dir=}/db" ;; esac; done
if [ -e "`+filepath.Join(dir, "fail")+`" ]; then
	echo "starting hot ingest"
	echo "Error: open chunk 7: packfile: corrupt file: invalid magic number" >&2
	exit 1
fi
echo "wrote 3 CSVs"
`)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// readCheckPlan is a plan of one packs-local dataset with a read check by bin,
// then one leg that touches ran.
func readCheckPlan(t *testing.T, dir, bin, location string) (*plan.Plan, string) {
	root := filepath.Join(dir, "golden", "sac")
	mustMkdir(t, filepath.Join(root, "ledgers"))
	scratch := filepath.Join(dir, "scratch", "readcheck-sac")
	ran := filepath.Join(dir, "leg-ran")
	ds := &plan.DatasetSpec{Name: "sac", Kind: config.KindPacksLocal, Root: root, Location: location,
		ReadCheckDir: scratch,
		ReadCheck: []string{bin, "--enable-rpcv2", "bench", "ingest", "hot", "--pack-dir=" + root + "/ledgers",
			"--start-chunk=7", "--num-ledgers=1", "--hot-dir=" + scratch + "/hot", "--out=" + scratch + "/out"}}
	return &plan.Plan{Bin: bin, PackGeneration: "packs-v5", PackMarker: "/trailerSize = 44/ in cmd", Steps: []plan.Step{
		{ID: "dataset-sac", Kind: plan.KindDataset, Argv: [][]string{}, Dataset: ds},
		shLeg("ingest-cold-sac-c7-run1", filepath.Join(dir, "out"), `: > "$2"`, ran),
	}}, ran
}

// TestReadCheckPasses checks that a dataset the binary reads lets the legs
// run, that the check gets the bench env, and that its scratch dir is removed.
func TestReadCheckPasses(t *testing.T) {
	dir := t.TempDir()
	bin := fakeReader(t, dir)
	p, ran := readCheckPlan(t, dir, bin, "/mnt/sac/packs-v5/cold")
	mustWrite(t, filepath.Join(p.Steps[0].Dataset.ReadCheckDir, "hot", "stale"), "x")
	got := walk(t, p, Options{BenchEnv: map[string]string{"GOMEMLIMIT": "4096MiB"}})
	got.assertStatuses(t, StatusOK, StatusOK)
	got.assertLogHas(t, "dataset sac: stellar-rpc-4d30c258 reads it")
	assertExists(t, ran)
	assertGone(t, p.Steps[0].Dataset.ReadCheckDir)
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(calls), "GOMEMLIMIT=4096MiB --enable-rpcv2 bench ingest hot") {
		t.Errorf("calls = %q, want one GOMEMLIMIT=4096MiB bench ingest hot", calls)
	}
}

// TestReadCheckFailureStopsTheCampaign checks that a dataset the binary
// cannot read stops the walk without FailFast, with the binary's error and
// the registry's choice in the message.
func TestReadCheckFailureStopsTheCampaign(t *testing.T) {
	dir := t.TempDir()
	bin := fakeReader(t, dir)
	mustWrite(t, filepath.Join(dir, "fail"), "")
	p, ran := readCheckPlan(t, dir, bin, "/mnt/sac/packs-v4/cold")
	got := walk(t, p, Options{})
	got.assertStatuses(t, StatusFailed)
	if got.err == nil {
		t.Error("Execute returned nil, want the failure")
	}
	if !errors.Is(got.results[0].Err, errPackGeneration) {
		t.Errorf("error = %v, want errPackGeneration", got.results[0].Err)
	}
	for _, want := range []string{
		"packfile: corrupt file: invalid magic number",
		"dataset sac is packs-v4; the registry chose it for 4d30c258 by marker /trailerSize = 44/ in cmd",
	} {
		if !strings.Contains(got.results[0].Err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", got.results[0].Err, want)
		}
	}
	assertGone(t, ran)
	assertGone(t, p.Steps[0].Dataset.ReadCheckDir)
}

// TestDatasetWithoutReadCheckRunsNoBinary checks that a dataset step with no
// read check never calls the binary.
func TestDatasetWithoutReadCheckRunsNoBinary(t *testing.T) {
	dir := t.TempDir()
	bin := fakeReader(t, dir)
	p, _ := readCheckPlan(t, dir, bin, "")
	p.Steps[0].Dataset.ReadCheck = nil
	walk(t, p, Options{}).assertStatuses(t, StatusOK, StatusOK)
	if _, err := os.Stat(filepath.Join(dir, "calls")); !os.IsNotExist(err) {
		t.Errorf("binary ran: %v", err)
	}
}

// TestLastLines checks the quoted tail of a failed read check.
func TestLastLines(t *testing.T) {
	if got := lastLines("a\n\nb\n  c  \nd\n", 3); got != "b | c | d" {
		t.Errorf("lastLines = %q", got)
	}
	if got := lastLines("", 3); got != "" {
		t.Errorf("lastLines of nothing = %q", got)
	}
}
