// Package config loads and validates campaign configs: TOML in, a validated
// Config out, with unknown keys and out-of-range values rejected up front.
//
// The rules here are ported from the validation block of the bash campaign
// runner this package replaces; its error messages are operator UX, so they are
// reproduced with the same specificity, naming the TOML key instead of the bash
// variable.
package config

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults for the keys that have one, matching the bash campaign runner this
// package replaces.
const (
	DefaultRepo          = "https://github.com/stellar/stellar-rpc.git"
	DefaultRef           = "feature/full-history"
	DefaultCloseInterval = "0"
	DefaultRuns          = 5
	DefaultColdIters     = 100
	DefaultHotIters      = 200
	DefaultWorkers       = 1
)

// Dataset kinds. Each names a different way of materializing a cold pack root;
// see runner/README.md for what location means for each.
const (
	KindPacksLocal = "packs-local"
	KindPacksGS    = "packs-gs"
	KindPacksS3    = "packs-s3"
	KindBSBS3      = "bsb-s3"
	KindFixture    = "fixture"
)

// minFixtureLedgers is the smallest non-zero fixture ledger count: the cold
// freeze streams a whole 10,000-ledger chunk, so a partial chunk cannot be
// frozen.
const minFixtureLedgers = 10000

// Config is a whole campaign config file.
type Config struct {
	Name             string    `toml:"name"`
	Repo             string    `toml:"repo"`
	Ref              string    `toml:"ref"`
	Ingest           string    `toml:"ingest"`
	Query            bool      `toml:"query"`
	CloseInterval    string    `toml:"close_interval"`
	Runs             int       `toml:"runs"`
	QueryConcurrency []int     `toml:"query_concurrency"`
	ColdIters        int       `toml:"cold_iters"`
	HotIters         int       `toml:"hot_iters"`
	Workers          int       `toml:"workers"`
	HotNumLedgers    int       `toml:"hot_num_ledgers"`
	PublishURI       string    `toml:"publish_uri"`
	Datasets         []Dataset `toml:"dataset"`
}

// Dataset is one [[dataset]] table: a named pack tree and the chunks of it
// this campaign benchmarks.
type Dataset struct {
	Name     string `toml:"name"`
	Kind     string `toml:"kind"`
	Location string `toml:"location"`
	Chunks   []int  `toml:"chunks"`
	// Ledgers is the per-chunk ledger count of a fixture dataset, and is
	// invalid for every other kind. It is a pointer because 0 is a
	// meaningful value (the whole chunk) that must be told from unset.
	Ledgers *int `toml:"ledgers"`
}

var reName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// defaults returns a Config pre-filled with the documented defaults. Decoding
// on top of it leaves absent keys at their default and lets present keys win.
func defaults() Config {
	return Config{
		Repo:             DefaultRepo,
		Ref:              DefaultRef,
		CloseInterval:    DefaultCloseInterval,
		Runs:             DefaultRuns,
		QueryConcurrency: []int{1, 4, 16},
		ColdIters:        DefaultColdIters,
		HotIters:         DefaultHotIters,
		Workers:          DefaultWorkers,
	}
}

// Load parses the TOML config at path, applies defaults, and validates it.
func Load(path string) (*Config, error) {
	cfg := defaults()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		plural := ""
		if len(keys) > 1 {
			plural = "s"
		}
		return nil, fmt.Errorf("config: unknown key%s: %s", plural, strings.Join(keys, ", "))
	}
	// A bool cannot express "unset", so ask the metadata whether the
	// operator actually chose.
	if !md.IsDefined("query") {
		return nil, errors.New("config: query is required (true|false)")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Name == "" {
		return errors.New("config: name is required")
	}
	if !reName.MatchString(c.Name) {
		return fmt.Errorf("config: name must match [A-Za-z0-9._-]+ (got '%s')", c.Name)
	}
	if err := validateRepo(c.Repo); err != nil {
		return err
	}
	if c.Ref == "" {
		return errors.New("config: ref must not be empty")
	}
	switch c.Ingest {
	case "cold", "hot", "both", "none":
	default:
		got := c.Ingest
		if got == "" {
			got = "<unset>"
		}
		return fmt.Errorf("config: ingest must be cold|hot|both|none (got '%s')", got)
	}
	if err := validateCloseInterval(c.CloseInterval); err != nil {
		return err
	}
	for _, f := range []struct {
		key   string
		value int
	}{
		{"runs", c.Runs},
		{"cold_iters", c.ColdIters},
		{"hot_iters", c.HotIters},
		{"workers", c.Workers},
	} {
		if f.value < 1 {
			return fmt.Errorf("config: %s must be an integer >= 1 (got '%d')", f.key, f.value)
		}
	}
	if c.HotNumLedgers < 0 {
		return fmt.Errorf("config: hot_num_ledgers must be an integer >= 0 (got '%d')", c.HotNumLedgers)
	}
	if len(c.QueryConcurrency) == 0 {
		return errors.New("config: query_concurrency must list at least one concurrency level")
	}
	for _, qc := range c.QueryConcurrency {
		if qc < 1 {
			return fmt.Errorf("config: query_concurrency entries must be integers >= 1 (got '%d')", qc)
		}
	}
	if c.PublishURI != "" && !strings.HasPrefix(c.PublishURI, "gs://") && !strings.HasPrefix(c.PublishURI, "s3://") {
		return fmt.Errorf("config: publish_uri must be a gs:// or s3:// URI (got '%s')", c.PublishURI)
	}
	if len(c.Datasets) == 0 {
		return errors.New("config: at least one [[dataset]] is required")
	}
	seen := make(map[string]bool, len(c.Datasets))
	for i := range c.Datasets {
		if err := c.Datasets[i].validate(seen); err != nil {
			return err
		}
	}
	return nil
}

func validateRepo(repo string) error {
	if repo == "" {
		return errors.New("config: repo must not be empty")
	}
	if strings.Contains(repo, "://") || isSCPLike(repo) {
		return nil
	}
	// Not a URL: must be an absolute path to a local git repository.
	// Relative paths are refused — they would silently depend on the
	// invocation cwd.
	if !filepath.IsAbs(repo) {
		return fmt.Errorf("config: repo must be a git URL or an absolute local path (got '%s')", repo)
	}
	if err := exec.Command("git", "-C", repo, "rev-parse", "--git-dir").Run(); err != nil {
		return fmt.Errorf("config: repo path '%s' is not a git repository", repo)
	}
	return nil
}

// isSCPLike reports whether repo has git's scp-like remote shape, user@host:path.
func isSCPLike(repo string) bool {
	at := strings.Index(repo, "@")
	return at >= 0 && strings.Contains(repo[at+1:], ":")
}

func validateCloseInterval(s string) error {
	if s == "0" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return fmt.Errorf("config: close_interval must be a Go duration or 0 (got '%s')", s)
	}
	return nil
}

// validate checks one dataset, recording its name in seen to catch duplicates.
func (d *Dataset) validate(seen map[string]bool) error {
	if !reName.MatchString(d.Name) {
		return fmt.Errorf("config: dataset name must match [A-Za-z0-9._-]+ (got '%s')", d.Name)
	}
	if seen[d.Name] {
		return fmt.Errorf("config: duplicate dataset name '%s'", d.Name)
	}
	seen[d.Name] = true
	if len(d.Chunks) == 0 {
		return fmt.Errorf("config: dataset '%s': chunks must list at least one chunk ID", d.Name)
	}
	// Duplicate chunk IDs are rejected outright, which bash did not do: step
	// IDs embed <dataset>-c<chunk>-run<r>, so a repeated chunk would produce
	// two steps with the same ID and the same output directory — ambiguous to
	// depend on and, on resume, indistinguishable from finished work. Bash
	// silently let the second copy overwrite the first's out dirs.
	seenChunks := make(map[int]bool, len(d.Chunks))
	for _, chunk := range d.Chunks {
		if chunk < 0 {
			return fmt.Errorf("config: dataset '%s': chunk IDs must be non-negative integers (got '%d')", d.Name, chunk)
		}
		if seenChunks[chunk] {
			return fmt.Errorf("config: dataset '%s': duplicate chunk ID '%d'", d.Name, chunk)
		}
		seenChunks[chunk] = true
	}
	switch d.Kind {
	case KindPacksLocal:
		// The root's contents are checked at prep time, as in bash.
		if d.Location == "" {
			return fmt.Errorf("config: dataset '%s': packs-local location must be a local cold pack root", d.Name)
		}
	case KindPacksGS:
		if !strings.HasPrefix(d.Location, "gs://") {
			return fmt.Errorf("config: dataset '%s': packs-gs location must start with gs:// (got '%s')", d.Name, d.Location)
		}
	case KindPacksS3:
		if !strings.HasPrefix(d.Location, "s3://") {
			return fmt.Errorf("config: dataset '%s': packs-s3 location must start with s3:// (got '%s')", d.Name, d.Location)
		}
	case KindBSBS3:
		if d.Location == "" {
			return fmt.Errorf("config: dataset '%s': bsb-s3 location must be an S3 bucket path", d.Name)
		}
	case KindFixture:
		if d.Location != "" {
			return fmt.Errorf("config: dataset '%s': fixture datasets use ledgers, not location (got location '%s')", d.Name, d.Location)
		}
		if d.Ledgers == nil {
			return fmt.Errorf("config: dataset '%s': fixture datasets need ledgers = <per-chunk ledger count> (0 or >= %d)", d.Name, minFixtureLedgers)
		}
		if n := *d.Ledgers; n != 0 && n < minFixtureLedgers {
			return fmt.Errorf("config: dataset '%s': fixture ledger count must be 0 or >= %d — the cold freeze streams the whole 10,000-ledger chunk (got '%d')", d.Name, minFixtureLedgers, n)
		}
	default:
		return fmt.Errorf("config: dataset '%s': kind must be packs-local|packs-gs|packs-s3|bsb-s3|fixture (got '%s')", d.Name, d.Kind)
	}
	if d.Kind != KindFixture && d.Ledgers != nil {
		return fmt.Errorf("config: dataset '%s': ledgers is only valid for fixture datasets (kind is %s)", d.Name, d.Kind)
	}
	return nil
}

// The three helpers below are the metadata.json compatibility layer: the
// manifest is a cross-repo contract with converter/convert.py, and it records
// these fields in the shapes the bash runner wrote. Keep them here so the
// quirks live in one place.

// QueryString renders query as metadata.json records it: the bash config key
// took the strings "yes" and "no".
func (c *Config) QueryString() string {
	if c.Query {
		return "yes"
	}
	return "no"
}

// QueryConcurrencyString renders the concurrency sweep as metadata.json
// records it: a comma-separated list, e.g. "1,4,16".
func (c *Config) QueryConcurrencyString() string {
	parts := make([]string, len(c.QueryConcurrency))
	for i, qc := range c.QueryConcurrency {
		parts[i] = strconv.Itoa(qc)
	}
	return strings.Join(parts, ",")
}

// LocationString renders the dataset's location as metadata.json records it.
// Fixture datasets have no location: the bash runner overloaded that field
// with the per-chunk ledger count, so the manifest keeps the decimal count.
func (d *Dataset) LocationString() string {
	if d.Kind == KindFixture {
		if d.Ledgers == nil {
			return ""
		}
		return strconv.Itoa(*d.Ledgers)
	}
	return d.Location
}
