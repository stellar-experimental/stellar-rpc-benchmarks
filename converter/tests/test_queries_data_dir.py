"""Query legs of `bench query --data-dir` (stellar-rpc#1070): latency.csv and
scenarios.csv convert to the cells and verdicts the tiered legs give."""
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import convert   # noqa: E402
import fixtures  # noqa: E402
from test_queries_rps import UNIT, run_convert   # noqa: E402


def build(data_dir, eviction="requested"):
    root = os.path.join(tempfile.mkdtemp(), "b")
    fixtures.build_rps_bundle(root)
    fixtures.to_run_records(root)
    if data_dir:
        fixtures.to_data_dir_legs(root, eviction=eviction)
    return root


class DataDirCellTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tiered, cls.tiered_warnings, _ = run_convert(build(data_dir=False))
        cls.data, cls.warnings, _ = run_convert(build(data_dir=True))

    def test_clean_and_structurally_valid(self):
        self.assertEqual(self.warnings, [])
        self.assertEqual(convert.validate_run(self.data, reps=2), [])

    def test_generation_marker(self):
        self.assertEqual(self.data["campaign"]["query_cli"], "data-dir")
        self.assertEqual(self.tiered["campaign"]["query_cli"], "tiered")

    def test_cells_and_verdicts_match_the_tiered_legs(self):
        for tier in ("cold", "hot"):
            got, want = self.data["queries"][tier][UNIT], self.tiered["queries"][tier][UNIT]
            for qt in ("ledgers", "txpage", "txhash", "events"):
                self.assertEqual(got[qt], want[qt], f"{tier} {qt}")

    def test_verdicts_present(self):
        txhash = self.data["queries"]["cold"][UNIT]["txhash"]
        self.assertTrue(txhash["verdict_sla"]["pass"])
        self.assertTrue(txhash["verdict_e2e"]["pass"])

    def test_setup_carries_open_evict_and_peak(self):
        cold = self.data["queries"]["cold"][UNIT]["setup"]
        want = self.tiered["queries"]["cold"][UNIT]["setup"]
        self.assertEqual(list(cold), ["open", "evict", "peak_rss_bytes"])
        for row in cold:
            self.assertEqual(cold[row]["r"], want[row]["r"], row)
        self.assertEqual(list(self.data["queries"]["hot"][UNIT]["setup"]),
                         ["open", "peak_rss_bytes"])


class DataDirWarningTests(unittest.TestCase):
    def test_cold_leg_without_eviction_warns(self):
        data, warnings, _ = run_convert(build(data_dir=True, eviction="unsupported-on-this-platform"))
        self.assertTrue(any("query-cold-" in w and "warm-cache" in w for w in warnings), warnings)
        self.assertFalse(any("query-hot-" in w for w in warnings), warnings)
        self.assertNotIn("evict", data["queries"]["cold"][UNIT]["setup"])

    def test_tier_mismatch_warns(self):
        root = build(data_dir=True)
        leg = os.path.join(root, f"query-hot-{UNIT}-txhash-run1")
        path = os.path.join(leg, "run.json")
        with open(path) as f:
            text = f.read()
        with open(path, "w") as f:
            f.write(text.replace('"tier": "hot"', '"tier": "cold"'))
        _, warnings, _ = run_convert(root)
        self.assertTrue(any("run.json tier 'cold'" in w for w in warnings), warnings)

    def test_leg_with_no_report_is_skipped(self):
        root = build(data_dir=True)
        for r in (1, 2):
            leg = os.path.join(root, f"query-hot-{UNIT}-events-run{r}")
            for name in ("latency.csv", "scenarios.csv"):
                os.remove(os.path.join(leg, name))
        data, warnings, _ = run_convert(root)
        self.assertNotIn("events", data["queries"]["hot"][UNIT])
        self.assertTrue(any("events: no CSVs" in w for w in warnings), warnings)


class TieredRunRecordTests(unittest.TestCase):
    """A tiered `bench query cold|hot` bundle with run.json legs, as
    bench-campaign 2ff814a6 writes it, still converts."""
    def test_tiered_run_json_bundle_converts(self):
        data, warnings, _ = run_convert(build(data_dir=False))
        self.assertEqual(warnings, [])
        self.assertEqual(convert.validate_run(data, reps=2), [])
        self.assertEqual(data["campaign"]["query_cli"], "tiered")
        self.assertIn("verdict_sla", data["queries"]["cold"][UNIT]["txhash"])


if __name__ == "__main__":
    unittest.main()
