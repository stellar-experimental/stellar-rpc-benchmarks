package hoststat

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustSnapshot(t *testing.T, proc string, at time.Time) snapshot {
	t.Helper()
	snap, err := readSnapshot(filepath.Join("testdata", proc), wholeDisks(filepath.Join("testdata", "sys")), at)
	if err != nil {
		t.Fatalf("readSnapshot(%s): %v", proc, err)
	}
	return snap
}

// TestRowsFromFixtures checks the host and per-device rows computed from two
// fixture /proc snapshots five seconds apart.
func TestRowsFromFixtures(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	got := rows(mustSnapshot(t, "proc0", t0), mustSnapshot(t, "proc1", t0.Add(5*time.Second)))
	want := [][]string{
		{"2026-10-21T12:00:05Z", "", "42.9", "14.3", "35.7", "7.1",
			"6144000000", "10240000000", "4096000000", "", "", "", "", ""},
		// The partition, the loop device, and the idle sda leave no row.
		{"2026-10-21T12:00:05Z", "nvme0n1", "", "", "", "", "", "", "",
			"10485760", "2097152", "100", "50", "50.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
		if len(got[i]) != len(Header) {
			t.Errorf("row %d has %d columns, header has %d", i, len(got[i]), len(Header))
		}
	}
}

// TestParseErrors checks that malformed /proc contents are errors, not zeros.
func TestParseErrors(t *testing.T) {
	if _, err := parseStat(strings.NewReader("intr 1 2\n")); err == nil {
		t.Error("parseStat without a cpu line succeeded")
	}
	if _, err := parseMeminfo(strings.NewReader("MemTotal: 1 kB\n")); err == nil {
		t.Error("parseMeminfo without MemAvailable succeeded")
	}
	if _, err := parseDiskstats(strings.NewReader("8 0 sda 1 2\n")); err == nil {
		t.Error("parseDiskstats of a short line succeeded")
	}
}

// fakeClock returns times five seconds apart, one per call.
func fakeClock() func() time.Time {
	var mu sync.Mutex
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(5 * time.Second)
		return at
	}
}

// TestSamplerWritesCSV checks that a Linux sampler writes the header and at
// least one host row by the time Stop returns.
func TestSamplerWritesCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.csv")
	s := start(config{
		path: path, interval: 5 * time.Millisecond,
		procRoot: filepath.Join("testdata", "proc0"), sysRoot: filepath.Join("testdata", "sys"),
		goos: "linux", now: fakeClock(),
	})
	time.Sleep(30 * time.Millisecond)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(recs) < 2 || !slices.Equal(recs[0], Header) || recs[1][1] != "" {
		t.Fatalf("host.csv = %q, want the header then a host row", recs)
	}
}

// TestSamplerStopsOnError checks that a sampler whose /proc is unreadable
// writes no file and reports the error from Stop.
func TestSamplerStopsOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.csv")
	s := start(config{
		path: path, interval: time.Millisecond,
		procRoot: filepath.Join(t.TempDir(), "missing"), sysRoot: "testdata/sys",
		goos: "linux", now: fakeClock(),
	})
	if err := s.Stop(); err == nil {
		t.Error("Stop returned nil for an unreadable /proc")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want not-exist", path, err)
	}
}

// TestSamplerNoOpOffLinux checks that a non-Linux sampler writes nothing and
// stops without error.
func TestSamplerNoOpOffLinux(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.csv")
	s := start(config{path: path, interval: time.Millisecond, goos: "darwin", now: time.Now})
	if err := s.Stop(); err != nil {
		t.Errorf("Stop = %v, want nil", err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("second Stop = %v, want nil", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want not-exist", path, err)
	}
}
