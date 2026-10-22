// Package memlimit resolves the campaign's gomemlimit setting into the
// GOMEMLIMIT of the bench processes.
package memlimit

import (
	"fmt"
	"os"
	"strings"
)

// Auto and Off are the gomemlimit settings.
const Auto, Off = "auto", "off"

// Resolved is the session's Go memory limit, as metadata.json records it.
type Resolved struct {
	Bytes         int64  `json:"bytes"`
	Source        string `json:"source"`
	MemTotalBytes int64  `json:"mem_total_bytes,omitempty"`
}

// Resolve turns a setting into the session's limit and the log line that
// names it, reading MemTotal from meminfo for auto.
func Resolve(setting, meminfo string) (Resolved, string) {
	off := Resolved{Source: Off}
	if setting == Off {
		return off, "Go memory limit: off"
	}
	total, err := memTotal(meminfo)
	if err != nil {
		return off, fmt.Sprintf("Go memory limit: off — gomemlimit = auto needs MemTotal: %v", err)
	}
	// The other 20% is headroom for the bench's C++ memory (RocksDB, zstd),
	// which GOMEMLIMIT does not cover.
	r := Resolved{Bytes: total * 80 / 100 >> 20 << 20, Source: Auto, MemTotalBytes: total}
	return r, fmt.Sprintf("Go memory limit: GOMEMLIMIT=%s (80%% of MemTotal) on bench processes", r.Env()["GOMEMLIMIT"])
}

// Env is the environment the runner adds to a bench process.
func (r Resolved) Env() map[string]string {
	if r.Bytes == 0 {
		return nil
	}
	return map[string]string{"GOMEMLIMIT": fmt.Sprintf("%dMiB", r.Bytes>>20)}
}

func memTotal(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	i := strings.Index(string(b), "MemTotal:")
	if i < 0 {
		return 0, fmt.Errorf("no MemTotal in %s", path)
	}
	var kb int64
	_, err = fmt.Sscanf(string(b[i:]), "MemTotal: %d kB", &kb)
	return kb << 10, err
}
