// Command campaign runs config-driven benchmark campaigns for stellar-rpc's
// full-history bench subcommands. It is the Go successor to
// runner/campaign.sh and runner/publish.sh.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/preflight"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/publish"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/run"
)

// defaultBenchRoot is the benchmark machine's NVMe mount; BENCH_ROOT overrides
// it on any other machine.
const defaultBenchRoot = "/mnt/nvme/bench"

// placeholderSha stands in for the built commit when the ref cannot be
// resolved locally. It must be 8 hex digits, or a --resume would reject the run
// ids derived from it as malformed.
const placeholderSha = "deadbeef"

// stampLayout is the run id's UTC timestamp, e.g. 20260101T000000Z.
const stampLayout = "20060102T150405Z"

const topUsage = `campaign — stellar-rpc full-history benchmark campaigns

usage: campaign <subcommand> [args]

subcommands:
  run        run a campaign from a config, producing a results bundle
  plan       print the steps a campaign would execute, without running them
  preflight  check tools, credentials, and disk this config needs
  publish    upload a finished results bundle to object storage

environment:
  BENCH_ROOT  storage root for the build clone, datasets, scratch space, and
              results (default /mnt/nvme/bench)
`

// Usage text for each subcommand, describing the shape it will have once
// ported. Keyed by subcommand name.
var subUsage = map[string]string{
	"run": `usage: campaign run <config.toml> [--dry-run] [--resume <results-dir>] [--fail-fast] [--no-preflight]

Run a campaign: build the configured ref, prepare its datasets, and execute
every ingest and query leg into a results bundle.

  --dry-run        print the plan; build, download, and run nothing
  --resume DIR     continue an interrupted campaign into an existing bundle
  --fail-fast      stop at the first failed step (default: keep going)
  --no-preflight   skip the up-front tool and credential checks
`,
	"plan": `usage: campaign plan <config.toml>

Print the ordered steps the campaign would execute, one command per line.
`,
	"preflight": `usage: campaign preflight <config.toml>

Check that the tools, credentials, mounts, and free disk this config needs are
available, before a campaign spends hours discovering otherwise.
`,
	"publish": `usage: campaign publish <results-dir> [dest-root] [--dry-run] [--force]

Upload a results bundle to <dest-root>/<run_id>/, where run_id is the bundle's
basename. dest-root defaults to $PUBLISH_URI from the environment; gs:// and
s3:// are the only supported schemes. Published runs are immutable.

  --dry-run   print the cloud commands, execute none of them
  --force     write into a non-empty destination (a merge, not a replace)
`,
}

// subFlags builds the flag set for one subcommand: the flags the ported
// command will accept, a usage function printing that subcommand's text, and
// ContinueOnError so the dispatcher — not flag — decides the exit code.
func subFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(fs.Output(), subUsage[name]) }
	switch name {
	case "run":
		fs.Bool("dry-run", false, "")
		fs.String("resume", "", "")
		fs.Bool("fail-fast", false, "")
		fs.Bool("no-preflight", false, "")
	case "publish":
		fs.Bool("dry-run", false, "")
		fs.Bool("force", false, "")
	}
	return fs
}

// parseArgs parses flags and positionals interleaved in any order,
// returning the positionals. flag.FlagSet.Parse stops at the first
// positional; benchmark operators put the config path first.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// Parse stops either at the first positional (consuming nothing)
		// or at a "--" terminator (consuming it). After "--" everything
		// is positional, verbatim — do not reparse it.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// planCmd prints the steps a campaign would execute. It builds no clone,
// fetches nothing, and writes nothing — it is readable on any machine.
func planCmd(pos []string, stdout, stderr io.Writer) int {
	if len(pos) != 1 {
		fmt.Fprint(stderr, subUsage["plan"])
		fmt.Fprint(stderr, "error: plan needs exactly one config path\n")
		return 2
	}
	cfg, err := config.Load(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}
	benchRoot := benchRootFromEnv()
	in := plan.Inputs{
		BenchRoot: benchRoot,
		Stamp:     time.Now().UTC().Format(stampLayout),
	}
	src := filepath.Join(benchRoot, "src")
	if sha, err := run.ResolveRef(src, cfg.Ref); err == nil {
		in.BuiltCommit, in.Sha8 = sha, sha[:8]
	} else {
		// Planning fetches nothing, so the ref may not resolve locally yet:
		// plan with the ref itself and a placeholder sha in derived paths.
		in.BuiltCommit, in.Sha8 = cfg.Ref, placeholderSha
		fmt.Fprintf(stdout, "== note: ref '%s' is not resolvable in %s — using placeholder sha '%s' in paths\n",
			cfg.Ref, src, placeholderSha)
	}
	plan.Build(cfg, in).Print(stdout)
	return 0
}

// preflightCmd checks the tools, credentials, mount, and free disk this config
// needs, before a campaign spends hours discovering otherwise. It resolves no
// ref and touches no clone: binPath is empty, so the toolchain checks assume a
// build will happen. `campaign run` passes the real versioned binary path,
// which lets an already-built ref skip them.
func preflightCmd(pos []string, stdout, stderr io.Writer) int {
	if len(pos) != 1 {
		fmt.Fprint(stderr, subUsage["preflight"])
		fmt.Fprint(stderr, "error: preflight needs exactly one config path\n")
		return 2
	}
	cfg, err := config.Load(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}
	if !printPreflight(preflight.Run(cfg, benchRootFromEnv(), "", preflight.Deps{}), stdout) {
		return 1
	}
	fmt.Fprintln(stdout, "preflight: ok")
	return 0
}

// publishCmd uploads a finished bundle to <dest-root>/<run_id>/. The
// destination is the argument or $PUBLISH_URI — the same default the campaign
// config's publish_uri feeds the end of a run.
func publishCmd(pos []string, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	if len(pos) == 0 {
		fmt.Fprint(stderr, subUsage["publish"])
		fmt.Fprint(stderr, "error: publish needs a results directory\n")
		return 2
	}
	if len(pos) > 2 {
		fmt.Fprintf(stderr, "error: unexpected extra argument: %s\n", pos[2])
		return 2
	}
	destRoot := os.Getenv("PUBLISH_URI")
	if len(pos) == 2 {
		destRoot = pos[1]
	}
	if destRoot == "" {
		fmt.Fprint(stderr, "error: no destination: pass <dest-root-uri> or set PUBLISH_URI\n")
		return 2
	}
	if err := publish.Run(pos[0], destRoot, boolFlag(fs, "dry-run"), boolFlag(fs, "force"), stdout); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	return 0
}

// benchRootFromEnv is the storage root every subcommand works under.
func benchRootFromEnv() string {
	if root := os.Getenv("BENCH_ROOT"); root != "" {
		return root
	}
	return defaultBenchRoot
}

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, topUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, topUsage)
		return 0
	case "run", "plan", "preflight", "publish":
		fs := subFlags(args[0], stderr)
		pos, err := parseArgs(fs, args[1:])
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		switch args[0] {
		case "run":
			return runCmd(pos, fs, stdout, stderr)
		case "plan":
			return planCmd(pos, stdout, stderr)
		case "preflight":
			return preflightCmd(pos, stdout, stderr)
		case "publish":
			return publishCmd(pos, fs, stdout, stderr)
		}
		fmt.Fprint(stderr, subUsage[args[0]])
		fmt.Fprintf(stderr, "error: %s is not implemented yet\n", args[0])
		return 2
	default:
		fmt.Fprintf(stderr, "error: unknown subcommand: %s\n", args[0])
		fmt.Fprint(stderr, topUsage)
		return 2
	}
}
