// Command campaign runs config-driven benchmark campaigns for stellar-rpc's
// full-history bench subcommands. It is the Go successor to
// runner/campaign.sh; the subcommands below are stubs until the port lands.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
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
		if _, err := parseArgs(fs, args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
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
