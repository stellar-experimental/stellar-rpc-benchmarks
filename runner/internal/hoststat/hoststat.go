// Package hoststat samples host CPU, memory, and block-device counters from
// /proc while a benchmark leg runs, and writes them as a CSV.
package hoststat

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Interval is how often a leg's host counters are sampled.
const Interval = 5 * time.Second

// sectorBytes is the unit of /proc/diskstats sector counts, whatever the
// device's real sector size.
const sectorBytes = 512

// Header is the column row of host.csv. A row with an empty device carries the
// host's CPU and memory; a row naming a device carries that device's I/O.
var Header = []string{
	"timestamp", "device",
	"cpu_user_pct", "cpu_system_pct", "cpu_iowait_pct", "cpu_idle_pct",
	"mem_used_bytes", "mem_available_bytes", "mem_page_cache_bytes",
	"read_bytes_per_s", "write_bytes_per_s", "read_iops", "write_iops", "util_pct",
}

// Sampler writes host samples to a file from a goroutine of its own until Stop.
type Sampler struct {
	stop chan struct{}
	done chan struct{}
	err  error
}

// config is what Start fixes and the tests vary.
type config struct {
	path     string
	interval time.Duration
	procRoot string
	sysRoot  string
	goos     string
	now      func() time.Time
}

// Start samples every Interval into path until Stop. It is a no-op off Linux.
func Start(path string) *Sampler {
	return start(config{
		path: path, interval: Interval,
		procRoot: "/proc", sysRoot: "/sys", goos: runtime.GOOS, now: time.Now,
	})
}

func start(cfg config) *Sampler {
	s := &Sampler{stop: make(chan struct{}), done: make(chan struct{})}
	if cfg.goos != "linux" {
		close(s.done)
		return s
	}
	go s.run(cfg)
	return s
}

// Stop takes a last sample, closes the file, and returns the error that ended
// sampling early, if any. Sampling errors never reach the leg: the first one
// stops the sampler and is kept for Stop to return.
func (s *Sampler) Stop() error {
	select {
	case <-s.done:
	default:
		close(s.stop)
		<-s.done
	}
	return s.err
}

func (s *Sampler) run(cfg config) {
	defer close(s.done)
	disks := wholeDisks(cfg.sysRoot)
	prev, err := readSnapshot(cfg.procRoot, disks, cfg.now())
	if err != nil {
		s.err = err
		return
	}
	f, err := os.Create(cfg.path)
	if err != nil {
		s.err = err
		return
	}
	w := csv.NewWriter(f)
	defer func() {
		if err := f.Close(); err != nil && s.err == nil {
			s.err = err
		}
	}()
	if err := writeRows(w, [][]string{Header}); err != nil {
		s.err = err
		return
	}
	tick := time.NewTicker(cfg.interval)
	defer tick.Stop()
	for {
		final := false
		select {
		case <-tick.C:
		case <-s.stop:
			final = true
		}
		cur, err := readSnapshot(cfg.procRoot, disks, cfg.now())
		if err != nil {
			s.err = err
			return
		}
		// A last interval under a second says little and would be noisy.
		if !final || cur.at.Sub(prev.at) >= time.Second {
			if err := writeRows(w, rows(prev, cur)); err != nil {
				s.err = err
				return
			}
		}
		if final {
			return
		}
		prev = cur
	}
}

func writeRows(w *csv.Writer, rows [][]string) error {
	if err := w.WriteAll(rows); err != nil {
		return fmt.Errorf("write host samples: %w", err)
	}
	return nil
}

// cpuTimes are the aggregate jiffies of /proc/stat's cpu line: user, nice,
// system, idle, iowait, irq, softirq, steal.
type cpuTimes [8]uint64

// memInfo holds the /proc/meminfo fields host.csv reports, in bytes.
type memInfo struct {
	total, available, cached uint64
}

// diskCounters are the cumulative /proc/diskstats fields of one device.
type diskCounters struct {
	reads, sectorsRead, writes, sectorsWritten, ioMs uint64
}

type snapshot struct {
	at    time.Time
	cpu   cpuTimes
	mem   memInfo
	disks map[string]diskCounters
}

func readSnapshot(procRoot string, disk func(string) bool, at time.Time) (snapshot, error) {
	snap := snapshot{at: at}
	var err error
	if snap.cpu, err = parseFile(filepath.Join(procRoot, "stat"), parseStat); err != nil {
		return snap, err
	}
	if snap.mem, err = parseFile(filepath.Join(procRoot, "meminfo"), parseMeminfo); err != nil {
		return snap, err
	}
	all, err := parseFile(filepath.Join(procRoot, "diskstats"), parseDiskstats)
	if err != nil {
		return snap, err
	}
	snap.disks = map[string]diskCounters{}
	for name, c := range all {
		if disk(name) {
			snap.disks[name] = c
		}
	}
	return snap, nil
}

func parseFile[T any](path string, parse func(io.Reader) (T, error)) (T, error) {
	f, err := os.Open(path)
	if err != nil {
		var zero T
		return zero, err
	}
	defer func() { _ = f.Close() }()
	v, err := parse(f)
	if err != nil {
		return v, fmt.Errorf("parse %s: %w", path, err)
	}
	return v, nil
}

func parseStat(r io.Reader) (cpuTimes, error) {
	var t cpuTimes
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		// Kernels before 2.6.11 lack steal; the missing tail stays zero.
		if len(fields) < 5 {
			return t, errors.New("short cpu line")
		}
		for i := 0; i < len(t) && i+1 < len(fields); i++ {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return t, fmt.Errorf("cpu field %d: %w", i+1, err)
			}
			t[i] = v
		}
		return t, nil
	}
	if err := sc.Err(); err != nil {
		return t, err
	}
	return t, errors.New("no cpu line")
}

func parseMeminfo(r io.Reader) (memInfo, error) {
	var m memInfo
	var seen int
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch key {
		case "MemTotal":
			dst = &m.total
		case "MemAvailable":
			dst = &m.available
		case "Cached":
			dst = &m.cached
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return m, fmt.Errorf("%s has no value", key)
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return m, fmt.Errorf("%s: %w", key, err)
		}
		*dst = v * 1024 // meminfo reports kB
		seen++
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if seen != 3 {
		return m, errors.New("missing MemTotal, MemAvailable or Cached")
	}
	return m, nil
}

func parseDiskstats(r io.Reader) (map[string]diskCounters, error) {
	out := map[string]diskCounters{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) < 14 {
			return nil, fmt.Errorf("short diskstats line %q", sc.Text())
		}
		var v [5]uint64
		for i, col := range []int{3, 5, 7, 9, 12} {
			n, err := strconv.ParseUint(f[col], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("device %s field %d: %w", f[2], col+1, err)
			}
			v[i] = n
		}
		out[f[2]] = diskCounters{reads: v[0], sectorsRead: v[1], writes: v[2], sectorsWritten: v[3], ioMs: v[4]}
	}
	return out, sc.Err()
}

// wholeDisks returns a filter that keeps the devices listed under
// <sysRoot>/block, so partitions do not double-count their disk, and drops
// loop and ram devices. Without a readable <sysRoot>/block it keeps every
// device but loop and ram.
func wholeDisks(sysRoot string) func(string) bool {
	virtual := func(name string) bool {
		return strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram")
	}
	entries, err := os.ReadDir(filepath.Join(sysRoot, "block"))
	if err != nil || len(entries) == 0 {
		return func(name string) bool { return !virtual(name) }
	}
	keep := map[string]bool{}
	for _, e := range entries {
		keep[e.Name()] = !virtual(e.Name())
	}
	return func(name string) bool { return keep[name] }
}

// rows renders the interval from prev to cur: one host row, then one row per
// device that did I/O in it, in name order.
func rows(prev, cur snapshot) [][]string {
	ts := cur.at.UTC().Format(time.RFC3339)
	secs := cur.at.Sub(prev.at).Seconds()
	var d cpuTimes
	var total uint64
	for i := range d {
		d[i] = delta(prev.cpu[i], cur.cpu[i])
		total += d[i]
	}
	pct := func(v uint64) string {
		if total == 0 {
			return "0.0"
		}
		return strconv.FormatFloat(100*float64(v)/float64(total), 'f', 1, 64)
	}
	used := delta(cur.mem.available, cur.mem.total)
	out := [][]string{{
		ts, "",
		pct(d[0] + d[1]), pct(d[2] + d[5] + d[6]), pct(d[4]), pct(d[3]),
		u(used), u(cur.mem.available), u(cur.mem.cached),
		"", "", "", "", "",
	}}
	for _, name := range slices.Sorted(maps.Keys(cur.disks)) {
		was, ok := prev.disks[name]
		if !ok {
			continue
		}
		now := cur.disks[name]
		reads, writes := delta(was.reads, now.reads), delta(was.writes, now.writes)
		ioMs := delta(was.ioMs, now.ioMs)
		if reads == 0 && writes == 0 && ioMs == 0 {
			continue
		}
		util := min(100, 100*float64(ioMs)/(secs*1000))
		out = append(out, []string{
			ts, name,
			"", "", "", "",
			"", "", "",
			rate(delta(was.sectorsRead, now.sectorsRead)*sectorBytes, secs),
			rate(delta(was.sectorsWritten, now.sectorsWritten)*sectorBytes, secs),
			rate(reads, secs), rate(writes, secs),
			strconv.FormatFloat(util, 'f', 1, 64),
		})
	}
	return out
}

// delta is b-a, or 0 when a counter went backwards (a wrap or a reset).
func delta(a, b uint64) uint64 {
	if b < a {
		return 0
	}
	return b - a
}

func rate(n uint64, secs float64) string {
	return strconv.FormatFloat(float64(n)/secs, 'f', 0, 64)
}

func u(v uint64) string { return strconv.FormatUint(v, 10) }
