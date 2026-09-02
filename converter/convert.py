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
# The RPS-era runner splits a query leg per query type, inserting one more token
# before the run number: query-<tier>-<dataset>-c<chunk>-<qtype>-run<R>.
_CAMPAIGN_RUN = re.compile(r"^(?:ingest|query)-(?:cold|hot)-.+-c\d+(?:-[a-z]+)?-run\d+$")
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
    """Discover unit ids and rep numbers from the ingest dir names — cold and
    hot both count, so a hot-only campaign (ingest = "hot") still yields its
    unit set.

    A campaign unit id is the composite "<dataset>-c<chunk>" (e.g. sac-6000-c1);
    pubnet/synthetic unit ids are the bare token between family and run.
    """
    if layout == "synthetic":
        pat = re.compile(r"^synth-(?:cold|hot)-(.+)-run(\d+)$")
    elif layout == "campaign":
        pat = re.compile(r"^ingest-(?:cold|hot)-(.+-c\d+)-run(\d+)$")
    else:
        pat = re.compile(r"^ingest-(?:cold|hot)-(.+)-run(\d+)$")
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
    if not os.path.isfile(os.path.join(d, "driver.csv")):
        # Hot-only campaign: the hot driver names the wall run_wall (new) or
        # chunk_wall (old).
        d = hot_dir(results_dir, layout, unit, reps[0])
    rows = read_csv(os.path.join(d, "driver.csv"))
    if "chunk_wall" in rows:
        return "old"
    if "backfill_wall" in rows or "run_wall" in rows:
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
    """[(dirname, invocation.json dict)] for every per-invocation dir that has one.
    A failed run also writes invocation.json, with an `error` field (stellar-rpc#907):
    its CSVs are partial, so a bundle carrying one is loudly suspect."""
    out = []
    for p in sorted(glob.glob(os.path.join(results_dir, "*", "invocation.json"))):
        try:
            with open(p) as f:
                inv = json.load(f)
        except (json.JSONDecodeError, OSError) as e:
            warn(f"could not read {os.path.relpath(p, results_dir)} ({e}); ignoring")
            continue
        d = os.path.basename(os.path.dirname(p))
        if inv.get("error"):
            warn(f"{d} is a FAILED run ({inv['error']!r}); its CSVs are partial "
                 "— re-run or drop that leg before publishing this conversion")
        out.append((d, inv))
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


def _normalize_binary(b):
    """invocation.json binary identity keyed snake_case regardless of producer:
    stellar-rpc#907 writes camelCase (commitHash, buildTimestamp); earlier
    drafts of the schema used snake_case. Accept both spellings."""
    renames = {"commitHash": "commit_hash", "buildTimestamp": "build_timestamp"}
    return {renames.get(k, k): v for k, v in b.items()}


def resolve_binary(build, metadata, invocations):
    """Merge invocation.json binary identity into build (authoritative over the
    machine-metadata `repo:` parse) and warn on any commit mismatch."""
    binaries = [_normalize_binary(inv["binary"]) for _, inv in invocations if inv.get("binary")]
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
# stellar/stellar-rpc issues #872-#874). They live in docs/targets.json — the
# single source shared by this converter, the reports viewer, and the latency
# model. The converter loads them here and copies them verbatim into every
# campaign-layout run JSON as campaign.phase_targets, so the viewer can read the
# targets as data. The ingest-slice target (ingest_p99_target_ns) is the row
# this benchmark measures — the per-ledger ingest_total p99. Cold ingestion
# (backfill) has no phase and no targets.
#
# The converter derives the ingest-slice target for any phase that does not
# state one. It uses the end-to-end latency budget, which follows the
# transaction lifecycle:
#
#   e2e = rtt/2 + send_tx_p99 + block_time*block_count + ingest_p99
#         + rtt + get_tx_p99
#
# The network legs are hardcoded constants from the assumed client<->RPC round
# trip (rtt): half a round trip carries the submission request (the response is
# off the critical path), and a full round trip carries the getTransaction
# call. The formula counts block time once per ledger close on the path
# (block_count, a per-phase consensus plan: 2 for phases 1-2, 3 for phase 3).
# The in-RPC handler latencies (sendTransaction, getTransaction) are fixed
# across phases. This leaves ingest_p99 as the only unknown.
TARGETS_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "docs", "targets.json")

with open(TARGETS_PATH, encoding="utf-8") as _f:
    _TARGETS = json.load(_f)

_FIXED = _TARGETS["fixed_estimates"]
NETWORK_RTT_NS = _FIXED["network_rtt_ns"]     # assumed client <-> RPC round trip
SEND_TX_P99_NS = _FIXED["send_tx_p99_ns"]     # P99 in-RPC sendTransaction handler (fixed)
GET_TX_P99_NS = _FIXED["get_tx_p99_ns"]       # P99 in-RPC getTransaction handler (fixed)

# Fixed non-ingest, non-consensus E2E cost: half a round trip for the
# submission request, a full round trip for the getTransaction call, plus the
# two in-RPC handler slices.
FIXED_E2E_NS = 3 * NETWORK_RTT_NS // 2 + SEND_TX_P99_NS + GET_TX_P99_NS


def ingest_p99_target(block_time_ns, e2e_budget_ns, block_count=2):
    """Ingest-slice p99 budget implied by the end-to-end latency formula."""
    return e2e_budget_ns - block_count * block_time_ns - FIXED_E2E_NS


# Open-loop query load model: the two families of requirement the read path
# answers to, and the 0.5/1/2 ladder every leg is paced on.
# Copied verbatim into campaign runs that carry RPS query cells, the way
# PHASE_TARGETS is, so the viewer reads the floors as data.
QUERY_LOAD = _TARGETS["query_load"]

# Family 1 — the SLA: one arrival rate per endpoint (its share of the 500 rps
# Standard watermark) and one p99 per endpoint and storage tier (hot = the SLA's
# Live window, cold = Recent). Neither depends on the phase or the profile.
SLA_FLOORS_RPS = QUERY_LOAD["sla"]["floors_rps"]
SLA_P99_NS = QUERY_LOAD["sla"]["p99_ns"]

# Family 2 — the E2E-budget probe: getTransaction alone, at the demand-derived
# floors of work item 856 (per profile, indexed by phase), answering only for
# its in-RPC p99, which is its slice of the end-to-end lifecycle budget. The
# scheduled p99 of that cell is reported and never judged.
E2E_IN_RPC_P99_NS = QUERY_LOAD["e2e_probe"]["in_rpc_p99_ns"]
E2E_FLOORS_RPS = QUERY_LOAD["e2e_probe"]["floors_rps"]

PHASE_TARGETS = _TARGETS["phases"]

# Fill the ingest-slice target for any phase that does not state one (phase 2).
# The value comes from the end-to-end budget via the formula above.
for _p in PHASE_TARGETS:
    _p.setdefault(
        "ingest_p99_target_ns",
        ingest_p99_target(_p["block_time_ns"], _p["e2e_budget_ns"],
                          _p.get("block_count", 2)),
    )


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


def phase_by_number(n):
    """The phase table entry numbered n, or None for anything else (including
    None and a number no phase carries)."""
    for p in PHASE_TARGETS:
        if p["phase"] == n:
            return p
    return None


def format_interval(ns):
    """Short human label for a close interval: "2 s", "1.5 s", "600 ms"."""
    if ns >= 1_000_000_000 and ns % 100_000_000 == 0:
        return f"{ns / 1e9:g} s"
    return f"{ns / 1e6:g} ms"


# ----------------------------------------------------------------- unit facts
def unit_counts(results_dir, layout, unit, reps):
    """Ledger / tx / event counts from the first available cold driver's
    n_items; a hot-only campaign carries the same counts as the hot leg's
    per-ledger stage n_items in hot.csv."""
    for r in reps:
        p = os.path.join(cold_dir(results_dir, layout, unit, r), "driver.csv")
        if os.path.isfile(p):
            d = read_csv(p)
            return {
                "ledgers": d["ledgers_total"]["n_items"],
                "txs": d["txhash_total"]["n_items"],
                "events": d["events_total"]["n_items"],
            }
    for r in reps:
        p = os.path.join(hot_dir(results_dir, layout, unit, r), "hot.csv")
        if os.path.isfile(p):
            d = read_csv(p)
            if all(k in d for k in ("ledgers", "txhash", "events")):
                return {
                    "ledgers": d["ledgers"]["n_items"],
                    "txs": d["txhash"]["n_items"],
                    "events": d["events"]["n_items"],
                }
    warn(f"no cold driver.csv or hot hot.csv found for unit {unit}; counts unavailable")
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
# The RPS-era per-leg driver rows: <qtype>_r<rate> and its _millirps/_lag/_shed
# siblings. Everything a driver carries that matches neither this nor _CW is
# setup (open, evict, peak_rss_bytes).
_RPS_ROW = re.compile(r"_r\d+(?:\.\d+)?(?:_(?:millirps|lag|shed))?$")
_TOTAL_R = re.compile(r"^total_r(\d+(?:\.\d+)?)$")


def _is_leg_row(stage):
    """True for a per-cell driver row (closed-loop _c<W> or paced _r<rate>)."""
    return bool(_CW.search(stage) or _RPS_ROW.search(stage))


def rps_query_types(results_dir, tier, unit):
    """Query types with their own leg dir for this (tier, unit), sorted.

    Empty for a pre-RPS bundle, where one query-<tier>-<unit>-run<R> dir holds
    every type's CSV — that shape keeps converting through the closed-loop path.
    """
    pat = re.compile(rf"^query-{tier}-{re.escape(unit)}-(.+)-run\d+$")
    return sorted({m.group(1) for n in subdirs(results_dir)
                   for m in [pat.match(n)] if m})


def rps_run_dirs(results_dir, tier, unit, qtype, reps):
    """Existing per-qtype leg dirs for a (tier, unit, qtype); warns on gaps."""
    out = []
    for r in reps:
        d = os.path.join(results_dir, f"query-{tier}-{unit}-{qtype}-run{r}")
        if os.path.isdir(d):
            out.append(d)
        else:
            warn(f"missing run dir: {os.path.basename(d)}")
    return out


def _opt_stage_agg(rows, stage, label, warn_nitems=True):
    """StageAgg for an optional row, or None (with a warning) when no run has it."""
    try:
        return stage_agg(rows, stage, warn_nitems=warn_nitems)
    except KeyError:
        warn(f"{label}: row {stage} missing; omitting")
        return None


def _opt_driver_v(drivers, row, label, col="total_ns", f=None):
    """V of one driver column across runs, or None (with a warning) when any run
    lacks the row — a short r-array would break the per-rep invariant."""
    if not all(row in d for d in drivers):
        warn(f"{label}: driver row {row} missing; omitting")
        return None
    vals = [d[row][col] for d in drivers]
    return stat([f(v) for v in vals] if f else vals)


def build_queries_rps(results_dir, tier, unit, reps, qtypes):
    """Open-loop (paced-RPS) query legs: one dir per query type, one cell per
    target rate.

    The headline latency is the SCHEDULED one (total_r<rate>, due->done): it is
    coordinated-omission-correct, so a leg that falls behind its arrival
    schedule shows the queueing it caused. Service time (dispatch->done) rides
    along as a secondary column. txhash also splits found/miss sub-stages; like
    the closed-loop found_c<W> rows, they are ignored here.
    """
    entry, setup_drivers = {}, None
    for qt in qtypes:
        dirs = rps_run_dirs(results_dir, tier, unit, qt, reps)
        rows = read_all(dirs, f"{qt}.csv")
        drivers = read_all(dirs, "driver.csv")
        if not rows or not drivers:
            warn(f"query-{tier}-{unit} {qt}: no CSVs; skipping")
            continue
        # Every leg's driver repeats the same setup rows (they measure the same
        # open/evict prep), so the first type's leg stands for the whole cell.
        if setup_drivers is None:
            setup_drivers = drivers
        tokens = sorted({m.group(1) for st in rows[0]
                         for m in [_TOTAL_R.match(st)] if m}, key=float)
        qout = {}
        for tok in tokens:
            label = f"query-{tier}-{unit} {qt} r{tok}"
            agg = stage_agg(rows, f"total_r{tok}", warn_nitems=(qt != "events"))
            agg["target_rps"] = float(tok)
            svc = _opt_stage_agg(rows, f"service_r{tok}", label, warn_nitems=False)
            if svc is not None:
                agg["service"] = svc
            wall = _opt_driver_v(drivers, f"{qt}_r{tok}", label)
            if wall is not None:
                agg["wall"] = wall
            # The achieved rate is carried x1000 as an int in the duration
            # columns (the way peak_rss_bytes carries bytes) — divide it back
            # out per run, then aggregate.
            achieved = _opt_driver_v(drivers, f"{qt}_r{tok}_millirps", label,
                                     f=lambda v: v / 1000)
            if achieved is not None:
                agg["achieved_rps"] = achieved
            lag = _opt_stage_agg(drivers, f"{qt}_r{tok}_lag", label, warn_nitems=False)
            if lag is not None:
                agg["dispatch_lag"] = lag
            shed = _opt_driver_v(drivers, f"{qt}_r{tok}_shed", label, col="n_items")
            if shed is not None:
                agg["shed"] = shed
            if qt == "events":
                # A sequential subscriber needs each page's cursor, so the MEAN
                # page latency (service total / pages) is what accumulates as
                # lag — a separate budget from the p99.
                svc_row = f"service_r{tok}"
                if all(svc_row in r and r[svc_row]["n"] for r in rows):
                    agg["mean_page_ns"] = stat([r[svc_row]["total_ns"] / r[svc_row]["n"]
                                                for r in rows])
                else:
                    warn(f"{label}: no {svc_row} sample count; omitting mean_page_ns")
                agg["items_r"] = [r[f"total_r{tok}"]["n_items"] for r in rows]
            qout[f"r{tok}"] = agg
        entry[qt] = qout

    if setup_drivers is None:
        return None
    setup = {}
    for st in setup_drivers[0]:
        if _is_leg_row(st):
            continue
        v = stat([d[st]["total_ns"] for d in setup_drivers])
        v["n_items"] = setup_drivers[0][st]["n_items"]
        setup[st] = v
    entry["setup"] = setup
    return entry


def build_queries(results_dir, layout, unit, reps, tier):
    qtypes = rps_query_types(results_dir, tier, unit)
    if qtypes:
        return build_queries_rps(results_dir, tier, unit, reps, qtypes)
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
        if not ws and any(_TOTAL_R.match(st) for st in rows[0]):
            # Paced rows in an unpaced dir shape: a bench binary newer than the
            # runner that laid the bundle out (or a hand-assembled mix). Nothing
            # here is a concurrency cell, so converting would emit an empty
            # qtype entry that reads as a clean result.
            warn(f"query-{tier}-{unit} {qt}: rows are the paced-RPS generation "
                 f"but the bundle has the one-dir-per-rep layout — mismatched "
                 f"runner/bench pair? skipping {qt}")
            continue
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
        if not _is_leg_row(st):
            v = stat([d[st]["total_ns"] for d in drivers])
            v["n_items"] = drivers[0][st]["n_items"]
            setup[st] = v
    entry["setup"] = setup
    return entry


# ------------------------------------------------------- query load verdicts
_PROFILE_TAIL = re.compile(r"-\d+$")
_RATE_KEY = re.compile(r"^r\d+(?:\.\d+)?$")


def query_profile(unit):
    """The query_load profile a unit is judged against: the dataset MODEL name.

    The unit id is <dataset>-c<chunk> and the dataset name carries its
    per-ledger tx count — sac-6000-c1 -> sac, custom_token-3600-c1 ->
    custom_token.
    """
    m = _UNIT_CHUNK.match(unit)
    return _PROFILE_TAIL.sub("", m.group(1) if m else unit)


def rps_cells(qout):
    """The rate cells of one qtype entry, keyed r<rate>.

    The key match carries the weight: each verdict is a sibling of the cells
    that also carries target_rps, so matching on that field alone would count a
    verdict as a cell on any second pass over converted data.
    """
    return {k: v for k, v in qout.items()
            if _RATE_KEY.match(k) and isinstance(v, dict) and "target_rps" in v}


def has_rps_cells(queries):
    return any(rps_cells(qout)
               for units in queries.values() for entry in units.values()
               for qt, qout in entry.items() if qt != "setup")


def _cell_at(cells, floor):
    """The rate cell paced at `floor`, or None. Matched by VALUE, so the rate
    token's spelling never has to be guessed."""
    return next((k for k, c in cells.items()
                 if abs(c["target_rps"] - floor) <= 1e-9), None)


def _verdict_head(cell, rate_key, floor):
    """The fields both families state about the cell they judge."""
    v = {"rate": rate_key, "target_rps": float(floor)}
    if "achieved_rps" in cell:
        v["achieved_rps_m"] = cell["achieved_rps"]["m"]
    v["p99_ns"] = cell["p99"]["m"]
    return v


def _verdict_sla(cell, rate_key, floor, qt, tier):
    """The SLA verdict: the SCHEDULED p99 at this endpoint's SLA rate, against
    the p99 that endpoint carries in this storage tier. Every endpoint has one,
    getTransaction included."""
    v = _verdict_head(cell, rate_key, floor)
    threshold = SLA_P99_NS[qt][tier]
    v["threshold_ns"] = threshold
    v["pass"] = v["p99_ns"] <= threshold
    return v


def _verdict_e2e(cell, rate_key, floor):
    """The E2E-budget verdict: getTransaction's time INSIDE the RPC at the
    demand-derived rate, against its slice of the end-to-end budget. The
    scheduled p99 rides along as context and is never judged — the arrival rate
    this cell is paced at is a demand estimate, not an SLA the tail answers to."""
    v = _verdict_head(cell, rate_key, floor)
    in_rpc = cell["service"]["p99"]["m"]
    v["in_rpc"] = {"p99_ns": in_rpc, "threshold_ns": E2E_IN_RPC_P99_NS,
                   "pass": in_rpc <= E2E_IN_RPC_P99_NS}
    v["pass"] = v["in_rpc"]["pass"]
    return v


def attach_query_verdicts(queries, phase):
    """Attach each qtype's verdicts beside its rate cells, in place.

    Two families, never folded together. verdict_sla sits on every endpoint's
    cell at that endpoint's SLA floor. verdict_e2e sits on getTransaction's cell
    at the demand-derived floor for this profile and phase. Where the two floors
    coincide the one cell carries both.
    """
    idx = phase["phase"] - 1
    for tier, units in queries.items():
        for unit, entry in units.items():
            profile = query_profile(unit)
            e2e_floors = E2E_FLOORS_RPS.get(profile)
            for qt, qout in entry.items():
                if qt == "setup":
                    continue
                cells = rps_cells(qout)
                if not cells:
                    continue
                # --- the SLA family: one floor and one p99 per endpoint ---
                floor = SLA_FLOORS_RPS.get(qt)
                if floor is None or tier not in SLA_P99_NS.get(qt, {}):
                    warn(f"query-{tier}-{unit} {qt}: no SLA floor or p99 in "
                         f"targets.json; no SLA verdict")
                elif (hit := _cell_at(cells, floor)) is None:
                    warn(f"query-{tier}-{unit} {qt}: no cell at the SLA floor "
                         f"of {floor} rps; no SLA verdict")
                else:
                    qout["verdict_sla"] = _verdict_sla(cells[hit], hit, floor, qt, tier)
                # --- the E2E-budget probe: getTransaction alone ---
                if qt != "txhash":
                    continue
                if e2e_floors is None:
                    warn(f"query-{tier}-{unit}: no query_load profile {profile!r} "
                         f"in targets.json; no E2E-budget verdict")
                    continue
                if idx >= len(e2e_floors):
                    warn(f"query-{tier}-{unit} {qt}: no phase-{phase['phase']} "
                         f"E2E-probe floor in targets.json; no E2E-budget verdict")
                    continue
                e2e_floor = e2e_floors[idx]
                hit = _cell_at(cells, e2e_floor)
                if hit is None:
                    warn(f"query-{tier}-{unit} {qt}: no cell at the phase-"
                         f"{phase['phase']} E2E-probe floor of {e2e_floor} rps; "
                         f"no E2E-budget verdict")
                elif "service" not in cells[hit]:
                    warn(f"query-{tier}-{unit} {qt}: no service row at the "
                         f"E2E-probe floor of {e2e_floor} rps; no E2E-budget verdict")
                else:
                    qout["verdict_e2e"] = _verdict_e2e(cells[hit], hit, e2e_floor)


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
def manifest_entry(data):
    """Manifest entry for a converted run JSON. Identity fields are always
    present; the listing metadata (phase, machine, hostname, commit, branch)
    is additive and omitted when the run doesn't carry it, so the viewer must
    tolerate entries without it."""
    entry = {
        "id": data["run_id"],
        "name": data["run_name"],
        "date": data["run_date"],
        "kind": data["dataset"]["kind"],
        "path": f"runs/{data['run_id']}.json",
    }
    build = data.get("build") or {}
    extras = {
        "phase": (data.get("campaign") or {}).get("phase"),
        "machine": ((data.get("hardware") or {}).get("instance_type")
                    or (data.get("machine") or {}).get("instance")),
        "hostname": data.get("hostname"),
        "commit": build.get("commit"),
        "branch": build.get("branch"),
    }
    # Omit only unknown values: None (field absent) and empty strings (parsed
    # from blank metadata). A numeric 0 would be a real value and must survive.
    entry.update({k: v for k, v in extras.items() if v is not None and v != ""})
    # Per-profile hot ingest p99 (the aggregated median, ns), in unit order.
    # The run-index listing computes each row's pass/miss verdict from these
    # against the live phase goals in targets.json, so it never fetches run
    # files. Units without the stat (old vocabulary) are skipped; the field is
    # omitted when no unit carries it.
    ds = data.get("dataset") or {}
    hot = data.get("ingest_hot") or {}
    p99s = []
    for unit in ds.get("unit_order") or list(ds.get("unit_meta") or {}):
        try:
            v = hot[unit]["driver"]["ingest_total"]["p99"]["m"]
        except (KeyError, TypeError):
            continue
        p99s.append({"unit": unit, "p99_ns": v})
    if p99s:
        entry["ingest_p99"] = p99s
    return entry


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
    runs.sort(key=lambda r: (r["date"], r["id"]))
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
    if args.source_uri:
        campaign["source_uri"] = args.source_uri
    if args.notes:
        campaign["notes"] = args.notes
    if close_interval_ns is not None:
        campaign["close_interval_ns"] = close_interval_ns
    # Phase targets are campaign-layout only; legacy layouts stay byte-identical.
    matched_phase = None
    if layout == "campaign":
        matched_phase = match_phase(close_interval_ns)
        # The campaign's GOAL phase. A paced campaign names it with its pace; an
        # unpaced one (a cold-only query run has nothing to pace) states it in
        # the manifest instead, as campaign.query_phase. The runner refuses a
        # config where the two disagree, so the pace wins here without a check.
        goal_phase = matched_phase or phase_by_number(
            (metadata or {}).get("campaign", {}).get("query_phase"))
        if goal_phase is not None:
            campaign["phase"] = goal_phase["phase"]
        campaign["phase_targets"] = PHASE_TARGETS
        # RPS query cells are judged against the load-model floors: embed the
        # table (as phase_targets is) and, when the run has a goal phase, attach
        # each qtype's verdicts at the cells those floors name. The SLA floors
        # do not depend on the phase, but the E2E probe's do, so a run still
        # needs a goal phase before either family can be judged.
        if has_rps_cells(queries):
            campaign["query_load"] = QUERY_LOAD
            if goal_phase is not None:
                attach_query_verdicts(queries, goal_phase)
            else:
                warn("no phase from the close interval or manifest query_phase; "
                     "query load verdicts omitted")
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

    # Every check the run's own shape earns, most-central first. A run can earn
    # more than one: a paced campaign that also swept queries is judged on both
    # keeping up with the block model AND meeting the read-path target, and the
    # two answer different questions about different sections. `checks` carries
    # the first for readers that predate the list; `checks_all` carries them all.
    checks = []
    # The read path answers to two requirements, so it earns two checks: the SLA
    # one carries the whole target table (every cell states its own number), the
    # probe one the single in-RPC budget shared by every profile, phase and tier.
    query_checks = [
        {"kind": "query_sla", "targets_ns": SLA_P99_NS, "floors_rps": SLA_FLOORS_RPS,
         "label": "query p99 ≤ the per-endpoint SLA at the SLA rate",
         "applies_to": "queries"},
        {"kind": "query_e2e_probe", "threshold_ns": E2E_IN_RPC_P99_NS,
         "label": "getTransaction in-RPC p99 ≤ "
                  f"{E2E_IN_RPC_P99_NS // 1_000_000} ms at the demand-derived rate",
         "applies_to": "queries"},
    ]
    if layout == "campaign":
        # The keep-up check derives from the run's own pace: a matched phase
        # names it, any other pace is judged as itself, and an unpaced
        # catch-up run gets no keep-up check at all.
        if close_interval_ns:
            label = (f"Phase {matched_phase['phase']} block model "
                     f"({format_interval(close_interval_ns)})" if matched_phase
                     else f"{format_interval(close_interval_ns)} pace")
            checks.append({"kind": "block_keepup", "interval_ns": close_interval_ns,
                           "label": label, "applies_to": "ingest_hot"})
    elif args.dataset_kind == "synthetic":
        checks.append({"kind": "block_keepup", "interval_ns": 600000000,
                       "label": "600 ms block model", "applies_to": "ingest_hot"})
    if queries:
        checks.extend(query_checks)

    if checks:
        data["checks"] = checks[0]
        data["checks_all"] = checks

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

    manifest_path = update_manifest(args.out_dir, manifest_entry(data))

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
    ap.add_argument("--source-gcs")   # legacy GCS-only provenance; prefer --source-uri
    ap.add_argument("--source-uri")   # bundle provenance, s3:// or gs://
    ap.add_argument("--notes")
    ap.add_argument("--description")
    ap.add_argument("--out-dir", required=True)
    args = ap.parse_args(argv)
    convert(args)


if __name__ == "__main__":
    main()
