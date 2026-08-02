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
// Step ordering and argv are ported from the bash campaign runner this package
// replaces. The flags and their order are copied exactly: the bench
// subcommands are a cross-repo contract with stellar-rpc, and a stable argv
// keeps the golden plan meaningful.
package plan
