package plan

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// SchemaVersion is the version of the plan.json contract. Additive changes
// (new fields, new step kinds) keep the version; a change that breaks a
// consumer bumps it.
const SchemaVersion = 1

// Step kinds.
const (
	KindBuild   = "build"
	KindDataset = "dataset"
	KindLeg     = "leg"
	KindTarball = "tarball"
	KindPublish = "publish"
)

// queryTypes are the endpoint types the query suites benchmark, in the order
// their legs run. Each type gets a leg of its own because the rate it is paced
// at differs per type, and one invocation drives one rate.
var queryTypes = []string{"ledgers", "txpage", "txhash", "events"}

// QueryTypes is queryTypes for callers that have to resolve a rate per type
// before they can build a plan. It is a copy: the leg order is the plan's.
func QueryTypes() []string {
	return slices.Clone(queryTypes)
}

// Inputs carries everything about the environment that Build would otherwise
// have to discover for itself. Keeping them in a struct is what makes Build
// pure: the caller resolves the ref, reads the clock, and picks the storage
// root, so a test can pin all three.
type Inputs struct {
	BenchRoot   string // e.g. /mnt/nvme/bench
	BuiltCommit string // full sha the ref resolved to, or the ref itself in placeholder mode
	Sha8        string // 8-hex short sha ("deadbeef" placeholder when unresolvable)
	Stamp       string // UTC, e.g. 20260101T000000Z
	// QueryRates is the arrival rate every query leg is paced at: dataset name →
	// query type → the RPS ladder for that cell. The CLI resolves it from
	// docs/targets.json before calling Build, which is what keeps the phase
	// floors out of this package and Build pure. Nil when the campaign has no
	// query legs.
	QueryRates map[string]map[string][]float64
	// QueryDuration is how long each query leg holds its rate, as a Go duration.
	// Empty when the campaign has no query legs.
	QueryDuration string
}

// Step is one unit of scheduling, skipping, and failure. Argv is a *list* of
// commands because a build or a dataset preparation runs several external
// commands that only make sense together — bash ran them inside one function,
// and a campaign that stopped between two of them would leave a half-built
// binary or a half-fetched pack tree. A timed leg always has exactly one
// command: the measurement is the process.
type Step struct {
	ID    string            `json:"id"`
	Kind  string            `json:"kind"`  // build|dataset|leg|tarball|publish
	Timed bool              `json:"timed"` // true only for benchmark legs
	Argv  [][]string        `json:"argv"`  // external commands, in order
	Env   map[string]string `json:"env,omitempty"`
	// OutDir is a leg's --out directory: where the bench subcommand writes
	// driver.csv and invocation.json, and what the executor inspects to decide
	// whether a resumed leg is already finished.
	OutDir string `json:"out_dir,omitempty"`
	// PreClean lists directories to rm -rf before the step runs, so every leg
	// starts from a known-empty scratch or hot DB and every dataset
	// materializes onto bare ground. It is the single source of truth for those
	// wipes: the executor removes exactly this list, and the dry run prints it.
	PreClean []string `json:"pre_clean,omitempty"`
	// PostClean lists directories to rm -rf after the step succeeds. This is a
	// deliberate change from bash, which only cleaned before the next rep and
	// so left the final rep's cold scratch on disk for the rest of the
	// campaign; freeing it immediately keeps peak disk to one cell's worth.
	PostClean []string `json:"post_clean,omitempty"`
	Needs     []string `json:"needs,omitempty"` // step ids
	// PublishURI is the destination root a publish step uploads to. It is not
	// an argv: the publish subcommand owns the destination checks and the
	// cloud tooling, so the plan records the intent and nothing more.
	PublishURI string       `json:"publish_uri,omitempty"`
	Dataset    *DatasetSpec `json:"dataset,omitempty"` // dataset steps only
}

// DatasetSpec is what the executor needs to run the .partial dance around a
// dataset step's commands: materialize into <root>.partial, then rename onto
// Root once whole, so an interrupted preparation is never mistaken for a
// finished one. The choreography is the executor's, not argv's.
type DatasetSpec struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location,omitempty"` // absent for fixture
	Ledgers  *int   `json:"ledgers,omitempty"`  // fixture only
	Root     string `json:"root"`               // the cold pack root this converges on
	Stage    string `json:"stage,omitempty"`    // fixture staging pack dir
}

// Plan is a whole campaign as data: the paths it derives and the steps it
// would execute, in order.
type Plan struct {
	SchemaVersion int      `json:"schema_version"`
	RunID         string   `json:"run_id"` // <name>-<sha8>-<stamp>
	BenchRoot     string   `json:"bench_root"`
	Src           string   `json:"src"`         // <bench_root>/src
	Bin           string   `json:"bin"`         // <bench_root>/bin/stellar-rpc-<sha8>
	ResultsDir    string   `json:"results_dir"` // <bench_root>/results/<run_id>
	Tarball       string   `json:"tarball"`     // /tmp/bench-results-<run_id>.tgz
	Notes         []string `json:"notes,omitempty"`
	Steps         []Step   `json:"steps"`
}

// Build turns a validated config into the ordered steps a campaign executes.
// It is pure: same cfg, same Inputs, same plan.
func Build(cfg *config.Config, in Inputs) *Plan {
	runID := cfg.Name + "-" + in.Sha8 + "-" + in.Stamp
	p := &Plan{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		BenchRoot:     in.BenchRoot,
		Src:           filepath.Join(in.BenchRoot, "src"),
		Bin:           filepath.Join(in.BenchRoot, "bin", "stellar-rpc-"+in.Sha8),
		ResultsDir:    filepath.Join(in.BenchRoot, "results", runID),
		Tarball:       "/tmp/bench-results-" + runID + ".tgz",
	}

	// Query-hot needs the hot DB a hot ingest leaves behind, so it only runs
	// when this campaign also ingests hot.
	ingestCold := cfg.Ingest == "cold" || cfg.Ingest == "both"
	ingestHot := cfg.Ingest == "hot" || cfg.Ingest == "both"
	queryCold := cfg.Query
	queryHot := cfg.Query && ingestHot
	if cfg.Query && !ingestHot {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"query = true with ingest = %s leaves no hot DB — running the cold query suite only", cfg.Ingest))
	}
	if cfg.Ingest == "none" && !cfg.Query {
		p.Notes = append(p.Notes, "ingest = none and query = false — this campaign only prepares datasets")
	}

	p.Steps = append(p.Steps, buildStep(p, in))
	for i := range cfg.Datasets {
		p.Steps = append(p.Steps, datasetStep(p, in, &cfg.Datasets[i]))
	}

	// The four suites run in bash's order: all of one before any of the next,
	// so a campaign killed partway through still has whole suites.
	if ingestCold {
		forEachCell(cfg, func(d *config.Dataset, chunk, rep int) {
			p.Steps = append(p.Steps, ingestColdLeg(p, in, cfg, d, chunk, rep))
		})
	}
	if ingestHot {
		forEachCell(cfg, func(d *config.Dataset, chunk, rep int) {
			p.Steps = append(p.Steps, ingestHotLeg(p, in, cfg, d, chunk, rep))
		})
	}
	if queryCold {
		forEachCell(cfg, func(d *config.Dataset, chunk, rep int) {
			for _, qtype := range queryTypes {
				p.Steps = append(p.Steps, queryColdLeg(p, in, d, chunk, rep, qtype))
			}
		})
	}
	if queryHot {
		forEachCell(cfg, func(d *config.Dataset, chunk, rep int) {
			for _, qtype := range queryTypes {
				p.Steps = append(p.Steps, queryHotLeg(p, in, cfg, d, chunk, rep, qtype))
			}
		})
	}

	// The tarball needs nothing: it runs even after failed legs, because a
	// campaign that went wrong is precisely the one whose bundle must be
	// preserved for diagnosis.
	p.Steps = append(p.Steps, Step{
		ID:   "tarball",
		Kind: KindTarball,
		Argv: [][]string{{"tar", "-C", filepath.Join(in.BenchRoot, "results"), "-czf", p.Tarball, runID}},
	})
	if cfg.PublishURI != "" {
		p.Steps = append(p.Steps, Step{
			ID:   "publish",
			Kind: KindPublish,
			// A publish step runs no external command, but argv is a required
			// plan.json field: an empty list, never null.
			Argv:       [][]string{},
			Needs:      []string{"tarball"},
			PublishURI: cfg.PublishURI,
		})
	}
	return p
}

// forEachCell visits every (dataset, chunk, rep) cell in config order — the
// loop nesting every bash suite function shares.
func forEachCell(cfg *config.Config, visit func(d *config.Dataset, chunk, rep int)) {
	for i := range cfg.Datasets {
		d := &cfg.Datasets[i]
		for _, chunk := range d.Chunks {
			for rep := 1; rep <= cfg.Runs; rep++ {
				visit(d, chunk, rep)
			}
		}
	}
}

func buildStep(p *Plan, in Inputs) Step {
	return Step{
		ID:   "build",
		Kind: KindBuild,
		Argv: [][]string{
			{"git", "-C", p.Src, "-c", "advice.detachedHead=false", "checkout", "-q", "--detach", in.BuiltCommit},
			{"make", "-C", p.Src, "build-libs"},
			// build-rpc-v2 goes through the Makefile so the binary carries the
			// repo's GOLDFLAGS (version, commit, branch, build timestamp) that
			// `stellar-rpc-v2 version` and invocation.json report. The target
			// writes ./stellar-rpc-v2 in the clone root; move it into the
			// versioned path the campaign runs.
			{"make", "-C", p.Src, "build-rpc-v2"},
			{"mv", filepath.Join(p.Src, "stellar-rpc-v2"), p.Bin},
		},
	}
}

// datasetStep converges one dataset on a local cold pack root, whatever kind it
// is. Only the kinds that invoke the binary under test depend on the build.
func datasetStep(p *Plan, in Inputs, d *config.Dataset) Step {
	root := datasetRoot(in, d)
	step := Step{
		ID:   "dataset-" + d.Name,
		Kind: KindDataset,
		// Starts empty rather than nil so a packs-local dataset, which appends
		// nothing below, still marshals argv as [] — the field is required.
		Argv: [][]string{},
		Dataset: &DatasetSpec{
			Name: d.Name,
			Kind: d.Kind,
			Root: root,
		},
	}
	switch d.Kind {
	case config.KindPacksLocal:
		// Nothing to materialize: the operator already has the pack root, and
		// the executor only validates it.
		step.Dataset.Location = d.Location
	case config.KindPacksGS:
		step.Dataset.Location = d.Location
		// Clear the empty leftover of an earlier cleared-out fetch, or the
		// rename below would nest the partial inside it. The .partial itself is
		// deliberately not listed: rsync resumes into a half-fetched tree.
		step.PreClean = []string{root}
		step.Argv = [][]string{{"gcloud", "storage", "rsync", "-r", d.Location, root + ".partial"}}
	case config.KindPacksS3:
		step.Dataset.Location = d.Location
		// Same fetch as packs-gs, with the other CLI: `aws s3 sync` copies the
		// prefix's contents into the destination, as gcloud rsync does, and
		// re-copies only what is missing or changed — so the .partial is kept
		// out of pre_clean here too.
		step.PreClean = []string{root}
		step.Argv = [][]string{{"aws", "s3", "sync", d.Location, root + ".partial"}}
	case config.KindBSBS3:
		step.Dataset.Location = d.Location
		step.Needs = []string{"build"}
		// Unlike a fetch, a cold backfill cannot resume: restarting on top of a
		// half-written pack tree would double-write it.
		step.PreClean = []string{root, root + ".partial"}
		// AWS_EC2_METADATA_DISABLED is set on these commands only: without it
		// the SDK signs requests with the machine's IAM role and the public
		// bucket 403s, but setting it for the whole campaign would also hide
		// those same instance-role credentials from the publish step's `aws
		// s3` calls.
		step.Env = map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}
		for _, chunk := range d.Chunks {
			step.Argv = append(step.Argv, []string{
				p.Bin, "bench-ingest", "cold",
				"--source=bsb", "--datastore-type=S3", "--region=us-east-2",
				"--bucket-path=" + d.Location,
				"--start-chunk=" + strconv.Itoa(chunk), "--num-chunks=1",
				"--cold-out-dir=" + root + ".partial",
				"--out=" + filepath.Join(p.ResultsDir, fmt.Sprintf("golden-%s-c%d", d.Name, chunk)),
			})
		}
	case config.KindFixture:
		stage := filepath.Join(in.BenchRoot, "fixture", d.Name, "ledgers")
		step.Needs = []string{"build"}
		step.Dataset.Stage = stage
		// The staging tree goes too, and by its parent: generation writes the
		// ledgers/ dir itself, so a stale one from a killed run must not survive.
		step.PreClean = []string{filepath.Dir(stage), root, root + ".partial"}
		if d.Ledgers != nil {
			ledgers := *d.Ledgers
			step.Dataset.Ledgers = &ledgers
		}
		for _, chunk := range d.Chunks {
			step.Argv = append(step.Argv, []string{
				p.Bin, "bench-ingest", "fixture",
				"--pack-dir=" + stage, "--chunk=" + strconv.Itoa(chunk),
				"--num-ledgers=" + strconv.Itoa(derefLedgers(d)), "--seed=1",
			})
		}
		// Freezing every chunk after generating every chunk, rather than
		// interleaving, is bash's order: the freeze reads the whole staged
		// pack tree.
		for _, chunk := range d.Chunks {
			step.Argv = append(step.Argv, []string{
				p.Bin, "bench-ingest", "cold",
				"--source=pack", "--pack-dir=" + stage,
				"--start-chunk=" + strconv.Itoa(chunk), "--num-chunks=1",
				"--cold-out-dir=" + root + ".partial",
				"--out=" + filepath.Join(p.ResultsDir, fmt.Sprintf("golden-%s-c%d", d.Name, chunk)),
			})
		}
	}
	return step
}

func ingestColdLeg(p *Plan, in Inputs, cfg *config.Config, d *config.Dataset, chunk, rep int) Step {
	id := fmt.Sprintf("ingest-cold-%s-c%d-run%d", d.Name, chunk, rep)
	out := filepath.Join(p.ResultsDir, id)
	scratch := filepath.Join(in.BenchRoot, "scratch", d.Name, strconv.Itoa(chunk))
	return Step{
		ID:    id,
		Kind:  KindLeg,
		Timed: true,
		Argv: [][]string{{
			p.Bin, "bench-ingest", "cold",
			"--source=pack", "--pack-dir=" + filepath.Join(datasetRoot(in, d), "ledgers"),
			"--start-chunk=" + strconv.Itoa(chunk), "--num-chunks=1",
			"--workers=" + strconv.Itoa(cfg.Workers),
			"--cold-out-dir=" + scratch,
			"--out=" + out,
		}},
		OutDir: out,
		// Nothing reads the cold scratch DB afterwards, so it goes away as
		// soon as the rep is done instead of squatting until the next rep.
		PreClean:  []string{scratch},
		PostClean: []string{scratch},
		Needs:     []string{"build", "dataset-" + d.Name},
	}
}

func ingestHotLeg(p *Plan, in Inputs, cfg *config.Config, d *config.Dataset, chunk, rep int) Step {
	id := fmt.Sprintf("ingest-hot-%s-c%d-run%d", d.Name, chunk, rep)
	out := filepath.Join(p.ResultsDir, id)
	hot := hotDir(in, d, chunk)
	argv := []string{
		p.Bin, "bench-ingest", "hot",
		"--source=pack", "--pack-dir=" + filepath.Join(datasetRoot(in, d), "ledgers"),
		"--start-chunk=" + strconv.Itoa(chunk), "--hot-dir=" + hot,
		"--close-interval=" + cfg.CloseInterval,
	}
	if cfg.HotNumLedgers > 0 {
		argv = append(argv, "--num-ledgers="+strconv.Itoa(cfg.HotNumLedgers))
	}
	argv = append(argv, "--out="+out)
	return Step{
		ID:     id,
		Kind:   KindLeg,
		Timed:  true,
		Argv:   [][]string{argv},
		OutDir: out,
		// The hot DB is wiped before each rep but deliberately kept after the
		// last one: the hot query suite reads what that rep left behind. Every
		// rep of a cell ingests the same chunk, so the DB the last one leaves
		// is the whole DB.
		PreClean: []string{hot},
		Needs:    []string{"build", "dataset-" + d.Name},
	}
}

func queryColdLeg(p *Plan, in Inputs, d *config.Dataset, chunk, rep int, qtype string) Step {
	id := fmt.Sprintf("query-cold-%s-c%d-%s-run%d", d.Name, chunk, qtype, rep)
	out := filepath.Join(p.ResultsDir, id)
	return Step{
		ID:    id,
		Kind:  KindLeg,
		Timed: true,
		Argv: [][]string{{
			p.Bin, "bench-query", "cold",
			"--cold-dir=" + datasetRoot(in, d),
			"--start-chunk=" + strconv.Itoa(chunk), "--num-chunks=1",
			"--types=" + qtype,
			"--target-rps=" + targetRPS(in, d, qtype),
			"--duration=" + in.QueryDuration,
			"--out=" + out,
		}},
		OutDir: out,
		Needs:  []string{"build", "dataset-" + d.Name},
	}
}

func queryHotLeg(p *Plan, in Inputs, cfg *config.Config, d *config.Dataset, chunk, rep int, qtype string) Step {
	id := fmt.Sprintf("query-hot-%s-c%d-%s-run%d", d.Name, chunk, qtype, rep)
	out := filepath.Join(p.ResultsDir, id)
	hot := hotDir(in, d, chunk)
	argv := []string{
		p.Bin, "bench-query", "hot",
		"--hot-dir=" + hot, "--chunk=" + strconv.Itoa(chunk),
		"--types=" + qtype,
		"--target-rps=" + targetRPS(in, d, qtype),
		"--duration=" + in.QueryDuration,
		"--warmup=20",
	}
	// A capped hot ingest leaves a truncated DB; keep the query sampler inside
	// what was ingested.
	if cfg.HotNumLedgers > 0 {
		argv = append(argv, "--sample-ledgers="+strconv.Itoa(cfg.HotNumLedgers))
	}
	argv = append(argv, "--out="+out)
	return Step{
		ID:     id,
		Kind:   KindLeg,
		Timed:  true,
		Argv:   [][]string{argv},
		OutDir: out,
		// The dependency is on the *last* hot-ingest rep of this cell, not on
		// this leg's own rep number: each rep wipes and rewrites the hot DB,
		// so only the final rep's success guarantees a whole DB to query.
		Needs: []string{
			"build",
			"dataset-" + d.Name,
			fmt.Sprintf("ingest-hot-%s-c%d-run%d", d.Name, chunk, cfg.Runs),
		},
	}
}

// targetRPS renders one cell's ladder as bench-query's --target-rps value: the
// rates in ladder order, comma-separated. The 'f' format with the shortest
// round-tripping precision is what bench-query itself spells its per-rate CSV
// rows with, so the two sides name the same rate the same way.
func targetRPS(in Inputs, d *config.Dataset, qtype string) string {
	rates := in.QueryRates[d.Name][qtype]
	parts := make([]string, len(rates))
	for i, r := range rates {
		parts[i] = strconv.FormatFloat(r, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}

// datasetRoot is the local cold pack root a dataset converges on: its own
// location when the operator already has the packs, a campaign-owned golden
// directory when the runner has to materialize them.
func datasetRoot(in Inputs, d *config.Dataset) string {
	if d.Kind == config.KindPacksLocal {
		return d.Location
	}
	return filepath.Join(in.BenchRoot, "golden", d.Name)
}

func hotDir(in Inputs, d *config.Dataset, chunk int) string {
	return filepath.Join(in.BenchRoot, "hot", d.Name, strconv.Itoa(chunk))
}

// derefLedgers is the fixture ledger count; config validation guarantees
// fixture datasets have one, and 0 means the whole chunk.
func derefLedgers(d *config.Dataset) int {
	if d.Ledgers == nil {
		return 0
	}
	return *d.Ledgers
}

// WriteFile writes the plan as indented JSON with a trailing newline.
func (p *Plan) WriteFile(path string) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Print writes the plan the way bash's --dry-run did: a header per step and one
// `$ command` line per command, with no quoting or escaping — these lines are
// for reading, not for pasting into a shell.
func (p *Plan) Print(w io.Writer) {
	for _, note := range p.Notes {
		fmt.Fprintf(w, "== note: %s\n", note)
	}
	for _, s := range p.Steps {
		fmt.Fprintf(w, "== %s\n", s.ID)
		if len(s.PreClean) > 0 {
			// One line for the whole list, as the executor's single rm -rf.
			fmt.Fprintf(w, "  $ rm -rf %s\n", strings.Join(s.PreClean, " "))
		}
		// A dataset step's .partial dance belongs to the executor, not to argv,
		// so the argv lines alone would hide the destructive half of what a
		// dataset preparation does. These lines are derived from the same
		// DatasetSpec the executor keys off, so a dry run and campaign.log read
		// identically.
		if s.Dataset != nil && fetches(s.Dataset.Kind) {
			fmt.Fprintf(w, "  $ mkdir -p %s.partial\n", s.Dataset.Root)
		}
		prefix := envPrefix(s.Env)
		for _, argv := range s.Argv {
			fmt.Fprintf(w, "  $ %s%s\n", prefix, strings.Join(argv, " "))
		}
		if s.Dataset != nil && materializes(s.Dataset.Kind) {
			fmt.Fprintf(w, "  $ mv %s.partial %s\n", s.Dataset.Root, s.Dataset.Root)
		}
		if s.Kind == KindPublish {
			fmt.Fprintf(w, "  $ campaign publish %s %s\n", p.ResultsDir, s.PublishURI)
		}
	}
}

// materializes reports whether a dataset kind builds its pack root under
// <root>.partial and renames it into place once whole. packs-local is the one
// kind that does not: the operator already has the packs.
func materializes(kind string) bool {
	switch kind {
	case config.KindPacksGS, config.KindPacksS3, config.KindBSBS3, config.KindFixture:
		return true
	}
	return false
}

// fetches reports whether a dataset kind copies an existing pack tree down from
// object storage. Both fetching CLIs need the destination directory to exist;
// the kinds that write their own pack tree create it themselves.
func fetches(kind string) bool {
	switch kind {
	case config.KindPacksGS, config.KindPacksS3:
		return true
	}
	return false
}

// envPrefix renders a step's extra environment as bash printed it: an `env
// K=V ` prefix on the command line. Keys are sorted so the output is stable.
func envPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("env ")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s ", k, env[k])
	}
	return b.String()
}
