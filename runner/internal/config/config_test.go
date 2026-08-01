package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// write puts src in a temp file and returns its path.
func write(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "campaign.toml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func load(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Load(write(t, src))
	if err != nil {
		t.Fatalf("Load = error %v, want nil", err)
	}
	return cfg
}

// minimal is the smallest accepted config: the four required things. It is
// split so that top-level keys can be added before the [[dataset]] table —
// text appended to the whole config lands inside that table instead.
const (
	minimalTop = `
name = "min"
ingest = "both"
query = true
`
	minimalDataset = `
[[dataset]]
name = "local"
kind = "packs-local"
location = "/data/packs"
chunks = [7]
`
	minimal = minimalTop + minimalDataset
)

// withTop returns the minimal config plus extra top-level keys.
func withTop(extra string) string {
	return minimalTop + extra + "\n" + minimalDataset
}

func TestLoadFullConfig(t *testing.T) {
	cfg := load(t, `
name = "phase4"
repo = "git@github.com:stellar/stellar-rpc.git"
ref = "v1.2.3"
ingest = "hot"
query = false
close_interval = "2s"
runs = 3
query_concurrency = [2, 8]
cold_iters = 50
hot_iters = 60
workers = 4
hot_num_ledgers = 1000
publish_uri = "s3://bucket/bench"

[[dataset]]
name = "pubnet"
kind = "packs-gs"
location = "gs://bucket/cold"
chunks = [1, 2]

[[dataset]]
name = "synth"
kind = "fixture"
ledgers = 10000
chunks = [0]
`)

	if cfg.Name != "phase4" {
		t.Errorf("name = %q, want phase4", cfg.Name)
	}
	if cfg.Repo != "git@github.com:stellar/stellar-rpc.git" {
		t.Errorf("repo = %q", cfg.Repo)
	}
	if cfg.Ref != "v1.2.3" {
		t.Errorf("ref = %q, want v1.2.3", cfg.Ref)
	}
	if cfg.Ingest != "hot" {
		t.Errorf("ingest = %q, want hot", cfg.Ingest)
	}
	if cfg.Query {
		t.Error("query = true, want false")
	}
	if cfg.CloseInterval != "2s" {
		t.Errorf("close_interval = %q, want 2s", cfg.CloseInterval)
	}
	if cfg.Runs != 3 {
		t.Errorf("runs = %d, want 3", cfg.Runs)
	}
	if !slices.Equal(cfg.QueryConcurrency, []int{2, 8}) {
		t.Errorf("query_concurrency = %v, want [2 8]", cfg.QueryConcurrency)
	}
	if cfg.ColdIters != 50 || cfg.HotIters != 60 {
		t.Errorf("iters = %d/%d, want 50/60", cfg.ColdIters, cfg.HotIters)
	}
	if cfg.Workers != 4 {
		t.Errorf("workers = %d, want 4", cfg.Workers)
	}
	if cfg.HotNumLedgers != 1000 {
		t.Errorf("hot_num_ledgers = %d, want 1000", cfg.HotNumLedgers)
	}
	if cfg.PublishURI != "s3://bucket/bench" {
		t.Errorf("publish_uri = %q", cfg.PublishURI)
	}
	if len(cfg.Datasets) != 2 {
		t.Fatalf("datasets = %d, want 2", len(cfg.Datasets))
	}
	gs := cfg.Datasets[0]
	if gs.Name != "pubnet" || gs.Kind != KindPacksGS || gs.Location != "gs://bucket/cold" {
		t.Errorf("dataset[0] = %+v", gs)
	}
	if !slices.Equal(gs.Chunks, []int{1, 2}) {
		t.Errorf("dataset[0].chunks = %v, want [1 2]", gs.Chunks)
	}
	if gs.Ledgers != nil {
		t.Errorf("dataset[0].ledgers = %d, want unset", *gs.Ledgers)
	}
	fixture := cfg.Datasets[1]
	if fixture.Name != "synth" || fixture.Kind != KindFixture || fixture.Location != "" {
		t.Errorf("dataset[1] = %+v", fixture)
	}
	if fixture.Ledgers == nil || *fixture.Ledgers != 10000 {
		t.Errorf("dataset[1].ledgers = %v, want 10000", fixture.Ledgers)
	}
	if !slices.Equal(fixture.Chunks, []int{0}) {
		t.Errorf("dataset[1].chunks = %v, want [0]", fixture.Chunks)
	}
}

func TestLoadMinimalConfigAppliesDefaults(t *testing.T) {
	cfg := load(t, minimal)

	if cfg.Repo != DefaultRepo {
		t.Errorf("repo = %q, want %q", cfg.Repo, DefaultRepo)
	}
	if cfg.Ref != DefaultRef {
		t.Errorf("ref = %q, want %q", cfg.Ref, DefaultRef)
	}
	if cfg.CloseInterval != "0" {
		t.Errorf("close_interval = %q, want 0", cfg.CloseInterval)
	}
	if cfg.Runs != 5 {
		t.Errorf("runs = %d, want 5", cfg.Runs)
	}
	if !slices.Equal(cfg.QueryConcurrency, []int{1, 4, 16}) {
		t.Errorf("query_concurrency = %v, want [1 4 16]", cfg.QueryConcurrency)
	}
	if cfg.ColdIters != 100 {
		t.Errorf("cold_iters = %d, want 100", cfg.ColdIters)
	}
	if cfg.HotIters != 200 {
		t.Errorf("hot_iters = %d, want 200", cfg.HotIters)
	}
	if cfg.Workers != 1 {
		t.Errorf("workers = %d, want 1", cfg.Workers)
	}
	if cfg.HotNumLedgers != 0 {
		t.Errorf("hot_num_ledgers = %d, want 0", cfg.HotNumLedgers)
	}
	if cfg.PublishURI != "" {
		t.Errorf("publish_uri = %q, want empty", cfg.PublishURI)
	}
}

func TestLoadAcceptedConfigs(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"query false", strings.Replace(minimal, "query = true", "query = false", 1)},
		{"ingest none", strings.Replace(minimal, `ingest = "both"`, `ingest = "none"`, 1)},
		{"bare zero close_interval", withTop(`close_interval = "0"`)},
		{"sub-second close_interval", withTop(`close_interval = "600ms"`)},
		{"compound close_interval", withTop(`close_interval = "1m30s"`)},
		{"gs publish uri", withTop(`publish_uri = "gs://bucket/prefix"`)},
		{"chunk id zero", strings.Replace(minimal, "chunks = [7]", "chunks = [0, 1]", 1)},
		{"fixture whole chunk", `
name = "f"
ingest = "cold"
query = true

[[dataset]]
name = "synth"
kind = "fixture"
ledgers = 0
chunks = [0]
`},
		{"bsb-s3 dataset", `
name = "b"
ingest = "cold"
query = true

[[dataset]]
name = "bsb"
kind = "bsb-s3"
location = "s3://bucket/prefix"
chunks = [1]
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.src)); err != nil {
				t.Fatalf("Load = error %v, want nil", err)
			}
		})
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // substrings the error must contain
	}{
		{
			name: "unknown top-level key",
			src:  withTop("runz = 3"),
			want: []string{"unknown key", "runz"},
		},
		{
			name: "unknown dataset key",
			src:  minimal + "\nlocations = \"/x\"\n",
			want: []string{"unknown key", "dataset.locations"},
		},
		{
			name: "several unknown keys",
			src:  withTop("runz = 3\nqc = \"1,4\""),
			want: []string{"unknown keys", "runz", "qc"},
		},
		{
			name: "missing query",
			src:  strings.Replace(minimal, "query = true", "", 1),
			want: []string{"query is required", "true|false"},
		},
		{
			name: "missing name",
			src:  strings.Replace(minimal, `name = "min"`, "", 1),
			want: []string{"name is required"},
		},
		{
			name: "bad name charset",
			src:  strings.Replace(minimal, `name = "min"`, `name = "phase 4!"`, 1),
			want: []string{"name must match [A-Za-z0-9._-]+", "phase 4!"},
		},
		{
			name: "empty repo",
			src:  withTop(`repo = ""`),
			want: []string{"repo must not be empty"},
		},
		{
			name: "relative repo path",
			src:  withTop(`repo = "../stellar-rpc"`),
			want: []string{"repo must be a git URL or an absolute local path", "../stellar-rpc"},
		},
		{
			name: "empty ref",
			src:  withTop(`ref = ""`),
			want: []string{"ref must not be empty"},
		},
		{
			name: "missing ingest",
			src:  strings.Replace(minimal, `ingest = "both"`, "", 1),
			want: []string{"ingest must be cold|hot|both|none", "<unset>"},
		},
		{
			name: "bad ingest",
			src:  strings.Replace(minimal, `ingest = "both"`, `ingest = "warm"`, 1),
			want: []string{"ingest must be cold|hot|both|none", "warm"},
		},
		{
			name: "unparsable close_interval",
			src:  withTop(`close_interval = "2 seconds"`),
			want: []string{"close_interval must be a Go duration or 0", "2 seconds"},
		},
		{
			name: "negative close_interval",
			src:  withTop(`close_interval = "-2s"`),
			want: []string{"close_interval must be a Go duration or 0", "-2s"},
		},
		{
			name: "zero runs",
			src:  withTop("runs = 0"),
			want: []string{"runs must be an integer >= 1", "0"},
		},
		{
			name: "negative cold_iters",
			src:  withTop("cold_iters = -1"),
			want: []string{"cold_iters must be an integer >= 1", "-1"},
		},
		{
			name: "zero hot_iters",
			src:  withTop("hot_iters = 0"),
			want: []string{"hot_iters must be an integer >= 1", "0"},
		},
		{
			name: "zero workers",
			src:  withTop("workers = 0"),
			want: []string{"workers must be an integer >= 1", "0"},
		},
		{
			name: "negative hot_num_ledgers",
			src:  withTop("hot_num_ledgers = -5"),
			want: []string{"hot_num_ledgers must be an integer >= 0", "-5"},
		},
		{
			name: "empty query_concurrency",
			src:  withTop("query_concurrency = []"),
			want: []string{"query_concurrency must list at least one concurrency level"},
		},
		{
			name: "zero query_concurrency entry",
			src:  withTop("query_concurrency = [1, 0]"),
			want: []string{"query_concurrency entries must be integers >= 1", "0"},
		},
		{
			name: "bad publish_uri scheme",
			src:  withTop(`publish_uri = "https://bucket/bench"`),
			want: []string{"publish_uri must be a gs:// or s3:// URI", "https://bucket/bench"},
		},
		{
			name: "no datasets",
			src: `
name = "nods"
ingest = "both"
query = true
`,
			want: []string{"at least one [[dataset]] is required"},
		},
		{
			name: "bad dataset name charset",
			src:  strings.Replace(minimal, `name = "local"`, `name = "my data"`, 1),
			want: []string{"dataset name must match [A-Za-z0-9._-]+", "my data"},
		},
		{
			name: "duplicate dataset names",
			src: minimal + `
[[dataset]]
name = "local"
kind = "packs-local"
location = "/data/other"
chunks = [8]
`,
			want: []string{"duplicate dataset name", "local"},
		},
		{
			name: "empty chunks",
			src:  strings.Replace(minimal, "chunks = [7]", "chunks = []", 1),
			want: []string{"dataset 'local'", "chunks must list at least one chunk ID"},
		},
		{
			name: "negative chunk id",
			src:  strings.Replace(minimal, "chunks = [7]", "chunks = [1, -2]", 1),
			want: []string{"dataset 'local'", "chunk IDs must be non-negative integers", "-2"},
		},
		{
			name: "duplicate chunk id",
			src:  strings.Replace(minimal, "chunks = [7]", "chunks = [1, 1]", 1),
			want: []string{"dataset 'local'", "duplicate chunk ID", "1"},
		},
		{
			name: "unknown dataset kind",
			src:  strings.Replace(minimal, `kind = "packs-local"`, `kind = "packs-http"`, 1),
			want: []string{"dataset 'local'", "kind must be packs-local|packs-gs|bsb-s3|fixture", "packs-http"},
		},
		{
			name: "packs-local without location",
			src:  strings.Replace(minimal, `location = "/data/packs"`, "", 1),
			want: []string{"dataset 'local'", "packs-local location must be a local cold pack root"},
		},
		{
			name: "packs-gs location is not gs://",
			src: strings.NewReplacer(
				`kind = "packs-local"`, `kind = "packs-gs"`,
				`location = "/data/packs"`, `location = "s3://bucket/cold"`,
			).Replace(minimal),
			want: []string{"dataset 'local'", "packs-gs location must start with gs://", "s3://bucket/cold"},
		},
		{
			name: "bsb-s3 without location",
			src: strings.NewReplacer(
				`kind = "packs-local"`, `kind = "bsb-s3"`,
				`location = "/data/packs"`, "",
			).Replace(minimal),
			want: []string{"dataset 'local'", "bsb-s3 location must be an S3 bucket path"},
		},
		{
			name: "fixture with a location",
			src: strings.NewReplacer(
				`kind = "packs-local"`, `kind = "fixture"`,
				`location = "/data/packs"`, "location = \"/data/packs\"\nledgers = 10000",
			).Replace(minimal),
			want: []string{"dataset 'local'", "fixture datasets use ledgers, not location", "/data/packs"},
		},
		{
			name: "fixture without ledgers",
			src: strings.NewReplacer(
				`kind = "packs-local"`, `kind = "fixture"`,
				`location = "/data/packs"`, "",
			).Replace(minimal),
			want: []string{"dataset 'local'", "fixture datasets need ledgers", ">= 10000"},
		},
		{
			name: "fixture with a partial chunk",
			src: strings.NewReplacer(
				`kind = "packs-local"`, `kind = "fixture"`,
				`location = "/data/packs"`, "ledgers = 5000",
			).Replace(minimal),
			want: []string{
				"dataset 'local'",
				"fixture ledger count must be 0 or >= 10000",
				"the cold freeze streams the whole 10,000-ledger chunk",
				"5000",
			},
		},
		{
			name: "non-fixture with ledgers",
			src:  strings.Replace(minimal, "chunks = [7]", "chunks = [7]\nledgers = 10000", 1),
			want: []string{"dataset 'local'", "ledgers is only valid for fixture datasets", "packs-local"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.src))
			if err == nil {
				t.Fatal("Load = nil error, want a rejection")
			}
			if !strings.HasPrefix(err.Error(), "config: ") {
				t.Errorf("error %q does not start with %q", err, "config: ")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil {
		t.Fatal("Load = nil error, want an error for a missing file")
	}
	if !strings.HasPrefix(err.Error(), "config: ") {
		t.Errorf("error %q does not start with %q", err, "config: ")
	}
}

func TestLoadLocalRepoPath(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	if _, err := Load(write(t, withTop(`repo = "`+dir+`"`))); err != nil {
		t.Fatalf("Load with a local git repo = error %v, want nil", err)
	}

	notARepo := t.TempDir()
	_, err := Load(write(t, withTop(`repo = "`+notARepo+`"`)))
	if err == nil {
		t.Fatal("Load with a non-git absolute path = nil error, want a rejection")
	}
	for _, want := range []string{"is not a git repository", notARepo} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestMetadataMappings(t *testing.T) {
	cfg := load(t, `
name = "map"
ingest = "both"
query = true
query_concurrency = [1, 4, 16]

[[dataset]]
name = "local"
kind = "packs-local"
location = "/data/packs"
chunks = [1]

[[dataset]]
name = "synth"
kind = "fixture"
ledgers = 20000
chunks = [0]
`)

	if got := cfg.QueryString(); got != "yes" {
		t.Errorf("QueryString() = %q, want yes", got)
	}
	cfg.Query = false
	if got := cfg.QueryString(); got != "no" {
		t.Errorf("QueryString() = %q, want no", got)
	}
	if got := cfg.QueryConcurrencyString(); got != "1,4,16" {
		t.Errorf("QueryConcurrencyString() = %q, want 1,4,16", got)
	}
	if got := cfg.Datasets[0].LocationString(); got != "/data/packs" {
		t.Errorf("LocationString() = %q, want /data/packs", got)
	}
	if got := cfg.Datasets[1].LocationString(); got != "20000" {
		t.Errorf("fixture LocationString() = %q, want 20000", got)
	}
}

func TestQueryConcurrencyStringSingleEntry(t *testing.T) {
	cfg := load(t, withTop("query_concurrency = [8]"))
	if got := cfg.QueryConcurrencyString(); got != "8" {
		t.Errorf("QueryConcurrencyString() = %q, want 8", got)
	}
}
