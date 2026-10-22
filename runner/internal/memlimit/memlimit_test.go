package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolve checks each setting against a c6id.8xlarge meminfo and a box
// without one.
func TestResolve(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal:       64801092 kB\nMemFree:  1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "meminfo")
	for _, c := range []struct {
		setting, meminfo, env string
		want                  Resolved
	}{
		{Auto, meminfo, "50625MiB", Resolved{Bytes: 50625 << 20, Source: Auto, MemTotalBytes: 64801092 << 10}},
		{Auto, missing, "", Resolved{Source: Off}},
		{Off, meminfo, "", Resolved{Source: Off}},
	} {
		got, line := Resolve(c.setting, c.meminfo)
		if got != c.want || got.Env()["GOMEMLIMIT"] != c.env || line == "" {
			t.Errorf("Resolve(%s, %s) = %+v, %v, %q; want %+v, GOMEMLIMIT=%q", c.setting, c.meminfo, got, got.Env(), line, c.want, c.env)
		}
	}
}
