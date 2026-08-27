// Package plan turns a validated config into an ordered list of steps with
// explicit dependencies — a pure function, and the campaign as data.
//
// Build is the whole package: config in, [Plan] out, with everything
// environment-dependent (bench root, resolved commit, timestamp) entering
// through [Inputs]. It reads no clock, touches no filesystem, and executes
// nothing, so the same config and inputs always yield the same plan — which is
// what makes the plan committable as a golden fixture and lets dry-run, resume,
// and progress reporting be plain consumers of it.
//
// Step ordering and argv follow the bench subcommands' own flags, which are a
// cross-repo contract with stellar-rpc; a stable argv is what keeps the golden
// plan meaningful. The query suites emit one leg per endpoint type, because
// each type is paced at its own arrival rate and one bench-query invocation
// drives one rate — the rates themselves arrive through [Inputs], already
// resolved from docs/targets.json.
package plan
