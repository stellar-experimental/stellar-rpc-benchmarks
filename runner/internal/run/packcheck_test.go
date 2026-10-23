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

// The fixture packs were written by the stellar-rpc packfile writer at
// 06927e1a^ (v4.pack, with a content hash as the ledger freeze writes it) and
// at 06927e1a (v5.pack). v4-sac-9000-tail.pack is the last 76 bytes of a
// ledger pack of the packs-v4 sac-9000 dataset.
const packFixtures = "testdata/packs"

// packRoot is a dataset root whose ledgers/ holds the fixture pack fixture as
// its first pack, with a later pack in the other generation.
func packRoot(t *testing.T, fixture string) string {
	t.Helper()
	root := t.TempDir()
	b, err := os.ReadFile(filepath.Join(packFixtures, fixture))
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "ledgers", "00000", "00000001.pack"), string(b))
	mustWrite(t, filepath.Join(root, "ledgers", "00000", "00000002.pack"), "not a pack")
	return root
}

// TestPackGenerationOf checks that a pack's trailer names its generation.
func TestPackGenerationOf(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    plan.PackGeneration
	}{
		{"v4.pack", plan.PackGenV4},
		{"v4-sac-9000-tail.pack", plan.PackGenV4},
		{"v5.pack", plan.PackGenV5},
	} {
		got, err := PackGenerationOf(filepath.Join(packFixtures, tc.fixture))
		if err != nil {
			t.Fatalf("%s: %v", tc.fixture, err)
		}
		if got != tc.want {
			t.Errorf("%s: generation = %q, want %q", tc.fixture, got, tc.want)
		}
	}
}

// TestPackGenerationOfRejectsNonPacks checks that a file with no valid
// trailer, or a damaged one, has no generation.
func TestPackGenerationOfRejectsNonPacks(t *testing.T) {
	v5, err := os.ReadFile(filepath.Join(packFixtures, "v5.pack"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte{}, v5...)
	damaged[len(damaged)-20] ^= 0xff
	for name, body := range map[string][]byte{
		"empty":         {},
		"text":          []byte("not a pack"),
		"v5, CRC wrong": damaged,
	} {
		path := filepath.Join(t.TempDir(), "x.pack")
		mustWrite(t, path, string(body))
		if got, err := PackGenerationOf(path); err == nil {
			t.Errorf("%s: generation = %q, want an error", name, got)
		}
	}
}

// TestCheckPackGeneration checks the dataset check against the first pack of
// the root, and the mismatch error the operator reads.
func TestCheckPackGeneration(t *testing.T) {
	bin := "/bench/bin/stellar-rpc-4d30c258"
	v4 := &plan.DatasetSpec{Name: "sac-9000", Kind: config.KindPacksS3, Root: packRoot(t, "v4.pack"),
		Location: "s3://bucket/sac-9000/packs-v5/cold"}
	if err := CheckPackGeneration(v4, plan.PackGenV4, bin); err != nil {
		t.Errorf("packs-v4 root for a packs-v4 binary: %v", err)
	}
	err := CheckPackGeneration(v4, plan.PackGenV5, bin)
	if !errors.Is(err, errPackGeneration) {
		t.Fatalf("packs-v4 root for a packs-v5 binary: error = %v, want errPackGeneration", err)
	}
	for _, want := range []string{
		"dataset sac-9000 is packs-v4 (" + filepath.Join(v4.Root, "ledgers", "00000", "00000001.pack") + ")",
		"stellar-rpc 4d30c258 reads packs-v5 (stellar-rpc#1128)",
		"rm -rf it to fetch s3://bucket/sac-9000/packs-v5/cold",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	local := &plan.DatasetSpec{Name: "local", Kind: config.KindPacksLocal, Root: packRoot(t, "v5.pack")}
	err = CheckPackGeneration(local, plan.PackGenV4, bin)
	if err == nil || strings.Contains(err.Error(), "rm -rf") {
		t.Errorf("packs-local mismatch: error = %v, want one that does not offer a re-fetch", err)
	}

	empty := &plan.DatasetSpec{Name: "empty", Kind: config.KindPacksS3, Root: t.TempDir()}
	mustMkdir(t, filepath.Join(empty.Root, "ledgers"))
	if err := CheckPackGeneration(empty, plan.PackGenV5, bin); err == nil || !strings.Contains(err.Error(), "no .pack file") {
		t.Errorf("root without packs: error = %v, want no .pack file", err)
	}
}

// TestExecuteStopsOnPackGenerationMismatch checks that a dataset in the wrong
// pack generation stops the walk before any later step, without FailFast, and
// that a plan with no pack generation checks nothing.
func TestExecuteStopsOnPackGenerationMismatch(t *testing.T) {
	steps := func(tmp string) []plan.Step {
		ran := filepath.Join(tmp, "leg-ran")
		return []plan.Step{
			{ID: "dataset-sac", Kind: plan.KindDataset, Argv: [][]string{},
				Dataset: &plan.DatasetSpec{Name: "sac", Kind: config.KindPacksLocal, Root: packRoot(t, "v4.pack")}},
			shLeg("ingest-cold-sac-c1-run1", filepath.Join(tmp, "out"), `: > "$2"`, ran),
		}
	}

	tmp := t.TempDir()
	p := &plan.Plan{PackGeneration: plan.PackGenV5, Bin: "/bench/bin/stellar-rpc-4d30c258", Steps: steps(tmp)}
	got := walk(t, p, Options{})
	got.assertStatuses(t, StatusFailed)
	got.assertLogHas(t, "dataset sac is packs-v4")
	if got.err == nil {
		t.Error("Execute returned nil, want the failure")
	}

	tmp = t.TempDir()
	p = &plan.Plan{PackGeneration: plan.PackGenV4, Bin: "/bench/bin/stellar-rpc-1c0572da", Steps: steps(tmp)}
	got = walk(t, p, Options{})
	got.assertStatuses(t, StatusOK, StatusOK)
	got.assertLogHas(t, "dataset sac: packs-v4, the generation stellar-rpc-1c0572da reads")

	tmp = t.TempDir()
	p = &plan.Plan{Bin: "/bench/bin/stellar-rpc-deadbeef", Steps: steps(tmp)}
	got = walk(t, p, Options{})
	got.assertStatuses(t, StatusOK, StatusOK)
	got.assertLogLacks(t, "the generation")
}
