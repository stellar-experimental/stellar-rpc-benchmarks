package bundle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stellar/stellar-rpc-benchmarks/runner/internal/config"
)

// BinaryInfoName and MachineMetadataName are the bundle root's two free-text
// provenance files. Nothing parses them; they are what an operator reads six
// months later to know what ran on what.
const (
	BinaryInfoName      = "binary.txt"
	MachineMetadataName = "machine-metadata.txt"
)

// WriteBinaryInfo writes <dir>/binary.txt, the benchmarked binary's identity in
// free text: binary path, commit, ref, repo, and the binary's own `version`
// output (first 3 lines, stdout+stderr).
func WriteBinaryInfo(dir, binPath, repo, ref, builtCommit string) error {
	lines := []string{
		"binary: " + binPath,
		"commit: " + builtCommit,
		"ref:    " + ref,
		"repo:   " + repo,
	}
	lines = append(lines, binaryVersion(binPath)...)
	return writeLines(filepath.Join(dir, BinaryInfoName), lines)
}

// MachineInput is what the machine-metadata writer needs beyond the shared
// hardware facts.
type MachineInput struct {
	Cfg                             *config.Config
	Repo, Ref, BuiltCommit, BinPath string
	BenchRoot                       string   // for the fsync probe file
	Hardware                        Hardware // the shared IMDS facts (no second query)
}

// WriteMachineMetadata writes <dir>/machine-metadata.txt: what the campaign ran
// on, in free text. Every fact is best-effort — one that cannot be gathered is
// absent from the file rather than an error, because a missing lsblk must never
// cost a campaign its bundle.
func WriteMachineMetadata(dir string, in MachineInput) error {
	var lines []string
	add := func(s string) {
		if s = strings.TrimRight(s, "\n"); s != "" {
			lines = append(lines, strings.Split(s, "\n")...)
		}
	}

	add(time.Now().UTC().Format("Mon Jan  2 15:04:05 MST 2006"))
	if in.Hardware.InstanceType != "" {
		add("instance-type: " + in.Hardware.InstanceType)
	}
	if in.Hardware.InstanceID != "" {
		add("instance-id:   " + in.Hardware.InstanceID)
	}
	add(commandOutput("uname", "-a"))
	add(commandOutput("lsb_release", "-ds"))
	add(cpuFacts())
	add(head(commandOutput("free", "-h"), 2))
	add(commandOutput("lsblk", "-o", "NAME,SIZE,MODEL"))
	add("repo: " + in.Repo)
	add(fmt.Sprintf("ref: %s (%s)", in.Ref, in.BuiltCommit))
	add(fmt.Sprintf("binary: %s (commit %s)", in.BinPath, in.BuiltCommit))
	lines = append(lines, binaryVersion(in.BinPath)...)
	add(commandOutput("go", "version"))
	add(rustcVersion())

	cfg := in.Cfg
	add(fmt.Sprintf("campaign: %s · ingest: %s · query: %s · runs: %d · concurrency: %s",
		cfg.Name, cfg.Ingest, cfg.QueryString(), cfg.Runs, cfg.QueryConcurrencyString()))
	add(fmt.Sprintf("cold-iters: %d · hot-iters: %d · close-interval: %s · workers: %d · hot-num-ledgers: %d",
		cfg.ColdIters, cfg.HotIters, cfg.CloseInterval, cfg.Workers, cfg.HotNumLedgers))
	add(fsyncProbe(in.BenchRoot))

	return writeLines(filepath.Join(dir, MachineMetadataName), lines)
}

// cpuFacts is lscpu's model and CPU count on Linux, sysctl's equivalents on
// macOS, and nothing at all where neither exists.
func cpuFacts() string {
	if out := commandOutput("lscpu"); out != "" {
		var keep []string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Model name") || strings.HasPrefix(line, "CPU(s)") {
				keep = append(keep, line)
			}
		}
		if len(keep) > 0 {
			return strings.Join(keep, "\n")
		}
	}
	return commandOutput("sysctl", "-n", "machdep.cpu.brand_string", "hw.memsize", "hw.ncpu")
}

// rustcVersion tries PATH first, then the rustup default install location —
// rustc is often installed for a user whose PATH the campaign does not inherit.
func rustcVersion() string {
	if out := commandOutput("rustc", "--version"); out != "" {
		return out
	}
	if home, err := os.UserHomeDir(); err == nil {
		return commandOutput(filepath.Join(home, ".cargo", "bin", "rustc"), "--version")
	}
	return ""
}

// fsyncProbeWrites and fsyncProbeBlock size the probe: 2000 synchronous 4 KiB
// writes, the same shape as the dd probe bash ran.
const (
	fsyncProbeWrites = 2000
	fsyncProbeBlock  = 4096
)

// fsyncProbe measures synchronous write throughput on the bench root's disk —
// the single number that explains an ingest campaign that came out slow. It is
// native Go rather than dd because dd's oflag=dsync does not exist on macOS,
// and the reported line says how it was measured so no one has to guess.
func fsyncProbe(benchRoot string) string {
	if benchRoot == "" {
		return "fsync probe: unavailable (no BENCH_ROOT)"
	}
	path := filepath.Join(benchRoot, ".fsync-probe")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_SYNC, 0o644)
	if err != nil {
		return fmt.Sprintf("fsync probe: unavailable (%v)", err)
	}
	buf := make([]byte, fsyncProbeBlock)
	start := time.Now()
	for i := 0; i < fsyncProbeWrites; i++ {
		n, err := f.Write(buf)
		if err == nil && n != fsyncProbeBlock {
			err = fmt.Errorf("short write: %d of %d bytes", n, fsyncProbeBlock)
		}
		if err != nil {
			f.Close()
			os.Remove(path)
			return fmt.Sprintf("fsync probe: unavailable (%v)", err)
		}
	}
	elapsed := time.Since(start)
	closeErr := f.Close()
	os.Remove(path) // best-effort cleanup, like bash's rm -f: a leftover probe file is truncated by the next probe
	if closeErr != nil {
		return fmt.Sprintf("fsync probe: unavailable (%v)", closeErr)
	}
	mbps := float64(fsyncProbeWrites*fsyncProbeBlock) / 1e6 / elapsed.Seconds()
	return fmt.Sprintf("fsync probe: %.1f MB/s (native Go O_SYNC probe, %dKiB x %d)",
		mbps, fsyncProbeBlock/1024, fsyncProbeWrites)
}

// binaryVersion is the benchmarked binary's own version output, first 3 lines,
// stdout and stderr together — the binary reports its build stamp there. A
// binary that cannot run, or does not answer inside factTimeout, says so in
// place of its version.
func binaryVersion(binPath string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), factTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "version")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	lines := splitLines(head(strings.TrimRight(string(out), "\n"), 3))
	if err != nil && len(lines) == 0 {
		return []string{fmt.Sprintf("version: %v", err)}
	}
	return lines
}

// head is the first n lines of s.
func head(s string, n int) string {
	lines := splitLines(s)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// writeLines writes one line per element, with a trailing newline.
func writeLines(path string, lines []string) error {
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("bundle: write %s: %w", path, err)
	}
	return nil
}
