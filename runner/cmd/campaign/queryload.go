package main

import (
	"fmt"
	"os"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/targets"
)

// queryLoad is a campaign's resolved query pacing: the RPS ladder every
// (dataset, query type) cell is driven at, and the goal phase whose floors
// those ladders came from. The zero value is a campaign with no query legs.
type queryLoad struct {
	rates map[string]map[string][]float64
	phase int
}

// resolveQueryLoad turns `query = true` into numbers, by reading the phase
// floors out of docs/targets.json and applying the ladder to each dataset's
// profile. It is the CLI's job rather than the plan's so that plan.Build stays
// a pure function of config plus inputs, and so that a config naming a dataset
// with no load profile fails here — before a campaign is started — instead of
// pacing a leg at nothing.
//
// targetsPath is the --targets flag; empty means find the checkout's own copy.
func resolveQueryLoad(cfg *config.Config, targetsPath string) (queryLoad, error) {
	if !cfg.Query {
		return queryLoad{}, nil
	}
	path := targetsPath
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return queryLoad{}, err
		}
		if path, err = targets.Find(cwd); err != nil {
			return queryLoad{}, err
		}
	}
	tg, err := targets.Load(path)
	if err != nil {
		return queryLoad{}, err
	}
	phase, err := resolvePhase(cfg, tg, path)
	if err != nil {
		return queryLoad{}, err
	}
	rates := make(map[string]map[string][]float64, len(cfg.Datasets))
	for i := range cfg.Datasets {
		d := &cfg.Datasets[i]
		profile := targets.ProfileKey(d.Name)
		perType := make(map[string][]float64, len(plan.QueryTypes()))
		for _, qtype := range plan.QueryTypes() {
			ladder, err := tg.Rates(profile, phase, qtype)
			if err != nil {
				return queryLoad{}, fmt.Errorf("dataset '%s' (load profile '%s'): %w", d.Name, profile, err)
			}
			perType[qtype] = ladder
		}
		rates[d.Name] = perType
	}
	return queryLoad{rates: rates, phase: phase}, nil
}

// resolvePhase decides which phase's floors this campaign targets. A paced
// campaign already says so in close_interval, so the phase is usually implied;
// `phase` states it outright for an unpaced campaign. When both are present
// they have to agree — a campaign pacing ledgers at one phase's block time
// while measuring another phase's read targets is not a phase run at all.
func resolvePhase(cfg *config.Config, tg *targets.Targets, path string) (int, error) {
	matched := 0
	if d, err := time.ParseDuration(cfg.CloseInterval); err == nil && d > 0 {
		matched = tg.MatchPhase(int64(d))
	}
	switch {
	case cfg.Phase != 0 && matched != 0 && cfg.Phase != matched:
		return 0, fmt.Errorf("config: phase = %d, but close_interval '%s' is phase %d's block time — set one or the other",
			cfg.Phase, cfg.CloseInterval, matched)
	case cfg.Phase != 0:
		return cfg.Phase, nil
	case matched != 0:
		return matched, nil
	}
	return 0, fmt.Errorf("config: query = true needs a phase for its RPS floors: set phase = 1|2|3, "+
		"or a close_interval matching a phase block time in %s (close_interval is '%s')", path, cfg.CloseInterval)
}

// queryDuration is the per-leg --duration value, empty for a campaign whose
// plan has no query legs at all.
func queryDuration(cfg *config.Config) string {
	if !cfg.Query {
		return ""
	}
	return cfg.QueryDuration
}
