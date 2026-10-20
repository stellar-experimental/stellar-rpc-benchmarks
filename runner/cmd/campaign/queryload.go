package main

import (
	"fmt"
	"os"
	"strings"
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
	phase targets.PhaseLabel
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
	if !cfg.Query && cfg.Phase == "" {
		return queryLoad{}, nil
	}
	tg, path, err := loadTargets(targetsPath)
	if err != nil {
		// A campaign with no query legs needs no floors, so a targets file it
		// cannot find or read leaves its phase unchecked instead of failing it.
		if !cfg.Query {
			return queryLoad{}, nil
		}
		return queryLoad{}, err
	}
	// A phase is checked against the table whenever the table loads, so a typo
	// fails here even on a campaign with no query legs to pace.
	if cfg.Phase != "" {
		if _, ok := tg.PhaseIndex(cfg.Phase); !ok {
			return queryLoad{}, fmt.Errorf("config: phase = %s is not in the phases table of %s (%s)", cfg.Phase, path, tg.PhaseLabels())
		}
	}
	if !cfg.Query {
		return queryLoad{}, nil
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

// loadTargets loads the targets file at path, or, when path is empty, the
// checkout's own docs/targets.json. It returns the path it read.
func loadTargets(path string) (*targets.Targets, string, error) {
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, "", err
		}
		if path, err = targets.Find(cwd); err != nil {
			return nil, "", err
		}
	}
	tg, err := targets.Load(path)
	return tg, path, err
}

// resolvePhase decides which phase's floors this campaign targets. A paced
// campaign already says so in close_interval, so the phase is usually implied;
// `phase` states it outright for an unpaced campaign. When both are present
// they have to agree — a campaign pacing ledgers at one phase's block time
// while measuring another phase's read targets is not a phase run at all.
// cfg.Phase, when set, must already be in the table.
func resolvePhase(cfg *config.Config, tg *targets.Targets, path string) (targets.PhaseLabel, error) {
	var matched targets.PhaseLabel
	if d, err := time.ParseDuration(cfg.CloseInterval); err == nil && d > 0 {
		matched = tg.MatchPhase(int64(d))
	}
	switch {
	case cfg.Phase != "" && matched != "" && cfg.Phase != matched:
		return "", fmt.Errorf("config: phase = %s, but close_interval '%s' is phase %s's block time — set one or the other",
			cfg.Phase, cfg.CloseInterval, matched)
	case cfg.Phase != "":
		return cfg.Phase, nil
	case matched != "":
		return matched, nil
	}
	return "", fmt.Errorf("config: query = true needs a phase for its RPS floors: set phase = %s, "+
		"or a close_interval matching a phase block time in %s (close_interval is '%s')",
		strings.ReplaceAll(tg.PhaseLabels(), ", ", "|"), path, cfg.CloseInterval)
}

// queryDuration is the per-leg --duration value, empty for a campaign whose
// plan has no query legs at all.
func queryDuration(cfg *config.Config) string {
	if !cfg.Query {
		return ""
	}
	return cfg.QueryDuration
}
