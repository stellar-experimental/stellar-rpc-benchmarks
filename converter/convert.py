#!/usr/bin/env python3
"""Convert a raw Stellar-RPC benchmark results directory into one schema-v1 run
JSON and update the manifest (docs/runs/index.json).

Two directory layouts and two CSV "vocabularies" are supported and auto-detected:

  layout    (from dir names): pubnet (ingest-*/golden-download-*) | synthetic (synth-*)
  vocabulary (from cold driver.csv rows): old (chunk_wall) | new (backfill_wall)

Every reported number is the median across the reps, kept alongside min/max and
the raw per-run array (see SCHEMA.md "Statistical conventions"). Nothing is
interpolated; every value traces to a raw CSV field. Derived rates are computed
per-run then aggregated.

Python 3 stdlib only.
"""
import argparse
import csv
import glob
import json
import numbers
import os
import re
import statistics
import sys

NS = 1e9
PCT_MAP = (("total_ns", "total"), ("p50_ns", "p50"), ("p90_ns", "p90"),
           ("p99_ns", "p99"), ("max_ns", "max"))
SECTION_ORDER = ["ingest_cold", "ingest_hot", "queries", "golden"]

_warnings = []


def warn(msg):
    _warnings.append(msg)
    print("WARN:", msg, file=sys.stderr)


def fail(msg):
    print("ERROR:", msg, file=sys.stderr)
    sys.exit(1)


# ------------------------------------------------------------- Go duration
_GODUR_RE = re.compile(r"(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)")
_GODUR_UNITS = {"ns": 1, "us": 1_000, "µs": 1_000, "ms": 1_000_000,
                "s": 1_000_000_000, "m": 60_000_000_000, "h": 3_600_000_000_000}


def parse_go_duration(s):
    """Parse a Go duration string to integer nanoseconds.

    Handles ns/us/µs/ms/s/m/h units, decimal magnitudes, an optional sign, and
    concatenated terms ("1h30m"). Bare "0" is zero. Raises ValueError on an
    unparseable non-zero string (caller decides whether to warn or fail).
    """
    t = (s or "").strip()
    if t in ("", "0"):
        return 0
    sign = 1
    if t[:1] in "+-":
        sign = -1 if t[0] == "-" else 1
        t = t[1:]
    total, pos = 0.0, 0
    for m in _GODUR_RE.finditer(t):
        if m.start() != pos:
            raise ValueError(f"bad Go duration: {s!r}")
        total += float(m.group(1)) * _GODUR_UNITS[m.group(2)]
        pos = m.end()
    if pos == 0 or pos != len(t):
        raise ValueError(f"bad Go duration: {s!r}")
    return sign * int(round(total))


# ----------------------------------------------------------------- CSV + stats
def read_csv(path):
    """Return {stage: {col: int}} for a benchmark CSV (all non-stage cols int)."""
    out = {}
    with open(path) as f:
        for row in csv.DictReader(f):
            out[row["stage"]] = {k: int(v) for k, v in row.items() if k != "stage"}
    return out


def stat(values):
    """V = median / min / max / raw per-run array.

    median_low so `m` is always a real observed sample, even if a rep is
    missing and the count is even (plain median would interpolate).
    """
    vals = list(values)
    return {"m": statistics.median_low(vals), "lo": min(vals), "hi": max(vals), "r": vals}


def stage_agg(runs_rows, stage, warn_nitems=True):
    """StageAgg for one stage across runs. runs_rows: list of {stage: {col:int}}."""
    present = [r[stage] for r in runs_rows if stage in r]
    if not present:
        raise KeyError(stage)
    if len(present) != len(runs_rows):
        warn(f"stage {stage}: only {len(present)}/{len(runs_rows)} runs have it")
    out = {}
    for csv_key, out_key in PCT_MAP:
        out[out_key] = stat([r[csv_key] for r in present])
    if len({r["n"] for r in present}) > 1:
        warn(f"stage {stage}: n varies across runs: {sorted({r['n'] for r in present})}")
    out["n"] = present[0]["n"]
    nitems = {r["n_items"] for r in present}
    if warn_nitems and len(nitems) > 1:
        warn(f"stage {stage}: n_items varies across runs: {sorted(nitems)}")
    out["n_items"] = present[0]["n_items"]
    return out


# ----------------------------------------------------------------- discovery
def subdirs(results_dir):
    return sorted(n for n in os.listdir(results_dir)
                  if os.path.isdir(os.path.join(results_dir, n)))


# A campaign bundle (from campaign.sh) names its timed dirs
# <family>-<dataset>-c<chunk>-run<R> and its untimed prep dirs golden-<dataset>-c<chunk>.
# The -c<chunk>-run<R> suffix distinguishes it from the flat pubnet ingest-*-run<R>.
_CAMPAIGN_RUN = re.compile(r"^(?:ingest|query)-(?:cold|hot)-.+-c\d+-run\d+$")
_CAMPAIGN_GOLDEN = re.compile(r"^golden-.+-c\d+$")
_UNIT_CHUNK = re.compile(r"^(.+)-c(\d+)$")


def detect_layout(names):
    if any(n.startswith("synth-") for n in names):
        return "synthetic"
    if any(_CAMPAIGN_RUN.match(n) for n in names):
        return "campaign"
    if any(n.startswith("ingest-") or n.startswith("golden-download-") for n in names):
        return "pubnet"
    return None


def discover_units_reps(names, layout):
    """Discover unit ids and rep numbers from the cold ingest dir names.

    A campaign unit id is the composite "<dataset>-c<chunk>" (e.g. sac-6000-c1);
    pubnet/synthetic unit ids are the bare token between family and run.
    """
    if layout == "synthetic":
        pat = re.compile(r"^synth-cold-(.+)-run(\d+)$")
    elif layout == "campaign":
        pat = re.compile(r"^ingest-cold-(.+-c\d+)-run(\d+)$")
    else:
        pat = re.compile(r"^ingest-cold-(.+)-run(\d+)$")
    units, reps = set(), set()
    for n in names:
        m = pat.match(n)
        if m:
            units.add(m.group(1))
            reps.add(int(m.group(2)))
    return units, sorted(reps)


def cold_dir(results_dir, layout, unit, rep):
    stem = "synth-cold" if layout == "synthetic" else "ingest-cold"
    return os.path.join(results_dir, f"{stem}-{unit}-run{rep}")


def hot_dir(results_dir, layout, unit, rep):
    stem = "synth-hot" if layout == "synthetic" else "ingest-hot"
    return os.path.join(results_dir, f"{stem}-{unit}-run{rep}")


def run_dirs(results_dir, layout, kind, unit, reps):
    """Existing run dirs for a (kind, unit); warns about missing reps."""
    if kind == "cold":
        make = cold_dir
    elif kind == "hot":
        make = hot_dir
    else:  # query-cold / query-hot
        tier = kind.split("-", 1)[1]
        make = lambda rd, ly, u, r: os.path.join(rd, f"query-{tier}-{u}-run{r}")
    out = []
    for r in reps:
        d = make(results_dir, layout, unit, r)
        if os.path.isdir(d):
            out.append(d)
        else:
            warn(f"missing run dir: {os.path.basename(d)}")
    return out


def read_all(dirs, filename):
    """Read `filename` from each dir that has it; warns on gaps."""
    rows = []
    for d in dirs:
        p = os.path.join(d, filename)
        if os.path.isfile(p):
            rows.append(read_csv(p))
        else:
            warn(f"missing {filename} in {os.path.basename(d)}")
    return rows


def detect_vocabulary(results_dir, layout, unit, reps):
    d = cold_dir(results_dir, layout, unit, reps[0])
    rows = read_csv(os.path.join(d, "driver.csv"))
    if "chunk_wall" in rows:
        return "old"
    if "backfill_wall" in rows:
        return "new"
    warn(f"could not detect vocabulary from {os.path.basename(d)}/driver.csv; defaulting to new")
    return "new"


# ----------------------------------------------------------------- metadata
_WEEKDAY = re.compile(r"^(Mon|Tue|Wed|Thu|Fri|Sat|Sun)\b")


def parse_machine(raw):
    machine = {"raw": raw}
    for line in raw.splitlines():
        ls = line.strip()
        if not ls:
            continue
        if _WEEKDAY.match(ls) and "captured_at" not in machine:
            machine["captured_at"] = ls
        elif ls.startswith("instance-type:"):
            machine["instance"] = ls.split(":", 1)[1].strip()
        elif ls.startswith("instance-id:"):
            machine["instance_id"] = ls.split(":", 1)[1].strip()
        elif ls.startswith("Linux "):
            machine["kernel"] = ls
        elif ls.startswith("Ubuntu"):
            machine["os"] = ls
        elif ls.startswith("Model name:"):
            machine["cpu"] = ls.split(":", 1)[1].strip()
        elif ls.startswith("CPU(s):"):
            try:
                machine["vcpus"] = int(ls.split(":", 1)[1].strip())
            except ValueError:
                pass
        elif ls.startswith("Mem:"):
            parts = ls.split()
            if len(parts) > 1:
                machine["mem"] = parts[1]
        elif ls.startswith("fsync probe:"):
            machine["fsync_probe"] = ls.split(":", 1)[1].strip()
    return machine


def parse_build(raw):
    build = {}
    for line in raw.splitlines():
        ls = line.strip()
        if ls.startswith("repo:"):
            m = re.match(r"repo:\s*(\S+)\s*\(([^)]*)\)", ls)
            if m:
                build["commit"], build["branch"] = m.group(1), m.group(2)
            else:
                build["commit"] = ls.split(":", 1)[1].strip()
        elif ls.startswith("go version"):
            build["go"] = ls
        elif ls.startswith("rustc"):
            build["rust"] = ls
    return build


def load_metadata(results_dir):
    hits = sorted(glob.glob(os.path.join(results_dir, "*machine-metadata*.txt")))
    if not hits:
        warn("no *machine-metadata*.txt found in results root")
        return {"raw": ""}, {}
    with open(hits[0]) as f:
        raw = f.read()
    return parse_machine(raw), parse_build(raw)


# --------------------------------------------------------- campaign manifests
def load_campaign_manifest(results_dir):
    """metadata.json at the bundle root, or None (additive — legacy bundles lack it)."""
    p = os.path.join(results_dir, "metadata.json")
    if not os.path.isfile(p):
        return None
    try:
        with open(p) as f:
            return json.load(f)
    except (json.JSONDecodeError, OSError) as e:
        warn(f"could not read metadata.json ({e}); ignoring")
        return None


def load_invocations(results_dir):
    """[(dirname, invocation.json dict)] for every per-invocation dir that has one."""
    out = []
    for p in sorted(glob.glob(os.path.join(results_dir, "*", "invocation.json"))):
        try:
            with open(p) as f:
                out.append((os.path.basename(os.path.dirname(p)), json.load(f)))
        except (json.JSONDecodeError, OSError) as e:
            warn(f"could not read {os.path.relpath(p, results_dir)} ({e}); ignoring")
    return out


def hardware_into_machine(machine, hw):
    """Structured hardware (metadata.json) wins over the free-text machine parse
    for the fields they share."""
    if not hw:
        return
    if hw.get("instance_type"):
        machine["instance"] = hw["instance_type"]
    if hw.get("instance_id"):
        machine["instance_id"] = hw["instance_id"]
    if isinstance(hw.get("cpus"), int):
        machine["vcpus"] = hw["cpus"]
    if isinstance(hw.get("mem_total_kb"), int):
        machine["mem"] = f"{round(hw['mem_total_kb'] / (1024 * 1024))}Gi"
    if hw.get("uname") and "kernel" not in machine:
        machine["kernel"] = hw["uname"]


def resolve_binary(build, metadata, invocations):
    """Merge invocation.json binary identity into build (authoritative over the
    machine-metadata `repo:` parse) and warn on any commit mismatch."""
    binaries = [inv["binary"] for _, inv in invocations if inv.get("binary")]
    commits = {b["commit_hash"] for b in binaries if b.get("commit_hash")}
    if metadata and metadata.get("campaign", {}).get("built_commit"):
        commits.add(metadata["campaign"]["built_commit"])
    if len(commits) > 1:
        warn(f"binary commit mismatch across invocations/manifest: {sorted(commits)}")
    b0 = binaries[0] if binaries else {}
    commit = b0.get("commit_hash") or (metadata or {}).get("campaign", {}).get("built_commit")
    if commit:
        build["commit"] = commit
    for src, dst in (("branch", "branch"), ("version", "version"),
                     ("build_timestamp", "build_timestamp")):
        if b0.get(src):
            build[dst] = b0[src]


def resolve_close_interval_ns(metadata, invocations):
    """Close interval in ns (0 == unpaced), or None when no manifest records it."""
    raw = None
    if metadata:
        raw = metadata.get("campaign", {}).get("close_interval")
    if raw is None:
        for _, inv in invocations:
            if inv.get("command", "").endswith("hot") and "close-interval" in inv.get("flags", {}):
                raw = inv["flags"]["close-interval"]
                break
    if raw is None:
        return None
    try:
        return parse_go_duration(raw)
    except ValueError:
        warn(f"could not parse close_interval {raw!r}; treating as unknown")
        return None


# ------------------------------------------------------------- phase targets
# Phase 1/2/3 performance targets for HOT ingestion (the numbers are public:
# stellar/stellar-rpc issues #872-#874). Copied verbatim into every
# campaign-layout run JSON as campaign.phase_targets so the viewer reads the
# targets as data and holds no target constants of its own. The ingest-slice
# target (ingest_p99_target_ns) is the row this benchmark measures — the
# per-ledger ingest_total p99; phase 2 defines no ingest-slice target, so the
# key is omitted there. Cold ingestion (backfill) has no phase and no targets.
PHASE_TARGETS = [
    {
        "phase": 1,
        "block_time_ns": 2_000_000_000,
        "e2e_budget_ns": 5_000_000_000,
        "ingest_p99_target_ns": 900_000_000,
        "workloads": [
            {"name": "SAC transfers", "tps": 3000, "tx_per_ledger": 6000},
            {"name": "OZ token transfers", "tps": 2000, "tx_per_ledger": 4000},
            {"name": "Soroswap swaps", "tps": 750, "tx_per_ledger": 1500},
        ],
        "orgs": 10,
        "retention": "3 months",
    },
    {
        "phase": 2,
        "block_time_ns": 1_000_000_000,
        "e2e_budget_ns": 2_500_000_000,
        "workloads": [
            {"name": "SAC transfers", "tps": 5000, "tx_per_ledger": 5000},
            {"name": "OZ token transfers", "tps": 4000, "tx_per_ledger": 4000},
            {"name": "Soroswap swaps", "tps": 1500, "tx_per_ledger": 1500},
        ],
        "orgs": 19,
        "retention": "6 months",
    },
    {
        "phase": 3,
        "block_time_ns": 600_000_000,
        "e2e_budget_ns": 2_000_000_000,
        "ingest_p99_target_ns": 100_000_000,
        "workloads": [
            {"name": "SAC transfers", "tps": 10000, "tx_per_ledger": 6000},
            {"name": "OZ token transfers", "tps": 6000, "tx_per_ledger": 3600},
            {"name": "Soroswap swaps", "tps": 3000, "tx_per_ledger": 1800},
        ],
        "orgs": 31,
        "retention": "2 years",
    },
]


def match_phase(close_interval_ns):
    """The phase whose block time equals the close interval exactly, or None.

    Exact match only: any other paced value is a pace-only run with no phase;
    unpaced (0 or None) has no phase and no keep-up check.
    """
    if not close_interval_ns:
        return None
    for p in PHASE_TARGETS:
        if p["block_time_ns"] == close_interval_ns:
            return p
    return None


def format_interval(ns):
    """Short human label for a close interval: "2 s", "1.5 s", "600 ms"."""
    if ns >= 1_000_000_000 and ns % 100_000_000 == 0:
        return f"{ns / 1e9:g} s"
    return f"{ns / 1e6:g} ms"


# ----------------------------------------------------------------- unit facts
def unit_counts(results_dir, layout, unit, reps):
    """Ledger / tx / event counts from the cold driver run-1 n_items."""
    for r in reps:
        p = os.path.join(cold_dir(results_dir, layout, unit, r), "driver.csv")
        if os.path.isfile(p):
            d = read_csv(p)
            return {
                "ledgers": d["ledgers_total"]["n_items"],
                "txs": d["txhash_total"]["n_items"],
                "events": d["events_total"]["n_items"],
            }
    warn(f"no cold driver.csv found for unit {unit}; counts unavailable")
    return {"ledgers": 0, "txs": 0, "events": 0}


def order_units(units, kind, facts):
    """Display order: numeric for chunks; facts-key order (then alpha) for profiles."""
    if kind == "pubnet":
        return sorted(units, key=lambda u: (int(u) if u.isdigit() else u))
    ordered = [u for u in facts if u in units]
    ordered += sorted(u for u in units if u not in ordered)
    return ordered


def order_units_campaign(units, metadata):
    """Campaign display order: datasets in manifest order (then alpha), chunk ascending."""
    ds_order = {}
    if metadata:
        for i, d in enumerate(metadata.get("datasets", [])):
            if d.get("name"):
                ds_order[d["name"]] = i

    def key(u):
        m = _UNIT_CHUNK.match(u)
        if m:
            return (ds_order.get(m.group(1), len(ds_order)), m.group(1), int(m.group(2)))
        return (len(ds_order), u, 0)

    return sorted(units, key=key)


# ----------------------------------------------------------------- sections
def build_ingest_cold(results_dir, layout, unit, reps, vocab, counts):
    dirs = run_dirs(results_dir, layout, "cold", unit, reps)
    drivers = read_all(dirs, "driver.csv")
    if not drivers:
        warn(f"no cold driver.csv for unit {unit}")
        return None
    driver_out = {st: stage_agg(drivers, st) for st in drivers[0] if st != PEAK_RSS}

    files_out = {}
    for csv_path in sorted(glob.glob(os.path.join(dirs[0], "*.csv"))):
        stem = os.path.splitext(os.path.basename(csv_path))[0]
        if stem == "driver":
            continue
        rows = read_all(dirs, f"{stem}.csv")
        if rows:
            files_out[stem] = {st: stage_agg(rows, st) for st in rows[0]}

    wall = "chunk_wall" if vocab == "old" else "backfill_wall"
    derived = {
        "ledgers_per_s": stat([counts["ledgers"] / (d[wall]["total_ns"] / NS) for d in drivers]),
        "tp_ledgers": stat([counts["ledgers"] / (d["ledgers_total"]["total_ns"] / NS) for d in drivers]),
        "tp_txs": stat([counts["txs"] / (d["txhash_total"]["total_ns"] / NS) for d in drivers]),
        "tp_events": stat([counts["events"] / (d["events_total"]["total_ns"] / NS) for d in drivers]),
    }
    out = {"driver": driver_out, "files": files_out, "derived": derived}
    peak = _peak_rss_bytes(drivers)
    if peak is not None:
        out["peak_rss_bytes"] = peak
    return out


PACE_LAG = "pace_lag"
PEAK_RSS = "peak_rss_bytes"


def _peak_rss_bytes(drivers):
    """Peak resident-set-size gauge (bytes), aggregated across runs, or None.

    The driver.csv `peak_rss_bytes` row is a single gauge the harness replicates
    across the duration columns — read `total_ns` as the byte value. Stored as a
    plain V (bytes), a sibling of `driver`, not a StageAgg: it is a memory
    high-water mark, not a latency distribution, and the ns column names do not
    apply to it. Present in every run or omitted entirely (never zero-filled).
    """
    if not all(PEAK_RSS in d for d in drivers):
        return None
    return stat([d[PEAK_RSS]["total_ns"] for d in drivers])


def _pace_lag_agg(drivers, unit):
    """StageAgg for the optional pace_lag row across hot runs, or None if unpaced.

    Present in every run  -> aggregated like any other stage.
    Present in no run     -> None (unpaced cell; the row is omitted entirely).
    Present in some runs  -> the missing runs are treated as all-zero lag (an
                             on-schedule run) and a warning is emitted, so the
                             r-array still has one entry per rep.
    """
    n_present = sum(PACE_LAG in d for d in drivers)
    if n_present == 0:
        return None
    if n_present == len(drivers):
        return stage_agg([{PACE_LAG: d[PACE_LAG]} for d in drivers], PACE_LAG, warn_nitems=False)
    warn(f"ingest-hot {unit}: pace_lag present in {n_present}/{len(drivers)} runs; "
         f"zero-filling the rest as on-schedule")
    template = next(d[PACE_LAG] for d in drivers if PACE_LAG in d)
    rows = []
    for d in drivers:
        if PACE_LAG in d:
            rows.append({PACE_LAG: d[PACE_LAG]})
        else:
            zero = {k: 0 for k in template}
            zero["n"], zero["n_items"] = template["n"], template["n_items"]
            rows.append({PACE_LAG: zero})
    return stage_agg(rows, PACE_LAG, warn_nitems=False)


def build_ingest_hot(results_dir, layout, unit, reps, vocab, counts):
    dirs = run_dirs(results_dir, layout, "hot", unit, reps)
    drivers = read_all(dirs, "driver.csv")
    hots = read_all(dirs, "hot.csv")
    if not drivers or not hots:
        warn(f"incomplete hot data for unit {unit}")
        return None
    # pace_lag is aggregated separately so inconsistent presence across runs is
    # zero-filled rather than yielding a short r-array (or being dropped when
    # absent from run 1).
    driver_out = {st: stage_agg(drivers, st) for st in drivers[0] if st not in (PACE_LAG, PEAK_RSS)}
    pace = _pace_lag_agg(drivers, unit)
    if pace is not None:
        driver_out[PACE_LAG] = pace
    phases_out = {st: stage_agg(hots, st) for st in hots[0]}
    wall = "chunk_wall" if vocab == "old" else "run_wall"
    derived = {"ledgers_per_s": stat([counts["ledgers"] / (d[wall]["total_ns"] / NS) for d in drivers])}
    out = {"driver": driver_out, "phases": phases_out, "derived": derived}
    peak = _peak_rss_bytes(drivers)
    if peak is not None:
        out["peak_rss_bytes"] = peak
    return out


_CW = re.compile(r"_c(\d+)$")


def build_queries(results_dir, layout, unit, reps, tier):
    dirs = run_dirs(results_dir, layout, f"query-{tier}", unit, reps)
    drivers = read_all(dirs, "driver.csv")
    if not drivers:
        return None
    entry = {}
    qtypes = sorted(os.path.splitext(os.path.basename(p))[0]
                    for p in glob.glob(os.path.join(dirs[0], "*.csv"))
                    if os.path.basename(p) != "driver.csv")
    for qt in qtypes:
        rows = read_all(dirs, f"{qt}.csv")
        if not rows:
            continue
        ws = sorted({int(m.group(1)) for st in rows[0]
                     for m in [re.match(r"^total_c(\d+)$", st)] if m})
        qout = {}
        for w in ws:
            agg = stage_agg(rows, f"total_c{w}", warn_nitems=(qt != "events"))
            walls = [d[f"{qt}_c{w}"]["total_ns"] for d in drivers]
            agg["wall"] = stat(walls)
            n_ops = rows[0][f"total_c{w}"]["n"]
            agg["ops_s"] = stat([n_ops / (wns / NS) for wns in walls])
            if qt == "events":
                items = [r[f"total_c{w}"]["n_items"] for r in rows]
                agg["items_s"] = stat([it / (wns / NS) for it, wns in zip(items, walls)])
                agg["items_r"] = items
                if len(set(items)) > 1:
                    spread = (max(items) - min(items)) / statistics.median(items) * 100
                    if spread > 1.0:
                        warn(f"query-{tier}-{unit} events c{w}: n_items varies "
                             f"{min(items)}..{max(items)} ({spread:.1f}%)")
            qout[f"c{w}"] = agg
        entry[qt] = qout

    setup = {}
    for st in drivers[0]:
        if not _CW.search(st):
            v = stat([d[st]["total_ns"] for d in drivers])
            v["n_items"] = drivers[0][st]["n_items"]
            setup[st] = v
    entry["setup"] = setup
    return entry


def build_golden(results_dir, unit):
    p = os.path.join(results_dir, f"golden-download-{unit}", "driver.csv")
    if not os.path.isfile(p):
        return None
    return {"wall_ns": read_csv(p)["chunk_wall"]["total_ns"]}


# ----------------------------------------------------------------- validator
def _is_v(d):
    return isinstance(d, dict) and all(k in d for k in ("m", "lo", "hi", "r"))


def _is_stage_agg(d):
    return isinstance(d, dict) and all(k in d for k in ("total", "p50", "p90", "p99", "max"))


def _walk(node, errors, reps, path):
    if isinstance(node, dict):
        if _is_v(node):
            r = node["r"]
            if not isinstance(r, list):
                errors.append(f"{path}: r is not a list")
                return
            if reps is not None and len(r) != reps:
                errors.append(f"{path}: len(r)={len(r)} != reps {reps}")
            if not r:
                errors.append(f"{path}: empty r")
                return
            if node["lo"] != min(r):
                errors.append(f"{path}: lo {node['lo']} != min(r) {min(r)}")
            if node["hi"] != max(r):
                errors.append(f"{path}: hi {node['hi']} != max(r) {max(r)}")
            if node["m"] != statistics.median_low(r):
                errors.append(f"{path}: m {node['m']} != median_low(r) {statistics.median_low(r)}")
            # setup rows carry an extra n_items alongside the V fields
            if "n_items" in node and not isinstance(node["n_items"], numbers.Integral):
                errors.append(f"{path}: n_items is not an int")
            return
        if _is_stage_agg(node):
            for k in ("n", "n_items"):
                if k not in node or not isinstance(node[k], int):
                    errors.append(f"{path}: StageAgg missing int {k}")
        for k, v in node.items():
            _walk(v, errors, reps, f"{path}/{k}")
    elif isinstance(node, list):
        for i, v in enumerate(node):
            _walk(v, errors, reps, f"{path}[{i}]")


def validate_run(data, reps=None):
    """Return a list of schema-invariant violations (empty == valid)."""
    errors = []
    required = ("schema_version", "run_id", "run_name", "run_date",
                "machine", "build", "dataset", "campaign", "sections")
    for k in required:
        if k not in data:
            errors.append(f"missing top-level key: {k}")
    if data.get("schema_version") != 1:
        errors.append("schema_version must be 1")
    if isinstance(data.get("machine"), dict) and "raw" not in data["machine"]:
        errors.append("machine.raw missing")
    ds = data.get("dataset", {})
    for k in ("kind", "units", "unit_label", "unit_order", "unit_meta"):
        if k not in ds:
            errors.append(f"dataset.{k} missing")
    camp = data.get("campaign", {})
    for k in ("reps", "vocabulary"):
        if k not in camp:
            errors.append(f"campaign.{k} missing")

    sections = data.get("sections", [])
    present = [k for k in SECTION_ORDER if k in data]
    if set(sections) != set(present):
        errors.append(f"sections {sections} != present section keys {present}")
    for s in sections:
        if s not in data:
            errors.append(f"section '{s}' listed but not present")

    if "checks" in data and not isinstance(data["checks"], dict):
        errors.append("checks must be an object")

    for s in present:
        _walk(data[s], errors, reps, s)
    return errors


# ----------------------------------------------------------------- manifest
def update_manifest(out_dir, entry):
    path = os.path.join(out_dir, "index.json")
    data = {"schema_version": 1, "runs": []}
    if os.path.isfile(path):
        try:
            with open(path) as f:
                data = json.load(f)
        except (json.JSONDecodeError, OSError) as e:
            warn(f"could not read existing manifest ({e}); recreating")
            data = {"schema_version": 1, "runs": []}
    data["schema_version"] = 1
    runs = [r for r in data.get("runs", []) if r.get("id") != entry["id"]]
    runs.append(entry)
    runs.sort(key=lambda r: (r["date"], r["id"]), reverse=True)
    data["runs"] = runs
    with open(path, "w") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")
    return path


# ----------------------------------------------------------------- assemble
def convert(args):
    results_dir = os.path.abspath(os.path.expanduser(args.results_dir))
    if not os.path.isdir(results_dir):
        fail(f"results dir not readable: {results_dir}")

    names = subdirs(results_dir)
    layout = detect_layout(names)
    if layout is None:
        fail("no benchmark run directories found (need synth-* or ingest-* dirs)")

    units, reps = discover_units_reps(names, layout)
    if not units or not reps:
        fail("no units/reps discovered from directory names")
    # dataset-kind is the data nature (pubnet vs synthetic); the campaign layout
    # is orthogonal to it, so only flag a pubnet/synthetic layout mismatch.
    if layout in ("pubnet", "synthetic") and args.dataset_kind != layout:
        warn(f"--dataset-kind {args.dataset_kind} but directory layout looks {layout}")
    if layout == "campaign":
        prep = [n for n in names if _CAMPAIGN_GOLDEN.match(n)]
        if prep:
            warn(f"skipping {len(prep)} golden prep dir(s) (dataset preparation, not "
                 f"results): {', '.join(prep)}")

    vocab = detect_vocabulary(results_dir, layout, sorted(units)[0], reps)

    facts = {}
    if args.unit_facts:
        with open(os.path.expanduser(args.unit_facts)) as f:
            facts = json.load(f)

    machine, build = load_metadata(results_dir)
    metadata = load_campaign_manifest(results_dir)
    invocations = load_invocations(results_dir)
    hardware = (metadata or {}).get("hardware") or {}
    hostname = ((metadata or {}).get("hostname")
                or (invocations[0][1].get("hostname") if invocations else None))
    hardware_into_machine(machine, hardware)
    resolve_binary(build, metadata, invocations)
    close_interval_ns = resolve_close_interval_ns(metadata, invocations)

    counts = {u: unit_counts(results_dir, layout, u, reps) for u in units}
    unit_order = (order_units_campaign(units, metadata) if layout == "campaign"
                  else order_units(units, args.dataset_kind, facts))

    # unit_meta
    unit_meta = {}
    for u in unit_order:
        meta = dict(counts[u])
        if args.dataset_kind == "pubnet" and u.isdigit():
            seq_start = int(u) * 10000 + 2
            meta["seq_start"] = seq_start
            meta["seq_end"] = seq_start + 9999
        if u in facts:
            meta.update(facts[u])
        unit_meta[u] = meta
    for u in facts:
        if u not in unit_meta:
            warn(f"--unit-facts has unit '{u}' not present in dataset")

    # sections
    ingest_cold, ingest_hot = {}, {}
    for u in unit_order:
        c = build_ingest_cold(results_dir, layout, u, reps, vocab, counts[u])
        if c is not None:
            ingest_cold[u] = c
        h = build_ingest_hot(results_dir, layout, u, reps, vocab, counts[u])
        if h is not None:
            ingest_hot[u] = h

    queries, golden = {}, {}
    if layout in ("pubnet", "campaign"):
        has_query = any(n.startswith("query-") for n in names)
        if has_query:
            for tier in ("cold", "hot"):
                tq = {}
                for u in unit_order:
                    q = build_queries(results_dir, layout, u, reps, tier)
                    if q is not None:
                        tq[u] = q
                if tq:
                    queries[tier] = tq
    if layout == "pubnet":
        # golden-download-<unit> is a timed sourcing leg; the campaign layout's
        # golden-<dataset>-c<chunk> dirs are untimed prep and were skipped above.
        for u in unit_order:
            g = build_golden(results_dir, u)
            if g is not None:
                golden[u] = g

    # run identity: explicit CLI args win; otherwise fall back to the manifest.
    run_id = args.run_id or (metadata.get("run_id") if metadata else None)
    if not run_id:
        fail("run id required: pass --run-id or provide metadata.json with a run_id")
    run_date = args.run_date
    if not run_date and metadata and metadata.get("started_at"):
        run_date = metadata["started_at"][:10]
    if not run_date:
        fail("run date required: pass --run-date or provide metadata.json started_at")
    run_name = (args.run_name or (metadata.get("campaign", {}).get("name") if metadata else None)
                or run_id)

    # top level
    if args.description:
        description = args.description
    elif args.dataset_kind == "pubnet":
        description = (f"Ingest & query benchmarks of the full-history storage engine on "
                       f"{len(unit_order)} sampled 10k-ledger chunks of real pubnet history "
                       f"(chunks {'/'.join(unit_order)}).")
    else:
        labels = [unit_meta.get(u, {}).get("label", u) for u in unit_order]
        description = (f"Synthetic apply-load ingest benchmarks across "
                       f"{len(unit_order)} profile(s): {', '.join(labels)}.")

    campaign = {"reps": len(reps), "vocabulary": vocab}
    if args.source_gcs:
        campaign["source_gcs"] = args.source_gcs
    if args.notes:
        campaign["notes"] = args.notes
    if close_interval_ns is not None:
        campaign["close_interval_ns"] = close_interval_ns
    # Phase targets are campaign-layout only; legacy layouts stay byte-identical.
    matched_phase = None
    if layout == "campaign":
        matched_phase = match_phase(close_interval_ns)
        if matched_phase is not None:
            campaign["phase"] = matched_phase["phase"]
        campaign["phase_targets"] = PHASE_TARGETS
    if metadata:
        mc = metadata.get("campaign", {})
        if mc.get("name"):
            campaign["name"] = mc["name"]
        if mc.get("config_file"):
            campaign["config_file"] = mc["config_file"]
        cfg = {k: v for k, v in mc.items()
               if k not in ("name", "config_file", "close_interval")}
        if cfg:
            campaign["config"] = cfg

    data = {
        "schema_version": 1,
        "run_id": run_id,
        "run_name": run_name,
        "run_date": run_date,
        "machine": machine,
        "build": build,
        "dataset": {
            "kind": args.dataset_kind,
            "description": description,
            "units": "chunk" if args.dataset_kind == "pubnet" else "profile",
            "unit_label": "Chunk" if args.dataset_kind == "pubnet" else "Profile",
            "unit_order": unit_order,
            "unit_meta": unit_meta,
        },
        "campaign": campaign,
    }
    if hardware:
        data["hardware"] = hardware
    if hostname:
        data["hostname"] = hostname

    if layout == "campaign":
        # The keep-up check derives from the run's own pace: a matched phase
        # names it, any other pace is judged as itself, and an unpaced
        # catch-up run gets no keep-up check at all.
        if close_interval_ns:
            label = (f"Phase {matched_phase['phase']} block model "
                     f"({format_interval(close_interval_ns)})" if matched_phase
                     else f"{format_interval(close_interval_ns)} pace")
            data["checks"] = {"kind": "block_keepup", "interval_ns": close_interval_ns,
                              "label": label, "applies_to": "ingest_hot"}
        elif queries:
            data["checks"] = {"kind": "query_p99_threshold", "threshold_ns": 500000000,
                              "label": "query p99 ≤ 500 ms", "applies_to": "queries"}
    elif args.dataset_kind == "synthetic":
        data["checks"] = {"kind": "block_keepup", "interval_ns": 600000000,
                          "label": "600 ms block model", "applies_to": "ingest_hot"}
    elif queries:
        data["checks"] = {"kind": "query_p99_threshold", "threshold_ns": 500000000,
                          "label": "query p99 ≤ 500 ms", "applies_to": "queries"}

    if ingest_cold:
        data["ingest_cold"] = ingest_cold
    if ingest_hot:
        data["ingest_hot"] = ingest_hot
    if queries:
        data["queries"] = queries
    if golden:
        data["golden"] = golden
    data["sections"] = [k for k in SECTION_ORDER if k in data]

    # self-check
    errors = validate_run(data, reps=len(reps))
    for e in errors:
        warn(f"validation: {e}")

    os.makedirs(args.out_dir, exist_ok=True)
    out_path = os.path.join(args.out_dir, f"{run_id}.json")
    with open(out_path, "w") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")

    manifest_path = update_manifest(args.out_dir, {
        "id": run_id, "name": run_name, "date": run_date,
        "kind": args.dataset_kind, "path": f"runs/{run_id}.json",
    })

    print(f"wrote {out_path} ({os.path.getsize(out_path) / 1024:.0f} KiB)")
    print(f"updated {manifest_path}")
    print(f"layout={layout} vocabulary={vocab} units={unit_order} reps={len(reps)}")
    print(f"sections={data['sections']}")
    print(f"{len(_warnings)} warning(s)" if _warnings else "no warnings")
    return data


def main(argv=None):
    ap = argparse.ArgumentParser(description="Convert benchmark results to schema-v1 run JSON.")
    ap.add_argument("results_dir")
    # Identity defaults from metadata.json when present; explicit flags still win.
    ap.add_argument("--run-id")
    ap.add_argument("--run-name")
    ap.add_argument("--run-date")
    ap.add_argument("--dataset-kind", required=True, choices=["pubnet", "synthetic"])
    ap.add_argument("--unit-facts")
    ap.add_argument("--source-gcs")
    ap.add_argument("--notes")
    ap.add_argument("--description")
    ap.add_argument("--out-dir", required=True)
    args = ap.parse_args(argv)
    convert(args)


if __name__ == "__main__":
    main()
