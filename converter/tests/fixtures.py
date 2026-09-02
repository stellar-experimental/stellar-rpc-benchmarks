"""Fixture-bundle builders for converter tests. Python 3 stdlib only.

Materialise a benchmark results bundle onto disk so both the unit tests and the
end-to-end / viewer checks convert a real bundle rather than a mock:

  build_campaign_bundle  — campaign.sh layout (<family>-<dataset>-c<chunk>-run<R>)
                           with metadata.json at the root + invocation.json in
                           every timed dir, paced or unpaced, plus golden-* prep
                           dirs that must be skipped.
  build_rps_bundle       — a one-unit campaign whose query legs are the RPS-era
                           per-qtype dirs (query-<tier>-<unit>-<qtype>-run<R>).
  build_legacy_bundle    — a manifest-less pubnet bundle (today's path).

All timing values scale linearly with the run number, so for the 2-rep default
`median_low` of each column is exactly the run-1 value and min/max are trivially
run-1/run-2 — hand-computable in the tests.
"""
import json
import os

HEADER = "stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns\n"
COMMIT = "b32bc9be0123456789abcdef0123456789abcdef"
BRANCH = "bench-ci-775"
VERSION = "v20.3.1-412-gb32bc9be"

# Per-unit natural-unit counts (ledgers / txs / events) — cold driver n_items.
CAMPAIGN_UNITS = {
    "sac-6000-c1":      {"ledgers": 100, "txs": 600, "events": 600},
    "soroswap-1500-c1": {"ledgers": 200, "txs": 300, "events": 300},
    "soroswap-1500-c2": {"ledgers": 200, "txs": 300, "events": 300},
}

MACHINE_META = """Tue Jul 21 00:00:00 UTC 2026
instance-type: OLD.instance
instance-id:   i-oldoldoldoldold0
Linux fixture 6.0.0-fixture x86_64 x86_64 x86_64 GNU/Linux
Ubuntu 24.04 LTS
CPU(s):                                  2
Model name:                              Fixture CPU
               total        used        free
Mem:            8Gi       1Gi       6Gi
repo: {commit} (old-branch)
go version go1.26.5 linux/amd64
rustc 1.97.0 (2d8144b78 2026-07-07)
campaign: fixture-rps-query · ingest: both · query: yes · runs: 3 · query-duration: 60s · query-phase: 1
close-interval: 2s · workers: 32 · hot-num-ledgers: 500
fsync probe: 8192000 bytes (8.2 MB, 7.8 MiB) copied, 0.3 s, 27 MB/s
"""


def _write_csv(path, rows):
    with open(path, "w") as f:
        f.write(HEADER)
        for r in rows:
            f.write(",".join(str(x) for x in r) + "\n")


def _scale(base, run):
    """(n, n_items) kept constant; the five timing columns scaled by run."""
    n, ni, t, p50, p90, p99, mx = base
    return (n, ni, t * run, p50 * run, p90 * run, p99 * run, mx * run)


def _cold_driver(u, run, vocab="new"):
    c = CAMPAIGN_UNITS.get(u, {"ledgers": 100, "txs": 600, "events": 600})
    L, T, E = c["ledgers"], c["txs"], c["events"]
    wall = ("backfill_wall" if vocab == "new" else "chunk_wall")
    specs = [(wall, (1, 0, 50000, 50000, 50000, 50000, 50000))]
    if vocab == "new":
        specs.append(("index_rebuild", (1, 0, 3000, 3000, 3000, 3000, 3000)))
    specs += [
        ("chunk_total",   (1, 0, 40000, 40000, 40000, 40000, 40000)),
        ("ledgers_total", (L, L, 8000, 80, 90, 99, 120)),
        ("txhash_total",  (T, T, 6000, 60, 70, 90, 100)),
        ("events_total",  (E, E, 9000, 90, 95, 99, 110)),
    ]
    if vocab == "new":
        specs.append(("cold_extract", (1, 0, 7000, 70, 80, 90, 100)))
        # peak RSS is a byte gauge (same value in every duration column); the
        # converter lifts it out of the driver into a sibling V.
        specs.append(("peak_rss_bytes", (1, 0) + (20_000_000_000,) * 5))
    return [(s,) + _scale(b, run) for s, b in specs]


def _cold_files(u, run):
    c = CAMPAIGN_UNITS.get(u, {"ledgers": 100, "txs": 600, "events": 600})
    L, T, E = c["ledgers"], c["txs"], c["events"]
    return {
        "events": [("term_index",) + _scale((E, E, 4000, 40, 45, 49, 60), run),
                   ("write",) + _scale((E, E, 3000, 30, 35, 39, 50), run),
                   ("finalize",) + _scale((1, 0, 2000, 2000, 2000, 2000, 2000), run)],
        "ledgers": [("write",) + _scale((L, L, 3000, 30, 35, 39, 50), run),
                    ("finalize",) + _scale((1, 0, 1000, 1000, 1000, 1000, 1000), run)],
        "txhash": [("finalize",) + _scale((1, 0, 1500, 1500, 1500, 1500, 1500), run)],
    }


def _hot_driver(u, run, paced, vocab="new"):
    c = CAMPAIGN_UNITS.get(u, {"ledgers": 100, "txs": 600, "events": 600})
    L = c["ledgers"]
    rows = [("ingest_total",) + _scale((L, L, 2000, 100, 200, 500, 900), run)]
    if vocab == "new":
        rows.append(("run_wall",) + _scale((1, L, 60000, 60000, 60000, 60000, 60000), run))
    else:
        rows.insert(0, ("chunk_wall",) + _scale((1, L, 60000, 60000, 60000, 60000, 60000), run))
        rows.append(("read_blocked",) + _scale((L, 0, 1500, 15, 20, 30, 60), run))
    if paced:
        # pace_lag (ns): p50 = 0 (on schedule >= half the time) by construction;
        # p99 = 0.8 s is a visible fraction of the 2 s close interval.
        rows.append(("pace_lag",) + _scale(
            (L, L, 1_000_000_000, 0, 300_000_000, 800_000_000, 1_400_000_000), run))
    if vocab == "new":
        # peak RSS byte gauge — lifted out of the driver into a sibling V.
        rows.append(("peak_rss_bytes",) + _scale((1, 0) + (14_000_000_000,) * 5, run))
    return rows


def _hot_phases(u, run):
    c = CAMPAIGN_UNITS.get(u, {"ledgers": 100, "txs": 600, "events": 600})
    L, T, E = c["ledgers"], c["txs"], c["events"]
    specs = [("extract", (L, 0, 300, 30, 40, 50, 80)),
             ("ledgers", (L, L, 400, 40, 50, 60, 90)),
             ("txhash",  (L, T, 200, 20, 25, 30, 50)),
             ("events",  (L, E, 500, 50, 55, 60, 90)),
             ("commit",  (L, 0, 800, 80, 90, 120, 200)),
             ("apply",   (L, 0, 250, 25, 30, 40, 70))]
    return [(s,) + _scale(b, run) for s, b in specs]


# The query legs' report contract (stellar-rpc bench-query): one CSV per swept
# type carrying a total_c<W> row, plus driver.csv with each cell's wall and the
# setup rows. Only a cold leg evicts, so only its driver carries that row.
QUERY_TYPES = ("ledgers", "txpage", "txhash", "events")
QUERY_CONC = (1, 4)


def _query_files(tier, run):
    files = {}
    driver = [("open", 1, 1, 60_000_000, 60_000_000, 60_000_000, 60_000_000, 60_000_000)]
    if tier == "cold":
        driver.append(("evict", len(QUERY_TYPES) * len(QUERY_CONC), 6, 4000, 500, 600, 700, 700))
    for i, qt in enumerate(QUERY_TYPES):
        rows = []
        for w in QUERY_CONC:
            n = 100 * w
            # Cold reads cost more than hot ones, and the tail grows with the
            # worker count — enough shape to read, all well inside 500 ms.
            base = (900_000 if tier == "cold" else 90_000) * (i + 1)
            p99 = base * 3 + w * 1000 + run
            rows.append((f"total_c{w}", n, n * 10, base * n, base, base * 2, p99, p99 * 2))
            driver.append((f"{qt}_c{w}", 1, n, base * n // w, 0, 0, 0, 0))
        files[qt] = rows
    driver.append(("peak_rss_bytes", 1, 0, 900_000_000, 0, 0, 0, 0))
    files["driver"] = driver
    return files


def _write_invocation(d, subcommand, close_interval):
    # Mirrors stellar-rpc's writer (invocation.go, #907): camelCase keys,
    # written for failed runs too (then with an `error` field).
    flags = {"out": d, "num-ledgers": "100", "source": "pack"}
    if subcommand.endswith("hot"):
        flags["close-interval"] = close_interval
    inv = {
        "schemaVersion": 1,
        "command": "stellar-rpc " + subcommand,
        "flags": flags,
        "binary": {"version": VERSION, "commitHash": COMMIT,
                   "buildTimestamp": "2026-07-22T00:10:00", "branch": BRANCH},
        "hostname": "user-dev-063a",
        "startedAt": "2026-07-22T01:00:00Z",
        "finishedAt": "2026-07-22T03:47:12Z",
    }
    with open(os.path.join(d, "invocation.json"), "w") as f:
        json.dump(inv, f, indent=2)


def build_campaign_bundle(root, paced=True, reps=2, machine_commit="0" * 40,
                          drop_pace=None, close_interval=None, queries=False):
    """Create a campaign-layout bundle under `root`. `drop_pace` = {unit: {runs}}
    omits the pace_lag row from those hot runs (to exercise zero-fill).
    `close_interval` overrides the recorded pace (a Go duration string);
    default is "2s" when paced, "0" when unpaced. `queries` adds the cold and
    hot query legs, as a campaign dispatched with query = yes produces."""
    os.makedirs(root, exist_ok=True)
    drop_pace = drop_pace or {}
    if close_interval is None:
        close_interval = "2s" if paced else "0"
    units = list(CAMPAIGN_UNITS)
    for u in units:
        for r in range(1, reps + 1):
            cdir = os.path.join(root, f"ingest-cold-{u}-run{r}")
            os.makedirs(cdir, exist_ok=True)
            _write_csv(os.path.join(cdir, "driver.csv"), _cold_driver(u, r))
            for name, rows in _cold_files(u, r).items():
                _write_csv(os.path.join(cdir, f"{name}.csv"), rows)
            _write_invocation(cdir, "bench-ingest cold", close_interval)

            hdir = os.path.join(root, f"ingest-hot-{u}-run{r}")
            os.makedirs(hdir, exist_ok=True)
            paced_here = paced and r not in drop_pace.get(u, set())
            _write_csv(os.path.join(hdir, "driver.csv"), _hot_driver(u, r, paced_here))
            _write_csv(os.path.join(hdir, "hot.csv"), _hot_phases(u, r))
            _write_invocation(hdir, "bench-ingest hot", close_interval)

            if not queries:
                continue
            for tier in ("cold", "hot"):
                qdir = os.path.join(root, f"query-{tier}-{u}-run{r}")
                os.makedirs(qdir, exist_ok=True)
                for name, rows in _query_files(tier, r).items():
                    _write_csv(os.path.join(qdir, f"{name}.csv"), rows)
                _write_invocation(qdir, f"bench-query {tier}", close_interval)

    # Untimed dataset-preparation ingests — must be skipped, not converted.
    for u in units:
        gdir = os.path.join(root, f"golden-{u}")
        os.makedirs(gdir, exist_ok=True)
        _write_csv(os.path.join(gdir, "driver.csv"),
                   [("chunk_wall", 1, 0, 10, 10, 10, 10, 10)])

    metadata = {
        "schema_version": 1,
        "run_id": "phase1-synthetic-minspec-b32bc9be-20260722T010000Z",
        "campaign": {
            "name": "phase1-synthetic-minspec",
            "config_file": "phase1-synthetic-minspec.cfg",
            "ref": "", "built_commit": COMMIT,
            "ingest": "both", "query": "yes" if queries else "no",
            "close_interval": close_interval,
            "runs": reps, "query_concurrency": "1,4",
            "cold_iters": 100, "hot_iters": 200, "workers": 1, "hot_num_ledgers": 100,
        },
        "datasets": [
            {"name": "sac-6000", "kind": "packs-gs",
             "location": "gs://bucket/sac-6000/packs/cold", "chunks": [1]},
            {"name": "soroswap-1500", "kind": "packs-gs",
             "location": "gs://bucket/soroswap-1500/packs/cold", "chunks": [1, 2]},
        ],
        "hardware": {
            "instance_type": "m6id.2xlarge", "instance_id": "i-0123456789abcdef0",
            "uname": "Linux 6.8.0-1015-aws x86_64", "cpus": 8, "mem_total_kb": 32000000,
        },
        "hostname": "user-dev-063a",
        "started_at": "2026-07-22T01:00:00Z",
        "finished_at": "2026-07-22T18:30:00Z",
    }
    with open(os.path.join(root, "metadata.json"), "w") as f:
        json.dump(metadata, f, indent=2)
    with open(os.path.join(root, "machine-metadata.txt"), "w") as f:
        f.write(MACHINE_META.format(commit=machine_commit))
    return root


# ------------------------------------------------------------ RPS query legs
# The open-loop contract (stellar-rpc bench-query, paced mode): ONE LEG DIR PER
# QUERY TYPE — query-<tier>-<unit>-<qtype>-run<R> — whose <qtype>.csv carries a
# scheduled row (total_r<rate>, due->done) and a service row (service_r<rate>,
# dispatch->done) per target rate, and whose driver.csv carries four rows per
# leg (<qtype>_r<rate>{,_millirps,_lag,_shed}) beside the usual setup rows.
# Rates are spelled the way Go's strconv.FormatFloat(rps,'f',-1,64) spells them.
#
# The default ladder is a phase-3 sac run: every endpoint sweeps its SLA floor
# x 0.5/1/2, so the MIDDLE rung is the cell the SLA verdict lands on. txhash
# carries BOTH families in one leg, so its ladder is the SLA one (150/300/600)
# unioned with sac's phase-3 demand ladder (500/1000/2000): the SLA verdict
# lands on r300 and the E2E-budget verdict on r1000.
RPS_DATASET = "sac-6000"
RPS_RATES = {
    "ledgers": ["12.5", "25", "50"],
    "txpage":  ["37.5", "75", "150"],
    "txhash":  ["150", "300", "500", "600", "1000", "2000"],
    "events":  ["50", "100", "200"],
}
RPS_ANSWERED = 100                 # answered requests per leg
RPS_SCHED_P99_NS = 15_000_000      # scheduled p99, inside every endpoint's SLA
                                   # (the tightest is getTransaction hot, 20 ms)
RPS_SERVICE_P99_NS = 5_000_000     # service p99, inside the 10 ms in-RPC budget
RPS_MEAN_PAGE_NS = 80_000_000      # getEvents mean page — reported, not judged
RPS_SHED = 7                       # dropped at the in-flight cap (top rung only)


def _millirps(tok):
    """Achieved rate x1000, a hair under target — an int in the duration cols."""
    return int(round(float(tok) * 1000)) - 2


def _rps_qtype_rows(qt, run, rates, sched_p99, svc_p99, mean_page):
    rows = []
    for tok in rates[qt]:
        n = RPS_ANSWERED
        items = n * (10 if qt == "events" else 1)
        s, v = sched_p99[qt], svc_p99[qt]
        rows.append((f"total_r{tok}",) + _scale(
            (n, items, n * s // 2, s // 4, s // 2, s, s * 2), run))
        rows.append((f"service_r{tok}",) + _scale(
            (n, items, n * mean_page[qt], v // 4, v // 2, v, v * 2), run))
        if qt == "txhash":
            # found/miss sub-stage splits: the converter matches total_ alone.
            rows.append((f"found_r{tok}",) + _scale(
                (n - 1, n - 1, n * s // 3, s // 4, s // 2, s, s * 2), run))
            rows.append((f"miss_r{tok}",) + _scale((1, 1, s, s, s, s, s), run))
    return rows


def _rps_driver_rows(tier, qt, run, rates):
    rows = [("open", 1, 1, 60_000_000, 60_000_000, 60_000_000, 60_000_000, 60_000_000)]
    if tier == "cold":
        rows.append(("evict", len(rates[qt]), 6, 4000, 500, 600, 700, 700))
    for i, tok in enumerate(rates[qt]):
        n = RPS_ANSWERED
        shed = RPS_SHED if i == len(rates[qt]) - 1 else 0
        wall = int(n / float(tok) * 1e9)
        rows.append((f"{qt}_r{tok}",) + _scale((1, n, wall, 0, 0, 0, 0), run))
        # Counts, not durations: they do not scale with the run number.
        rows.append((f"{qt}_r{tok}_millirps", 1, 0) + (_millirps(tok),) * 5)
        rows.append((f"{qt}_r{tok}_lag",) + _scale(
            (n + shed, n + shed, 50_000_000, 0, 1_000_000, 3_000_000, 10_000_000), run))
        rows.append((f"{qt}_r{tok}_shed", shed, shed, 0, 0, 0, 0, 0))
    rows.append(("peak_rss_bytes", 1, 0, 900_000_000, 0, 0, 0, 0))
    return rows


def build_rps_bundle(root, reps=2, close_interval="600ms", dataset=RPS_DATASET,
                     rates=None, sched_p99=None, svc_p99=None, mean_page=None,
                     query_phase="auto"):
    """Campaign bundle with ONE unit (<dataset>-c1) whose query legs use the
    RPS-era per-qtype dirs. The ingest legs, metadata.json and machine metadata
    mirror build_campaign_bundle, so the whole convert() pipeline runs.

    Timing columns scale with the run number (median_low of the 2-rep default is
    the run-1 value); the count-carrying rows (millirps, shed) do not, so the
    achieved rate reads the same in every rep.

    `query_phase` is the manifest's campaign.query_phase, the goal phase the
    runner resolved. "auto" derives it from the pace (what a paced campaign
    records); pass an int for an unpaced campaign that stated `phase` outright,
    or 0 to omit the key the way the runner's omitempty does.
    """
    rates = dict(RPS_RATES if rates is None else rates)
    sched_p99 = dict({q: RPS_SCHED_P99_NS for q in rates}, **(sched_p99 or {}))
    svc_p99 = dict({q: RPS_SERVICE_P99_NS for q in rates}, **(svc_p99 or {}))
    mean_page = dict({q: RPS_MEAN_PAGE_NS for q in rates}, **(mean_page or {}))
    paced = close_interval not in ("0", "")
    if query_phase == "auto":
        query_phase = {"2s": 1, "1s": 2, "600ms": 3}.get(close_interval, 0)
    unit = f"{dataset}-c1"
    os.makedirs(root, exist_ok=True)
    for r in range(1, reps + 1):
        cdir = os.path.join(root, f"ingest-cold-{unit}-run{r}")
        os.makedirs(cdir, exist_ok=True)
        _write_csv(os.path.join(cdir, "driver.csv"), _cold_driver(unit, r))
        for name, rows in _cold_files(unit, r).items():
            _write_csv(os.path.join(cdir, f"{name}.csv"), rows)
        _write_invocation(cdir, "bench-ingest cold", close_interval)

        hdir = os.path.join(root, f"ingest-hot-{unit}-run{r}")
        os.makedirs(hdir, exist_ok=True)
        _write_csv(os.path.join(hdir, "driver.csv"), _hot_driver(unit, r, paced))
        _write_csv(os.path.join(hdir, "hot.csv"), _hot_phases(unit, r))
        _write_invocation(hdir, "bench-ingest hot", close_interval)

        for tier in ("cold", "hot"):
            for qt in rates:
                qdir = os.path.join(root, f"query-{tier}-{unit}-{qt}-run{r}")
                os.makedirs(qdir, exist_ok=True)
                _write_csv(os.path.join(qdir, f"{qt}.csv"),
                           _rps_qtype_rows(qt, r, rates, sched_p99, svc_p99, mean_page))
                _write_csv(os.path.join(qdir, "driver.csv"),
                           _rps_driver_rows(tier, qt, r, rates))
                _write_invocation(qdir, f"bench-query {tier}", close_interval)

    campaign = {
        "name": "rps-query-load", "config_file": "rps-query-load.toml",
        "ref": "", "built_commit": COMMIT,
        "ingest": "both", "query": "yes", "close_interval": close_interval,
        "runs": reps, "query_duration": "60s",
        "workers": 1, "hot_num_ledgers": 100,
    }
    if query_phase:
        # omitempty: a campaign with no query legs resolves no phase, and a
        # recorded 0 would read as one.
        campaign["query_phase"] = query_phase
    metadata = {
        "schema_version": 1,
        "run_id": f"rps-{dataset}-b32bc9be-20260826T010000Z",
        "campaign": campaign,
        "datasets": [{"name": dataset, "kind": "packs-local",
                      "location": f"/data/{dataset}/packs/cold", "chunks": [1]}],
        "hardware": {
            "instance_type": "m6id.2xlarge", "instance_id": "i-0123456789abcdef0",
            "uname": "Linux 6.8.0-1015-aws x86_64", "cpus": 8, "mem_total_kb": 32000000,
        },
        "hostname": "user-dev-063a",
        "started_at": "2026-08-26T01:00:00Z",
        "finished_at": "2026-08-26T09:30:00Z",
    }
    with open(os.path.join(root, "metadata.json"), "w") as f:
        json.dump(metadata, f, indent=2)
    with open(os.path.join(root, "machine-metadata.txt"), "w") as f:
        f.write(MACHINE_META.format(commit="0" * 40))
    return root


def build_legacy_bundle(root, reps=2):
    """Manifest-less pubnet bundle (old vocabulary) — today's conversion path."""
    os.makedirs(root, exist_ok=True)
    for r in range(1, reps + 1):
        cdir = os.path.join(root, f"ingest-cold-3000-run{r}")
        os.makedirs(cdir, exist_ok=True)
        _write_csv(os.path.join(cdir, "driver.csv"), _cold_driver("sac-6000-c1", r, vocab="old"))
        for name, rows in _cold_files("sac-6000-c1", r).items():
            _write_csv(os.path.join(cdir, f"{name}.csv"), rows)
        hdir = os.path.join(root, f"ingest-hot-3000-run{r}")
        os.makedirs(hdir, exist_ok=True)
        _write_csv(os.path.join(hdir, "driver.csv"), _hot_driver("sac-6000-c1", r, paced=False, vocab="old"))
        _write_csv(os.path.join(hdir, "hot.csv"), _hot_phases("sac-6000-c1", r))
    gdir = os.path.join(root, "golden-download-3000")
    os.makedirs(gdir, exist_ok=True)
    _write_csv(os.path.join(gdir, "driver.csv"),
               [("chunk_wall", 1, 0, 12000, 12000, 12000, 12000, 12000)])
    with open(os.path.join(root, "machine-metadata.txt"), "w") as f:
        f.write(MACHINE_META.format(commit="a" * 40))
    return root


if __name__ == "__main__":
    import sys
    kind = sys.argv[1] if len(sys.argv) > 1 else "campaign-paced"
    dest = sys.argv[2]
    if kind == "campaign-paced":
        build_campaign_bundle(dest, paced=True)
    elif kind == "campaign-unpaced":
        build_campaign_bundle(dest, paced=False)
    elif kind == "rps":
        build_rps_bundle(dest)
    elif kind == "legacy":
        build_legacy_bundle(dest)
    else:
        raise SystemExit(f"unknown fixture kind {kind!r}")
    print(dest)
