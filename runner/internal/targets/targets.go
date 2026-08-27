// Package targets reads docs/targets.json, this repo's single source of truth
// for the performance goal, and answers the one question the runner asks of it:
// how fast should each query leg be paced?
//
// The floors differ per endpoint type, per dataset profile, and per phase, so
// nothing here is hardcoded in Go — the file is loaded at campaign time and the
// numbers it carries become the `--target-rps` ladders in the plan. Everything
// else in the file (latency budgets, verdict inputs) belongs to the converter
// and the viewer; this package models only the query_load section and the phase
// block times it is indexed by.
package targets

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// QueryLoad is the open-loop load model: a floor per (profile, phase, endpoint
// type), and the ladder of multipliers every leg sweeps around that floor.
type QueryLoad struct {
	Ladder   []float64          `json:"ladder"`
	Profiles map[string]Profile `json:"profiles"`
}

// Profile is one dataset model's floors. Each array is indexed by phase minus
// one, so entry 0 is phase 1.
type Profile struct {
	LedgersRPS []float64 `json:"ledgers_rps"`
	TxpageRPS  []float64 `json:"txpage_rps"`
	TxhashRPS  []float64 `json:"txhash_rps"`
	EventsRPS  []float64 `json:"events_rps"`
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
	if len(t.QueryLoad.Profiles) == 0 {
		return nil, fmt.Errorf("targets: %s: query_load.profiles is empty — this file predates the paced-RPS query model", path)
	}
	if len(t.QueryLoad.Ladder) == 0 {
		return nil, fmt.Errorf("targets: %s: query_load.ladder must list at least one multiplier", path)
	}
	for _, name := range sortedKeys(t.QueryLoad.Profiles) {
		p := t.QueryLoad.Profiles[name]
		for _, qtype := range QueryTypes {
			rps, err := p.rates(qtype)
			if err != nil {
				return nil, fmt.Errorf("targets: %s: %w", path, err)
			}
			if len(rps) != len(t.Phases) {
				return nil, fmt.Errorf("targets: %s: query_load.profiles.%s.%s_rps has %d entries, want one per phase (%d)",
					path, name, qtype, len(rps), len(t.Phases))
			}
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

// Rates is the RPS ladder one query leg targets: the (profile, phase, qtype)
// floor multiplied by each ladder step, in ladder order.
func (t *Targets) Rates(profileKey string, phase int, qtype string) ([]float64, error) {
	profile, ok := t.QueryLoad.Profiles[profileKey]
	if !ok {
		return nil, fmt.Errorf("targets: no query_load profile '%s' (known profiles: %s)",
			profileKey, strings.Join(sortedKeys(t.QueryLoad.Profiles), ", "))
	}
	floors, err := profile.rates(qtype)
	if err != nil {
		return nil, err
	}
	if phase < 1 || phase > len(floors) {
		return nil, fmt.Errorf("targets: phase %d has no floors (query_load carries phases 1-%d)", phase, len(floors))
	}
	floor := floors[phase-1]
	rates := make([]float64, len(t.QueryLoad.Ladder))
	for i, step := range t.QueryLoad.Ladder {
		rates[i] = floor * step
	}
	return rates, nil
}

// rates picks the floor array one endpoint type is paced from.
func (p Profile) rates(qtype string) ([]float64, error) {
	switch qtype {
	case "ledgers":
		return p.LedgersRPS, nil
	case "txpage":
		return p.TxpageRPS, nil
	case "txhash":
		return p.TxhashRPS, nil
	case "events":
		return p.EventsRPS, nil
	}
	return nil, fmt.Errorf("targets: unknown query type '%s' (known types: %s)", qtype, strings.Join(QueryTypes, ", "))
}

// sortedKeys keeps every message that lists profiles stable.
func sortedKeys(profiles map[string]Profile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
