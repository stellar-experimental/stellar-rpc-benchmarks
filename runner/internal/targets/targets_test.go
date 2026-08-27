package targets

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoTargets is this checkout's own docs/targets.json — the file every real
// campaign resolves, so the numbers these tests assert are the numbers the
// runner paces at.
const repoTargets = "../../../docs/targets.json"

func loadRepo(t *testing.T) *Targets {
	t.Helper()
	tg, err := Load(repoTargets)
	if err != nil {
		t.Fatalf("Load(%s) = error %v, want nil", repoTargets, err)
	}
	return tg
}

func TestLoadRepoTargets(t *testing.T) {
	tg := loadRepo(t)
	if !slices.Equal(tg.QueryLoad.Ladder, []float64{0.5, 1, 2}) {
		t.Errorf("ladder = %v, want [0.5 1 2]", tg.QueryLoad.Ladder)
	}
	for _, name := range []string{"sac", "custom_token", "soroswap"} {
		if _, ok := tg.QueryLoad.Profiles[name]; !ok {
			t.Errorf("query_load has no profile %q", name)
		}
	}
	if len(tg.Phases) != 3 {
		t.Fatalf("phases = %d, want 3", len(tg.Phases))
	}
	for i, want := range []int64{2000000000, 1000000000, 600000000} {
		if got := tg.Phases[i].BlockTimeNs; got != want {
			t.Errorf("phase %d block_time_ns = %d, want %d", tg.Phases[i].Phase, got, want)
		}
	}
}

func TestRates(t *testing.T) {
	tg := loadRepo(t)
	cases := []struct {
		profile string
		phase   int
		qtype   string
		want    []float64
	}{
		{"sac", 1, "txhash", []float64{150, 300, 600}},
		{"sac", 3, "txhash", []float64{500, 1000, 2000}},
		{"sac", 3, "ledgers", []float64{0.835, 1.67, 3.34}},
		{"sac", 2, "txpage", []float64{12.5, 25, 50}},
		{"sac", 1, "events", []float64{1.5, 3, 6}},
		{"custom_token", 2, "txhash", []float64{200, 400, 800}},
		{"soroswap", 1, "txpage", []float64{1.875, 3.75, 7.5}},
		{"soroswap", 3, "events", []float64{7.5, 15, 30}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s-phase%d-%s", tc.profile, tc.phase, tc.qtype), func(t *testing.T) {
			got, err := tg.Rates(tc.profile, tc.phase, tc.qtype)
			if err != nil {
				t.Fatalf("Rates = error %v, want nil", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Rates(%s, %d, %s) = %v, want %v", tc.profile, tc.phase, tc.qtype, got, tc.want)
			}
		})
	}
}

func TestRatesRejects(t *testing.T) {
	tg := loadRepo(t)
	cases := []struct {
		name    string
		profile string
		phase   int
		qtype   string
		want    []string
	}{
		{"unknown profile", "pubnet", 1, "txhash", []string{"no query_load profile 'pubnet'", "sac"}},
		{"unknown query type", "sac", 1, "blocks", []string{"unknown query type 'blocks'", "ledgers"}},
		{"phase below the range", "sac", 0, "txhash", []string{"phase 0 has no floors", "phases 1-3"}},
		{"phase above the range", "sac", 4, "txhash", []string{"phase 4 has no floors", "phases 1-3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tg.Rates(tc.profile, tc.phase, tc.qtype)
			if err == nil {
				t.Fatal("Rates = nil error, want a rejection")
			}
			if !strings.HasPrefix(err.Error(), "targets: ") {
				t.Errorf("error %q does not start with %q", err, "targets: ")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestProfileKey(t *testing.T) {
	cases := []struct{ name, want string }{
		{"sac-6000", "sac"},
		{"custom_token-3600", "custom_token"},
		{"soroswap-1500", "soroswap"},
		{"sac", "sac"},
		{"pubnet-063a", "pubnet-063a"}, // not a decimal suffix: 063a keeps its name
		{"sac-6000-b", "sac-6000-b"},   // the count must be the last segment
	}
	for _, tc := range cases {
		if got := ProfileKey(tc.name); got != tc.want {
			t.Errorf("ProfileKey(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMatchPhase(t *testing.T) {
	tg := loadRepo(t)
	cases := []struct {
		ns   int64
		want int
	}{
		{2000000000, 1},
		{1000000000, 2},
		{600000000, 3},
		{0, 0},
		{500000000, 0}, // near phase 3, but not it
	}
	for _, tc := range cases {
		if got := tg.MatchPhase(tc.ns); got != tc.want {
			t.Errorf("MatchPhase(%d) = %d, want %d", tc.ns, got, tc.want)
		}
	}
}

// fixture writes a targets file body to a temp path and returns it.
func fixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "targets.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadRejects(t *testing.T) {
	// A two-phase file with one whole profile, edited per case below.
	const good = `{
  "query_load": {
    "ladder": [1, 2],
    "profiles": {
      "sac": {
        "ledgers_rps": [1, 2],
        "txpage_rps": [10, 20],
        "txhash_rps": [100, 200],
        "events_rps": [3, 6]
      }
    }
  },
  "phases": [{"phase": 1, "block_time_ns": 2000000000}, {"phase": 2, "block_time_ns": 1000000000}]
}`
	if _, err := Load(fixture(t, good)); err != nil {
		t.Fatalf("Load(good fixture) = error %v, want nil", err)
	}

	cases := []struct {
		name string
		body string
		want []string
	}{
		{"not json", "{", []string{"unexpected end"}},
		{
			name: "no query_load",
			body: `{"phases": [{"phase": 1, "block_time_ns": 2000000000}]}`,
			want: []string{"query_load.profiles is empty", "paced-RPS"},
		},
		{
			name: "no phases",
			body: strings.Replace(good, `"phases": [{"phase": 1, "block_time_ns": 2000000000}, {"phase": 2, "block_time_ns": 1000000000}]`, `"phases": []`, 1),
			want: []string{"phases is empty"},
		},
		{
			name: "empty ladder",
			body: strings.Replace(good, `"ladder": [1, 2]`, `"ladder": []`, 1),
			want: []string{"query_load.ladder must list at least one multiplier"},
		},
		{
			name: "short floor array",
			body: strings.Replace(good, `"txhash_rps": [100, 200]`, `"txhash_rps": [100]`, 1),
			want: []string{"query_load.profiles.sac.txhash_rps has 1 entries", "one per phase (2)"},
		},
		{
			name: "missing floor array",
			body: strings.Replace(good, `"events_rps": [3, 6]`, `"events_rps": []`, 1),
			want: []string{"query_load.profiles.sac.events_rps has 0 entries"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(fixture(t, tc.body))
			if err == nil {
				t.Fatal("Load = nil error, want a rejection")
			}
			if !strings.HasPrefix(err.Error(), "targets: ") {
				t.Errorf("error %q does not start with %q", err, "targets: ")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("Load = nil error, want an error for a missing file")
	}
	if !strings.HasPrefix(err.Error(), "targets: ") {
		t.Errorf("error %q does not start with %q", err, "targets: ")
	}
}

func TestFind(t *testing.T) {
	// The package's own directory is several levels below the checkout root, so
	// a successful walk proves the parent search, not a lucky cwd.
	found, err := Find(".")
	if err != nil {
		t.Fatalf("Find(.) = error %v, want this checkout's docs/targets.json", err)
	}
	want, err := filepath.Abs(repoTargets)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if found != want {
		t.Errorf("Find(.) = %q, want %q", found, want)
	}
}

func TestFindReachesTheRootAndSaysHow(t *testing.T) {
	// A temp dir has no checkout above it on any supported platform.
	_, err := Find(t.TempDir())
	if err == nil {
		t.Fatal("Find = nil error, want a not-found error")
	}
	for _, want := range []string{"no " + FileName, "--targets="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestFindPrefersTheNearestCheckout(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(deep, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, dir := range []string{root, deep} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got, err := Find(deep)
	if err != nil {
		t.Fatalf("Find = error %v, want nil", err)
	}
	if want := filepath.Join(deep, FileName); got != want {
		t.Errorf("Find = %q, want the nearest copy %q", got, want)
	}
}
