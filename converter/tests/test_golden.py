"""Golden tests against the two real local datasets. Skip cleanly (with a clear
message) when the source directories are absent."""
import argparse
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import convert  # noqa: E402

PUBNET_DIR = os.path.expanduser("~/Downloads/bench-063a/results")
SYNTH_DIR = os.path.expanduser("~/Downloads/bench-synth/results")
FACTS = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                     "facts", "synthetic-2026-07-15.json")


def run_convert(results_dir, **overrides):
    out_dir = tempfile.mkdtemp()
    args = argparse.Namespace(
        results_dir=results_dir, run_id="test", run_name="Test", run_date="2026-01-01",
        dataset_kind="pubnet", unit_facts=None, source_gcs=None, notes=None,
        description=None, out_dir=out_dir)
    for k, v in overrides.items():
        setattr(args, k, v)
    convert._warnings.clear()
    data = convert.convert(args)
    return data, list(convert._warnings)


@unittest.skipUnless(os.path.isdir(PUBNET_DIR),
                     f"pubnet dataset absent at {PUBNET_DIR}; skipping golden test")
class PubnetGoldenTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.data, cls.warnings = run_convert(
            PUBNET_DIR, run_id="pubnet-2026-07-13", dataset_kind="pubnet")

    def test_structural_validity(self):
        self.assertEqual(convert.validate_run(self.data, reps=5), [])

    def test_chunk_6345_cold_chunk_wall_median(self):
        m = self.data["ingest_cold"]["6345"]["driver"]["chunk_wall"]["total"]["m"]
        self.assertAlmostEqual(m, 61.7e9, delta=61.7e9 * 0.02)  # within 2%

    def test_queries_complete(self):
        q = self.data["queries"]
        self.assertEqual(set(q), {"cold", "hot"})
        for tier in ("cold", "hot"):
            self.assertEqual(set(q[tier]), {"3000", "5000", "6100", "6345"})
            for chunk in q[tier].values():
                qtypes = {k for k in chunk if k != "setup"}
                self.assertEqual(qtypes, {"ledgers", "txpage", "txhash", "events"})
                for qt in qtypes:
                    self.assertEqual(set(chunk[qt]), {"c1", "c4", "c16"})

    def test_every_v_has_five_runs_and_ordered(self):
        # reuse the validator's V invariants with reps=5 across the whole doc
        self.assertEqual(convert.validate_run(self.data, reps=5), [])

    def test_events_query_carries_items_r(self):
        c16 = self.data["queries"]["cold"]["6345"]["events"]["c16"]
        self.assertIn("items_r", c16)
        self.assertEqual(len(c16["items_r"]), 5)
        self.assertIn("items_s", c16)

    def test_golden_and_unit_meta(self):
        self.assertEqual(set(self.data["golden"]), {"3000", "5000", "6100", "6345"})
        um = self.data["dataset"]["unit_meta"]["6345"]
        self.assertEqual(um["seq_start"], 63450002)
        self.assertEqual(um["seq_end"], 63460001)

    def test_vocabulary_and_checks(self):
        self.assertEqual(self.data["campaign"]["vocabulary"], "old")
        self.assertEqual(self.data["checks"]["kind"], "query_p99_threshold")


@unittest.skipUnless(os.path.isdir(SYNTH_DIR),
                     f"synthetic dataset absent at {SYNTH_DIR}; skipping golden test")
class SynthGoldenTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.data, cls.warnings = run_convert(
            SYNTH_DIR, run_id="synthetic-2026-07-15", dataset_kind="synthetic",
            unit_facts=FACTS)

    def test_structural_validity(self):
        self.assertEqual(convert.validate_run(self.data, reps=5), [])

    def test_hot_ingest_total_medians(self):
        sac = self.data["ingest_hot"]["sac"]["driver"]["ingest_total"]
        self.assertEqual(sac["p50"]["m"], 143899396)
        self.assertEqual(sac["p99"]["m"], 919957614)
        self.assertEqual(self.data["ingest_hot"]["token"]["driver"]["ingest_total"]["p99"]["m"],
                         339513966)
        self.assertEqual(self.data["ingest_hot"]["soroswap"]["driver"]["ingest_total"]["p99"]["m"],
                         192360318)

    def test_cold_backfill_wall_present(self):
        self.assertIn("backfill_wall", self.data["ingest_cold"]["sac"]["driver"])

    def test_unit_meta_counts_and_facts(self):
        um = self.data["dataset"]["unit_meta"]["sac"]
        self.assertEqual(um["txs"], 59868022)
        self.assertEqual(um["events"], 59868008)
        self.assertEqual(um["model"], "SAC transfer")  # merged from --unit-facts
        self.assertEqual(um["tps"], 10000)

    def test_vocabulary_checks_sections(self):
        self.assertEqual(self.data["campaign"]["vocabulary"], "new")
        self.assertEqual(self.data["checks"]["kind"], "block_keepup")
        self.assertEqual(self.data["sections"], ["ingest_cold", "ingest_hot"])

    def test_clean_conversion(self):
        self.assertEqual(self.warnings, [])  # no warnings on the real dataset


if __name__ == "__main__":
    unittest.main()
