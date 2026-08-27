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
		BenchRoot:     "/bench",
		BuiltCommit:   "deadbeefcafebabefeedface1234567890abcdef",
		Sha8:          "deadbeef",
		Stamp:         "20260101T000000Z",
		QueryRates:    goldenRates(),
		QueryDuration: "60s",
	}
}

// goldenRates stands in for what the CLI resolves out of docs/targets.json: the
// phase-1 ladders of the profiles these tests' datasets name. Build never reads
// that file, so pinning the numbers keeps the golden plan a function of the
// config alone.
func goldenRates() map[string]map[string][]float64 {
	sac := map[string][]float64{
		"ledgers": {0.25, 0.5, 1},
		"txpage":  {7.5, 15, 30},
		"txhash":  {150, 300, 600},
		"events":  {1.5, 3, 6},
	}
	soroswap := map[string][]float64{
		"ledgers": {0.25, 0.5, 1},
		"txpage":  {1.875, 3.75, 7.5},
		"txhash":  {37.5, 75, 150},
		"events":  {1.875, 3.75, 7.5},
	}
	rates := map[string]map[string][]float64{"soroswap-1500": soroswap}
	// The single-dataset configs below name their dataset for what it is rather
	// than for a profile; they all benchmark the sac ladders here.
	for _, dataset := range []string{"sac-6000", "ds", "mainnet"} {
		rates[dataset] = sac
	}
	return rates
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
		// runs = 2, so every rep — not just rep 2 — waits on rep 2. The ingest
		// legs are per cell, not per query type, so the type drops off the id.
		cell, _, _ := strings.Cut(strings.TrimPrefix(s.ID, "query-hot-"), "-run")
		cell = cell[:strings.LastIndex(cell, "-")]
		want := "ingest-hot-" + cell + "-run2"
		if !slices.Contains(s.Needs, want) {
			t.Errorf("%s needs %v, want it to include %q", s.ID, s.Needs, want)
		}
		if !slices.Contains(s.Needs, "build") {
			t.Errorf("%s needs %v, want it to include \"build\"", s.ID, s.Needs)
		}
	}
	if seen != 32 { // 2 datasets x 2 chunks x 2 reps x 4 query types
		t.Errorf("found %d query-hot steps, want 32", seen)
	}
}

// TestQueryLegsAreOneTypeEach pins the shape of the paced query suites: a leg
// per endpoint type, each driving that type's own rate ladder for one duration.
func TestQueryLegsAreOneTypeEach(t *testing.T) {
	p := buildGolden(t)
	cases := []struct {
		id   string
		want []string
	}{
		{"query-cold-sac-6000-c1-ledgers-run1", []string{
			"--types=ledgers", "--target-rps=0.25,0.5,1", "--duration=60s",
		}},
		{"query-cold-sac-6000-c1-txhash-run1", []string{
			"--types=txhash", "--target-rps=150,300,600", "--duration=60s",
		}},
		{"query-hot-soroswap-1500-c2-events-run2", []string{
			"--types=events", "--target-rps=1.875,3.75,7.5", "--duration=60s", "--warmup=20",
		}},
		{"query-hot-soroswap-1500-c2-txpage-run2", []string{
			"--types=txpage", "--target-rps=1.875,3.75,7.5", "--duration=60s",
		}},
	}
	for _, tc := range cases {
		argv := stepByID(t, p, tc.id).Argv[0]
		for _, want := range tc.want {
			if !slices.Contains(argv, want) {
				t.Errorf("%s argv = %v, want it to contain %q", tc.id, argv, want)
			}
		}
		for _, gone := range []string{"--iters=", "--query-concurrency="} {
			if slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, gone) }) {
				t.Errorf("%s argv = %v, want no %s flag — the query suites are open-loop", tc.id, argv, gone)
			}
		}
		if last := argv[len(argv)-1]; !strings.HasPrefix(last, "--out=") {
			t.Errorf("%s last arg = %q, want an --out flag", tc.id, last)
		}
	}

	// Every cell produces the four types, in queryTypes order.
	var got []string
	for _, s := range p.Steps {
		if strings.HasPrefix(s.ID, "query-cold-sac-6000-c1-") {
			cell, _, _ := strings.Cut(strings.TrimPrefix(s.ID, "query-cold-sac-6000-c1-"), "-run")
			got = append(got, cell)
		}
	}
	want := append(append([]string{}, queryTypes...), queryTypes...) // two reps
	if !slices.Equal(got, want) {
		t.Errorf("query-cold types = %v, want %v", got, want)
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
		{capped, "query-hot-ds-c7-ledgers-run1", "--sample-ledgers=50000", true},
		{uncapped, "query-hot-ds-c7-ledgers-run1", "--sample-ledgers=", false},
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
	for _, qtype := range queryTypes {
		stepByID(t, p, "query-cold-ds-c1-"+qtype+"-run1")
	}
}

// TestQueryTypesIsACopy guards the accessor the CLI resolves rates through: a
// caller that reorders what it gets back must not reorder the legs.
func TestQueryTypesIsACopy(t *testing.T) {
	got := QueryTypes()
	if !slices.Equal(got, queryTypes) {
		t.Fatalf("QueryTypes() = %v, want %v", got, queryTypes)
	}
	got[0] = "clobbered"
	if queryTypes[0] == "clobbered" {
		t.Error("QueryTypes() handed out the package's own slice")
	}
}

// packsS3Config is a datasets-only campaign over one fetched s3:// pack tree.
const packsS3Config = `
name = "n"
ingest = "none"
query = false
runs = 1

[[dataset]]
name = "packs"
kind = "packs-s3"
location = "s3://bucket/cold"
chunks = [1]
`

func TestPacksS3Step(t *testing.T) {
	p := Build(load(t, packsS3Config), goldenInputs())
	ds := stepByID(t, p, "dataset-packs")
	want := []string{"aws", "s3", "sync", "s3://bucket/cold", "/bench/golden/packs.partial"}
	if len(ds.Argv) != 1 || !slices.Equal(ds.Argv[0], want) {
		t.Errorf("argv = %v, want one command %v", ds.Argv, want)
	}
	if len(ds.Needs) != 0 {
		t.Errorf("needs = %v, want none — a fetch does not run the binary under test", ds.Needs)
	}
	// The instance role is what reads this private bucket, so unlike bsb-s3 the
	// metadata endpoint must stay reachable.
	if len(ds.Env) != 0 {
		t.Errorf("env = %v, want none — the fetch signs with the machine's instance role", ds.Env)
	}
	if ds.Dataset.Root != "/bench/golden/packs" {
		t.Errorf("root = %q, want the campaign-owned golden directory", ds.Dataset.Root)
	}
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
	ds := stepByID(t, p, "dataset-soroswap-1500")
	if len(ds.Argv) != 4 { // 2 chunks generated, then 2 frozen
		t.Fatalf("argv has %d commands, want 4", len(ds.Argv))
	}
	for i, want := range []string{"fixture", "fixture", "cold", "cold"} {
		if got := ds.Argv[i][2]; got != want {
			t.Errorf("argv[%d] subcommand = %q, want %q", i, got, want)
		}
	}
	if ds.Dataset.Stage != "/bench/fixture/soroswap-1500/ledgers" {
		t.Errorf("stage = %q, want the fixture staging pack dir", ds.Dataset.Stage)
	}
	if ds.Dataset.Location != "" {
		t.Errorf("location = %q, want it absent for a fixture", ds.Dataset.Location)
	}
	if ds.Dataset.Ledgers == nil || *ds.Dataset.Ledgers != 10000 {
		t.Errorf("ledgers = %v, want 10000", ds.Dataset.Ledgers)
	}
}

func TestDatasetPreClean(t *testing.T) {
	p := buildGolden(t)
	// packs-gs clears the leftover root so the rename cannot nest the partial
	// inside it, but keeps the partial itself: rsync resumes into it.
	if got, want := stepByID(t, p, "dataset-sac-6000").PreClean, []string{"/bench/golden/sac-6000"}; !slices.Equal(got, want) {
		t.Errorf("packs-gs pre_clean = %v, want %v — the .partial is resumable", got, want)
	}
	// A fixture wipes the staging tree by its parent, plus both roots.
	want := []string{"/bench/fixture/soroswap-1500", "/bench/golden/soroswap-1500", "/bench/golden/soroswap-1500.partial"}
	if got := stepByID(t, p, "dataset-soroswap-1500").PreClean; !slices.Equal(got, want) {
		t.Errorf("fixture pre_clean = %v, want %v", got, want)
	}

	// packs-s3 fetches, so it keeps its partial for the same reason packs-gs does.
	s3 := Build(load(t, packsS3Config), goldenInputs())
	if got, want := stepByID(t, s3, "dataset-packs").PreClean, []string{"/bench/golden/packs"}; !slices.Equal(got, want) {
		t.Errorf("packs-s3 pre_clean = %v, want %v — the .partial is resumable", got, want)
	}

	bsb := Build(load(t, `
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
	want = []string{"/bench/golden/mainnet", "/bench/golden/mainnet.partial"}
	if got := stepByID(t, bsb, "dataset-mainnet").PreClean; !slices.Equal(got, want) {
		t.Errorf("bsb-s3 pre_clean = %v, want %v — a cold backfill cannot resume", got, want)
	}

	local := Build(load(t, fmt.Sprintf(hotConfig, 0)), goldenInputs())
	if got := stepByID(t, local, "dataset-ds").PreClean; got != nil {
		t.Errorf("packs-local pre_clean = %v, want none — those packs are the operator's", got)
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
		// The whole pre_clean list on one line, as the executor wipes it.
		"== dataset-mainnet\n  $ rm -rf /bench/golden/mainnet /bench/golden/mainnet.partial\n",
		"  $ mv /bench/golden/mainnet.partial /bench/golden/mainnet\n",
		"  $ campaign publish /bench/results/n-deadbeef-20260101T000000Z gs://bucket/results\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Print output missing %q, got:\n%s", want, got)
		}
	}
	// The backfill writes its own --cold-out-dir; only a fetch needs the
	// partial to exist first.
	if strings.Contains(got, "mkdir -p") {
		t.Errorf("Print output has a mkdir for a bsb-s3 dataset, got:\n%s", got)
	}
}

// TestPrintDatasetChoreography pins the destructive half of a dataset
// preparation in the dry run: the wipes, the partial, and the rename are the
// executor's, so nothing but this printer would show them.
func TestPrintDatasetChoreography(t *testing.T) {
	var out bytes.Buffer
	buildGolden(t).Print(&out)
	got := out.String()
	for _, want := range []string{
		"== dataset-sac-6000\n" +
			"  $ rm -rf /bench/golden/sac-6000\n" +
			"  $ mkdir -p /bench/golden/sac-6000.partial\n" +
			"  $ gcloud storage rsync -r gs://bucket/cold /bench/golden/sac-6000.partial\n" +
			"  $ mv /bench/golden/sac-6000.partial /bench/golden/sac-6000\n",
		"== dataset-soroswap-1500\n  $ rm -rf /bench/fixture/soroswap-1500 " +
			"/bench/golden/soroswap-1500 /bench/golden/soroswap-1500.partial\n",
		"  $ mv /bench/golden/soroswap-1500.partial /bench/golden/soroswap-1500\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Print output missing %q, got:\n%s", want, got)
		}
	}
	// A fixture freezes straight into --cold-out-dir, so it gets no mkdir.
	if strings.Contains(got, "mkdir -p /bench/golden/soroswap-1500.partial") {
		t.Errorf("Print output has a mkdir for the fixture partial, got:\n%s", got)
	}

	// The other fetching kind prints the same choreography around the other CLI.
	var s3Out bytes.Buffer
	Build(load(t, packsS3Config), goldenInputs()).Print(&s3Out)
	want := "== dataset-packs\n" +
		"  $ rm -rf /bench/golden/packs\n" +
		"  $ mkdir -p /bench/golden/packs.partial\n" +
		"  $ aws s3 sync s3://bucket/cold /bench/golden/packs.partial\n" +
		"  $ mv /bench/golden/packs.partial /bench/golden/packs\n"
	if !strings.Contains(s3Out.String(), want) {
		t.Errorf("Print output missing %q, got:\n%s", want, s3Out.String())
	}
}
