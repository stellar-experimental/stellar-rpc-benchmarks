package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// legStateKind is what an existing --out directory means to a resumed campaign.
type legStateKind int

const (
	legAbsent        legStateKind = iota // no directory at all
	legPartial                           // something is there, but nothing says it finished
	legFailedEarlier                     // a previous session ran this leg and it failed
	legComplete                          // a previous session ran this leg and it succeeded
)

// legState is a classification plus, for a failure, the reason to show the
// operator — the sentinel's error, the exit status, or the invocation manifest's
// own error field.
type legState struct {
	kind   legStateKind
	reason string
}

// legSentinelName is the runner-owned completion marker. The bench subcommands
// own invocation.json; nothing but this runner writes leg.json, which is why it
// is the sentinel resume trusts first.
const legSentinelName = "leg.json"

// classifyLegDir decides what a resumed campaign should do with a leg's
// existing --out directory. Only a marker that positively records success
// counts as complete; every ambiguous state resolves to "wipe and re-run",
// because a half-written leg silently kept would corrupt the aggregates the
// converter computes over the bundle.
func classifyLegDir(dir string) legState {
	// Only positive absence is absence; everything unreadable resolves to
	// wipe-and-re-run. Lstat rather than Stat so a dangling symlink is seen as
	// something-is-there, and permission or I/O errors fall to partial too —
	// otherwise resume would call MkdirAll on a path that can never be created.
	if _, err := os.Lstat(dir); err != nil {
		if os.IsNotExist(err) {
			return legState{kind: legAbsent}
		}
		return legState{kind: legPartial}
	}

	switch sentinel, err := readLegSentinel(filepath.Join(dir, legSentinelName)); {
	case err == nil && sentinel.ExitCode == 0 && sentinel.Error == "":
		return legState{kind: legComplete}
	case err == nil && sentinel.Error != "":
		return legState{kind: legFailedEarlier, reason: sentinel.Error}
	case err == nil:
		return legState{kind: legFailedEarlier, reason: fmt.Sprintf("exit status %d", sentinel.ExitCode)}
	case !os.IsNotExist(err):
		// The sentinel is there but unreadable or corrupt: it proves nothing,
		// so the leg is treated as partial rather than trusted either way.
		return legState{kind: legPartial}
	}

	// No leg.json: this may be a bundle the bash runner produced, which had no
	// sentinel of its own and inferred completion from the manifests the bench
	// subcommand writes. A failed run also writes invocation.json — with an
	// `error` field (stellar-rpc#907) — so completion means both files present
	// AND no error recorded.
	inv, err := readInvocation(filepath.Join(dir, "invocation.json"))
	if err != nil {
		return legState{kind: legPartial}
	}
	if _, err := os.Stat(filepath.Join(dir, "driver.csv")); err != nil {
		return legState{kind: legPartial}
	}
	if inv.Error != "" {
		return legState{kind: legFailedEarlier, reason: inv.Error}
	}
	return legState{kind: legComplete}
}

// readLegSentinel reads and parses a leg.json. A missing file is reported as
// os.IsNotExist so the caller can fall back to the bash-era manifests.
func readLegSentinel(path string) (legSentinel, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return legSentinel{}, err
	}
	var s legSentinel
	if err := json.Unmarshal(b, &s); err != nil {
		return legSentinel{}, err
	}
	return s, nil
}

// invocationManifest is the sliver of stellar-rpc's invocation.json this runner
// reads: whether the run recorded a failure. The file is camelCase and owned by
// the other repo; the runner never writes it.
type invocationManifest struct {
	Error string `json:"error"`
}

func readInvocation(path string) (invocationManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return invocationManifest{}, err
	}
	var m invocationManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return invocationManifest{}, err
	}
	return m, nil
}
