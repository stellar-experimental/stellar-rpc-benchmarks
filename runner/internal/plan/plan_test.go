package plan

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

var update = flag.Bool("update", false, "rewrite testdata/plan.golden.json from the current Build output")

const goldenPath = "testdata/plan.golden.json"

// goldenInputs pins everything environment-dependent, so the plan below is a
// function of the config alone.
func goldenInputs() Inputs {
	return Inputs{
		BenchRoot:   "/bench",
		BuiltCommit: "deadbeefcafebabefeedface1234567890abcdef",
		Sha8:        "deadbeef",
		Stamp:       "20260101T000000Z",
	}
}

// load writes src to a temp file and loads it as a campaign config.
func load(t *testing.T, src string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "campaign.toml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// buildGolden builds the plan the committed testdata config describes.
func buildGolden(t *testing.T) *Plan {
	t.Helper()
	cfg, err := config.Load("testdata/campaign.toml")
	if err != nil {
		t.Fatalf("config.Load(testdata/campaign.toml): %v", err)
	}
	return Build(cfg, goldenInputs())
}

// stepByID finds one step, failing the test when the id is absent.
func stepByID(t *testing.T, p *Plan, id string) Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("plan has no step %q", id)
	return Step{}
}

// assertEmptyArgvJSON checks that a commandless step still serializes argv as
// an empty list: argv is a required plan.json field, and null is not a list.
func assertEmptyArgvJSON(t *testing.T, s Step) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %s: %v", s.ID, err)
	}
	if !strings.Contains(string(b), `"argv":[]`) {
		t.Errorf("%s marshals as %s, want it to contain `\"argv\":[]`", s.ID, b)
	}
}

func stepIDs(p *Plan) []string {
	ids := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		ids[i] = s.ID
	}
	return ids
}

func TestGolden(t *testing.T) {
	p := buildGolden(t)
	got := filepath.Join(t.TempDir(), "plan.json")
	if err := p.WriteFile(got); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	gotBytes, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read written plan: %v", err)
	}
	if *update {
		if err := os.WriteFile(goldenPath, gotBytes, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}
	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Errorf("plan.json differs from %s — rerun with -update to accept:\n%s", goldenPath, diffLines(wantBytes, gotBytes))
	}
}

// diffLines reports the first differing line, which is all a golden mismatch
// usually needs.
func diffLines(want, got []byte) string {
	w := strings.Split(string(want), "\n")
	g := strings.Split(string(got), "\n")
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return fmt.Sprintf("line %d:\n  want: %s\n  got:  %s", i+1, w[i], g[i])
		}
	}
	return fmt.Sprintf("want %d lines, got %d", len(w), len(g))
}

func TestQueryHotNeedsLastIngestHotRep(t *testing.T) {
	p := buildGolden(t)
	seen := 0
	for _, s := range p.Steps {
		if !strings.HasPrefix(s.ID, "query-hot-") {
			continue
		}
		seen++
		// runs = 2, so every rep — not just rep 2 — waits on rep 2.
		cell, _, _ := strings.Cut(strings.TrimPrefix(s.ID, "query-hot-"), "-run")
		want := "ingest-hot-" + cell + "-run2"
		if !slices.Contains(s.Needs, want) {
			t.Errorf("%s needs %v, want it to include %q", s.ID, s.Needs, want)
		}
		if !slices.Contains(s.Needs, "build") {
			t.Errorf("%s needs %v, want it to include \"build\"", s.ID, s.Needs)
		}
	}
	if seen != 8 { // 2 datasets x 2 chunks x 2 reps
		t.Errorf("found %d query-hot steps, want 8", seen)
	}
}

func TestSuiteOrdering(t *testing.T) {
	p := buildGolden(t)
	// Collapse each step id to its suite, then to runs of consecutive suites:
	// an interleaved plan would repeat a label.
	suites := []string{"dataset", "ingest-cold", "ingest-hot", "query-cold", "query-hot"}
	var blocks []string
	for _, id := range stepIDs(p) {
		label := id
		for _, s := range suites {
			if strings.HasPrefix(id, s+"-") {
				label = s
				break
			}
		}
		if len(blocks) == 0 || blocks[len(blocks)-1] != label {
			blocks = append(blocks, label)
		}
	}
	want := []string{"build", "dataset", "ingest-cold", "ingest-hot", "query-cold", "query-hot", "tarball", "publish"}
	if !slices.Equal(blocks, want) {
		t.Errorf("step blocks = %v, want %v", blocks, want)
	}
}

const hotConfig = `
name = "hot"
ingest = "hot"
query = true
runs = 1
hot_num_ledgers = %d

[[dataset]]
name = "ds"
kind = "packs-local"
location = "/packs/ds"
chunks = [7]
`

func TestHotLedgerCaps(t *testing.T) {
	capped := Build(load(t, fmt.Sprintf(hotConfig, 50000)), goldenInputs())
	uncapped := Build(load(t, fmt.Sprintf(hotConfig, 0)), goldenInputs())

	cases := []struct {
		plan *Plan
		id   string
		flag string
		want bool
	}{
		{capped, "ingest-hot-ds-c7-run1", "--num-ledgers=50000", true},
		{uncapped, "ingest-hot-ds-c7-run1", "--num-ledgers=", false},
		{capped, "query-hot-ds-c7-run1", "--sample-ledgers=50000", true},
		{uncapped, "query-hot-ds-c7-run1", "--sample-ledgers=", false},
	}
	for _, tc := range cases {
		argv := stepByID(t, tc.plan, tc.id).Argv[0]
		got := slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, tc.flag) })
		if got != tc.want {
			t.Errorf("%s argv %v: has %q = %v, want %v", tc.id, argv, tc.flag, got, tc.want)
		}
	}

	// The --out flag stays last, after the optional cap.
	for _, p := range []*Plan{capped, uncapped} {
		argv := stepByID(t, p, "ingest-hot-ds-c7-run1").Argv[0]
		if last := argv[len(argv)-1]; !strings.HasPrefix(last, "--out=") {
			t.Errorf("last ingest-hot arg = %q, want an --out flag", last)
		}
	}
}

func TestPacksLocalRootIsTheConfiguredLocation(t *testing.T) {
	p := Build(load(t, fmt.Sprintf(hotConfig, 0)), goldenInputs())
	ds := stepByID(t, p, "dataset-ds")
	if len(ds.Argv) != 0 {
		t.Errorf("packs-local dataset argv = %v, want no commands", ds.Argv)
	}
	if len(ds.Needs) != 0 {
		t.Errorf("packs-local dataset needs = %v, want none — it does not invoke the binary", ds.Needs)
	}
	if ds.Dataset.Root != "/packs/ds" {
		t.Errorf("packs-local root = %q, want the configured location", ds.Dataset.Root)
	}
	assertEmptyArgvJSON(t, ds)
}

func TestColdIngestCleansScratchBothSides(t *testing.T) {
	p := Build(load(t, `
name = "c"
ingest = "cold"
query = false
runs = 1

[[dataset]]
name = "ds"
kind = "packs-local"
location = "/packs/ds"
chunks = [3]
`), goldenInputs())
	leg := stepByID(t, p, "ingest-cold-ds-c3-run1")
	want := []string{"/bench/scratch/ds/3"}
	if !slices.Equal(leg.PreClean, want) {
		t.Errorf("pre_clean = %v, want %v", leg.PreClean, want)
	}
	if !slices.Equal(leg.PostClean, want) {
		t.Errorf("post_clean = %v, want %v", leg.PostClean, want)
	}
	hot := Build(load(t, fmt.Sprintf(hotConfig, 0)), goldenInputs())
	if pc := stepByID(t, hot, "ingest-hot-ds-c7-run1").PostClean; pc != nil {
		t.Errorf("ingest-hot post_clean = %v, want none — the query suite reads that DB", pc)
	}
}

func TestNotes(t *testing.T) {
	cases := []struct {
		name   string
		ingest string
		query  bool
		want   []string
	}{
		{
			name:   "query with cold ingest gets the cold-suite-only note",
			ingest: "cold",
			query:  true,
			want:   []string{"query = true with ingest = cold leaves no hot DB — running the cold query suite only"},
		},
		{
			name:   "query with no ingest gets the same note",
			ingest: "none",
			query:  true,
			want:   []string{"query = true with ingest = none leaves no hot DB — running the cold query suite only"},
		},
		{
			name:   "datasets-only campaign says so",
			ingest: "none",
			query:  false,
			want:   []string{"ingest = none and query = false — this campaign only prepares datasets"},
		},
		{
			name:   "a full campaign has nothing to warn about",
			ingest: "both",
			query:  true,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name = \"n\"\ningest = \"" + tc.ingest + "\"\nquery = " + boolString(tc.query) + `
runs = 1

[[dataset]]
name = "ds"
kind = "packs-local"
location = "/packs/ds"
chunks = [1]
`
			p := Build(load(t, src), goldenInputs())
			if !slices.Equal(p.Notes, tc.want) {
				t.Errorf("notes = %q, want %q", p.Notes, tc.want)
			}
		})
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestQuerySuitesFollowIngest(t *testing.T) {
	p := Build(load(t, `
name = "n"
ingest = "cold"
query = true
runs = 1

[[dataset]]
name = "ds"
kind = "packs-local"
location = "/packs/ds"
chunks = [1]
`), goldenInputs())
	for _, id := range stepIDs(p) {
		if strings.HasPrefix(id, "query-hot-") {
			t.Errorf("plan has %s, but ingest = cold leaves no hot DB", id)
		}
	}
	stepByID(t, p, "query-cold-ds-c1-run1")
}

func TestBSBS3Step(t *testing.T) {
	p := Build(load(t, `
name = "n"
ingest = "none"
query = false
runs = 1

[[dataset]]
name = "mainnet"
kind = "bsb-s3"
location = "s3://bucket/prefix"
chunks = [4]
`), goldenInputs())
	ds := stepByID(t, p, "dataset-mainnet")
	if got := ds.Env["AWS_EC2_METADATA_DISABLED"]; got != "true" {
		t.Errorf("env = %v, want AWS_EC2_METADATA_DISABLED=true", ds.Env)
	}
	if !slices.Equal(ds.Needs, []string{"build"}) {
		t.Errorf("needs = %v, want [build] — the backfill runs the binary under test", ds.Needs)
	}
	if len(ds.Argv) != 1 {
		t.Fatalf("argv = %v, want one command per chunk", ds.Argv)
	}
	if !slices.Contains(ds.Argv[0], "--cold-out-dir=/bench/golden/mainnet.partial") {
		t.Errorf("argv = %v, want it to materialize into the .partial root", ds.Argv[0])
	}
}

func TestFixtureStepGeneratesThenFreezes(t *testing.T) {
	p := buildGolden(t)
	ds := stepByID(t, p, "dataset-fix")
	if len(ds.Argv) != 4 { // 2 chunks generated, then 2 frozen
		t.Fatalf("argv has %d commands, want 4", len(ds.Argv))
	}
	for i, want := range []string{"fixture", "fixture", "cold", "cold"} {
		if got := ds.Argv[i][2]; got != want {
			t.Errorf("argv[%d] subcommand = %q, want %q", i, got, want)
		}
	}
	if ds.Dataset.Stage != "/bench/fixture/fix/ledgers" {
		t.Errorf("stage = %q, want the fixture staging pack dir", ds.Dataset.Stage)
	}
	if ds.Dataset.Location != "" {
		t.Errorf("location = %q, want it absent for a fixture", ds.Dataset.Location)
	}
	if ds.Dataset.Ledgers == nil || *ds.Dataset.Ledgers != 10000 {
		t.Errorf("ledgers = %v, want 10000", ds.Dataset.Ledgers)
	}
}

func TestTarballRunsUnconditionally(t *testing.T) {
	p := buildGolden(t)
	tarball := stepByID(t, p, "tarball")
	if len(tarball.Needs) != 0 {
		t.Errorf("tarball needs = %v, want none — the bundle matters most when legs failed", tarball.Needs)
	}
	want := []string{"tar", "-C", "/bench/results", "-czf", p.Tarball, p.RunID}
	if !slices.Equal(tarball.Argv[0], want) {
		t.Errorf("tarball argv = %v, want %v", tarball.Argv[0], want)
	}
}

func TestPublishStepRecordsIntentOnly(t *testing.T) {
	p := buildGolden(t)
	pub := stepByID(t, p, "publish")
	if len(pub.Argv) != 0 {
		t.Errorf("publish argv = %v, want none — the publish subcommand owns the tooling", pub.Argv)
	}
	if !slices.Equal(pub.Needs, []string{"tarball"}) {
		t.Errorf("publish needs = %v, want [tarball]", pub.Needs)
	}
	if pub.PublishURI != "gs://bucket/results" {
		t.Errorf("publish_uri = %q, want the configured destination", pub.PublishURI)
	}
	assertEmptyArgvJSON(t, pub)

	noPublish := Build(load(t, `
name = "n"
ingest = "none"
query = false
runs = 1

[[dataset]]
name = "ds"
kind = "packs-local"
location = "/packs/ds"
chunks = [1]
`), goldenInputs())
	for _, id := range stepIDs(noPublish) {
		if id == "publish" {
			t.Error("plan has a publish step, but publish_uri is unset")
		}
	}
}

func TestBuildIsPure(t *testing.T) {
	cfg, err := config.Load("testdata/campaign.toml")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	first := Build(cfg, goldenInputs())
	second := Build(cfg, goldenInputs())
	if !reflect.DeepEqual(first, second) {
		t.Error("two Build calls with identical inputs produced different plans")
	}
}

func TestPrint(t *testing.T) {
	p := Build(load(t, `
name = "n"
ingest = "cold"
query = true
runs = 1
publish_uri = "gs://bucket/results"

[[dataset]]
name = "mainnet"
kind = "bsb-s3"
location = "s3://bucket/prefix"
chunks = [4]
`), goldenInputs())
	var out bytes.Buffer
	p.Print(&out)
	got := out.String()
	for _, want := range []string{
		"== note: query = true with ingest = cold leaves no hot DB",
		"== build\n  $ git -C /bench/src -c advice.detachedHead=false checkout -q --detach deadbeefcafebabefeedface1234567890abcdef\n",
		"  $ env AWS_EC2_METADATA_DISABLED=true /bench/bin/stellar-rpc-deadbeef bench-ingest cold",
		"== ingest-cold-mainnet-c4-run1\n  $ rm -rf /bench/scratch/mainnet/4\n",
		"  $ campaign publish /bench/results/n-deadbeef-20260101T000000Z gs://bucket/results\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Print output missing %q, got:\n%s", want, got)
		}
	}
}
