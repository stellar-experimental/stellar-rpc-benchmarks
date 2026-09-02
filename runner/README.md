# Campaign runner

The operations side of this repo: a config-driven runner that produces the result
bundles the rest of the pipeline consumes. It treats **stellar-rpc as a black box** — it
clones it, builds the requested ref, and drives its `bench-ingest` / `bench-query`
subcommands; it never lives inside a stellar-rpc checkout and never modifies one.

```
runner/
├── bootstrap.sh            # provision the devbox (NVMe, apt, Go, Rust, native libs, env)
├── cmd/campaign/           # the campaign CLI: run · plan · preflight · publish
├── internal/
│   ├── config/             # TOML in, validated Config out (unknown keys rejected)
│   ├── targets/            # docs/targets.json → the query legs' RPS floors
│   ├── plan/               # config → ordered steps; the campaign as data (plan.json)
│   ├── run/                # executes a plan: legs, sentinels, resume, the lock
│   ├── preflight/          # does this machine have what this config needs?
│   ├── publish/            # bundle → gs:// or s3://
│   └── bundle/             # the bundle root: metadata.json, provenance files,
│                           #   and the checks a --resume must pass
└── example-campaign.toml   # annotated config to copy from
```

Everything is invoked from `runner/`:

```bash
cd runner
BENCH_ROOT=/mnt/nvme/bench go run ./cmd/campaign run my-campaign.toml
```

`go run` is the normal way in — the devbox has Go (bootstrap.sh installs a pinned
version), and a campaign spends hours in child processes, so the second it takes to
compile the runner is noise. `make runner-build` from the repo root compiles it instead;
`make runner-test` is the vet-and-test gate CI runs.

`plan` and `run --dry-run` build nothing, download nothing, and execute nothing, so both
are readable on a laptop:

```bash
cd runner
BENCH_ROOT=/tmp/bench go run ./cmd/campaign plan example-campaign.toml
```

The [top-level README](../README.md#run-a-campaign) walks through the operator flow end
to end, from a fresh devbox to a published bundle.

## CLI reference

### `campaign run <config.toml> [flags]`

Build the configured ref, prepare its datasets, and execute every ingest and query leg
into a results bundle. One campaign per `$BENCH_ROOT`: the runner holds an exclusive
lock on `$BENCH_ROOT/.campaign.lock` and refuses a second campaign immediately rather
than queueing it — two campaigns sharing a machine measure each other.

| Flag | Effect |
|------|--------|
| `--dry-run` | Print the plan and exit. Nothing is built, fetched, locked, or created. |
| `--resume DIR` | Continue an interrupted campaign into an existing bundle (see [Resuming](#resuming-a-crashed-campaign)). |
| `--fail-fast` | Stop at the first failed step. The default is to keep going: a failure only skips the steps that need it. |
| `--no-preflight` | Skip the up-front tool, credential, mount, and disk checks. |
| `--targets PATH` | Read the query RPS floors from this `targets.json` instead of the checkout's own (see [Query load](#query-load-open-loop-paced-rps)). |

A failed step never costs the bundle: the epilogue — machine metadata, the final
`metadata.json`, the tarball — runs even after failures, because a campaign that went
wrong is exactly the one whose bundle has to be complete. The run exits nonzero and ends
with a summary naming every failed and skipped step.

### `campaign plan <config.toml> [--targets <path>]`

Print the ordered steps the campaign would execute, one `$ command` line each. It
resolves no ref against the network and writes nothing; when the ref is not resolvable in
the local build clone it plans with the placeholder sha `deadbeef` in the derived paths
and says so. This is the same data the runner writes to `plan.json` in the bundle. A
query campaign's RPS ladders are resolved for real, so the printed `--target-rps` lists
are the ones a run would drive.

### `campaign preflight <config.toml> [--targets <path>]`

Answer, in seconds, whether this machine has what this config needs: `git` and `make`,
the Go and Rust toolchains when a build is going to happen, `gcloud` for `packs-gs`
datasets and `gs://` publishing, `aws` for `packs-s3` datasets and `s3://` publishing, a
listable `publish_uri` with today's credentials, the NVMe mount when `BENCH_ROOT` is left
at its default, and free disk. Failures name both the missing thing and the config key that wants it.
On a query campaign it also resolves the whole load model — the phase and every dataset's
profile — so an unpaceable config fails here rather than at the first query leg.
`campaign run` does this automatically unless `--no-preflight` is passed.

### `campaign publish <results-dir> [dest-root] [flags]`

Upload a finished bundle to `<dest-root>/<run_id>/`, where `run_id` is the bundle's
basename. `dest-root` defaults to `$PUBLISH_URI` from the environment — the same value a
config's `publish_uri` feeds the end of a run. `gs://` uploads with `gcloud storage rsync
-r`, `s3://` with `aws s3 sync`; no other scheme is supported. On success the last line
is `published: <dest>`, which scripts grep for.

| Flag | Effect |
|------|--------|
| `--dry-run` | Print every cloud command, execute none of them. |
| `--force` | Write into a destination that already holds objects. |

Published runs are immutable, which is why a non-empty destination is refused without
`--force`. **`--force` is a merge, not a replace:** the sync writes every object this
bundle has, but objects already at the destination that this bundle does not contain
survive it. Re-publishing a differently-shaped bundle over an old one therefore leaves
the old one's extra files in place and the destination is the union of the two — delete
the prefix first if you need a clean replace.

## Config reference (TOML)

A campaign config is a TOML file. Copy [`example-campaign.toml`](example-campaign.toml),
which carries the same reference as inline comments. **Unknown keys are rejected**, not
ignored: a misspelled key fails in the first second of the campaign, naming itself.

| Key | Type | Default | Meaning |
|-----|------|---------|---------|
| `name` | string | — (required) | Campaign name, `[A-Za-z0-9._-]+`. The run id is `<name>-<sha>-<stamp>`. |
| `repo` | string | `https://github.com/stellar/stellar-rpc.git` | Where stellar-rpc comes from: a git URL or an **absolute** local path to a git repository (relative paths are refused — they would depend on the invocation cwd). The persistent build clone at `$BENCH_ROOT/src` is cloned/fetched from it each campaign; `repo` itself is never modified. To benchmark local work-in-progress, point it at a local checkout — only committed state is benchmarkable. |
| `ref` | string | `feature/full-history` | Git ref to benchmark, resolved inside `$BENCH_ROOT/src` after fetching `repo`'s branches and tags. Built into `$BENCH_ROOT/bin/stellar-rpc-<sha>`. |
| `ingest` | string | — (required) | `cold` \| `hot` \| `both` \| `none`. |
| `query` | bool | — (required) | Run the query suites after ingest. Both suites read a database this campaign builds — query-cold the cold store the cell's cold ingest leaves behind, query-hot the hot DB the hot ingest leaves behind — so `query = true` is refused unless `ingest` is `cold` or `both`; with `cold` the runner notes that it is running the cold suite only. With `runs > 1` the query legs read the store the **last** rep built. Each suite emits **one leg per endpoint type** — see [Query load](#query-load-open-loop-paced-rps) and [Which database the query legs read](#which-database-the-query-legs-read). |
| `close_interval` | string | `"0"` | `bench-ingest hot --close-interval`: a Go duration (`"2s"`, `"1s"`, `"600ms"`) for phase pacing, or `"0"` for unpaced catch-up. |
| `runs` | int ≥ 1 | `5` | Repetitions per (dataset, chunk) cell. |
| `query_duration` | string | `"60s"` | `bench-query --duration`: how long each query leg holds its paced load. A Go duration greater than zero. |
| `phase` | int | `0` (unset) | Which goal phase's RPS floors the query legs target: `1` \| `2` \| `3`. Consulted only when `query = true`. Left unset, the phase is derived by matching `close_interval` against a phase block time in [`docs/targets.json`](../docs/targets.json); set both and they must agree. |
| `workers` | int ≥ 1 | `1` | `bench-ingest cold --workers`. |
| `hot_num_ledgers` | int ≥ 0 | `0` | Cap the hot ingest at this many ledgers; `0` = the whole range. A capped ingest also caps the hot query sampler (`--sample-ledgers`), so it stays inside what was ingested. |
| `publish_uri` | string | `""` | Object-storage root to publish the finished bundle to; must be `gs://` or `s3://`. Empty = no publish. The bundle lands at `<publish_uri>/<run_id>/`. |
| `[[dataset]]` | table array | — (at least one) | See below. |

Each `[[dataset]]` table:

| Key | Type | Meaning |
|-----|------|---------|
| `name` | string | `[A-Za-z0-9._-]+`, unique across the config. Names the golden directory, the leg ids, and the converter's unit ids. On a query campaign it also picks the load profile — see [Query load](#query-load-open-loop-paced-rps). |
| `kind` | string | `packs-local` \| `packs-gs` \| `packs-s3` \| `bsb-s3` \| `fixture`. |
| `location` | string | Meaning depends on the kind (below). Invalid for `fixture`. |
| `chunks` | int array | Chunk IDs to benchmark; at least one, non-negative, no duplicates. |
| `ledgers` | int | **`fixture` only** (and required there): the per-chunk ledger count. `0` = the whole chunk; otherwise ≥ 10000, because the untimed cold freeze streams a whole 10,000-ledger chunk and cannot freeze a partial one. |

Every kind converges on a local cold pack root — the directory holding `ledgers/`,
`events/`, `txhash/` — of which the timed legs read `<root>/ledgers/` and nothing else:
both ingest tiers stream those raw packs, and the `events/`/`txhash/` indexes banked
beside them are never read. Those indexes were written by whatever binary prepared the
dataset, and the cold-index on-disk format is allowed to change under them, so the cold
query legs read the store this campaign's own cold ingest builds instead — see [Which
database the query legs read](#which-database-the-query-legs-read). The kinds differ only
in how that root is materialized:

- **`packs-local`** — `location` **is** the cold pack root, used in place and never
  written to.
- **`packs-gs`** — `location` is a `gs://` prefix of the same tree; fetched once into
  `$BENCH_ROOT/golden/<name>/` and reused by later campaigns.
- **`packs-s3`** — the same fetch from an `s3://` prefix, with `aws s3 sync`. On EC2 the
  CLI signs with the box's instance role, which is what reads the private bucket the
  synthetic-ledger packs live in.
- **`bsb-s3`** — `location` is an S3 bucket path; an untimed cold backfill materializes
  `$BENCH_ROOT/golden/<name>/`, one chunk at a time.
- **`fixture`** — no location and no network: each chunk in `chunks` is generated with
  `ledgers` ledgers, then an untimed cold ingest freezes them into
  `$BENCH_ROOT/golden/<name>/`.

Anything the runner materializes is built under `<root>.partial` and renamed onto
`<root>` only once whole, so an interrupted preparation is never mistaken for a finished
one. To force a re-fetch or a regeneration: `rm -rf $BENCH_ROOT/golden/<name>`.

## Migrating a `.cfg` to `.toml`

Bash-era configs were sourced shell fragments; the CLI reads TOML. The keys map
one-to-one — lowercase the name, and give the value TOML's type instead of a string:

| `.cfg` | `.toml` | Note |
|--------|---------|------|
| `NAME=phase4` | `name = "phase4"` | |
| `REPO=…` | `repo = "…"` | |
| `REF=feature/full-history` | `ref = "feature/full-history"` | |
| `INGEST=both` | `ingest = "both"` | |
| `QUERY=yes` / `QUERY=no` | `query = true` / `query = false` | A real bool now. |
| `CLOSE_INTERVAL=2s` | `close_interval = "2s"` | Still a string; `"0"` for unpaced. |
| `RUNS=5` | `runs = 5` | |
| `QC=1,4,16` | — | Gone: the query suites are open-loop now. See [Query load](#query-load-open-loop-paced-rps). |
| `COLD_ITERS=100` | — | Gone with `QC`: a leg runs for `query_duration`, not for a count. |
| `HOT_ITERS=200` | — | Same. |
| `WORKERS=1` | `workers = 1` | |
| `HOT_NUM_LEDGERS=0` | `hot_num_ledgers = 0` | |
| `PUBLISH_URI=gs://…` | `publish_uri = "gs://…"` | |
| `DATASETS=("name\|kind\|location\|chunks")` | one `[[dataset]]` table each | See below. |

A dataset's four pipe-separated fields become four keys, and the chunk list becomes an
array:

```bash
# .cfg
DATASETS=(
  "mydata|packs-gs|gs://my-bucket/cold|1 2"
  "synth|fixture|10000|1"
)
```

```toml
# .toml
[[dataset]]
name = "mydata"
kind = "packs-gs"
location = "gs://my-bucket/cold"
chunks = [1, 2]

[[dataset]]
name = "synth"
kind = "fixture"
ledgers = 10000   # not location: the .cfg overloaded that field with the ledger count
chunks = [1]
```

The fixture dataset is the one entry whose shape really changes. Its `.cfg` location was
never a location — it was the per-chunk ledger count — so it moves to its own `ledgers`
key, and a `location` on a fixture dataset is now an error rather than a silently
reinterpreted number.

Two configs bash accepted are now rejected: a repeated chunk ID within a dataset (it
produced two legs with the same id and the same `--out` directory, the second quietly
overwriting the first) and any key outside the table above.

## Query load: open-loop, paced RPS

The query suites measure latency **at a rate**, not at a concurrency level: each leg
drives an open-loop arrival rate for `query_duration` and reports what the RPC node did
under it. The rates are not a knob — they come from
[`docs/targets.json`](../docs/targets.json), this repo's single source of truth for the
goal, and no floor is hardcoded in the runner.

### Two families of floor

The campaign answers two separate requirements, and the load model keeps them apart. The full derivation of both is in [`docs/sla-derivation.md`](../docs/sla-derivation.md).

- **The SLA family** (`query_load.sla`) asks every endpoint to hold its latency target while it serves its share of the sustained request-rate watermark. Its floor is a property of the endpoint alone — `txhash` 300, `events` 100, `txpage` 75, `ledgers` 25 rps — so it is the same in every phase and every dataset profile.
- **The end-to-end-budget probe** (`query_load.e2e_probe`) asks one endpoint, `txhash`, to answer inside the 10 ms slice it owns in the transaction-lifecycle budget, at the rate the demand model of work item 856 predicts. Those floors ARE per profile and per phase: sac reads 300 / 500 / 1000, custom_token 200 / 400 / 600, soroswap 75 / 150 / 300.

Three things decide a leg's rate ladder:

- **Endpoint type.** `ledgers`, `txpage`, `txhash`, `events` each have their own floor, so
  a single invocation can no longer sweep all four. Every (dataset, chunk, rep) cell
  therefore produces **four legs per tier**, in that order:
  `query-{cold,hot}-<dataset>-c<chunk>-<type>-run<R>`.
- **Dataset profile.** The dataset name minus a trailing `-<per-ledger tx count>` is the
  `query_load.e2e_probe.floors_rps` key: `sac-6000` → `sac`, `soroswap-1500` → `soroswap`. A
  dataset whose name has no profile is refused before the campaign starts, naming both.
- **Phase.** `phase = 1|2|3`, or derived from a `close_interval` that is a phase's block
  time (`"2s"` → phase 1, `"1s"` → 2, `"600ms"` → 3). When both are set they must agree —
  pacing ledgers at one phase while measuring another phase's read targets is not a phase
  run. A query campaign with neither is refused.

Each leg then sweeps `query_load.ladder` (`0.5×`, `1×`, `2×`) around its floor and passes
the result as one `--target-rps` list, so a `getEvents` leg reads:

```
$ …/stellar-rpc-<sha> bench-query cold --cold-dir=… --start-chunk=1 --num-chunks=1 \
    --types=events --target-rps=50,100,200 --duration=60s --out=…
```

For the SLA family the ladder IS the SLA's load tiers: `0.5×` is Light (250 rps aggregate),
`1×` is Standard (500 rps, the sustained watermark), and `2×` is Heavy (1000 rps). The
verdict is read at `1×`; the other two rungs are the context around it.

**`txhash` carries both families in one leg.** `bench-query --target-rps` already takes a
list and emits one cell per rate, so no second leg is needed: the ladder is the union of
the two, deduplicated and sorted ascending. For sac at phase 3 the SLA ladder is
`150,300,600` and the demand ladder is `500,1000,2000`, so the leg reads:

```
$ …/stellar-rpc-<sha> bench-query cold --cold-dir=… --start-chunk=1 --num-chunks=1 \
    --types=txhash --target-rps=150,300,500,600,1000,2000 --duration=60s --out=…
```

The converter then judges `r300` against the SLA p99 and `r1000` against the 10 ms in-RPC
budget. Where the two floors coincide — sac at phase 1, whose demand floor is also 300 rps
— the ladder collapses to `150,300,600` and the one `r300` cell carries both verdicts.

`--targets <path>` on `run`, `plan`, and `preflight` overrides which targets file is read;
by default the runner walks up from the working directory to the `docs/targets.json` of
the checkout it is running from. `preflight` resolves the whole load model, so a bad
profile or an unresolvable phase fails in seconds rather than at the first query leg.

### Which database the query legs read

Both tiers read a database this campaign built: `query-hot` opens the hot DB the cell's
hot ingest left at `$BENCH_ROOT/hot/<dataset>/<chunk>`, and `query-cold` opens the cold
store its cold ingest built at `$BENCH_ROOT/scratch/<dataset>/<chunk>` — never the
`events/`/`txhash/` tree banked inside the golden dataset. With `runs > 1` both read what
the **last** rep built: every rep wipes and rebuilds the same chunk, so only the last
one's success guarantees a whole store. The disk cost is that a query campaign carries
every cell's cold store, on top of its hot DBs, until that cell's last cold query leg
succeeds and frees it. What it buys is hermeticity — the binary answering the queries is
the binary that wrote the store — and that is not theoretical: the golden tree's indexes
were frozen by whatever binary prepared the dataset, and when stellar-rpc#932 changed the
cold-index on-disk format, the first query campaigns' cold txhash legs all died at open
(`cold index user metadata malformed`). Only `<dataset>/ledgers/` is still read from the
golden tree, by both ingest tiers, because pack files are format-stable XDR.

## Compatibility floor

The runner requires a stellar-rpc ref whose bench subcommands:

- **write `invocation.json` into every `--out` directory** — stellar-rpc#907 (`6f35679f`)
  or any descendant of it. Older refs produce bundles without per-invocation manifests,
  which the converter accepts but with weaker provenance (see `SCHEMA.md` § Inputs).
- **accept `bench-query --target-rps` and `--duration`** — the open-loop query model. The
  runner emits neither `--query-concurrency` nor `--iters` any more, so a ref that only
  knows the closed-loop flags fails every query leg immediately.

Both are on `feature/full-history`, so the default `ref = "feature/full-history"`
satisfies the floor.

## `$BENCH_ROOT` layout

Everything the runner touches lives under `$BENCH_ROOT` (default `/mnt/nvme/bench`, the
devbox's NVMe instance store — wiped on instance stop/start; everything here is
re-creatable):

```
$BENCH_ROOT/
├── .campaign.lock  the one-campaign-per-root flock; created once, never deleted
├── src/       persistent build clone of repo (re-pointed, fetched, and hard-reset
│              every campaign; gitignored build caches survive, so rebuilds are
│              incremental)
├── bin/       versioned binaries: stellar-rpc-<sha>
├── golden/    immutable prepared datasets, one dir per dataset name
│              (rm -rf golden/<name> to force a re-fetch)
├── fixture/   staging area for generated fixture packs
├── scratch/   cold stores, one per (dataset, chunk); wiped before every
│              cold-ingest rep. On a query campaign a store is kept until the
│              cell's last cold query leg succeeds; otherwise it goes as soon as
│              the ingest rep succeeds
├── hot/       hot DBs; the last run's DB is kept for the hot query suite
└── results/   campaign bundles: <name>-<sha>-<stamp>/
```

The finished bundle is also tarred to `/tmp/bench-results-<name>-<sha>-<stamp>.tgz` (EBS
root, survives an instance stop) and, when `publish_uri` is set, uploaded to
`<publish_uri>/<name>-<sha>-<stamp>/`.

## Resuming a crashed campaign

A campaign is hours of work — the phase-1 reference run took ~17 hours, with single
hot-ingest legs near 5.5 hours — so a crash or an OOM kill at the last rep should not cost the
whole thing. `--resume` continues into the existing results directory instead of starting
a new one:

```bash
cd runner
BENCH_ROOT=/mnt/nvme/bench go run ./cmd/campaign run my-campaign.toml \
  --resume /mnt/nvme/bench/results/<name>-<sha>-<stamp>
```

**Identity comes from the bundle, not its name.** The runner reads the bundle's
`metadata.json` and refuses unless its `run_id`, campaign `name`, and `built_commit`
match this config and the commit `ref` resolves to right now — resuming onto a different
commit would mix two binaries inside one bundle. A renamed or copied directory therefore
cannot pass itself off as another campaign, and a bundle with no readable
`metadata.json` (pre-crash-safe, or not a bundle at all) is refused outright.

**The config must be byte-identical.** The bundle stores the config it was started with;
a resume compares the file you passed against that stored copy and, on any difference,
prints a unified diff and stops. The stored copy is never overwritten — it is the record
of what produced the legs already in the bundle, and a resume with edited knobs would
otherwise produce mixed data under a manifest uniformly claiming the new ones.

**Completion is decided by the runner's own sentinel.** Every timed leg gets a `leg.json`
written into its `--out` directory when the process ends, success or failure; a leg is
skipped only when its sentinel identifies itself — this schema version and *this leg's*
id — and records a zero exit code with no error. Anything else — a recorded failure, a
corrupt sentinel, a sentinel naming another leg, a leg killed before the sentinel was
written — is wiped and re-run, because a half-written leg silently kept would corrupt the
aggregates the converter computes. Bundles produced by the bash runner have no `leg.json`, so those
fall back to the old heuristic: both `invocation.json` and `driver.csv` present, with no
`error` field in the manifest.

Add `--dry-run` to print the plan against the real directory, annotated with the legs a
resume would skip, before committing hours to it. That combination is the one dry run
that needs the build clone: a resume is only valid against the commit its bundle was
benchmarked with, so it is refused rather than planned with a placeholder sha — run it on
the box, not on a laptop. The run id is reused, so the bundle
keeps its identity: `metadata.json` still carries the original `started_at` (recovered
from the bundle), `finished_at` is the last session's end, and `campaign.resumed` records
that the bundle took more than one session. `campaign.log` accumulates every session's
console output, so the whole history stays in the bundle.

A resumed session keeps going past failures like any other, so one broken leg does not
end the session — it fails, the steps that need it are skipped, and the rest of the
campaign runs. `--fail-fast` stops at the first failure instead, which is what you want
when the failure is likely to repeat.

**Same boot only.** `$BENCH_ROOT` is the NVMe instance store: stopping and starting the
instance wipes the results directory, the golden datasets, and the hot DBs together. If the
results directory survived, resume it; if the box restarted, there is nothing to resume onto
and the campaign starts over. The hot query suite reads the DB the last hot ingest left
behind and the cold query suite the store the last cold ingest built, so in the unlikely
case that one of those is gone but the results directory is not, the query legs that read
it fail (and the summary names them) — re-run that tier's ingest for the cell by deleting
its `ingest-<tier>-<dataset>-c<chunk>-run*` directories and resuming again.

One resume hazard belongs to the cold store alone: the cell's **last** cold query leg
deletes it on success, so a resume that has to re-run an *earlier* cold query leg of that
cell — one that failed while the later ones succeeded — finds no store to open. The fix is
the same one: delete that cell's `ingest-cold-<dataset>-c<chunk>-run*` directories, so the
resume rebuilds the store before the query legs read it.

## Campaign bundle layout — the cross-repo contract

A campaign bundle is what `campaign publish` uploads and what `converter/convert.py`
consumes (as the **campaign** input layout). Two repos write into it, so its shape is a
contract:

```
<name>-<sha>-<stamp>/                        # run_id = the bundle basename
├── <config>.toml                            # the campaign config, verbatim
├── binary.txt                               # benchmarked binary identity (free text)
├── machine-metadata.txt                     # machine facts (free text)
├── campaign.log                             # the runner's console output, one session
│                                            #   appended per --resume (free text)
├── metadata.json                            # ← written by the campaign CLI (THIS repo)
├── plan.json                                # ← written by the campaign CLI (THIS repo)
├── golden-<dataset>-c<chunk>/               # untimed dataset prep — not results;
│                                            #   the converter skips these and warns
├── ingest-{cold,hot}-<dataset>-c<chunk>-run<R>/
│   ├── driver.csv, hot.csv, *.csv           # ← written by stellar-rpc bench subcommands
│   ├── invocation.json                      # ← written by stellar-rpc bench subcommands
│   └── leg.json                             # ← written by the campaign CLI (THIS repo)
└── query-{cold,hot}-<dataset>-c<chunk>-<type>-run<R>/
    └── …same shape…                         #   one dir per endpoint type: ledgers,
                                             #   txpage, txhash, events
```

Who owns what:

- **`metadata.json`** (bundle root, `schema_version` 1) — written by the campaign CLI
  here. Run identity (`run_id`, `started_at`), the campaign config knobs (incl.
  `close_interval`), the dataset list, structured `hardware`, and `hostname`. It is written
  as soon as the bundle directory exists — without `finished_at`, which only the
  end-of-campaign rewrite adds — so a campaign that is killed still leaves a parseable
  bundle. It also carries `status`: `running` up front, rewritten to `finished` or
  `failed` at the end (additive; bash-era bundles have none, and readers treat an absent
  status as unknown). Only the root-level free files (the config, `binary.txt`,
  `machine-metadata.txt`, `campaign.log`) sit outside the contract; the converter reads
  named files and per-leg subdirectories, so adding one is safe.
- **`plan.json`** (bundle root, `schema_version` 1) — written by the campaign CLI here:
  the campaign as data, the same steps `campaign plan` prints, with their ids, kinds,
  argv, dependencies, and derived paths. It is rewritten on every session of a resumed
  campaign. Additive changes (new fields, new step kinds) keep the version. The converter
  ignores it today; it is there so a bundle can say what it intended to run, not only
  what it produced.
- **`leg.json`** (each timed `--out` dir, `schema_version` 1) — written by the campaign
  CLI here, after the benchmark process ends, whether it succeeded or not:
  `schema_version`, `id`, `argv`, `exit_code`, `started_at`, `finished_at`,
  `duration_ns`, and `error` on a failure. It is the runner's completion sentinel and the
  reason resume no longer has to infer completion from the presence of
  `invocation.json` — that file is written *by the process being measured*, so a process
  killed before it got there leaves no trace at all. The converter ignores it today.
- **`invocation.json`** (each `--out` dir, `schemaVersion` 1, camelCase keys) — written by
  stellar-rpc's `bench-ingest` / `bench-query` (`invocation.go`, merged as
  stellar-rpc#907). Binary identity (`binary.{commitHash, branch, version,
  buildTimestamp}`), the resolved subcommand flags, `hostname`,
  `startedAt`/`finishedAt`, and — on a failed run only — an `error` field. It is written
  for failed runs too, so its presence alone is not a success marker.

The consumer side of this contract — exactly which fields the converter reads, and the
precedence rules between the manifests, the free-text metadata, and CLI arguments — is
documented in [`SCHEMA.md` § Inputs](../SCHEMA.md#inputs--result-bundle-layouts--manifests).
Changing any of these manifests' shape, the bundle directory naming, or the CSV columns
is a cross-repo change: update the producer (here or in stellar-rpc), the converter, and
`SCHEMA.md` together.
