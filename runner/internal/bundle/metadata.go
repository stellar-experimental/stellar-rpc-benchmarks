package bundle

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// SchemaVersion is the version of the metadata.json contract. Additive changes
// (new fields) keep the version; a change that breaks the converter bumps it.
const SchemaVersion = 1

// Hardware is metadata.json's hardware object. Empty facts are omitted
// entirely, never "" — the bash writer dropped them with jq's with_entries.
type Hardware struct {
	InstanceType string `json:"instance_type,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	Uname        string `json:"uname"`
	CPUs         int    `json:"cpus,omitempty"`
	MemTotalKB   int64  `json:"mem_total_kb,omitempty"`
}

// MetadataInput is everything the writer records that no single invocation
// knows. StartedAt/FinishedAt/Status/Resumed come from the run wiring.
type MetadataInput struct {
	Cfg         *config.Config
	ConfigFile  string // basename of the config, e.g. "phase4.toml"
	RunID       string
	BuiltCommit string
	Resumed     bool
	// QueryPhase is the goal phase the query legs were paced against, resolved
	// by the CLI from the config and docs/targets.json. 0 on a campaign with no
	// query legs, and omitted from the manifest then.
	QueryPhase int
	Hardware   Hardware
	Hostname   string
	StartedAt  string // RFC3339-like UTC, 2026-07-31T22:00:00Z
	FinishedAt string // "" on the up-front write
	Status     string // StatusRunning / StatusFinished / StatusFailed
}

// metadataFile is the write-side of the manifest. It is a separate type from
// Metadata (the read side, which models only what the runner consumes) because
// this one is the contract: field order is declaration order, and the quirks
// the bash writer had — "yes"/"no", the fields that vanish when empty — live in
// these tags and in config's *String helpers.
type metadataFile struct {
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	Campaign      campaignMetadata  `json:"campaign"`
	Datasets      []datasetMetadata `json:"datasets"`
	Hardware      Hardware          `json:"hardware"`
	Hostname      string            `json:"hostname"`
	StartedAt     string            `json:"started_at"`
	FinishedAt    string            `json:"finished_at,omitempty"`
	Status        string            `json:"status"`
}

type campaignMetadata struct {
	Name          string `json:"name"`
	ConfigFile    string `json:"config_file"`
	Ref           string `json:"ref"`
	BuiltCommit   string `json:"built_commit"`
	Ingest        string `json:"ingest"`
	Query         string `json:"query"`
	CloseInterval string `json:"close_interval"`
	Runs          int    `json:"runs"`
	QueryDuration string `json:"query_duration"`
	// QueryPhase is absent on a campaign with no query legs, where no phase was
	// resolved and recording 0 would read as a phase.
	QueryPhase    int `json:"query_phase,omitempty"`
	Workers       int `json:"workers"`
	HotNumLedgers int `json:"hot_num_ledgers"`
	// Resumed is present only on resumed bundles, matching bash's del().
	Resumed bool `json:"resumed,omitempty"`
}

type datasetMetadata struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	Chunks   []int  `json:"chunks"`
}

// WriteMetadata writes <dir>/metadata.json. Written twice per campaign: up
// front (no finished_at, status running) so a killed campaign leaves a
// parseable bundle, and at the end with finished_at and a final status.
func WriteMetadata(dir string, in MetadataInput) error {
	b, err := marshalMetadata(in)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, MetadataName)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("bundle: write %s: %w", path, err)
	}
	return nil
}

// marshalMetadata renders the manifest bytes, trailing newline and all, so the
// contract test can compare them without touching the filesystem.
func marshalMetadata(in MetadataInput) ([]byte, error) {
	cfg := in.Cfg
	m := metadataFile{
		SchemaVersion: SchemaVersion,
		RunID:         in.RunID,
		Campaign: campaignMetadata{
			Name:          cfg.Name,
			ConfigFile:    in.ConfigFile,
			Ref:           cfg.Ref,
			BuiltCommit:   in.BuiltCommit,
			Ingest:        cfg.Ingest,
			Query:         cfg.QueryString(),
			CloseInterval: cfg.CloseInterval,
			Runs:          cfg.Runs,
			QueryDuration: cfg.QueryDuration,
			QueryPhase:    in.QueryPhase,
			Workers:       cfg.Workers,
			HotNumLedgers: cfg.HotNumLedgers,
			Resumed:       in.Resumed,
		},
		Datasets:   make([]datasetMetadata, 0, len(cfg.Datasets)),
		Hardware:   in.Hardware,
		Hostname:   in.Hostname,
		StartedAt:  in.StartedAt,
		FinishedAt: in.FinishedAt,
		Status:     in.Status,
	}
	for i := range cfg.Datasets {
		d := &cfg.Datasets[i]
		m.Datasets = append(m.Datasets, datasetMetadata{
			Name:     d.Name,
			Kind:     d.Kind,
			Location: d.LocationString(),
			Chunks:   d.Chunks,
		})
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bundle: marshal %s: %w", MetadataName, err)
	}
	return append(b, '\n'), nil
}

// DefaultIMDSBase is the EC2 instance metadata service root.
const DefaultIMDSBase = "http://169.254.169.254"

// imdsTimeout caps every metadata-service request: off EC2 the address is
// unroutable, and the campaign must not stall on it.
const imdsTimeout = 2 * time.Second

// CollectHardware gathers the structured hardware facts: EC2 instance identity
// via IMDSv2 (absent off EC2), CPU count, and MemTotal on Linux. imdsBase is
// the metadata service root, overridable for tests. Every fact is best-effort:
// what cannot be gathered stays zero, and the writer omits it.
func CollectHardware(imdsBase string) Hardware {
	hw := Hardware{
		Uname: commandOutput("uname", "-srm"),
		CPUs:  runtime.NumCPU(),
	}
	hw.InstanceType, hw.InstanceID = ec2Identity(imdsBase)
	hw.MemTotalKB = memTotalKB()
	return hw
}

// ec2Identity asks IMDSv2 for the instance type and id. Any failure means "not
// on EC2" — normal, not an error.
func ec2Identity(imdsBase string) (instanceType, instanceID string) {
	client := &http.Client{Timeout: imdsTimeout}
	req, err := http.NewRequest(http.MethodPut, imdsBase+"/latest/api/token", nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	token, err := readBody(client, req)
	if err != nil {
		return "", ""
	}
	get := func(path string) string {
		req, err := http.NewRequest(http.MethodGet, imdsBase+path, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("X-aws-ec2-metadata-token", token)
		v, err := readBody(client, req)
		if err != nil {
			return ""
		}
		return v
	}
	return get("/latest/meta-data/instance-type"), get("/latest/meta-data/instance-id")
}

// readBody performs req and returns its trimmed body, treating any non-2xx as
// an error the way curl -f does.
func readBody(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// memTotalKB reads MemTotal from /proc/meminfo, in kB. Absent (macOS) → 0, and
// the field is omitted, exactly as bash only set it when /proc/meminfo existed.
func memTotalKB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb
		}
	}
	return 0
}

// factTimeout bounds every fact-gathering command. A probe that hangs — lsblk on
// a wedged device, a binary whose `version` never returns — must not cost an
// unattended campaign its start, so a fact that takes too long is treated the
// same as one that cannot be gathered. A var so tests can shrink it.
var factTimeout = 5 * time.Second

// commandOutput runs a fact-gathering command and returns its trimmed stdout,
// or "" if it cannot be run or does not finish inside factTimeout. Every caller
// here is best-effort by contract.
func commandOutput(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), factTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
