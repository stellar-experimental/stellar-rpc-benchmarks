package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// --- helpers --------------------------------------------------------------

const (
	testName   = "nvme-full"
	testCommit = "1e0e7f9c0d2b4a6f8e0a1c3d5f7b9d1e2a4c6e80"
	testSha8   = "1e0e7f9c"
	testStamp  = "20260715T101500Z"
	testRunID  = testName + "-" + testSha8 + "-" + testStamp
	testStart  = "2026-07-15T10:15:00Z"
)

const testCfgBody = `name = "nvme-full"
ref = "feature/full-history"
ingest = "both"
query = true
runs = 5

[[dataset]]
name = "pubnet"
kind = "packs-local"
location = "/mnt/nvme/packs"
chunks = [63]
`

// bashMetadata is the manifest shape write_campaign_metadata emits, extra
// fields and all: readers must ignore what they do not model.
func bashMetadata(runID, name, configFile, builtCommit, startedAt string) string {
	return `{
  "schema_version": 1,
  "run_id": "` + runID + `",
  "campaign": {
    "name": "` + name + `",
    "config_file": "` + configFile + `",
    "ref": "feature/full-history",
    "built_commit": "` + builtCommit + `",
    "ingest": "both",
    "query": "yes",
    "close_interval": "0",
    "runs": 5,
    "query_concurrency": "1,4,16",
    "cold_iters": 100,
    "hot_iters": 200,
    "workers": 1,
    "hot_num_ledgers": 0,
    "resumed": true
  },
  "datasets": [
    {"name": "pubnet", "kind": "packs-local", "location": "/mnt/nvme/packs", "chunks": [63]}
  ],
  "hardware": {"instance_type": "i4i.4xlarge", "uname": "Linux 6.8.0 x86_64", "cpus": 16},
  "hostname": "bench-devbox",
  "started_at": "` + startedAt + `"
}
`
}

type bundleOpts struct {
	runID       string
	name        string
	configFile  string // name of the stored copy; "" means write no copy
	metaConfig  string // config_file as recorded in metadata.json; "" uses configFile
	builtCommit string
	startedAt   string
	storedCfg   string // stored copy's body; "" uses testCfgBody
	metadata    string // whole metadata.json body; "" builds the bash shape
	noMetadata  bool
}

// makeBundle writes <benchRoot>/results/<run_id> and returns benchRoot and the
// bundle dir.
func makeBundle(t *testing.T, o bundleOpts) (benchRoot, dir string) {
	t.Helper()
	if o.runID == "" {
		o.runID = testRunID
	}
	if o.name == "" {
		o.name = testName
	}
	if o.builtCommit == "" {
		o.builtCommit = testCommit
	}
	if o.metaConfig == "" {
		o.metaConfig = o.configFile
	}
	benchRoot = t.TempDir()
	dir = filepath.Join(benchRoot, "results", o.runID)
	mustMkdir(t, dir)
	if !o.noMetadata {
		body := o.metadata
		if body == "" {
			body = bashMetadata(o.runID, o.name, o.metaConfig, o.builtCommit, o.startedAt)
		}
		mustWrite(t, filepath.Join(dir, MetadataName), body)
	}
	if o.configFile != "" {
		stored := o.storedCfg
		if stored == "" {
			stored = testCfgBody
		}
		mustWrite(t, filepath.Join(dir, o.configFile), stored)
	}
	return benchRoot, dir
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// currentConfig writes the config the resuming session would pass on the
// command line.
func currentConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "campaign.cfg")
	mustWrite(t, path, body)
	return path
}

func testConfig() *config.Config {
	return &config.Config{Name: testName, Ref: "feature/full-history"}
}

// validate runs ValidateResume with the standard inputs, capturing diff output.
func validate(t *testing.T, dir, cfgPath string, cfg *config.Config, builtCommit, benchRoot string) (*Resume, string, error) {
	t.Helper()
	var diff bytes.Buffer
	r, err := ValidateResume(dir, cfgPath, cfg, builtCommit, benchRoot, &diff)
	return r, diff.String(), err
}

// --- ReadMetadata ---------------------------------------------------------

func TestReadMetadataBashShape(t *testing.T) {
	_, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg", startedAt: testStart})
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.SchemaVersion != 1 || meta.RunID != testRunID {
		t.Errorf("schema_version/run_id = %d/%q, want 1/%q", meta.SchemaVersion, meta.RunID, testRunID)
	}
	if meta.Campaign.Name != testName || meta.Campaign.ConfigFile != "campaign.cfg" {
		t.Errorf("campaign name/config_file = %q/%q", meta.Campaign.Name, meta.Campaign.ConfigFile)
	}
	if meta.Campaign.BuiltCommit != testCommit || meta.Campaign.Ref != "feature/full-history" {
		t.Errorf("campaign built_commit/ref = %q/%q", meta.Campaign.BuiltCommit, meta.Campaign.Ref)
	}
	if meta.StartedAt != testStart {
		t.Errorf("started_at = %q, want %q", meta.StartedAt, testStart)
	}
	// A bash-era bundle has neither field; absent status is unknown, not an error.
	if meta.Status != "" {
		t.Errorf("status = %q, want \"\" for a bash-era bundle", meta.Status)
	}
	if meta.FinishedAt != "" {
		t.Errorf("finished_at = %q, want \"\"", meta.FinishedAt)
	}
}

func TestReadMetadataStatus(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, MetadataName),
		`{"run_id":"x","status":"`+StatusFinished+`","finished_at":"2026-07-15T20:00:00Z"}`)
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.Status != StatusFinished || meta.FinishedAt != "2026-07-15T20:00:00Z" {
		t.Errorf("status/finished_at = %q/%q", meta.Status, meta.FinishedAt)
	}
}

func TestReadMetadataErrors(t *testing.T) {
	if _, err := ReadMetadata(t.TempDir()); err == nil {
		t.Fatal("ReadMetadata of a bundle with no metadata.json: want error")
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, MetadataName), "{not json")
	if _, err := ReadMetadata(dir); err == nil {
		t.Fatal("ReadMetadata of corrupt metadata.json: want error")
	}
}

// --- ValidateResume: happy paths ------------------------------------------

func TestValidateResumeHappyPath(t *testing.T) {
	benchRoot, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg", startedAt: testStart})
	cfgPath := currentConfig(t, testCfgBody)

	r, diff, err := validate(t, dir, cfgPath, testConfig(), testCommit, benchRoot)
	if err != nil {
		t.Fatalf("ValidateResume: %v", err)
	}
	if diff != "" {
		t.Errorf("diff output on an identical config: %q", diff)
	}
	if r.Dir != dir || r.RunID != testRunID {
		t.Errorf("Dir/RunID = %q/%q", r.Dir, r.RunID)
	}
	if r.Sha8 != testSha8 || r.Stamp != testStamp {
		t.Errorf("Sha8/Stamp = %q/%q, want %q/%q", r.Sha8, r.Stamp, testSha8, testStamp)
	}
	if r.StartedAt != testStart {
		t.Errorf("StartedAt = %q, want %q", r.StartedAt, testStart)
	}
}

// A bundle from before metadata.json was written up front has no started_at:
// resumable, with the caller left to record this session's start.
func TestValidateResumeNoStartedAt(t *testing.T) {
	benchRoot, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg"})
	r, _, err := validate(t, dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot)
	if err != nil {
		t.Fatalf("ValidateResume: %v", err)
	}
	if r.StartedAt != "" {
		t.Errorf("StartedAt = %q, want \"\"", r.StartedAt)
	}
}

func TestValidateResumeNameWithDashes(t *testing.T) {
	const name = "my-camp-aign"
	runID := name + "-deadbeef-" + testStamp
	benchRoot, dir := makeBundle(t, bundleOpts{runID: runID, name: name, configFile: "campaign.cfg"})
	cfg := testConfig()
	cfg.Name = name

	r, _, err := validate(t, dir, currentConfig(t, testCfgBody), cfg, testCommit, benchRoot)
	if err != nil {
		t.Fatalf("ValidateResume: %v", err)
	}
	if r.Sha8 != "deadbeef" || r.Stamp != testStamp {
		t.Errorf("Sha8/Stamp = %q/%q, want deadbeef/%q", r.Sha8, r.Stamp, testStamp)
	}
}

// --- ValidateResume: refusals ---------------------------------------------

func TestValidateResumeRefusals(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the dir to resume, the current config's path, the
		// config, the commit the ref now resolves to, and the bench root.
		setup func(t *testing.T) (dir, cfgPath string, cfg *config.Config, builtCommit, benchRoot string)
		want  string
	}{
		{
			name: "missing dir",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot := t.TempDir()
				return filepath.Join(benchRoot, "results", testRunID), currentConfig(t, testCfgBody),
					testConfig(), testCommit, benchRoot
			},
			want: "--resume: cannot read",
		},
		{
			name: "not a directory",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot := t.TempDir()
				dir := filepath.Join(benchRoot, "results", testRunID)
				mustMkdir(t, filepath.Dir(dir))
				mustWrite(t, dir, "not a bundle")
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "is not a directory",
		},
		{
			name: "missing metadata.json",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{noMetadata: true, configFile: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "no readable metadata.json in",
		},
		{
			name: "corrupt metadata.json",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{
					metadata: `{"run_id": "` + testRunID + `", "campaign": {`, configFile: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "no readable metadata.json in",
		},
		{
			name: "malformed run_id",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{runID: "nvme-full-20260715", configFile: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "is not a <NAME>-<sha>-<stamp> run id",
		},
		{
			name: "campaign name mismatch",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{name: "other-campaign", configFile: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "belongs to campaign 'other-campaign', but this config's name is 'nvme-full'",
		},
		{
			// The run id's short sha still matches: only the full built_commit
			// in metadata.json can tell these two apart.
			name: "built_commit mismatch",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{
					builtCommit: testSha8 + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", configFile: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "resuming would mix two binaries in one bundle",
		},
		{
			name: "wrong bench root",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				_, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, t.TempDir()
			},
			want: "is not this BENCH_ROOT's results directory (expected",
		},
		{
			name: "no config_file recorded",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "records no config_file",
		},
		{
			// The traversal target is a byte-identical config: without the
			// guard this resume would be accepted, so the refusal proves the
			// file outside the bundle is never read.
			name: "config_file escapes the bundle root",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{metaConfig: "../evil.toml"})
				mustWrite(t, filepath.Join(filepath.Dir(dir), "evil.toml"), testCfgBody)
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "records a config_file that is not a bundle-root filename ('../evil.toml') — " +
				"refusing to compare against a path outside the bundle",
		},
		{
			name: "stored config copy missing",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{metaConfig: "campaign.cfg"})
				return dir, currentConfig(t, testCfgBody), testConfig(), testCommit, benchRoot
			},
			want: "cannot read the config this campaign started with",
		},
		{
			name: "current config unreadable",
			setup: func(t *testing.T) (string, string, *config.Config, string, string) {
				benchRoot, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg"})
				return dir, filepath.Join(t.TempDir(), "gone.cfg"), testConfig(), testCommit, benchRoot
			},
			want: "--resume: cannot read",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfgPath, cfg, builtCommit, benchRoot := tc.setup(t)
			r, _, err := validate(t, dir, cfgPath, cfg, builtCommit, benchRoot)
			if err == nil {
				t.Fatalf("ValidateResume: want refusal, got %+v", r)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "--resume: ") {
				t.Errorf("error = %q, want the --resume: prefix", err)
			}
		})
	}
}

// The guard finding 1 is about: an edited config is refused, the operator is
// shown what changed, and the bundle's own copy is left alone.
func TestValidateResumeEditedConfig(t *testing.T) {
	benchRoot, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg", startedAt: testStart})
	edited := strings.Replace(testCfgBody, "runs = 5", "runs = 3", 1)
	cfgPath := currentConfig(t, edited)

	_, diff, err := validate(t, dir, cfgPath, testConfig(), testCommit, benchRoot)
	if err == nil {
		t.Fatal("ValidateResume with an edited config: want refusal")
	}
	if !strings.Contains(err.Error(), "a resumed campaign must run the exact config it began with") {
		t.Errorf("error = %q", err)
	}
	if !strings.Contains(diff, "-runs = 5") || !strings.Contains(diff, "+runs = 3") {
		t.Errorf("diff output = %q, want a -runs = 5 / +runs = 3 pair", diff)
	}
	stored, readErr := os.ReadFile(filepath.Join(dir, "campaign.cfg"))
	if readErr != nil {
		t.Fatalf("read stored config: %v", readErr)
	}
	if string(stored) != testCfgBody {
		t.Errorf("the stored config was modified:\n%s", stored)
	}
}

// A nil diff writer is a caller that does not want the diff, not a panic.
func TestValidateResumeNilDiffWriter(t *testing.T) {
	benchRoot, dir := makeBundle(t, bundleOpts{configFile: "campaign.cfg"})
	cfgPath := currentConfig(t, strings.Replace(testCfgBody, "runs = 5", "runs = 3", 1))
	if _, err := ValidateResume(dir, cfgPath, testConfig(), testCommit, benchRoot, nil); err == nil {
		t.Fatal("ValidateResume with an edited config: want refusal")
	}
}
