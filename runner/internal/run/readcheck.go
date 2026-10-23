package run

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/plan"
)

// errPackGeneration marks a dataset the binary under test cannot read;
// Execute stops the walk on it.
var errPackGeneration = errors.New("dataset unreadable by the binary under test")

// readCheckTail is how many trailing output lines of a failed read check the
// error quotes.
const readCheckTail = 3

// checkReadable runs the dataset step's read check in a fresh scratch dir,
// removed afterwards, and fails with the binary's own error when it exits
// non-zero.
func checkReadable(p *plan.Plan, d *plan.DatasetSpec, opts Options) error {
	if err := removeAll(opts.Output, d.ReadCheckDir); err != nil {
		return err
	}
	var buf bytes.Buffer
	runErr := runCommand(d.ReadCheck, nil, opts.commandEnv(d.ReadCheck, nil), io.MultiWriter(opts.Output, &buf))
	if err := removeAll(opts.Output, d.ReadCheckDir); err != nil && runErr == nil {
		return err
	}
	if runErr == nil {
		return nil
	}
	return fmt.Errorf("%w: dataset %s: %s cannot read %s (%v): %s — %s", errPackGeneration, d.Name,
		filepath.Base(p.Bin), filepath.Join(d.Root, "ledgers"), runErr, lastLines(buf.String(), readCheckTail),
		readCheckDiagnosis(p, d))
}

// readCheckDiagnosis names the generation the dataset is and the one the
// registry chose for the binary, and how to drop a stale fetch.
func readCheckDiagnosis(p *plan.Plan, d *plan.DatasetSpec) string {
	is := "of no registered generation"
	if g := plan.LocationGeneration(d.Location); g != "" {
		is = string(g)
	}
	sha := p.Sha8()
	msg := fmt.Sprintf("dataset %s is %s; ", d.Name, is)
	if p.PackGeneration == "" {
		msg += fmt.Sprintf("no registry marker matched %s", sha)
	} else {
		msg += fmt.Sprintf("the registry chose it for %s by marker %s", sha, p.PackMarker)
	}
	if d.Kind != config.KindPacksLocal {
		msg += fmt.Sprintf("; when %s holds an earlier fetch, rm -rf it to fetch %s", d.Root, d.Location)
	}
	return msg
}

// lastLines is the last n non-empty lines of s, joined by " | ".
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines[max(0, len(lines)-n):], " | ")
}
