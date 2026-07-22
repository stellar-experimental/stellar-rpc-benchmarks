"""Fixture-bundle builders for converter tests. Python 3 stdlib only.

Materialise a benchmark results bundle onto disk so both the unit tests and the
end-to-end / viewer checks convert a real bundle rather than a mock:

  build_campaign_bundle  — campaign.sh layout (<family>-<dataset>-c<chunk>-run<R>)
                           with metadata.json at the root + invocation.json in
                           every timed dir, paced or unpaced, plus golden-* prep
                           dirs that must be skipped.
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


def _write_invocation(d, subcommand, close_interval):
    flags = {"out": d, "num-ledgers": "100", "source": "pack"}
    if subcommand.endswith("hot"):
        flags["close-interval"] = close_interval
    inv = {
        "schema_version": 1,
        "command": "stellar-rpc " + subcommand,
        "flags": flags,
        "binary": {"version": VERSION, "commit_hash": COMMIT,
                   "build_timestamp": "2026-07-22T00:10:00", "branch": BRANCH},
        "hostname": "user-dev-063a",
        "started_at": "2026-07-22T01:00:00Z",
        "finished_at": "2026-07-22T03:47:12Z",
    }
    with open(os.path.join(d, "invocation.json"), "w") as f:
        json.dump(inv, f, indent=2)


def build_campaign_bundle(root, paced=True, reps=2, machine_commit="0" * 40,
                          drop_pace=None):
    """Create a campaign-layout bundle under `root`. `drop_pace` = {unit: {runs}}
    omits the pace_lag row from those hot runs (to exercise zero-fill)."""
    os.makedirs(root, exist_ok=True)
    drop_pace = drop_pace or {}
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
            "ingest": "both", "query": "no", "close_interval": close_interval,
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
    elif kind == "legacy":
        build_legacy_bundle(dest)
    else:
        raise SystemExit(f"unknown fixture kind {kind!r}")
    print(dest)
