#!/usr/bin/env python3
"""Generate the viewer smoke test's synthetic run JSONs. Python 3 stdlib only.

One fixture today: a phase-3 campaign whose query legs are the OPEN-LOOP
(paced-RPS) shape — one leg dir per query type, one cell per target rate. It is
built as a real results bundle in a tempdir and pushed through the real
`convert()`, so the smoke test loads exactly what the converter emits, not a
hand-written JSON that can drift from it.

The output is scratch, never data: it lands in tests/smoke/fixtures/ (gitignored)
and NEVER in docs/runs/, which is the live site. `convert()` also rewrites the
manifest beside its output, so the conversion goes to a throwaway out-dir and
only the run JSON is copied across — the manifest is left behind with it.

Latencies are chosen so the phase-3 verdicts are MIXED and deterministic: three
named cells breach one budget each (see OVERRIDES), everything else passes.
"""
import argparse
import contextlib
import io
import json
import os
import shutil
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
sys.path.insert(0, os.path.join(ROOT, "converter"))
sys.path.insert(0, os.path.join(ROOT, "converter", "tests"))

import convert as C          # noqa: E402
import fixtures as F         # noqa: E402

OUT_DIR = os.path.join(HERE, "fixtures")
FIXTURE_ID = "fixture-rps-phase3"
FIXTURE_NAME = "Fixture — open-loop query load (phase 3)"
FIXTURE_DATE = "2026-08-27"

PHASE = 3                    # the close interval below is phase 3's block time
CLOSE_INTERVAL = "600ms"
REPS = 2
# Two profiles: enough to exercise the profile picker, and soroswap's floors are
# the ones that produce fractional rate tokens (r7.5, r30).
UNITS = ["sac-6000-c1", "soroswap-1500-c1"]
QTYPES = ["ledgers", "txpage", "txhash", "events"]

ANSWERED = 100               # answered requests per leg
EVENTS_ITEMS_PER_REQ = 10    # events pages carry ten records each
SHED_TOP = 5                 # dropped at the in-flight cap on the 2x rung

# Per-rung latencies in MILLISECONDS, by tier: [0.5x, 1x, 2x]. `sched` is the
# scheduled p99 (due->done, the headline), `svc` the service p99 (dispatch->done,
# getTransaction's in-RPC budget), `page` the mean getEvents page. Cold pays more
# than hot everywhere, and the 2x rung sits past the knee.
BASE = {
    "cold": {"sched": [90, 180, 460], "svc": [3.0, 5.0, 9.5], "page": [35, 55, 95]},
    "hot":  {"sched": [60, 120, 380], "svc": [2.0, 4.0, 9.0], "page": [25, 40, 80]},
}
# The mixed verdicts, one breach per line. `achieved` is the fraction of the
# target rate actually served (None = keeps up); `lag_p99` is the dispatch lag.
OVERRIDES = {
    # 1x cells: one breached budget each, the other axes still clear.
    ("cold", "sac", "txpage", 1): {"sched": 640},                    # headline 500 ms
    ("hot", "sac", "txhash", 1): {"sched": 130, "svc": 14.0},        # in-RPC 10 ms
    ("cold", "soroswap", "events", 1): {"sched": 150, "page": 88},   # mean page 67 ms
    # 2x saturation: sac's txhash rung cannot be served at 2000 rps. Cold cannot
    # be faster than hot at the same rate, so both tiers saturate together.
    ("cold", "sac", "txhash", 2): {"sched": 1400, "achieved": 0.60, "shed": 140, "lag_p99": 900},
    ("hot", "sac", "txhash", 2): {"sched": 1400, "achieved": 0.60, "shed": 140, "lag_p99": 900},
}


def _ns(ms):
    return int(round(ms * 1e6))


def rate_token(v):
    """A rate spelled the way Go's strconv.FormatFloat(v,'f',-1,64) spells it —
    the bundle's own spelling, which the converter matches to a floor by value."""
    return str(int(v)) if float(v).is_integer() else repr(float(v))


def ladder(profile, qt):
    """This profile's phase-3 floor times the 0.5/1/2 ladder, as rate tokens.
    Both come from docs/targets.json, so the middle rung IS the judged 1x cell."""
    floors = C.QUERY_LOAD["profiles"][profile][C._QTYPE_RPS_KEY[qt]]
    return [rate_token(floors[PHASE - 1] * x) for x in C.QUERY_LOAD["ladder"]]


def cell_spec(tier, profile, qt, i):
    spec = {"sched": BASE[tier]["sched"][i], "svc": BASE[tier]["svc"][i],
            "page": BASE[tier]["page"][i], "achieved": None,
            "shed": SHED_TOP if i == len(C.QUERY_LOAD["ladder"]) - 1 else 0,
            "lag_p99": 3.0}
    spec.update(OVERRIDES.get((tier, profile, qt, i), {}))
    return spec


def qtype_rows(qt, tokens, tier, profile, run):
    """<qtype>.csv: a scheduled row and a service row per target rate."""
    rows = []
    for i, tok in enumerate(tokens):
        c = cell_spec(tier, profile, qt, i)
        n = ANSWERED
        items = n * EVENTS_ITEMS_PER_REQ if qt == "events" else n
        s, v = _ns(c["sched"]), _ns(c["svc"])
        # getEvents' mean page is a judged budget, so it is set outright; every
        # other type gets a plausible mean under its own service p99.
        mean = _ns(c["page"]) if qt == "events" else v // 2
        rows.append((f"total_r{tok}",) + F._scale(
            (n, items, n * s // 2, s // 4, s // 2, s, s * 2), run))
        rows.append((f"service_r{tok}",) + F._scale(
            (n, items, n * mean, v // 4, v // 2, v, v * 2), run))
        if qt == "txhash":
            # found/miss sub-stage splits ride along; the converter matches
            # total_ alone and ignores them.
            rows.append((f"found_r{tok}",) + F._scale(
                (n - 1, n - 1, n * s // 3, s // 4, s // 2, s, s * 2), run))
            rows.append((f"miss_r{tok}",) + F._scale((1, 1, s, s, s, s, s), run))
    return rows


def driver_rows(qt, tokens, tier, profile, run):
    """driver.csv: the setup rows plus four rows per rate cell."""
    rows = [("open", 1, 1) + (60_000_000,) * 5]
    if tier == "cold":
        rows.append(("evict", len(tokens), 6, 4000, 500, 600, 700, 700))
    for i, tok in enumerate(tokens):
        c = cell_spec(tier, profile, qt, i)
        n, lag, shed = ANSWERED, _ns(c["lag_p99"]), c["shed"]
        # The achieved rate is carried x1000 as an int in the duration columns.
        # A keeping-up leg lands a hair under target; a saturated one lands far
        # below it.
        milli = (int(round(float(tok) * 1000 * c["achieved"])) if c["achieved"]
                 else int(round(float(tok) * 1000)) - 2)
        wall = int(n / (milli / 1000) * 1e9)
        rows.append((f"{qt}_r{tok}",) + F._scale((1, n, wall, 0, 0, 0, 0), run))
        # Counts, not durations: they read the same in every rep.
        rows.append((f"{qt}_r{tok}_millirps", 1, 0) + (milli,) * 5)
        rows.append((f"{qt}_r{tok}_lag",) + F._scale(
            (n + shed, n + shed, n * lag // 4, 0, lag // 3, lag, lag * 3), run))
        rows.append((f"{qt}_r{tok}_shed", shed, shed, 0, 0, 0, 0, 0))
    rows.append(("peak_rss_bytes", 1, 0) + (900_000_000,) * 5)
    return rows


def build_bundle(root):
    """A two-unit phase-3 campaign bundle: ingest cold + hot legs from the
    converter's own fixture builders, query legs in the per-qtype RPS shape."""
    for u in UNITS:
        profile = C.query_profile(u)
        for r in range(1, REPS + 1):
            cdir = os.path.join(root, f"ingest-cold-{u}-run{r}")
            os.makedirs(cdir, exist_ok=True)
            F._write_csv(os.path.join(cdir, "driver.csv"), F._cold_driver(u, r))
            for name, rows in F._cold_files(u, r).items():
                F._write_csv(os.path.join(cdir, f"{name}.csv"), rows)
            F._write_invocation(cdir, "bench-ingest cold", CLOSE_INTERVAL)

            hdir = os.path.join(root, f"ingest-hot-{u}-run{r}")
            os.makedirs(hdir, exist_ok=True)
            F._write_csv(os.path.join(hdir, "driver.csv"), F._hot_driver(u, r, paced=True))
            F._write_csv(os.path.join(hdir, "hot.csv"), F._hot_phases(u, r))
            F._write_invocation(hdir, "bench-ingest hot", CLOSE_INTERVAL)

            for tier in ("cold", "hot"):
                for qt in QTYPES:
                    tokens = ladder(profile, qt)
                    qdir = os.path.join(root, f"query-{tier}-{u}-{qt}-run{r}")
                    os.makedirs(qdir, exist_ok=True)
                    F._write_csv(os.path.join(qdir, f"{qt}.csv"),
                                 qtype_rows(qt, tokens, tier, profile, r))
                    F._write_csv(os.path.join(qdir, "driver.csv"),
                                 driver_rows(qt, tokens, tier, profile, r))
                    F._write_invocation(qdir, f"bench-query {tier}", CLOSE_INTERVAL)

    metadata = {
        "schema_version": 1,
        "run_id": FIXTURE_ID,
        "campaign": {
            "name": FIXTURE_NAME, "config_file": "fixture-rps-phase3.toml",
            "ref": "", "built_commit": F.COMMIT,
            "ingest": "both", "query": "yes", "close_interval": CLOSE_INTERVAL,
            "runs": REPS, "query_rps_ladder": ",".join(str(x) for x in C.QUERY_LOAD["ladder"]),
        },
        "datasets": [
            {"name": "sac-6000", "kind": "packs-local",
             "location": "/data/sac-6000/packs/cold", "chunks": [1]},
            {"name": "soroswap-1500", "kind": "packs-local",
             "location": "/data/soroswap-1500/packs/cold", "chunks": [1]},
        ],
        "hardware": {
            "instance_type": "m6id.2xlarge", "instance_id": "i-0123456789abcdef0",
            "uname": "Linux 6.8.0-1015-aws x86_64", "cpus": 8, "mem_total_kb": 32000000,
        },
        "hostname": "user-dev-063a",
        "started_at": FIXTURE_DATE + "T01:00:00Z",
        "finished_at": FIXTURE_DATE + "T09:30:00Z",
    }
    with open(os.path.join(root, "metadata.json"), "w") as f:
        json.dump(metadata, f, indent=2)
    with open(os.path.join(root, "machine-metadata.txt"), "w") as f:
        f.write(F.MACHINE_META.format(commit="0" * 40))
    return root


def rate_cells(qout):
    """The r<rate> cells of one qtype entry. verdict_1x is a sibling of theirs
    that also carries a target_rps, so it is named out rather than sniffed."""
    return {k: v for k, v in qout.items()
            if k != "verdict_1x" and isinstance(v, dict) and "target_rps" in v}


def verdict_failures(D):
    """Every (tier, unit, qtype) whose 1x verdict breaches some budget, with the
    axis that broke — the run's whole verdict picture in one comparable set."""
    out = set()
    for tier, units in D["queries"].items():
        for unit, entry in units.items():
            for qt, qout in entry.items():
                if qt == "setup":
                    continue
                v = qout["verdict_1x"]
                for axis, ok in [("p99", v["pass"]),
                                 ("in_rpc", v.get("in_rpc", {}).get("pass", True)),
                                 ("page_budget", v.get("page_budget", {}).get("pass", True))]:
                    if not ok:
                        out.add((tier, unit, qt, axis))
    return out


def check(D):
    """Pin the shape and the verdicts the smoke test is here to exercise. Any
    drift in the converter or in targets.json breaks generation, not the viewer."""
    assert D["campaign"]["phase"] == PHASE, D["campaign"].get("phase")
    assert D["campaign"]["query_load"]["profiles"], "campaign.query_load missing"
    assert D["dataset"]["unit_order"] == UNITS, D["dataset"]["unit_order"]
    for u in UNITS:
        meta = D["dataset"]["unit_meta"][u]
        assert all(k in meta for k in ("ledgers", "txs", "events")), meta

    want = {"target_rps", "achieved_rps", "service", "dispatch_lag", "shed"}
    for tier in ("cold", "hot"):
        for u in UNITS:
            for qt in QTYPES:
                qout = D["queries"][tier][u][qt]
                cells = rate_cells(qout)
                assert len(cells) == len(C.QUERY_LOAD["ladder"]), (tier, u, qt, list(cells))
                for key, cell in cells.items():
                    missing = want - set(cell)
                    assert not missing, (tier, u, qt, key, sorted(missing))
                v = qout.get("verdict_1x")
                assert v, f"no verdict_1x on {tier}/{u}/{qt}"
                assert v["target_rps"] == float(v["rate"][1:]), v

    # The three designed breaches, and nothing else.
    expected = {
        ("cold", "sac-6000-c1", "txpage", "p99"),
        ("hot", "sac-6000-c1", "txhash", "in_rpc"),
        ("cold", "soroswap-1500-c1", "events", "page_budget"),
    }
    got = verdict_failures(D)
    assert got == expected, f"verdict drift: {sorted(got)}"

    # Saturation: the 2x sac txhash cell is served far below its target rate.
    for tier in ("cold", "hot"):
        cell = rate_cells(D["queries"][tier]["sac-6000-c1"]["txhash"])["r2000"]
        assert cell["achieved_rps"]["m"] < 0.8 * cell["target_rps"], cell["achieved_rps"]
        knee = rate_cells(D["queries"][tier]["sac-6000-c1"]["txhash"])["r1000"]
        assert cell["p99"]["m"] > 2 * knee["p99"]["m"], (cell["p99"], knee["p99"])


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out-dir", default=OUT_DIR)
    args = ap.parse_args()

    os.makedirs(args.out_dir, exist_ok=True)
    # Generated output is scratch: it must never be committed, and never reach
    # docs/runs/ (which is the published site).
    with open(os.path.join(args.out_dir, ".gitignore"), "w") as f:
        f.write("# Generated by tests/smoke/gen-fixtures.py — never committed.\n*.json\n")

    with tempfile.TemporaryDirectory(prefix="rps-fixture-") as tmp:
        bundle = build_bundle(os.path.join(tmp, "results"))
        # convert() rewrites index.json beside its output: convert into the
        # tempdir and copy only the run JSON out.
        staging = os.path.join(tmp, "out")
        # convert() narrates the paths it wrote, all of them inside the tempdir —
        # swallow that and report what actually landed instead.
        with contextlib.redirect_stdout(io.StringIO()):
            D = C.convert(argparse.Namespace(
                results_dir=bundle, run_id=FIXTURE_ID, run_name=FIXTURE_NAME,
                run_date=FIXTURE_DATE, dataset_kind="synthetic", unit_facts=None,
                source_gcs=None, source_uri=None, notes=None, description=None,
                out_dir=staging))
        if C._warnings:
            raise SystemExit("converter warnings (the bundle is missing rows):\n  "
                             + "\n  ".join(C._warnings))
        check(D)
        dest = os.path.join(args.out_dir, f"{FIXTURE_ID}.json")
        shutil.copyfile(os.path.join(staging, f"{FIXTURE_ID}.json"), dest)

    print(f"wrote {dest} ({os.path.getsize(dest) / 1024:.0f} KiB)")
    print(f"  phase={PHASE} units={UNITS} reps={REPS} qtypes={QTYPES}")
    print(f"  1x breaches: {sorted(verdict_failures(D))}")


if __name__ == "__main__":
    main()
