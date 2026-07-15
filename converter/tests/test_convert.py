"""Unit tests for converter core math, vocabulary detection, manifest handling,
and the structural validator. Uses tiny synthetic CSV fixtures (stdlib only)."""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import convert  # noqa: E402


HEADER = "stage,n,n_items,total_ns,p50_ns,p90_ns,p99_ns,max_ns\n"


def write_csv(path, rows):
    """rows: list of (stage, n, n_items, total, p50, p90, p99, max)."""
    with open(path, "w") as f:
        f.write(HEADER)
        for r in rows:
            f.write(",".join(str(x) for x in r) + "\n")


class StatTests(unittest.TestCase):
    def test_median_of_5_is_middle_element(self):
        v = convert.stat([5, 1, 3, 2, 4])
        self.assertEqual(v["m"], 3)      # median = middle, never interpolated
        self.assertEqual(v["lo"], 1)
        self.assertEqual(v["hi"], 5)
        self.assertEqual(v["r"], [5, 1, 3, 2, 4])  # raw order preserved

    def test_median_picks_a_real_value(self):
        vals = [920089609, 909496734, 919957614, 951894589, 917963863]
        self.assertEqual(convert.stat(vals)["m"], 919957614)
        self.assertIn(convert.stat(vals)["m"], vals)

    def test_r_array_preserved_and_not_sorted(self):
        v = convert.stat([10.0, 30.0, 20.0])
        self.assertEqual(v["r"], [10.0, 30.0, 20.0])


class StageAggTests(unittest.TestCase):
    def _rows(self, totals):
        # one {stage: {...}} per run for stage "x"
        return [{"x": {"n": 100, "n_items": 200, "total_ns": t,
                       "p50_ns": t, "p90_ns": t, "p99_ns": t, "max_ns": t}}
                for t in totals]

    def test_maps_columns_and_constant_n(self):
        agg = convert.stage_agg(self._rows([3, 1, 2, 5, 4]), "x")
        self.assertEqual(set(agg), {"total", "p50", "p90", "p99", "max", "n", "n_items"})
        self.assertEqual(agg["total"]["m"], 3)
        self.assertEqual(agg["n"], 100)
        self.assertEqual(agg["n_items"], 200)

    def test_warns_when_n_items_varies(self):
        rows = self._rows([1, 2, 3, 4, 5])
        rows[2]["x"]["n_items"] = 999
        before = len(convert._warnings)
        convert.stage_agg(rows, "x")
        self.assertGreater(len(convert._warnings), before)

    def test_no_nitems_warning_when_suppressed(self):
        rows = self._rows([1, 2, 3, 4, 5])
        rows[2]["x"]["n_items"] = 999
        before = len(convert._warnings)
        convert.stage_agg(rows, "x", warn_nitems=False)
        self.assertEqual(len(convert._warnings), before)


class VocabularyTests(unittest.TestCase):
    def test_old_vs_new_detection(self):
        with tempfile.TemporaryDirectory() as d:
            old = os.path.join(d, "ingest-cold-3000-run1")
            new = os.path.join(d, "synth-cold-sac-run1")
            os.makedirs(old)
            os.makedirs(new)
            write_csv(os.path.join(old, "driver.csv"),
                      [("chunk_wall", 1, 0, 100, 100, 100, 100, 100),
                       ("ledgers_total", 1, 10, 5, 5, 5, 5, 5)])
            write_csv(os.path.join(new, "driver.csv"),
                      [("backfill_wall", 1, 0, 100, 100, 100, 100, 100),
                       ("ledgers_total", 1, 10, 5, 5, 5, 5, 5)])
            self.assertEqual(convert.detect_vocabulary(d, "pubnet", "3000", [1]), "old")
            self.assertEqual(convert.detect_vocabulary(d, "synthetic", "sac", [1]), "new")


class LayoutDiscoveryTests(unittest.TestCase):
    def test_layout_detection(self):
        self.assertEqual(convert.detect_layout(["synth-cold-sac-run1"]), "synthetic")
        self.assertEqual(convert.detect_layout(["ingest-cold-3000-run1"]), "pubnet")
        self.assertEqual(convert.detect_layout(["golden-download-3000"]), "pubnet")
        self.assertIsNone(convert.detect_layout(["random-dir"]))

    def test_units_reps_discovery(self):
        names = [f"ingest-cold-{c}-run{r}" for c in ("3000", "6345") for r in range(1, 6)]
        units, reps = convert.discover_units_reps(names, "pubnet")
        self.assertEqual(units, {"3000", "6345"})
        self.assertEqual(reps, [1, 2, 3, 4, 5])

    def test_order_units_numeric_and_facts(self):
        self.assertEqual(
            convert.order_units({"6345", "3000", "5000"}, "pubnet", {}),
            ["3000", "5000", "6345"])
        facts = {"sac": {}, "token": {}, "soroswap": {}}
        self.assertEqual(
            convert.order_units({"soroswap", "sac", "token"}, "synthetic", facts),
            ["sac", "token", "soroswap"])


class DerivedRateTests(unittest.TestCase):
    def test_per_run_derived_then_aggregated(self):
        # ledgers=10000; walls 1s,2s,4s -> rates 10000, 5000, 2500 -> median 5000
        drivers = [{"backfill_wall": {"total_ns": w}} for w in (1_000_000_000, 2_000_000_000, 4_000_000_000)]
        v = convert.stat([10000 / (d["backfill_wall"]["total_ns"] / convert.NS) for d in drivers])
        self.assertEqual(v["m"], 5000.0)
        self.assertEqual(v["lo"], 2500.0)
        self.assertEqual(v["hi"], 10000.0)
        self.assertEqual(len(v["r"]), 3)


class ValidatorTests(unittest.TestCase):
    def _minimal(self):
        V = {"m": 3, "lo": 1, "hi": 5, "r": [3, 1, 5]}
        sa = {"total": dict(V), "p50": dict(V), "p90": dict(V),
              "p99": dict(V), "max": dict(V), "n": 1, "n_items": 2}
        return {
            "schema_version": 1, "run_id": "x", "run_name": "X", "run_date": "2026-01-01",
            "machine": {"raw": "..."}, "build": {}, "dataset": {
                "kind": "synthetic", "description": "d", "units": "profile",
                "unit_label": "Profile", "unit_order": ["sac"], "unit_meta": {"sac": {}}},
            "campaign": {"reps": 3, "vocabulary": "new"},
            "checks": {"kind": "block_keepup"},
            "ingest_cold": {"sac": {"driver": {"backfill_wall": sa}}},
            "sections": ["ingest_cold"],
        }

    def test_valid_passes(self):
        self.assertEqual(convert.validate_run(self._minimal(), reps=3), [])

    def test_lo_hi_mismatch_flagged(self):
        d = self._minimal()
        d["ingest_cold"]["sac"]["driver"]["backfill_wall"]["total"]["lo"] = 999
        errs = convert.validate_run(d, reps=3)
        self.assertTrue(any("lo" in e for e in errs))

    def test_sections_mismatch_flagged(self):
        d = self._minimal()
        d["sections"] = ["ingest_cold", "queries"]  # queries not present
        self.assertTrue(any("sections" in e for e in convert.validate_run(d)))

    def test_reps_length_enforced(self):
        d = self._minimal()
        self.assertTrue(any("len(r)" in e for e in convert.validate_run(d, reps=5)))

    def test_missing_required_key(self):
        d = self._minimal()
        del d["machine"]
        self.assertTrue(any("machine" in e for e in convert.validate_run(d, reps=3)))


class ManifestTests(unittest.TestCase):
    def test_insert_replace_and_sort(self):
        with tempfile.TemporaryDirectory() as d:
            convert.update_manifest(d, {"id": "a", "name": "A", "date": "2026-01-01",
                                        "kind": "pubnet", "path": "runs/a.json"})
            convert.update_manifest(d, {"id": "b", "name": "B", "date": "2026-03-01",
                                        "kind": "synthetic", "path": "runs/b.json"})
            # replace "a" with a newer date -> should move to front and not duplicate
            convert.update_manifest(d, {"id": "a", "name": "A2", "date": "2026-05-01",
                                        "kind": "pubnet", "path": "runs/a.json"})
            with open(os.path.join(d, "index.json")) as fh:
                man = json.load(fh)
            self.assertEqual(man["schema_version"], 1)
            ids = [r["id"] for r in man["runs"]]
            self.assertEqual(ids, ["a", "b"])           # date desc: 2026-05, 2026-03
            self.assertEqual(len(man["runs"]), 2)        # replaced, not duplicated
            self.assertEqual(man["runs"][0]["name"], "A2")


if __name__ == "__main__":
    unittest.main()
