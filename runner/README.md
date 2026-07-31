# Campaign runner

The operations side of this repo: a config-driven runner that produces the result
bundles the rest of the pipeline consumes. It treats **stellar-rpc as a black box** — it
clones it, builds the requested ref, and drives its `bench-ingest` / `bench-query`
subcommands; it never lives inside a stellar-rpc checkout and never modifies one.

```
runner/
├── bootstrap.sh          # provision the devbox (NVMe, apt, Go, Rust, native libs, env)
├── campaign.sh           # run one campaign from a config file
├── publish.sh            # upload a finished bundle to gs:// or s3://
└── example-campaign.cfg  # annotated config to copy from
```

`campaign.sh`'s header comment is the authoritative reference for config keys and
dataset kinds; the [top-level README](../README.md#run-a-campaign) walks through the
operator flow end to end.

## Compatibility floor

The runner requires a stellar-rpc ref whose bench subcommands **write `invocation.json`
into every `--out` directory** — that is stellar-rpc's `bench-run-metadata` branch or any
descendant of it (its merge commit into `feature/full-history`, once merged). The default
`REF=feature/full-history` satisfies this only after that merge lands; until then, set
`REF=bench-run-metadata` (or a descendant) in the campaign config. Older refs produce
bundles without per-invocation manifests, which the converter accepts but with weaker
provenance (see `SCHEMA.md` § Inputs).

## `$BENCH_ROOT` layout

Everything the runner touches lives under `$BENCH_ROOT` (default `/mnt/nvme/bench`, the
devbox's NVMe instance store — wiped on instance stop/start; everything here is
re-creatable):

```
$BENCH_ROOT/
├── src/       persistent build clone of $REPO (re-pointed, fetched, and hard-reset
│              every campaign; gitignored build caches survive, so rebuilds are
│              incremental)
├── bin/       versioned binaries: stellar-rpc-<sha>
├── golden/    immutable prepared datasets, one dir per dataset name
│              (rm -rf golden/<name> to force a re-fetch)
├── fixture/   staging area for generated fixture packs
├── scratch/   cold-ingest output, deleted before every run
├── hot/       hot DBs; the last run's DB is kept for the hot query suite
└── results/   campaign bundles: <NAME>-<sha>-<stamp>/
```

The finished bundle is also tarred to `/tmp/bench-results-<NAME>-<sha>-<stamp>.tgz` (EBS
root, survives an instance stop) and, when `PUBLISH_URI` is set, uploaded to
`<PUBLISH_URI>/<NAME>-<sha>-<stamp>/`.

## Campaign bundle layout — the cross-repo contract

A campaign bundle is what `publish.sh` uploads and what `converter/convert.py` consumes
(as the **campaign** input layout). Two repos write into it, so its shape is a contract:

```
<NAME>-<sha>-<stamp>/                        # run_id = the bundle basename
├── <config>.cfg                             # the campaign config, verbatim
├── binary.txt                               # benchmarked binary identity (free text)
├── machine-metadata.txt                     # machine facts (free text)
├── metadata.json                            # ← written by campaign.sh (THIS repo)
├── golden-<dataset>-c<chunk>/               # untimed dataset prep — not results;
│                                            #   the converter skips these and warns
├── ingest-{cold,hot}-<dataset>-c<chunk>-run<R>/
│   ├── driver.csv, hot.csv, *.csv           # ← written by stellar-rpc bench subcommands
│   └── invocation.json                      # ← written by stellar-rpc bench subcommands
└── query-{cold,hot}-<dataset>-c<chunk>-run<R>/
    └── …same shape…
```

Who owns what:

- **`metadata.json`** (bundle root, `schema_version` 1) — written by `campaign.sh` here.
  Run identity (`run_id`, `started_at`), the campaign config knobs (incl.
  `close_interval`), the dataset list, structured `hardware`, and `hostname`.
- **`invocation.json`** (each `--out` dir, `schema_version` 1) — written by stellar-rpc's
  `bench-ingest` / `bench-query`. Binary identity (`binary.{commit_hash, branch, version,
  build_timestamp}`) and the resolved subcommand flags.

The consumer side of this contract — exactly which fields the converter reads, and the
precedence rules between the manifests, the free-text metadata, and CLI arguments — is
documented in [`SCHEMA.md` § Inputs](../SCHEMA.md#inputs--result-bundle-layouts--manifests).
Changing either manifest's shape, the bundle directory naming, or the CSV columns is a
cross-repo change: update the producer (here or in stellar-rpc), the converter, and
`SCHEMA.md` together.
