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


def detect_layout(names):
    if any(n.startswith("synth-") for n in names):
        return "synthetic"
    if any(n.startswith("ingest-") or n.startswith("golden-download-") for n in names):
        return "pubnet"
    return None


def discover_units_reps(names, layout):
    """Discover unit ids and rep numbers from the cold ingest dir names."""
    pat = (re.compile(r"^synth-cold-(.+)-run(\d+)$") if layout == "synthetic"
           else re.compile(r"^ingest-cold-(.+)-run(\d+)$"))
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


# ----------------------------------------------------------------- sections
def build_ingest_cold(results_dir, layout, unit, reps, vocab, counts):
    dirs = run_dirs(results_dir, layout, "cold", unit, reps)
    drivers = read_all(dirs, "driver.csv")
    if not drivers:
        warn(f"no cold driver.csv for unit {unit}")
        return None
    driver_out = {st: stage_agg(drivers, st) for st in drivers[0]}

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
    return {"driver": driver_out, "files": files_out, "derived": derived}


def build_ingest_hot(results_dir, layout, unit, reps, vocab, counts):
    dirs = run_dirs(results_dir, layout, "hot", unit, reps)
    drivers = read_all(dirs, "driver.csv")
    hots = read_all(dirs, "hot.csv")
    if not drivers or not hots:
        warn(f"incomplete hot data for unit {unit}")
        return None
    driver_out = {st: stage_agg(drivers, st) for st in drivers[0]}
    phases_out = {st: stage_agg(hots, st) for st in hots[0]}
    wall = "chunk_wall" if vocab == "old" else "run_wall"
    derived = {"ledgers_per_s": stat([counts["ledgers"] / (d[wall]["total_ns"] / NS) for d in drivers])}
    return {"driver": driver_out, "phases": phases_out, "derived": derived}


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
    if args.dataset_kind == "pubnet" and layout != "pubnet":
        warn(f"--dataset-kind pubnet but directory layout looks {layout}")
    if args.dataset_kind == "synthetic" and layout != "synthetic":
        warn(f"--dataset-kind synthetic but directory layout looks {layout}")

    vocab = detect_vocabulary(results_dir, layout, sorted(units)[0], reps)

    facts = {}
    if args.unit_facts:
        with open(os.path.expanduser(args.unit_facts)) as f:
            facts = json.load(f)

    machine, build = load_metadata(results_dir)
    counts = {u: unit_counts(results_dir, layout, u, reps) for u in units}
    unit_order = order_units(units, args.dataset_kind, facts)

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
    if layout == "pubnet":
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
        for u in unit_order:
            g = build_golden(results_dir, u)
            if g is not None:
                golden[u] = g

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

    data = {
        "schema_version": 1,
        "run_id": args.run_id,
        "run_name": args.run_name,
        "run_date": args.run_date,
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
        "campaign": {"reps": len(reps), "vocabulary": vocab},
    }
    if args.source_gcs:
        data["campaign"]["source_gcs"] = args.source_gcs
    if args.notes:
        data["campaign"]["notes"] = args.notes

    if args.dataset_kind == "synthetic":
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
    out_path = os.path.join(args.out_dir, f"{args.run_id}.json")
    with open(out_path, "w") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")

    manifest_path = update_manifest(args.out_dir, {
        "id": args.run_id, "name": args.run_name, "date": args.run_date,
        "kind": args.dataset_kind, "path": f"runs/{args.run_id}.json",
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
    ap.add_argument("--run-id", required=True)
    ap.add_argument("--run-name", required=True)
    ap.add_argument("--run-date", required=True)
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
