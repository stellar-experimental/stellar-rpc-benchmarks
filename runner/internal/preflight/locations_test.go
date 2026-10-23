package preflight

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/publish"
)

// TestDatasetLocations checks that a packs-s3 dataset the campaign will fetch
// fails when its location is empty or unlistable, naming the registry's
// choice, and that a fetched root or another kind is not listed.
func TestDatasetLocations(t *testing.T) {
	dir := t.TempDir()
	fetched := filepath.Join(dir, "golden", "fetched")
	if err := os.MkdirAll(filepath.Join(fetched, "ledgers"), 0o755); err != nil {
		t.Fatal(err)
	}
	step := func(name, kind, loc, root string) plan.Step {
		return plan.Step{ID: "dataset-" + name, Kind: plan.KindDataset,
			Dataset: &plan.DatasetSpec{Name: name, Kind: kind, Location: loc, Root: root}}
	}
	p := &plan.Plan{Bin: "/bench/bin/stellar-rpc-a130b9e2", PackGeneration: "packs-v6", PackMarker: "/groupHasCounts/ in cmd",
		Steps: []plan.Step{
			step("missing", config.KindPacksS3, "s3://b/c1/sac-9000/packs-v6/cold", filepath.Join(dir, "golden", "missing")),
			step("there", config.KindPacksS3, "s3://b/c1/soroswap-2700/packs-v6/cold", filepath.Join(dir, "golden", "there")),
			step("denied", config.KindPacksS3, "s3://b/c1/denied/packs-v6/cold", filepath.Join(dir, "golden", "denied")),
			step("fetched", config.KindPacksS3, "s3://b/c1/fetched/packs-v6/cold", fetched),
			step("gs", config.KindPacksGS, "gs://b/c1/gs/packs-v6/cold", filepath.Join(dir, "golden", "gs")),
		}}
	var listed []string
	d := okDeps()
	d.ListPrefix = func(uri string) (publish.State, error) {
		listed = append(listed, uri)
		switch {
		case strings.Contains(uri, "soroswap"):
			return publish.HasObjects, nil
		case strings.Contains(uri, "denied"):
			return publish.Empty, errors.New("AccessDenied")
		}
		return publish.Empty, nil
	}
	res := DatasetLocations(p, d)
	if len(res.Failures) != 2 {
		t.Fatalf("failures = %q, want 2", res.Failures)
	}
	want := "dataset missing: s3://b/c1/sac-9000/packs-v6/cold holds no objects — the registry chose packs-v6 for " +
		"a130b9e2 by marker /groupHasCounts/ in cmd, and packs-v6 is not uploaded for this dataset"
	if !strings.HasPrefix(res.Failures[0], want) {
		t.Errorf("failure = %q, want prefix %q", res.Failures[0], want)
	}
	if !strings.Contains(res.Failures[1], "dataset denied: cannot list") {
		t.Errorf("failure = %q, want a listing error", res.Failures[1])
	}
	if strings.Join(listed, " ") != "s3://b/c1/sac-9000/packs-v6/cold s3://b/c1/soroswap-2700/packs-v6/cold s3://b/c1/denied/packs-v6/cold" {
		t.Errorf("listed = %q", listed)
	}
}
