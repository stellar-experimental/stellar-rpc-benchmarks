# stellar-rpc-benchmarks

Benchmark reports — **as data** — for stellar-rpc's `feature/full-history` branch
(RocksDB hot tier + immutable packfile cold tier). Each benchmark run is committed as a
plain JSON file and rendered by a static, dependency-free viewer. No database, no build
step, no server: the numbers live in git and the site is just HTML/JS reading them.

**Live site:** https://stellar-experimental.github.io/stellar-rpc-benchmarks

## What this is

The bench suite (`stellar-rpc bench-ingest cold|hot`, `bench-query cold|hot`) runs in
campaigns on an AWS NVMe devbox (`m6id.2xlarge`). Each campaign is several configurations
× 5 fresh-process runs; every run writes CSVs
(`stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns`) into its own directory, and the
results are mirrored to GCS under `gs://rpc-full-history/benchmarks/`.

`converter/convert.py` (Python 3, standard library only) turns one results directory into
`docs/runs/<run-id>.json` (schema v1) and updates the manifest `docs/runs/index.json`. The
static viewer in `docs/` renders any committed run via a dropdown or `?run=<id>`. Appending
`&view=hot` (or the toolbar toggle) shows a focused, stakeholder-facing hot-ingestion-only view.

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

## Add a run locally (the primary flow today)

On a laptop that's authenticated to GCS (`gcloud auth login`), pull a results directory
down, convert it, and commit. Worked example:

```bash
# 1. Pull the results directory from GCS. (This is the exact path recorded as this
#    run's provenance in docs/runs/pubnet-2026-07-13.json.)
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

# 3. Review the diff, then commit + push. The deploy-pages workflow syncs
#    docs/ to the gh-pages branch and Pages redeploys.
git add docs/runs
git commit -m "Add run pubnet-2026-07-13"
git push
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
| `GCS`      | no       | Source `gs://…` path, recorded in the run for provenance       |

Omitting any required variable fails with a message naming the missing one. `results-in/`
is git-ignored.

## GitHub Action flow (`.github/workflows/ingest.yml`)

`workflow_dispatch` with inputs `gcs_path`, `run_id`, `run_name`, `dataset_kind`
(pubnet|synthetic), `run_date`, and optional `unit_facts` (a repo path to a `--unit-facts`
sidecar JSON for synthetic dataset meta). It checks out the repo, authenticates to GCP via Workload
Identity Federation, `gcloud storage cp -r` the results directory into `./results-in`, runs
the converter, and commits the new/updated `docs/runs/*.json` + manifest back to `main`.
Permissions are minimal: `contents: write` (to commit) and `id-token: write` (for the OIDC
token WIF exchanges).

**This workflow does not run yet — the GCP-side setup is pending.** It fails early with a
clear message until two repository variables exist
(Settings → Secrets and variables → Actions → Variables):

- `GCP_WORKLOAD_IDENTITY_PROVIDER`
- `GCP_SERVICE_ACCOUNT`

Creating them requires, in GCP project **`dev-hubble`**, a workload identity pool + provider
(federating this GitHub repo) and a service account with `roles/storage.objectViewer` on
`gs://rpc-full-history`. That GCP setup is out of this repo's hands; until it lands, use the
local flow above.

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
├── Makefile                 # convert / test / serve / help
├── README.md
├── SCHEMA.md                # run JSON schema v1 (the data contract)
├── .github/
│   └── workflows/
│       └── ingest.yml       # workflow_dispatch: GCS results dir → committed run
├── converter/
│   ├── convert.py           # results dir → docs/runs/<id>.json (+ manifest), stdlib only
│   ├── facts/               # per-unit sidecar facts (e.g. synthetic model/tps/pack)
│   └── tests/               # make test → python3 -m unittest discover converter/tests
├── tests/
│   └── smoke/               # make smoke → jsdom viewer smoke test (Node, dev-only)
└── docs/                    # GitHub Pages root (static vanilla-JS viewer)
    ├── index.html           # the viewer shell (dropdown / ?run=<id>)
    ├── app.js               # renderers (per dataset.kind) + charts
    ├── styles.css           # design system (light + dark)
    └── runs/
        ├── index.json       # manifest of runs (newest date first)
        └── <run-id>.json    # one file per run (schema v1)
```

## Future work

- **Nightly scheduled ingestion / auto-discovery** of new GCS runs (vs. today's manual
  dispatch).
- **`bench-compare` A/B views** in the viewer.
- **Cross-run diffing / trend charts** across campaigns over time.
- **Migration to a stellar-org repo** from `stellar-experimental`.
- **The WIF setup itself** (the pending `dev-hubble` pool/provider + service account that
  unblocks the GitHub Action flow).
