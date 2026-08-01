package bundle

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

var update = flag.Bool("update", false, "rewrite testdata/metadata.golden.json from the current writer output")

const metadataGoldenPath = "testdata/metadata.golden.json"

// goldenInput pins everything the writer cannot derive from the config, so the
// manifest below is a function of the committed testdata config alone.
func goldenInput(t *testing.T) MetadataInput {
	t.Helper()
	cfg, err := config.Load("testdata/campaign.toml")
	if err != nil {
		t.Fatalf("config.Load(testdata/campaign.toml): %v", err)
	}
	return MetadataInput{
		Cfg:         cfg,
		ConfigFile:  "campaign.toml",
		RunID:       "golden-deadbeef-20260101T000000Z",
		BuiltCommit: "deadbeefcafebabefeedface1234567890abcdef",
		Hardware: Hardware{
			InstanceType: "i4i.4xlarge",
			InstanceID:   "i-0123456789abcdef0",
			Uname:        "Linux 6.8.0-1029-aws x86_64",
			CPUs:         16,
			MemTotalKB:   131033600,
		},
		Hostname:   "bench-devbox",
		StartedAt:  "2026-01-01T00:00:00Z",
		FinishedAt: "2026-01-01T04:30:00Z",
		Status:     StatusFinished,
	}
}

// marshal renders the manifest bytes for in, failing the test on error.
func marshal(t *testing.T, in MetadataInput) string {
	t.Helper()
	b, err := marshalMetadata(in)
	if err != nil {
		t.Fatalf("marshalMetadata: %v", err)
	}
	return string(b)
}

// TestMetadataGolden is the contract test: metadata.json is read by
// converter/convert.py in another repo, so its bytes are pinned.
func TestMetadataGolden(t *testing.T) {
	got := marshal(t, goldenInput(t))
	if *update {
		if err := os.WriteFile(metadataGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s", metadataGoldenPath)
		return
	}
	want, err := os.ReadFile(metadataGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run: go test ./internal/bundle -run Golden -update)", err)
	}
	if got != string(want) {
		t.Errorf("metadata.json differs from %s.\n--- got ---\n%s", metadataGoldenPath, got)
	}
}

// TestMetadataQuirks asserts the shapes the bash writer had, independently of
// the golden bytes: the converter reads these three and nothing else would
// catch a "fix" that made them natural Go types.
func TestMetadataQuirks(t *testing.T) {
	got := marshal(t, goldenInput(t))
	for _, want := range []string{
		`"query": "yes"`,
		`"query_concurrency": "1,4,16"`,
		`"close_interval": "2s"`,
		`"schema_version": 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("metadata.json missing %s:\n%s", want, got)
		}
	}
	// The fixture dataset has no location; bash recorded its per-chunk ledger
	// count in that field, and the converter reads it there.
	if !strings.Contains(got, `"kind": "fixture",`) || !strings.Contains(got, `"location": "10000"`) {
		t.Errorf("fixture dataset should record location \"10000\":\n%s", got)
	}
}

func TestMetadataQueryNo(t *testing.T) {
	in := goldenInput(t)
	in.Cfg.Query = false
	in.Cfg.QueryConcurrency = []int{8}
	got := marshal(t, in)
	if !strings.Contains(got, `"query": "no"`) || !strings.Contains(got, `"query_concurrency": "8"`) {
		t.Errorf("query = false should render \"no\" and a one-element sweep:\n%s", got)
	}
}

// TestMetadataUpFrontWrite covers the write that happens before any leg runs:
// no finished_at, status running, and no resumed key on a fresh campaign.
func TestMetadataUpFrontWrite(t *testing.T) {
	in := goldenInput(t)
	in.FinishedAt = ""
	in.Status = StatusRunning
	got := marshal(t, in)
	if strings.Contains(got, "finished_at") {
		t.Errorf("up-front write must omit finished_at entirely:\n%s", got)
	}
	if !strings.Contains(got, `"status": "running"`) {
		t.Errorf("up-front write should be status running:\n%s", got)
	}
	if strings.Contains(got, "resumed") {
		t.Errorf("a fresh campaign must omit campaign.resumed:\n%s", got)
	}
}

func TestMetadataResumed(t *testing.T) {
	in := goldenInput(t)
	in.Resumed = true
	if got := marshal(t, in); !strings.Contains(got, `"resumed": true`) {
		t.Errorf("a resumed campaign should record resumed: true:\n%s", got)
	}
}

// TestMetadataHardwareOmitted checks that an unavailable fact is absent rather
// than "" or 0 — the with_entries filter bash's jq applied.
func TestMetadataHardwareOmitted(t *testing.T) {
	in := goldenInput(t)
	in.Hardware = Hardware{Uname: "Darwin 25.5.0 arm64"}
	got := marshal(t, in)
	for _, absent := range []string{"instance_type", "instance_id", "cpus", "mem_total_kb"} {
		if strings.Contains(got, absent) {
			t.Errorf("unavailable hardware fact %s should be omitted:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, `"uname": "Darwin 25.5.0 arm64"`) {
		t.Errorf("uname is always recorded:\n%s", got)
	}
}

// TestWriteMetadataRoundTrip proves the writer and the resume reader agree on
// the fields --resume recovers.
func TestWriteMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := goldenInput(t)
	if err := WriteMetadata(dir, in); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	meta, err := ReadMetadata(dir)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	for _, f := range []struct{ name, got, want string }{
		{"run_id", meta.RunID, in.RunID},
		{"campaign.name", meta.Campaign.Name, in.Cfg.Name},
		{"campaign.config_file", meta.Campaign.ConfigFile, in.ConfigFile},
		{"campaign.ref", meta.Campaign.Ref, in.Cfg.Ref},
		{"campaign.built_commit", meta.Campaign.BuiltCommit, in.BuiltCommit},
		{"started_at", meta.StartedAt, in.StartedAt},
		{"finished_at", meta.FinishedAt, in.FinishedAt},
		{"status", meta.Status, in.Status},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
	if meta.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", meta.SchemaVersion, SchemaVersion)
	}
}

// --- hardware collection --------------------------------------------------

// imdsServer serves IMDSv2 the way EC2 does: a token PUT, then facts that
// require the token header.
func imdsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("TOKEN123"))
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token") != "TOKEN123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/latest/meta-data/instance-type":
			_, _ = w.Write([]byte("i4i.4xlarge"))
		case "/latest/meta-data/instance-id":
			_, _ = w.Write([]byte("i-0123456789abcdef0"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCollectHardwareOnEC2(t *testing.T) {
	hw := CollectHardware(imdsServer(t).URL)
	if hw.InstanceType != "i4i.4xlarge" || hw.InstanceID != "i-0123456789abcdef0" {
		t.Errorf("instance identity = %q/%q, want i4i.4xlarge/i-0123456789abcdef0", hw.InstanceType, hw.InstanceID)
	}
	if hw.Uname == "" {
		t.Error("uname should be recorded on every platform")
	}
	if hw.CPUs < 1 {
		t.Errorf("cpus = %d, want >= 1", hw.CPUs)
	}
}

// TestCollectHardwareNoToken covers a metadata service that refuses IMDSv2
// tokens: the facts are absent, not an error.
func TestCollectHardwareNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	hw := CollectHardware(srv.URL)
	if hw.InstanceType != "" || hw.InstanceID != "" {
		t.Errorf("instance identity = %q/%q, want both empty", hw.InstanceType, hw.InstanceID)
	}
	if hw.Uname == "" {
		t.Error("uname should still be recorded")
	}
}

// TestCollectHardwareOffEC2 is the normal laptop case: nothing answers at the
// metadata address, and the campaign must not stall on it.
func TestCollectHardwareOffEC2(t *testing.T) {
	// A closed port on loopback: refused immediately, and even a black-holed
	// address would be capped by the 2s client timeout.
	start := time.Now()
	hw := CollectHardware("http://127.0.0.1:1")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("CollectHardware took %s off EC2, want well under the 2s-per-request timeout", elapsed)
	}
	if hw.InstanceType != "" || hw.InstanceID != "" {
		t.Errorf("instance identity = %q/%q, want both empty", hw.InstanceType, hw.InstanceID)
	}
}

func TestWriteMetadataUnwritableDir(t *testing.T) {
	err := WriteMetadata(filepath.Join(t.TempDir(), "nope"), goldenInput(t))
	if err == nil {
		t.Fatal("WriteMetadata into a missing directory should fail")
	}
}
