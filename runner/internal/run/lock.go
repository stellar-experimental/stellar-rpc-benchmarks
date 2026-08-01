package run

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockName is the lock file at the root of a BENCH_ROOT. It is created once
// and never deleted: unlinking a lock file races with the next campaign, which
// may already hold the old inode open.
const lockName = ".campaign.lock"

// AcquireLock takes an exclusive, non-blocking flock on <benchRoot>/.campaign.lock
// and returns the function that releases it. Two campaigns sharing a BENCH_ROOT
// would fight over the same build clone, scratch dirs, and hot DBs, so the
// second one is refused immediately rather than queued: the operator wants to
// know now, not in six hours.
func AcquireLock(benchRoot string) (release func(), err error) {
	if err := os.MkdirAll(benchRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create BENCH_ROOT %s: %w", benchRoot, err)
	}
	path := filepath.Join(benchRoot, lockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another campaign is already running on this BENCH_ROOT (held lock: %s)", path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// The lock lives on the open file description, so the fd stays open until
	// release; closing it anywhere earlier would drop the lock silently.
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
