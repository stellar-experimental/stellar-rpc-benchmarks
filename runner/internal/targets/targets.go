// Package targets reads docs/targets.json, this repo's single source of truth
// for the performance goal, and answers the one question the runner asks of it:
// how fast should each query leg be paced?
//
// Two families of floor answer that question. The SLA floors belong to the
// endpoint alone and hold in every phase and every dataset profile; the
// E2E-probe floors belong to getTransaction and differ per profile and per
// phase. Nothing here is hardcoded in Go — the file is loaded at campaign time
// and the numbers it carries become the `--target-rps` ladders in the plan.
// Everything else in the file (latency budgets, verdict inputs) belongs to the
// converter and the viewer; this package models only the query_load section and
// the phase block times it is indexed by.
package targets

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// FileName is the path docs/targets.json has relative to a checkout root.
var FileName = filepath.Join("docs", "targets.json")

// Targets is the slice of docs/targets.json the runner consumes. Every other
// key in that file is ignored on purpose: adding one must not break a campaign.
type Targets struct {
	Phases    []Phase   `json:"phases"`
	QueryLoad QueryLoad `json:"query_load"`
}

// Phase is one entry of the file's phases array. BlockTimeNs is what a paced
// campaign's close_interval is matched against.
type Phase struct {
	Phase       int   `json:"phase"`
	BlockTimeNs int64 `json:"block_time_ns"`
}

// QueryLoad is the open-loop load model: two families of floor and the ladder
// of multipliers every leg sweeps around them. SLA floors belong to the
// endpoint alone; the E2E probe's floors belong to the dataset profile and the
// phase. getTransaction carries both, so its leg sweeps both ladders.
type QueryLoad struct {
	Ladder   []float64 `json:"ladder"`
	SLA      SLA       `json:"sla"`
	E2EProbe E2EProbe  `json:"e2e_probe"`
}

// SLA is the read-path requirement: one arrival rate and one p99 per endpoint,
// the same in every phase and every profile. P99Ns is keyed by endpoint then
// storage tier; the runner reads only the floors, the converter judges the p99.
type SLA struct {
	FloorsRPS map[string]float64          `json:"floors_rps"`
	P99Ns     map[string]map[string]int64 `json:"p99_ns"`
}

// E2EProbe is getTransaction's second leg: the demand-derived floors of work
// item 856, per profile, each array indexed by phase minus one so entry 0 is
// phase 1. It answers for its in-RPC p99 alone.
type E2EProbe struct {
	InRPCP99Ns int64                `json:"in_rpc_p99_ns"`
	FloorsRPS  map[string][]float64 `json:"floors_rps"`
}

// Load reads and validates the targets file at path. The validation is
// deliberately strict about the shape the runner indexes into — a profile
// missing a phase would otherwise surface hours later as a leg paced at zero.
func Load(path string) (*Targets, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("targets: %w", err)
	}
	var t Targets
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("targets: %s: %w", path, err)
	}
	if len(t.Phases) == 0 {
		return nil, fmt.Errorf("targets: %s: phases is empty", path)
	}
	if len(t.QueryLoad.E2EProbe.FloorsRPS) == 0 {
		return nil, fmt.Errorf("targets: %s: query_load.e2e_probe.floors_rps is empty — this file predates the two-family query model", path)
	}
	if len(t.QueryLoad.Ladder) == 0 {
		return nil, fmt.Errorf("targets: %s: query_load.ladder must list at least one multiplier", path)
	}
	// The SLA family names every endpoint, twice: once for the rate it is
	// driven at and once for the p99 it answers for. A missing entry would
	// otherwise surface hours later as a leg paced at zero.
	for _, qtype := range QueryTypes {
		if _, ok := t.QueryLoad.SLA.FloorsRPS[qtype]; !ok {
			return nil, fmt.Errorf("targets: %s: query_load.sla.floors_rps has no %s floor, want one per endpoint (%s)",
				path, qtype, strings.Join(QueryTypes, ", "))
		}
		if len(t.QueryLoad.SLA.P99Ns[qtype]) == 0 {
			return nil, fmt.Errorf("targets: %s: query_load.sla.p99_ns has no %s entry, want one per endpoint (%s)",
				path, qtype, strings.Join(QueryTypes, ", "))
		}
	}
	for _, name := range sortedKeys(t.QueryLoad.SLA.FloorsRPS) {
		if !slices.Contains(QueryTypes, name) {
			return nil, fmt.Errorf("targets: %s: query_load.sla.floors_rps names '%s', which is not a query type (%s)",
				path, name, strings.Join(QueryTypes, ", "))
		}
	}
	for _, name := range sortedKeys(t.QueryLoad.E2EProbe.FloorsRPS) {
		if rps := t.QueryLoad.E2EProbe.FloorsRPS[name]; len(rps) != len(t.Phases) {
			return nil, fmt.Errorf("targets: %s: query_load.e2e_probe.floors_rps.%s has %d entries, want one per phase (%d)",
				path, name, len(rps), len(t.Phases))
		}
	}
	return &t, nil
}

// Find walks up from startDir looking for docs/targets.json, so a campaign run
// from anywhere inside a checkout of this repo finds that checkout's own copy.
func Find(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("targets: %w", err)
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("targets: no %s in %s or any parent directory — pass --targets=<path>", FileName, dir)
		}
		dir = parent
	}
}

// reModelSuffix is a dataset name's trailing per-ledger transaction count.
var reModelSuffix = regexp.MustCompile(`-\d+$`)

// ProfileKey is the load profile a dataset benchmarks: the dataset name with
// its per-ledger transaction count stripped, so sac-6000 and sac-3000 both
// target the sac floors. A name with no such suffix is already a profile key.
func ProfileKey(datasetName string) string {
	return reModelSuffix.ReplaceAllString(datasetName, "")
}

// MatchPhase is the phase whose block time is exactly closeIntervalNs, or 0
// when no phase paces its ledgers that way. Exact equality is the point: a
// close_interval that is nearly a phase's block time is a different experiment,
// and guessing which phase it meant would silently mislabel the run.
func (t *Targets) MatchPhase(closeIntervalNs int64) int {
	for _, p := range t.Phases {
		if p.BlockTimeNs == closeIntervalNs {
			return p.Phase
		}
	}
	return 0
}

// QueryTypes are the endpoint types the load model carries floors for. The
// plan's leg order is its own; this list is the vocabulary of the file.
var QueryTypes = []string{"ledgers", "txpage", "txhash", "events"}

// Rates is the RPS ladder one query leg targets, sorted ascending and
// deduplicated: the endpoint's SLA floor times every ladder step. txhash runs
// as ONE leg answering to both families, so its ladder also unions the
// (profile, phase) E2E-probe ladder — a floor the two families share becomes
// one cell carrying both verdicts.
func (t *Targets) Rates(profileKey string, phase int, qtype string) ([]float64, error) {
	slaFloor, ok := t.QueryLoad.SLA.FloorsRPS[qtype]
	if !ok {
		return nil, fmt.Errorf("targets: unknown query type '%s' (known types: %s)", qtype, strings.Join(QueryTypes, ", "))
	}
	// Every leg resolves the profile and the phase, even the three that do not
	// pace by them: a run that names neither is a run nobody can judge.
	e2eFloors, ok := t.QueryLoad.E2EProbe.FloorsRPS[profileKey]
	if !ok {
		return nil, fmt.Errorf("targets: no query_load.e2e_probe profile '%s' (known profiles: %s)",
			profileKey, strings.Join(sortedKeys(t.QueryLoad.E2EProbe.FloorsRPS), ", "))
	}
	if phase < 1 || phase > len(e2eFloors) {
		return nil, fmt.Errorf("targets: phase %d has no floors (query_load carries phases 1-%d)", phase, len(e2eFloors))
	}
	rates := t.ladderAround(slaFloor)
	if qtype == "txhash" {
		rates = append(rates, t.ladderAround(e2eFloors[phase-1])...)
	}
	slices.Sort(rates)
	return slices.Compact(rates), nil
}

// ladderAround is one floor times every ladder step, in ladder order.
func (t *Targets) ladderAround(floor float64) []float64 {
	rates := make([]float64, len(t.QueryLoad.Ladder))
	for i, step := range t.QueryLoad.Ladder {
		rates[i] = floor * step
	}
	return rates
}

// sortedKeys keeps every message that lists profiles stable.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
