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
  "sections": ["ingest_cold", "ingest_hot", "queries", "golden"],  // exactly the keys present
  "ingest_cold":  { … }, "ingest_hot": { … },
  "queries": { … },                        // pubnet only
  "golden":  { … }                         // pubnet only
}
```

A viewer encountering an unknown section name or unknown stage row must degrade to a
generic table view — never crash.

## Sections

All stage names are **kept exactly as they appear in the CSVs** (no renaming across
vocabularies). The `campaign.vocabulary` + `dataset.kind` tell the viewer which named
renderers apply; anything unrecognized renders generically.

```jsonc
"ingest_cold": { "<unit>": {
  "driver": { "<stage>": StageAgg },      // every row of driver.csv
                                          //   old: chunk_wall, chunk_total, ledgers_total, txhash_total, events_total
                                          //   new: backfill_wall, index_rebuild, chunk_total, ledgers_total,
                                          //        txhash_total, events_total, cold_extract
  "files": { "ledgers"|"txhash"|"events": { "<stage>": StageAgg } },  // every non-driver *.csv, every row
  "derived": {                            // per-run then aggregated
    "ledgers_per_s": V,                   // ledgers / wall     (wall = chunk_wall old, backfill_wall new)
    "tp_ledgers": V, "tp_txs": V, "tp_events": V   // unit_meta count / {type}_total per run
  }
}}

"ingest_hot": { "<unit>": {
  "driver": { "<stage>": StageAgg },      // old: chunk_wall, ingest_total, read_blocked
                                          // new: ingest_total, run_wall
                                          // + optional pace_lag (see below), present iff paced
  "phases": { "<stage>": StageAgg },      // hot.csv rows: extract, ledgers, txhash, events, commit, apply
  "derived": { "ledgers_per_s": V }       // ledgers / wall     (wall = chunk_wall old, run_wall new)
}}
```

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
