# stellar-rpc-benchmarks

Benchmark reports — **as data** — for stellar-rpc's `feature/full-history` branch
(RocksDB hot tier + immutable packfile cold tier). Each benchmark run is committed as a
plain JSON file and rendered by a static, dependency-free viewer. No database, no build
step, no server: the numbers live in git and the site is just HTML/JS reading them.

**Live site:** https://stellar-experimental.github.io/stellar-rpc-benchmarks

## What this is

The bench suite (`stellar-rpc bench-ingest cold|hot`, `bench-query cold|hot`) runs in
campaigns on an AWS NVMe devbox (`m6id.2xlarge`), driven by the config-driven runner in
[`runner/`](runner/) (see "Run a campaign" below). Each campaign is several configurations
× 5 fresh-process runs; every run writes CSVs
(`stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns`) into its own directory, and the
results are mirrored to GCS under `gs://rpc-full-history/results/`.

`converter/convert.py` (Python 3, standard library only) turns one results directory into
`docs/runs/<run-id>.json` (schema v1) and updates the manifest `docs/runs/index.json`. The
static viewer in `docs/` renders any committed run via a dropdown or `?run=<id>`. Appending
`&view=hot` (or the toolbar toggle) shows a focused, stakeholder-facing hot-ingestion-only view.

Campaign runs also carry the **Phase 1/2/3 performance targets** as data
(see [SCHEMA.md](SCHEMA.md#phase-123-performance-targets-campaign-layout)). The converter
matches the run's phase from its recorded close interval (2 s → Phase 1, 1 s → Phase 2,
600 ms → Phase 3) and the viewer judges hot ingestion against that phase by default —
budget lines, keep-up verdicts, and the ingest-slice p99 target. Append `&phase=N` (or use
the selector next to the target table) to view the same measurements against another
phase's targets; a caveat states the run's actual pace when it differs. The pacing-lag
figure always uses the run's actual close interval.

**How deploys work:** there is no build step — the viewer is static vanilla JS and the
run JSONs are committed alongside it. GitHub Pages serves the `gh-pages` branch, and the
`deploy-pages.yml` workflow syncs `main:/docs` to it on every push to `main`, so
deploying is still just committing to `main`. The indirection exists for one reason:
PR previews. `pr-preview.yml` deploys each PR's `docs/` to
`https://stellar-experimental.github.io/stellar-rpc-benchmarks/pr-preview/pr-<number>/`
and comments the link on the PR (removed automatically when the PR closes).

## View locally

```bash
make serve   # python3 -m http.server 8000 -d docs
```

Then open http://localhost:8000. The viewer fetches `runs/index.json` and the run files
over HTTP, so opening `docs/index.html` as a `file://` URL will **not** work — use
`make serve`.

To smoke-test the viewer headlessly (loads each run in a jsdom DOM, asserts zero JS
errors and the expected figure/section counts and sanity values), run `make smoke`
(needs Node; installs `jsdom` under `tests/smoke/` on first run).

## Run a campaign

Campaigns run on the benchmark devbox via the campaign CLI in [`runner/`](runner/) — this
repo's operations side. The runner treats stellar-rpc as a **black box**: it maintains a
build clone of it under `$BENCH_ROOT/src`, builds the configured ref, and drives the
bench subcommands — no standalone stellar-rpc checkout is needed anywhere. See
[runner/README.md](runner/README.md) for the full CLI and config reference, the bundle
layout it produces, and the minimum stellar-rpc ref it requires (the compatibility floor).

```bash
# 0. One-time on a fresh devbox (and again after every instance stop/start,
#    which wipes the NVMe instance store): provision the machine.
./runner/bootstrap.sh

# Everything below runs from runner/ (the devbox has Go; bootstrap installs it).
cd runner

# 1. Write a campaign config (copy example-campaign.toml, adjust the keys) and
#    sanity-check the full command plan. --dry-run builds, downloads, and runs
#    nothing — it works on any machine, e.g. a laptop:
go run ./cmd/campaign run my-campaign.toml --dry-run

# 2. Run it (in tmux — campaigns run for hours). Results land in
#    $BENCH_ROOT/results/<name>-<sha>-<stamp>/, tarred to /tmp so the bundle
#    survives an instance stop.
go run ./cmd/campaign run my-campaign.toml

# 3. Publish the bundle to GCS. This happens automatically when the config
#    sets publish_uri; run it by hand otherwise (or to retry a failed upload —
#    an upload that died partway leaves objects at the destination, so the
#    retry needs --force, which is a MERGE: files already there that this
#    bundle lacks survive it. Against a destination that really is empty,
#    --force only skips the emptiness check, so it is safe on a first publish):
go run ./cmd/campaign publish /mnt/nvme/bench/results/<run-id> \
  gs://rpc-full-history/results --force
```

The published bundle is exactly what the next section ingests into a committed run
JSON — closing the loop: campaign config → run → publish → ingest → viewer.

## Add a run

`scripts/ingest.sh` is the one path from a published bundle to a committed run: it
fetches the bundle, converts it, and commits the site update. The same script backs
`make ingest` and the automated flow below, so a run ingested from a laptop and one
ingested from CI are byte-identical, commit message included.

```bash
# On a laptop with AWS credentials for the results bucket. The s3:// path is
# recorded as campaign.source_uri in the run JSON (the viewer's "Source data" link).
make ingest \
  BUNDLE=s3://stellar-rpc-bench/results/<run-id> \
  KIND=synthetic
```

`BUNDLE` is auto-detected and may be a `gs://` or `s3://` bundle URI, a local bundle
directory, or a `bench-results-<run_id>.tgz` tarball — the shapes the campaign CLI's
`run` and `publish` leave behind. `KIND` is the one thing you have to state, because it is
the one fact the bundle doesn't record: `datasets[].kind` in the manifest is the dataset's
*transport* (`packs-gs`, `bsb-s3`, …), not pubnet-vs-synthetic.

Everything else is derived from the bundle's own `metadata.json`
(see [SCHEMA.md § Inputs](SCHEMA.md#inputs--result-bundle-layouts--manifests)):

| Derived                 | From                                             |
|-------------------------|--------------------------------------------------|
| Run id, and so the file | `run_id` → `docs/runs/<run_id>.json`             |
| Run date                | `started_at`                                     |
| Run name                | `campaign.name`                                  |
| `campaign.source_uri`   | the `s3://` (or `gs://`) URI you passed (recorded as provenance) |
| Commit body             | campaign config, close interval, datasets, hardware |

Three modes, least to most committal:

| Mode          | Effect                                                                     |
|---------------|-----------------------------------------------------------------------------|
| `--dry-run`   | Converts into a **temp** directory — never `docs/runs/` — and prints the converter output, the derived run id, the would-be commit body, the would-be branch, and the exact commands `--push-main` would run. Executes none of them. A remote bundle isn't fetched either; the fetch command is printed instead, so a dry run works offline. |
| `--local`     | (default) Converts into `docs/runs/`, creates `run/<run_id>` off HEAD, commits the two changed files. No push. |
| `--push-main` | Converts into `docs/runs/`, runs `make test` + `make smoke` as a push gate, commits on HEAD (which must be at `origin/main`), and pushes it to `origin main`. Prints `viewer: <url>`. Pushing to main is the deploy (`deploy-pages.yml`). |

`make ingest` passes `--local` on purpose: it stops at the commit, so you can read the
diff and `make serve` the result before anything leaves the machine. Call the script
directly for the other two modes:

```bash
# Look before you leap — converts to a temp dir, touches nothing:
scripts/ingest.sh s3://stellar-rpc-bench/results/<run-id> --dataset-kind synthetic --dry-run

# Publish: convert, gate on the tests, commit on main, push (the deploy):
scripts/ingest.sh s3://stellar-rpc-bench/results/<run-id> --dataset-kind synthetic --push-main
```

Two rails keep the flow from surprising you. The script refuses to run against a working
tree with uncommitted tracked changes, and refuses to overwrite an already-ingested run —
an existing `docs/runs/<run_id>.json` is an error until you pass `--force`. A `--force`
re-ingest reuses the existing `run/<run_id>` branch instead of failing on it, and exits
quietly when the reconverted JSON turns out byte-identical to the committed one.

Anything after a literal `--` is passed straight through to `convert.py`, which is how you
override a derived field without leaving the flow:

```bash
scripts/ingest.sh gs://rpc-full-history/results/<run-id> --dataset-kind synthetic -- \
  --run-name "Phase 3 — c6id.8xlarge rerun" \
  --unit-facts converter/facts/synthetic-2026-07-15.json
```

### `make convert` — the layer underneath

`make convert` calls `converter/convert.py` and nothing else: no fetch, no branch, no
commit. Reach for it when the bundle can't identify itself — the **legacy** pubnet and
synthetic layouts predate `metadata.json`, so `--run-id`/`--run-name`/`--run-date` have no
defaults to fall back on — or when you want to name every field by hand. Worked example
against the archived pubnet run (`docs/runs/archive/pubnet-2026-07-13.json`):

```bash
# 1. Pull the results directory from GCS. (This is the exact path recorded as that
#    run's provenance.)
gcloud storage cp -r \
  gs://rpc-full-history/benchmarks/2026-07-13-user-dev-063a \
  ./results-in

# 2. Convert it into docs/runs/pubnet-2026-07-13.json and update docs/runs/index.json.
make convert \
  RESULTS=./results-in/2026-07-13-user-dev-063a \
  RUN_ID=pubnet-2026-07-13 \
  RUN_NAME="Pubnet — 4 sampled chunks (user-dev-063a)" \
  KIND=pubnet \
  RUN_DATE=2026-07-13 \
  GCS=gs://rpc-full-history/benchmarks/2026-07-13-user-dev-063a

# 3. Review the diff, then commit on a branch and open a PR — the same place
#    `scripts/ingest.sh` would have left you.
git add docs/runs
git commit -m "Add run pubnet-2026-07-13"
```

`make convert` variables:

| Variable   | Required | Meaning                                                        |
|------------|----------|----------------------------------------------------------------|
| `RESULTS`  | yes      | Path to the downloaded results directory                       |
| `RUN_ID`   | yes      | Slug; the file becomes `docs/runs/<RUN_ID>.json`               |
| `RUN_NAME` | yes      | Human-readable run name                                        |
| `KIND`     | yes      | `pubnet` or `synthetic`                                        |
| `RUN_DATE` | yes      | `YYYY-MM-DD`                                                   |
| `FACTS`    | no       | Path to a `--unit-facts` sidecar JSON (synthetic dataset meta) |
| `URI`      | no       | Source `s3://…` (or `gs://…`) path, recorded in the run for provenance |
| `GCS`      | no       | Legacy: source `gs://…` path as `campaign.source_gcs`; prefer `URI` |

Omitting any required variable fails with a message naming the missing one. `results-in/`
is git-ignored.

## Transaction-submission results (interim page)

`docs/tx-submission.html` reports a one-off `sendTransaction` benchmark
(stellar/stellar-rpc#869): the time the stellar-rpc process spends inside one submission
request, across three transaction profiles on two networks. Those results don't fit the
schema-v1 run JSON (single run, exact per-request percentiles — not 5-rep CSV campaigns),
so the harvest summaries produced by stellar-rpc-blaster's
`scripts/tx-submission/harvest.py` are committed **verbatim** under `docs/txsub/` and
rendered by `docs/txsub.js`. This is deliberate and interim — a unifying refactor happens
when the other benchmark families land.

To add a future run: upload the bundle to GCS, copy its `summary-<mode>.json` files
byte-identical into `docs/txsub/<bundle-id>/`, and add an entry to
`docs/txsub/index.json` (network, role, date, rps × duration, rpc_version, instance,
GCS path, summary paths).

## Automated ingest (stellar-rpc's `bench-campaign.yml`)

Campaigns dispatched from stellar-rpc's `bench-campaign.yml` workflow ingest their own
results: after a passing campaign, its notify job downloads the results tarball the
benchmark box uploaded, clones this repo, and runs

```bash
scripts/ingest.sh bench-results-<run_id>.tgz --dataset-kind synthetic --push-main \
  -- --source-uri s3://stellar-rpc-bench/results/<run_id>
```

— the same script as the local flow above, which is the point: CI is not a second
implementation that can drift. `--push-main` gates the push on `make test` +
`make smoke`, commits on `main`, and pushes; `deploy-pages.yml` then publishes the site,
and the campaign's summary and Slack message carry the
`https://stellar-experimental.github.io/stellar-rpc-benchmarks/?run=<run_id>` link.

The push authenticates with a write-capable token for this repo, stored as the
`BENCHMARKS_PUSH_TOKEN` secret on stellar/stellar-rpc. No AWS credentials live in this
repo: the tarball travels through the campaign's own S3 prefix, which stellar-rpc's
existing CI role can read. A failed ingest never fails the campaign — the notification
falls back to the raw `s3://` results URI, and the run can be ingested later from a
laptop with the same command.

## Data model

One JSON file per run under `docs/runs/`, listed in `docs/runs/index.json`. The full schema
(value shapes, sections, manifest) is documented in **[SCHEMA.md](SCHEMA.md)**.

## Statistical conventions

- Every reported value is the **median across the 5 runs**; spread is **min–max**; the raw
  5-run array is kept alongside.
- Percentiles are per-run values aggregated the same way — **never averaged across runs**.
- Derived rates (throughputs) are computed **per run** from raw fields, then aggregated —
  never derived from already-aggregated medians.
- Nothing is interpolated or smoothed; every rendered number traces to a raw CSV field.

See [SCHEMA.md](SCHEMA.md#statistical-conventions-non-negotiable) for the authoritative
statement.

## Repo layout

```
stellar-rpc-benchmarks/
├── Makefile                 # ingest / convert / test / smoke / serve / help
├── README.md
├── SCHEMA.md                # run JSON schema v1 (the data contract)
├── .github/
│   └── workflows/
│       ├── ingest.yml       # workflow_dispatch: GCS bundle → run PR (delegates to scripts/ingest.sh)
│       ├── deploy-pages.yml # sync main:/docs to the gh-pages branch Pages serves
│       ├── pr-preview.yml   # publish each PR's docs/ under gh-pages:/pr-preview/pr-<n>/
│       ├── runner-go.yml    # go vet + go test for the campaign runner
│       └── shellcheck.yml   # lint the remaining shell scripts on every PR that touches them
├── runner/                  # benchmark operations: the devbox side producing result bundles
│   ├── bootstrap.sh         # provision the devbox (idempotent, no builds)
│   ├── cmd/campaign/        # campaign CLI: run · plan · preflight · publish (see runner/README.md)
│   ├── internal/            # config · plan · run · preflight · publish · bundle
│   └── example-campaign.toml # annotated config to copy from
├── scripts/
│   └── ingest.sh            # bundle → converted run → run/<id> branch → PR (make ingest)
├── converter/
│   ├── convert.py           # results dir → docs/runs/<id>.json (+ manifest), stdlib only
│   ├── facts/               # per-unit sidecar facts (e.g. synthetic model/tps/pack)
│   └── tests/               # make test → python3 -m unittest discover converter/tests
├── tests/
│   └── smoke/               # make smoke → jsdom viewer smoke test (Node, dev-only)
└── docs/                    # GitHub Pages root (static vanilla-JS viewer)
    ├── index.html           # the viewer shell (dropdown / ?run=<id>)
    ├── app.js               # renderers (per dataset.kind) + charts
    ├── styles.css           # design system (light + dark), shared by every page
    ├── summary.html         # stakeholder summary page (summary.js + summary.css)
    ├── latency-model.html   # end-to-end latency model against the phase targets
    ├── tx-submission.html   # transaction-submission report (txsub.js)
    ├── targets.json         # Phase 1/2/3 performance targets — single source of truth
    ├── dataset-sizes.json   # measured sizes of the synthetic dataset profiles
    ├── txsub/               # tx-submission harvest summaries, verbatim (+ index.json)
    └── runs/
        ├── index.json       # manifest of runs (oldest date first)
        ├── <run-id>.json    # one file per run (schema v1)
        └── archive/         # retired pre-campaign runs, with their own manifest
```

## Future work

- **Nightly scheduled ingestion / auto-discovery** of new GCS runs (vs. today's manual
  dispatch).
- **`bench-compare` A/B views** in the viewer.
- **Cross-run diffing / trend charts** across campaigns over time.
- **Migration to a stellar-org repo** from `stellar-experimental`.
- **The WIF setup itself** (the pending `dev-hubble` pool/provider + service account that
  unblocks the GitHub Action flow).
