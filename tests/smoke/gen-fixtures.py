#!/usr/bin/env python3
"""Generate the viewer smoke test's synthetic run JSONs. Python 3 stdlib only.

Every fixture here is the OPEN-LOOP (paced-RPS) query shape — one leg dir per
query type, one cell per target rate — in a variation no committed run carries:

  fixture-rps-phase3     two profiles, phase 3, mixed 1x verdicts.
  fixture-rps-unfloored  one profile, paced at an interval that matches no
                         phase and with no query_phase in the manifest, so the
                         converter emits r-cells with NO verdict_1x and no
                         campaign.phase — the viewer must judge the rows on the
                         latency target alone and say so.
  fixture-rps-partial    two profiles, phase 3, first profile's COLD query legs
                         removed before conversion. The grid must still be
                         discovered from the units that do have cold legs
                         (docs/app.js queryGrid), and every consumer must render
                         the gap rather than throw.

Each is built as a real results bundle in a tempdir and pushed through the real
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
import re
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
FIXTURE_DATE = "2026-08-27"

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


def ladder(profile, qt, phase):
    """This profile's floor for `phase` times the 0.5/1/2 ladder, as rate tokens.
    Both come from docs/targets.json, so the middle rung IS the judged 1x cell
    whenever the run has a goal phase to judge it against."""
    floors = C.QUERY_LOAD["profiles"][profile][C._QTYPE_RPS_KEY[qt]]
    return [rate_token(floors[phase - 1] * x) for x in C.QUERY_LOAD["ladder"]]


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


def dataset_entry(unit):
    """The metadata.json datasets entry for a campaign unit id (<name>-c<chunk>)."""
    name, chunk = re.match(r"^(.+)-c(\d+)$", unit).groups()
    return {"name": name, "kind": "packs-local",
            "location": f"/data/{name}/packs/cold", "chunks": [int(chunk)]}


def build_bundle(root, spec):
    """One campaign bundle: ingest cold + hot legs from the converter's own
    fixture builders, query legs in the per-qtype RPS shape.

    A unit named in `drop_cold_query` gets no cold query legs at all — the
    partial-coverage shape a failed or skipped cold sweep leaves behind."""
    units, reps = spec["units"], spec["reps"]
    close, no_cold_q = spec["close_interval"], set(spec.get("drop_cold_query", ()))
    for u in units:
        profile = C.query_profile(u)
        for r in range(1, reps + 1):
            cdir = os.path.join(root, f"ingest-cold-{u}-run{r}")
            os.makedirs(cdir, exist_ok=True)
            F._write_csv(os.path.join(cdir, "driver.csv"), F._cold_driver(u, r))
            for name, rows in F._cold_files(u, r).items():
                F._write_csv(os.path.join(cdir, f"{name}.csv"), rows)
            F._write_invocation(cdir, "bench-ingest cold", close)

            hdir = os.path.join(root, f"ingest-hot-{u}-run{r}")
            os.makedirs(hdir, exist_ok=True)
            F._write_csv(os.path.join(hdir, "driver.csv"), F._hot_driver(u, r, paced=True))
            F._write_csv(os.path.join(hdir, "hot.csv"), F._hot_phases(u, r))
            F._write_invocation(hdir, "bench-ingest hot", close)

            for tier in ("cold", "hot"):
                if tier == "cold" and u in no_cold_q:
                    continue
                for qt in QTYPES:
                    tokens = ladder(profile, qt, spec["ladder_phase"])
                    qdir = os.path.join(root, f"query-{tier}-{u}-{qt}-run{r}")
                    os.makedirs(qdir, exist_ok=True)
                    F._write_csv(os.path.join(qdir, f"{qt}.csv"),
                                 qtype_rows(qt, tokens, tier, profile, r))
                    F._write_csv(os.path.join(qdir, "driver.csv"),
                                 driver_rows(qt, tokens, tier, profile, r))
                    F._write_invocation(qdir, f"bench-query {tier}", close)

    metadata = {
        "schema_version": 1,
        "run_id": spec["id"],
        "campaign": {
            "name": spec["name"], "config_file": spec["id"] + ".toml",
            "ref": "", "built_commit": F.COMMIT,
            "ingest": "both", "query": "yes", "close_interval": close,
            "runs": reps, "query_rps_ladder": ",".join(str(x) for x in C.QUERY_LOAD["ladder"]),
        },
        "datasets": [dataset_entry(u) for u in units],
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


def qtype_entries(D):
    """Every (tier, unit, qtype, entry) the run recorded, gaps and all."""
    for tier, units in D["queries"].items():
        for unit, entry in units.items():
            for qt, qout in entry.items():
                if qt != "setup":
                    yield tier, unit, qt, qout


def verdict_failures(D):
    """Every (tier, unit, qtype) whose 1x verdict breaches some budget, with the
    axis that broke — the run's whole verdict picture in one comparable set."""
    out = set()
    for tier, unit, qt, qout in qtype_entries(D):
        v = qout["verdict_1x"]
        for axis, ok in [("p99", v["pass"]),
                         ("in_rpc", v.get("in_rpc", {}).get("pass", True)),
                         ("page_budget", v.get("page_budget", {}).get("pass", True))]:
            if not ok:
                out.add((tier, unit, qt, axis))
    return out


def check_common(D, spec):
    """What every fixture must carry: the unit list, per-unit counts, and a full
    ladder of well-formed rate cells wherever a (tier, unit, qtype) exists."""
    assert D["dataset"]["unit_order"] == spec["units"], D["dataset"]["unit_order"]
    for u in spec["units"]:
        meta = D["dataset"]["unit_meta"][u]
        assert all(k in meta for k in ("ledgers", "txs", "events")), meta
    assert D["campaign"]["query_load"]["profiles"], "campaign.query_load missing"
    want = {"target_rps", "achieved_rps", "service", "dispatch_lag", "shed"}
    for tier, unit, qt, qout in qtype_entries(D):
        cells = rate_cells(qout)
        assert len(cells) == len(C.QUERY_LOAD["ladder"]), (tier, unit, qt, list(cells))
        for key, cell in cells.items():
            missing = want - set(cell)
            assert not missing, (tier, unit, qt, key, sorted(missing))


def check_phase3(D, spec):
    """Pin the shape and the verdicts the smoke test is here to exercise. Any
    drift in the converter or in targets.json breaks generation, not the viewer."""
    assert D["campaign"]["phase"] == spec["phase"], D["campaign"].get("phase")
    for tier, unit, qt, qout in qtype_entries(D):
        v = qout.get("verdict_1x")
        assert v, f"no verdict_1x on {tier}/{unit}/{qt}"
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


def check_unfloored(D, spec):
    """No phase means no floor: every rate cell still converts, and not one of
    them earns a verdict_1x for the viewer to judge against."""
    assert D["campaign"].get("phase") is None, D["campaign"].get("phase")
    assert D["campaign"]["close_interval_ns"] > 0, "the fixture is paced, just off-phase"
    for tier, unit, qt, qout in qtype_entries(D):
        assert "verdict_1x" not in qout, f"unexpected verdict on {tier}/{unit}/{qt}"
    # The viewer labels rungs by multiplier off the campaign ladder when no floor
    # names them, so the two lengths have to agree.
    assert len(C.QUERY_LOAD["ladder"]) == len(
        rate_cells(D["queries"]["hot"][spec["units"][0]]["ledgers"])), "ladder length"
    assert (D["checks"] or {}).get("applies_to"), "a paced run still earns a check"


def check_partial(D, spec):
    """The first unit has hot query legs only; the second has both tiers. The
    grid the viewer discovers has to come from the union, not from unit one."""
    first, second = spec["units"][0], spec["units"][1]
    assert first not in D["queries"]["cold"], "cold legs were meant to be dropped"
    assert second in D["queries"]["cold"], sorted(D["queries"]["cold"])
    assert first in D["queries"]["hot"] and second in D["queries"]["hot"], \
        sorted(D["queries"]["hot"])
    assert D["campaign"]["phase"] == spec["phase"], D["campaign"].get("phase")
    for qt in QTYPES:
        assert D["queries"]["hot"][first][qt].get("verdict_1x"), qt
        assert D["queries"]["cold"][second][qt].get("verdict_1x"), qt


SPECS = [
    {
        "id": "fixture-rps-phase3",
        "name": "Fixture — open-loop query load (phase 3)",
        # Two profiles: enough to exercise the profile picker, and soroswap's
        # floors are the ones that produce fractional rate tokens (r7.5, r30).
        "units": ["sac-6000-c1", "soroswap-1500-c1"],
        "reps": 2,
        "close_interval": "600ms",   # phase 3's block time
        "phase": 3,
        "ladder_phase": 3,
        "check": check_phase3,
    },
    {
        "id": "fixture-rps-unfloored",
        "name": "Fixture — open-loop query load, no phase floor",
        "units": ["sac-6000-c1"],
        "reps": 1,
        "close_interval": "450ms",   # matches no phase, and no query_phase either
        "phase": None,
        "ladder_phase": 3,
        "check": check_unfloored,
        # The converter says so itself when it drops the verdicts; that warning
        # IS the shape under test.
        "allow_warnings": [r"^no phase from the close interval"],
    },
    {
        "id": "fixture-rps-partial",
        # The name reaches the public summary through the masthead, so it stays
        # in the vocabulary that page allows — no tier word.
        "name": "Fixture — open-loop query load, one tier missing on the first profile",
        "units": ["sac-6000-c1", "soroswap-1500-c1"],
        "reps": 1,
        "close_interval": "600ms",
        "phase": 3,
        "ladder_phase": 3,
        "drop_cold_query": ["sac-6000-c1"],
        "check": check_partial,
        "allow_warnings": [r"^missing run dir: query-cold-sac-6000-c1-run\d+$"],
    },
]


def generate(spec, out_dir):
    with tempfile.TemporaryDirectory(prefix="rps-fixture-") as tmp:
        bundle = build_bundle(os.path.join(tmp, "results"), spec)
        # convert() rewrites index.json beside its output: convert into the
        # tempdir and copy only the run JSON out.
        staging = os.path.join(tmp, "out")
        # Warnings accumulate in a module global across conversions.
        C._warnings.clear()
        # convert() narrates the paths it wrote, all of them inside the tempdir —
        # swallow that and report what actually landed instead.
        with contextlib.redirect_stdout(io.StringIO()):
            D = C.convert(argparse.Namespace(
                results_dir=bundle, run_id=spec["id"], run_name=spec["name"],
                run_date=FIXTURE_DATE, dataset_kind="synthetic", unit_facts=None,
                source_gcs=None, source_uri=None, notes=None, description=None,
                out_dir=staging))
        # Only the warnings this fixture is built to provoke are tolerated; any
        # other one means the bundle is missing rows.
        allowed = [re.compile(p) for p in spec.get("allow_warnings", [])]
        stray = [w for w in C._warnings if not any(p.search(w) for p in allowed)]
        if stray:
            raise SystemExit(f"converter warnings on {spec['id']} "
                             "(the bundle is missing rows):\n  " + "\n  ".join(stray))
        check_common(D, spec)
        spec["check"](D, spec)
        dest = os.path.join(out_dir, f"{spec['id']}.json")
        shutil.copyfile(os.path.join(staging, f"{spec['id']}.json"), dest)
    return D, dest


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out-dir", default=OUT_DIR)
    args = ap.parse_args()

    os.makedirs(args.out_dir, exist_ok=True)
    # Generated output is scratch: it must never be committed, and never reach
    # docs/runs/ (which is the published site).
    with open(os.path.join(args.out_dir, ".gitignore"), "w") as f:
        f.write("# Generated by tests/smoke/gen-fixtures.py — never committed.\n*.json\n")

    for spec in SPECS:
        D, dest = generate(spec, args.out_dir)
        print(f"wrote {dest} ({os.path.getsize(dest) / 1024:.0f} KiB)")
        print(f"  phase={D['campaign'].get('phase')} units={spec['units']} "
              f"reps={spec['reps']} qtypes={QTYPES}")
        print(f"  1x breaches: {sorted(verdict_failures(D)) if spec['phase'] else 'n/a (no floor)'}")


if __name__ == "__main__":
    main()
