# Run JSON schema v1

One JSON file per benchmark run at `docs/runs/<run_id>.json`, listed in the manifest
`docs/runs/index.json`. The viewer renders any file conforming to this schema.

## Statistical conventions (non-negotiable)

- Every reported value = **median across the 5 runs**; spread = min–max; the raw 5-run
  array is kept alongside (`r`).
- Percentiles are per-run values from the CSVs, aggregated the same way — **never average
  percentiles across runs**.
- Nothing interpolated or smoothed; every rendered number must trace to a raw CSV field.
- Derived rates (throughputs) are computed **per run** from raw fields, then aggregated
  (median/min/max) — never derived from already-aggregated medians.
- All durations are **nanoseconds** (ints from the CSVs; derived rates may be floats).

## Value shapes

```
V        = { "m": <median>, "lo": <min>, "hi": <max>, "r": [<run1>..<run5>] }
StageAgg = { "total": V, "p50": V, "p90": V, "p99": V, "max": V, "n": int, "n_items": int }
```

`StageAgg` maps the CSV columns `total_ns,p50_ns,p90_ns,p99_ns,max_ns` → `total,p50,p90,p99,max`.
`n` / `n_items` are taken from run 1 and must be constant across runs (warn if not; for
query `events` rows, `n_items` may vary — keep the per-run array as `items_r`, see below).

## Top level

```jsonc
{
  "schema_version": 1,
  "run_id": "pubnet-2026-07-13",          // slug; file is runs/<run_id>.json
  "run_name": "Pubnet — 4 sampled chunks (user-dev-063a)",
  "run_date": "2026-07-13",
  "machine": {                             // parsed from machine-metadata.txt
    "raw": "<file verbatim>",              // required; parsed fields below best-effort
    "instance": "m6id.2xlarge", "instance_id": "i-…", "cpu": "…", "vcpus": 8,
    "mem": "30Gi", "os": "…", "kernel": "…", "fsync_probe": "…", "captured_at": "…"
  },
  "build": { "commit": "<sha>", "branch": "<name>", "go": "…", "rust": "…",
             "version": "v20.3.1-412-g…", "build_timestamp": "…" },
             // commit/branch/go/rust from the machine-metadata `repo:` line; when a
             // campaign bundle carries invocation.json, its binary.{commit_hash,
             // branch,version,build_timestamp} override commit/branch and add
             // version/build_timestamp (the structured binary identity wins).
  "hardware": {                            // optional; verbatim from metadata.json (campaign bundles)
    "instance_type": "m6id.2xlarge", "instance_id": "i-…",  // instance_* omitted off EC2
    "uname": "Linux 6.8.0-1015-aws x86_64", "cpus": 8, "mem_total_kb": 32000000  // mem_total_kb omitted on non-Linux
  },
  "hostname": "user-dev-063a",             // optional; metadata.json (fallback: invocation.json)
  "dataset": {
    "kind": "pubnet" | "synthetic",
    "description": "<human sentence>",
    "units": "chunk" | "profile",
    "unit_label": "Chunk" | "Profile",
    "unit_order": ["3000","5000","6100","6345"],   // display order
    "unit_meta": { "<unit>": {
      "ledgers": int, "txs": int, "events": int,   // from cold driver run1 n_items
      // pubnet extras:    "seq_start": int, "seq_end": int   (chunk*10000+2 … +9999)
      // synthetic extras: "model": "SAC transfer", "tps": 10000, "tx_per_ledger": 6000,
      //                   "pack": "19.5 GiB", "source_chunks": "1"   (from --unit-facts sidecar)
    } }
  },
  "campaign": {
    "reps": 5,
    "vocabulary": "old" | "new",           // auto-detected: chunk_wall ⇒ old, backfill_wall ⇒ new
    "source_gcs": "gs://…",                // optional
    "notes": "…",                          // optional
    "close_interval_ns": 2000000000,       // optional; ledger close schedule in ns, 0 = unpaced.
                                           //   From metadata.json campaign.close_interval (a Go
                                           //   duration, e.g. "2s"/"600ms"/"0"), else the hot
                                           //   invocation.json --close-interval flag. Absent when
                                           //   no manifest records it (legacy bundles).
    "phase": 1,                            // optional; campaign layout only. The phase whose
                                           //   block time equals close_interval_ns exactly
                                           //   (2 s → 1, 1 s → 2, 600 ms → 3). Absent when the
                                           //   pace matches no phase or the run is unpaced.
    "phase_targets": [ { … }, … ],         // campaign layout only; the full three-phase target
                                           //   table, copied verbatim at convert time — see
                                           //   "Phase 1/2/3 performance targets" below.
    "name": "phase1-synthetic-minspec",    // optional; metadata.json campaign.name
    "config_file": "…​.cfg",               // optional; metadata.json campaign.config_file
    "config": { … }                        // optional; remaining metadata.json campaign knobs
                                           //   (ingest/query/runs/query_concurrency/cold_iters/
                                           //   hot_iters/workers/hot_num_ledgers/ref/built_commit)
  },
  "checks":                                 // this run's pass/fail semantics AS DATA
    { "kind": "query_p99_threshold", "threshold_ns": 500000000,
      "label": "query p99 ≤ 500 ms", "applies_to": "queries" }
  | { "kind": "block_keepup", "interval_ns": 600000000,
      "label": "600 ms block model", "applies_to": "ingest_hot" },
      // block_keepup interval_ns: the legacy synthetic layout keeps the constant
      // 600 ms model. The campaign layout derives it from close_interval_ns —
      // label "Phase 1 block model (2 s)" on an exact phase match, "1.5 s pace"
      // otherwise. An unpaced campaign run emits no block_keepup check; with
      // queries present it falls back to query_p99_threshold, else no checks.
  "sections": ["ingest_cold", "ingest_hot", "queries", "golden"],  // exactly the keys present
  "ingest_cold":  { … }, "ingest_hot": { … },
  "queries": { … },                        // pubnet only
  "golden":  { … }                         // pubnet only
}
```

A viewer encountering an unknown section name or unknown stage row must degrade to a
generic table view — never crash.

## Phase 1/2/3 performance targets (campaign layout)

The performance program defines three load phases for hot ingestion. The numbers are
public (stellar/stellar-rpc issues #872–#874) and live in **`docs/targets.json`** — the
single source of truth shared by the converter, the reports viewer (`docs/app.js`), and
the latency model (`docs/latency-model.html`). Edit a target once, there: the viewer and
model fetch `targets.json` live, and the converter reads it to fill `PHASE_TARGETS`.
`targets.json` also holds the two fixed E2E estimates (`fixed_estimates.tx_submit_p99_ns`,
`fixed_estimates.client_read_p99_ns`).

The converter matches a campaign run's phase from `campaign.close_interval_ns`:
`2000000000` → phase 1, `1000000000` → phase 2, `600000000` → phase 3. The match must be
exact. Any other paced value gives a pace-only run with no phase. An unpaced run (0 or
absent) gets no phase and no keep-up check.

The converter copies the full three-phase table into `campaign.phase_targets` of every
campaign-layout run. That baked copy pins each run to the targets in force when it was
converted and lets the viewer degrade gracefully offline; the live viewer/model prefer
`targets.json` and use the baked copy only as a fallback. Each entry has:

```jsonc
{ "phase": 1,
  "block_time_ns": 2000000000,        // the phase block time; also the keep-up budget
  "e2e_budget_ns": 5000000000,        // end-to-end budget (externalized → client);
                                      //   context only — this benchmark does not measure it
  "ingest_p99_target_ns": 900000000,  // ingest-slice target (meta available in captive
                                      //   core → ingested in RPC) — the row this benchmark
                                      //   measures as per-ledger ingest_total p99. When a phase
                                      //   omits it (phase 2), the converter derives it from the
                                      //   e2e budget: e2e = block_time*2 + 60ms + ingest + 40ms.
  "workloads": [                      // the three model workloads at this phase
    { "name": "SAC transfers", "tps": 3000, "tx_per_ledger": 6000 }, … ],
  "orgs": 10,
  "retention": "3 months" }
```

Phases apply to hot ingestion only. Cold ingestion (backfill) has no phase and no
targets. Legacy layouts (pubnet, synthetic) never carry `phase` or `phase_targets` —
their output is byte-identical to before this field existed.

## Sections

All stage names are **kept exactly as they appear in the CSVs** (no renaming across
vocabularies). The `campaign.vocabulary` + `dataset.kind` tell the viewer which named
renderers apply; anything unrecognized renders generically.

```jsonc
"ingest_cold": { "<unit>": {
  "driver": { "<stage>": StageAgg },      // every driver.csv row EXCEPT peak_rss_bytes
                                          //   old: chunk_wall, chunk_total, ledgers_total, txhash_total, events_total
                                          //   new: backfill_wall, index_rebuild, chunk_total, ledgers_total,
                                          //        txhash_total, events_total, cold_extract
  "files": { "ledgers"|"txhash"|"events": { "<stage>": StageAgg } },  // every non-driver *.csv, every row
  "derived": {                            // per-run then aggregated
    "ledgers_per_s": V,                   // ledgers / wall     (wall = chunk_wall old, backfill_wall new)
    "tp_ledgers": V, "tp_txs": V, "tp_events": V   // unit_meta count / {type}_total per run
  },
  "peak_rss_bytes": V                     // optional; see "Peak RSS" below
}}

"ingest_hot": { "<unit>": {
  "driver": { "<stage>": StageAgg },      // old: chunk_wall, ingest_total, read_blocked
                                          // new: ingest_total, run_wall
                                          // + optional pace_lag (see below), present iff paced
                                          // (the peak_rss_bytes row is lifted to the sibling field below)
  "phases": { "<stage>": StageAgg },      // hot.csv rows: extract, ledgers, txhash, events, commit, apply
  "derived": { "ledgers_per_s": V },      // ledgers / wall     (wall = chunk_wall old, run_wall new)
  "peak_rss_bytes": V                     // optional; see "Peak RSS" below
}}
```

### Peak RSS

`peak_rss_bytes` is the process **peak resident-set size in BYTES** — a memory
high-water-mark gauge, not a latency distribution. The `driver.csv` carries it as
a `peak_rss_bytes` row whose gauge value is replicated across the `*_ns` columns;
the converter reads that value and stores it as a plain `V` (median/min/max/`r`
across the reps) that is a **sibling of `driver`**, not one of its `StageAgg`
rows — the ns column names do not apply to a byte gauge, so it is deliberately not
shoehorned into `StageAgg`. Present iff the `peak_rss_bytes` row appears in every
rep's `driver.csv`; omitted entirely otherwise (never zero-filled). Compare it
against the box RAM (`hardware.mem_total_kb`, or the parsed `machine.mem`) to read
memory headroom. The viewer surfaces it in the synthetic report's dataset section
(peak RSS per profile, cold and hot, against the box's RAM ceiling); a run without
the field renders exactly as before.

`driver.pace_lag` is the optional per-ledger **lag behind the close schedule** on a
paced hot run: for each committed ledger, `max(commit_time − due_time, 0)`. Its
distribution **includes the zero-lag samples** (on-time ledgers), so `n` = `n_items`
= committed ledgers and `p50 = 0` means the run was on schedule at least half the
time. Aggregated across run repetitions exactly like every other StageAgg. **Present
iff the cell is paced** (close-interval > 0); omitted entirely for unpaced cells.
Compare against `campaign.close_interval_ns`: `lag ÷ close_interval` = ledgers behind
tip. When a cell's runs are inconsistent (row present in some runs, absent in others),
the missing runs are filled with an all-zero (on-schedule) distribution and the
converter warns.

```jsonc

"queries": { "cold"|"hot": { "<unit>": {
  "<qtype>": {                            // qtype ∈ discovered per-type CSVs: ledgers, txpage, txhash, events
    "c<W>": StageAgg & {                  // from <qtype>.csv row total_c<W>; W discovered from row names
      "wall": V,                          // driver.csv row <qtype>_c<W> total_ns
      "ops_s": V,                         // n / wall, per run
      "items_s": V, "items_r": [int×5]    // events only: n_items / wall per run; raw per-run n_items
    }
  },
  "setup": { "<stage>": { …V of total_ns, "n_items": int } }   // driver rows not matching *_c<W>
}}}

"golden": { "<unit>": { "wall_ns": int } }   // golden-download-<unit>/driver.csv chunk_wall total_ns (single run)
```

## Manifest — `docs/runs/index.json`

```jsonc
{ "schema_version": 1,
  "runs": [                                // newest date first
    { "id": "synthetic-2026-07-15", "name": "…", "date": "2026-07-15",
      "kind": "synthetic", "path": "runs/synthetic-2026-07-15.json" },
    { "id": "pubnet-2026-07-13", "name": "…", "date": "2026-07-13",
      "kind": "pubnet", "path": "runs/pubnet-2026-07-13.json" } ] }
```

The converter inserts/replaces its run's entry keyed by `id` and re-sorts by date desc.

## Inputs — result-bundle layouts & manifests

The converter auto-detects the input bundle layout from its subdirectory names:

- **synthetic** — `synth-{cold,hot}-<profile>-run<R>`.
- **pubnet** — `ingest-{cold,hot}-<chunk>-run<R>`, `query-{cold,hot}-<chunk>-run<R>`,
  `golden-download-<chunk>` (a timed sourcing leg surfaced as the `golden` section).
- **campaign** — produced by `campaign.sh`. Timed dirs sit at the bundle root as
  `{ingest,query}-{cold,hot}-<dataset>-c<chunk>-run<R>`; the unit id is the composite
  `<dataset>-c<chunk>` (e.g. `sac-6000-c1`). Untimed prep dirs `golden-<dataset>-c<chunk>`
  are dataset preparation, **not results** — the converter skips them and warns. The
  `-c<chunk>-run<R>` suffix is what distinguishes this layout from flat pubnet dirs; it is
  orthogonal to `dataset.kind` (a campaign may carry pubnet or synthetic data).

Every bundle also carries a free-text `*machine-metadata*.txt` at the root (parsed into
`machine`). A campaign bundle additionally carries **two JSON manifests** — both optional
and additive, so manifest-less bundles convert unchanged:

- **`metadata.json`** at the bundle root (schema_version 1) — the campaign runner's record.
  Source of truth for run identity (`run_id` → default `run_id`; `started_at` → default
  `run_date`), the `campaign` config (incl. `close_interval` → `campaign.close_interval_ns`),
  the structured `hardware` object, and `hostname`. `datasets[].kind` is the dataset
  **transport** (`packs-local|packs-gs|bsb-s3|fixture`), not pubnet-vs-synthetic, and sets
  campaign display order.
- **`invocation.json`** in each per-invocation `--out` dir (schema_version 1) — written by
  the four bench subcommands. Source of truth for binary identity (`binary.{commit_hash,
  branch,version,build_timestamp}`) and the resolved subcommand `flags`. Consistency of the
  binary commit is cross-checked across invocations (and against `metadata.campaign.built_commit`);
  a mismatch warns.

Explicitly-passed CLI args (`--run-id`, `--run-date`, …) always win over manifest defaults.
Where free-text machine metadata and the structured manifests overlap, the **structured data
wins**: `hardware` supersedes the parsed `machine` instance/vcpus/mem, and `invocation.json`
binary identity supersedes the `repo:` line.
