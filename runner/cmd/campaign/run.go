package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/bundle"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/memlimit"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/preflight"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/publish"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/run"
)

// meminfoPath is where gomemlimit = auto reads MemTotal.
const meminfoPath = "/proc/meminfo"

// startedAtLayout is metadata.json's timestamp format: UTC, second precision.
const startedAtLayout = "2006-01-02T15:04:05Z"

// runCmd runs a campaign: converge the build clone, resolve the ref, preflight
// the machine, then walk the plan into a results bundle. It is the successor to
// the main sequence of the bash campaign runner, in the same order, for the
// same reasons.
func runCmd(pos []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	if len(pos) != 1 {
		fmt.Fprint(stderr, subUsage["run"])
		fmt.Fprint(stderr, "error: run needs exactly one config path\n")
		return 2
	}
	cfgPath, err := filepath.Abs(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	// The RPS floors are resolved before anything is built or locked: a config
	// the load model cannot pace is a config error, not a campaign failure.
	ql, err := resolveQueryLoad(cfg, stringFlag(fs, "targets"))
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	benchRoot := benchRootFromEnv()
	src := filepath.Join(benchRoot, "src")
	resumeDir := stringFlag(fs, "resume")
	if boolFlag(fs, "dry-run") {
		return dryRun(cfg, ql, cfgPath, benchRoot, src, resumeDir, stdout, stderr)
	}
	return realRun(cfg, ql, cfgPath, benchRoot, src, resumeDir,
		boolFlag(fs, "fail-fast"), boolFlag(fs, "no-preflight"), stdout, stderr)
}

// dryRun prints the plan and executes nothing — no clone, no lock, no
// directory, exactly as the bash campaign runner's --dry-run did. Whatever the
// clone already knows is used; what it does not know is planned with a
// placeholder sha.
func dryRun(cfg *config.Config, ql queryLoad, cfgPath, benchRoot, src, resumeDir string, stdout, stderr io.Writer) int {
	run.Notef(stdout, "dry run: printing commands only — nothing is built, downloaded, or executed")
	run.Notef(stdout, "source: %s @ %s → %s (the build clone is not touched)", cfg.Repo, cfg.Ref, src)
	if cfg.Query {
		run.Notef(stdout, "query load: phase %s floors × the targets.json ladder, %s per leg", ql.phase, cfg.QueryDuration)
	}
	_, limitNote := memlimit.Resolve(cfg.GoMemLimit, meminfoPath)
	run.Notef(stdout, "%s", limitNote)

	in := plan.Inputs{
		BenchRoot:         benchRoot,
		Stamp:             time.Now().UTC().Format(stampLayout),
		QueryRates:        ql.rates,
		QueryDuration:     queryDuration(cfg),
		QueryLedgersLimit: ql.ledgersLimit,
	}
	builtCommit, resolveErr := run.ResolveRef(src, cfg.Ref)
	if resolveErr == nil {
		in.BuiltCommit, in.Sha8 = builtCommit, builtCommit[:8]
		probeShape(cfg, src, builtCommit, &in, stdout)
	} else {
		// A dry run fetches nothing, so the ref may not resolve locally yet:
		// plan with the ref itself and a placeholder sha in derived paths. The
		// placeholder is 8 hex digits, or a --resume would reject the run ids
		// derived from it as malformed.
		in.BuiltCommit, in.Sha8 = cfg.Ref, placeholderSha
		run.Notef(stdout, "dry run: ref '%s' not resolvable without the clone — using placeholder sha '%s' in paths, "+
			"%s", cfg.Ref, placeholderSha, assumeCurrentShape(cfg, &in))
	}

	var resume *bundle.Resume
	if resumeDir != "" {
		if resolveErr != nil {
			// A resume is only valid against the commit its bundle was
			// benchmarked with, and nothing here can know that commit without
			// the clone. Refusing beats printing a plan for a run id that a
			// real resume would reject.
			fmt.Fprintf(stderr, "error: --dry-run --resume needs the build clone at %s to resolve ref '%s' — "+
				"a resume must be validated against the commit its bundle was benchmarked with\n", src, cfg.Ref)
			return 1
		}
		dir, err := filepath.Abs(resumeDir)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
		resume, err = bundle.ValidateResume(dir, cfgPath, cfg, in.BuiltCommit, benchRoot, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
		in.Stamp = resume.Stamp
	}

	p := plan.Build(cfg, in)
	run.Notef(stdout, "campaign %s → %s", cfg.Name, p.ResultsDir)
	if resume != nil {
		run.Notef(stdout, "resume: continuing %s — finished legs are skipped", resume.RunID)
		for _, s := range p.Steps {
			if s.Kind == plan.KindLeg && run.LegComplete(s.OutDir, s.ID) {
				run.Notef(stdout, "resume: %s already complete — would skip", s.ID)
			}
		}
	}
	p.Print(stdout)
	run.Notef(stdout, "dry run complete")
	return 0
}

// probeShape fills in's commit shape for the commands that only print a plan:
// run.DetectBenchCLI, run.DetectBuildTarget, run.DetectRPCV2BuildFlag,
// run.DetectEmptyColdOutDir, and run.DetectQueryCLI for a query campaign. A failed probe, a missing query
// bench, or a config the shape cannot run is a note, and the plan falls back
// to the default shape.
func probeShape(cfg *config.Config, src, commit string, in *plan.Inputs, stdout io.Writer) {
	cli, err := run.DetectBenchCLI(src, commit)
	if err != nil {
		run.Notef(stdout, "%s — planning the %s CLI", err, plan.BenchCLIRoot)
		return
	}
	in.BenchCLI = cli
	if target, static, err := run.DetectBuildTarget(src, commit); err != nil {
		run.Notef(stdout, "%s — planning %s", err, plan.BuildRPCV2)
	} else {
		in.BuildTarget, in.StaticLibs = target, static
	}
	if flag, err := run.DetectRPCV2BuildFlag(src, commit); err != nil {
		run.Notef(stdout, "%s", err)
	} else {
		in.RPCV2BuildFlag = flag
	}
	if empty, err := run.DetectEmptyColdOutDir(src, commit); err != nil {
		run.Notef(stdout, "%s", err)
	} else {
		in.EmptyColdOutDir = empty
	}
	if cfg.Query {
		switch shape, ok, err := run.DetectQueryCLI(src, commit, cli); {
		case err != nil:
			run.Notef(stdout, "%s", err)
		case !ok:
			run.Notef(stdout, "%s", noQueryCLI(commit, cli))
		default:
			in.QueryCLI = shape
		}
	}
	if err := plan.Check(cfg, *in); err != nil {
		run.Notef(stdout, "%s", err)
	}
}

// assumeCurrentShape fills in with the shape of current stellar-rpc commits,
// for a plan whose ref the build clone cannot resolve, and returns the words
// that name it for the note. A config that shape cannot run is named too.
func assumeCurrentShape(cfg *config.Config, in *plan.Inputs) string {
	in.BenchCLI, in.QueryCLI = plan.BenchCLIRoot, plan.QueryCLIDataDir
	in.BuildTarget, in.StaticLibs, in.RPCV2BuildFlag, in.EmptyColdOutDir = plan.BuildStellarRPC, true, true, true
	assumed := fmt.Sprintf("assuming the current shapes: the %s CLI (%s), the %s query CLI, and make %s "+
		"RPCV2=true with static libs (--enable-rpcv2)", in.BenchCLI, benchCLIDescription(in.BenchCLI), in.QueryCLI, in.BuildTarget)
	if err := plan.Check(cfg, *in); err != nil {
		assumed += "; " + err.Error()
	}
	return assumed
}

// noQueryCLI is the message for a query campaign whose commit has no query bench.
func noQueryCLI(commit string, cli plan.BenchCLI) string {
	return fmt.Sprintf("commit %s has no query bench (%s CLI: %s); query = true needs a ref that has one",
		commit, cli, benchCLIDescription(cli))
}

// benchCLIDescription names the commands a CLI shape stands for.
func benchCLIDescription(cli plan.BenchCLI) string {
	if cli == plan.BenchCLILegacy {
		return "bench-ingest/bench-query"
	}
	return "bench ingest/bench query"
}

// realRun is the campaign itself.
func realRun(cfg *config.Config, ql queryLoad, cfgPath, benchRoot, src, resumeDir string,
	failFast, noPreflight bool, stdout, stderr io.Writer) int {

	// One campaign per BENCH_ROOT: two would fight over the build clone, the
	// scratch dirs, and the hot DBs, and each would measure the other.
	release, err := run.AcquireLock(benchRoot)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	defer release()

	// Until the bundle exists there is nowhere to tee the log to; these first
	// notes go to the terminal only, as they did in bash.
	out := io.Writer(stdout)

	run.Notef(out, "source: %s @ %s → %s", cfg.Repo, cfg.Ref, src)
	if err := run.EnsureSrc(src, cfg.Repo, out); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	builtCommit, err := run.ResolveRef(src, cfg.Ref)
	if err != nil {
		fmt.Fprintf(stderr, "error: ref '%s' does not resolve to a commit in %s\n", cfg.Ref, cfg.Repo)
		return 1
	}
	sha8 := builtCommit[:8]
	binPath := filepath.Join(benchRoot, "bin", "stellar-rpc-"+sha8)
	benchCLI, err := run.DetectBenchCLI(src, builtCommit)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	run.Notef(out, "bench CLI: %s (%s)", benchCLI, benchCLIDescription(benchCLI))
	buildTarget, staticLibs, err := run.DetectBuildTarget(src, builtCommit)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	rpcv2BuildFlag, err := run.DetectRPCV2BuildFlag(src, builtCommit)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	run.Notef(out, "build target: make %s (static libs: %t, RPCV2=true: %t)", buildTarget, staticLibs, rpcv2BuildFlag)
	emptyColdOutDir, err := run.DetectEmptyColdOutDir(src, builtCommit)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	var queryCLI plan.QueryCLI
	if cfg.Query {
		shape, ok, err := run.DetectQueryCLI(src, builtCommit, benchCLI)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(stderr, "error: %s\n", noQueryCLI(builtCommit, benchCLI))
			return 1
		}
		queryCLI = shape
		run.Notef(out, "query CLI: %s", queryCLI)
	}

	if !noPreflight {
		// Everything this campaign needs and does not have, named now rather
		// than seventeen hours from now.
		if !printPreflight(preflight.Run(cfg, benchRoot, binPath, preflight.Deps{}), out) {
			return 1
		}
	}

	session := "start"
	stamp := time.Now().UTC().Format(stampLayout)
	startedAt := time.Now().UTC().Format(startedAtLayout)
	configFile := filepath.Base(cfgPath)
	var resume *bundle.Resume
	if resumeDir != "" {
		dir, err := filepath.Abs(resumeDir)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
		resume, err = bundle.ValidateResume(dir, cfgPath, cfg, builtCommit, benchRoot, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
		session, stamp, configFile = "resume", resume.Stamp, resume.ConfigFile
		// started_at comes from the bundle so metadata.json still spans the
		// whole campaign; bundles written before it was recorded have none.
		if resume.StartedAt != "" {
			startedAt = resume.StartedAt
		}
	}

	in := plan.Inputs{
		BenchRoot:         benchRoot,
		BuiltCommit:       builtCommit,
		Sha8:              sha8,
		Stamp:             stamp,
		QueryRates:        ql.rates,
		QueryDuration:     queryDuration(cfg),
		QueryLedgersLimit: ql.ledgersLimit,
		BenchCLI:          benchCLI,
		QueryCLI:          queryCLI,
		BuildTarget:       buildTarget,
		StaticLibs:        staticLibs,
		RPCV2BuildFlag:    rpcv2BuildFlag,
		EmptyColdOutDir:   emptyColdOutDir,
	}
	if err := plan.Check(cfg, in); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	p := plan.Build(cfg, in)
	res := p.ResultsDir
	if resume != nil && filepath.Clean(res) != filepath.Clean(resume.Dir) {
		fmt.Fprintf(stderr, "error: --resume: '%s' is not the bundle this config and commit produce (%s)\n",
			resume.Dir, res)
		return 1
	}

	run.Notef(out, "campaign %s → %s", cfg.Name, res)
	for _, dir := range []string{"bin", "golden", "scratch", "hot", "fixture"} {
		if err := os.MkdirAll(filepath.Join(benchRoot, dir), 0o755); err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
	}
	if err := os.MkdirAll(res, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	if resume == nil {
		// Only a fresh bundle stores the config. On a resume the stored copy
		// has already been proven byte-identical to this one, and overwriting
		// it is precisely how the bash runner lost the record of what a
		// campaign was started with.
		if err := copyFile(cfgPath, filepath.Join(res, configFile)); err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err)
			return 1
		}
	}

	// From here on the runner's console is part of the bundle: on a campaign
	// that dies it is the only record of how far it got. Appended, so the
	// sessions of a resumed campaign accumulate in one file.
	logFile, err := run.OpenCampaignLog(res)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	defer logFile.Close()
	out = io.MultiWriter(stdout, logFile)
	run.Notef(out, "session %s %s — logging to %s", session, time.Now().UTC().Format(startedAtLayout), logFile.Name())
	if cfg.Query {
		// Which floors the query legs were paced at is the first thing anyone
		// reading this bundle six months later will want to know.
		run.Notef(out, "query load: phase %s floors × the targets.json ladder, %s per leg", ql.phase, cfg.QueryDuration)
	}
	limit, limitNote := memlimit.Resolve(cfg.GoMemLimit, meminfoPath)
	run.Notef(out, "%s", limitNote)

	hostname, _ := os.Hostname()
	meta := bundle.MetadataInput{
		Cfg:               cfg,
		ConfigFile:        configFile,
		RunID:             p.RunID,
		BuiltCommit:       builtCommit,
		QueryPhase:        ql.phase,
		QueryLedgersLimit: p.QueryLedgersLimit,
		GoMemLimit:        &limit,
		Resumed:           resume != nil,
		Hardware:          bundle.CollectHardware(bundle.DefaultIMDSBase),
		Hostname:          hostname,
		StartedAt:         startedAt,
		Status:            bundle.StatusRunning,
	}
	if resume != nil && resume.StartedAt == "" {
		run.Notef(out, "resume: no started_at in %s/%s — recording this session's start", resume.RunID, bundle.MetadataName)
	}
	// A manifest up front makes a killed campaign's partial bundle parseable;
	// the end-of-campaign rewrite adds finished_at and the final status.
	if err := bundle.WriteMetadata(res, meta); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	// The plan is written on every session: a resume builds the same plan by
	// construction, and rewriting it keeps the file current with the runner
	// that produced the bundle's newest legs.
	if err := p.WriteFile(filepath.Join(res, "plan.json")); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}

	_, execErr := run.Execute(p, run.Options{
		Output:   out,
		Resume:   resume != nil,
		FailFast: failFast,
		BenchEnv: limit.Env(),
		OnStepDone: func(s plan.Step, r run.StepResult) {
			// Right after the build, not at the end: a campaign that dies in
			// its legs still says which binary produced them.
			if s.Kind != plan.KindBuild || r.Status != run.StatusOK {
				return
			}
			if err := bundle.WriteBinaryInfo(res, p.Bin, cfg.Repo, cfg.Ref, builtCommit); err != nil {
				run.Notef(out, "warning: %s", err)
			}
		},
	})

	// The epilogue runs even after a failed campaign: a campaign that went
	// wrong is exactly the one whose bundle has to be complete and preserved.
	run.Notef(out, "machine metadata")
	if err := bundle.WriteMachineMetadata(res, bundle.MachineInput{
		Cfg: cfg, Repo: cfg.Repo, Ref: cfg.Ref, BuiltCommit: builtCommit,
		BinPath: p.Bin, BenchRoot: benchRoot, Hardware: meta.Hardware,
		QueryPhase: ql.phase,
	}); err != nil {
		run.Notef(out, "warning: %s", err)
	}
	meta.FinishedAt = time.Now().UTC().Format(startedAtLayout)
	meta.Status = bundle.StatusFinished
	if execErr != nil {
		meta.Status = bundle.StatusFailed
	}
	if err := bundle.WriteMetadata(res, meta); err != nil {
		run.Notef(out, "warning: %s", err)
	}

	// Tar last, so the bundle it preserves contains every file above.
	tarErr := tarBundle(p, out)
	if tarErr != nil {
		// Reported, never masking a leg failure: the data is still in res.
		run.Notef(out, "warning: tar failed: %s — the bundle is intact at %s", tarErr, res)
	} else {
		run.Notef(out, "campaign done: %s", p.Tarball)
	}
	// Publishing is a separate final step, after the "campaign done" note: the
	// data is already safe in res and the tarball, so a publish failure is not
	// a benchmark failure — it exits 1 with the exact retry command rather than
	// corrupting that signal.
	var publishErr error
	if cfg.PublishURI != "" {
		if tarErr != nil {
			// The plan's publish step needs the tarball, and bash's set -e
			// aborted before publish whenever tar failed. Publishing here would
			// upload a bundle the retry note cannot honestly call safe.
			// covered by the task-10 e2e (tar-failure scenario)
			run.Notef(out, "skipping publish: tar failed — publish manually once the bundle is intact: campaign publish %s %s",
				res, cfg.PublishURI)
		} else if publishErr = publish.Run(res, cfg.PublishURI, false, false, out); publishErr != nil {
			fmt.Fprintf(out, "error: %s\n", publishErr)
			// A failed upload can still have written objects, which the retry's
			// immutability check then refuses: name --force here rather than
			// leaving the operator to rediscover it.
			run.Notef(out, "publish failed — data is safe in %s and %s; retry with: campaign publish %s %s "+
				"(add --force if the failed upload left objects behind)",
				res, p.Tarball, res, cfg.PublishURI)
		}
	}

	if execErr != nil || tarErr != nil || publishErr != nil {
		return 1
	}
	return 0
}

// tarBundle runs the plan's tarball step. Execute leaves it to the wiring; the
// step still comes from the plan, so what runs is what the plan printed.
func tarBundle(p *plan.Plan, out io.Writer) error {
	for _, s := range p.Steps {
		if s.Kind == plan.KindTarball {
			return run.RunStep(s, out)
		}
	}
	return fmt.Errorf("plan has no tarball step")
}

// printPreflight reports what preflight found and says whether the campaign may
// proceed. Failures print before anything is built or fetched.
func printPreflight(res preflight.Result, w io.Writer) bool {
	for _, failure := range res.Failures {
		fmt.Fprintf(w, "preflight: FAIL — %s\n", failure)
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "preflight: warn — %s\n", warning)
	}
	return len(res.Failures) == 0
}

// copyFile copies src to dst verbatim: the bundle's stored config must be the
// bytes the operator ran, byte for byte, since that is what a later resume
// compares against.
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// boolFlag and stringFlag read a parsed flag by name. The flag sets are built
// in one place (subFlags) and consumed here, so a lookup by name keeps the two
// from drifting apart through a forgotten pointer.
func boolFlag(fs *flag.FlagSet, name string) bool {
	return fs.Lookup(name).Value.String() == "true"
}

func stringFlag(fs *flag.FlagSet, name string) string {
	return fs.Lookup(name).Value.String()
}
