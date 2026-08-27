"""Tests for the open-loop (paced-RPS) query legs: the per-qtype bundle layout,
the r<rate> cells and their extra columns, and the 1x verdicts against the
docs/targets.json query_load floors. Bundles are materialised on disk by
fixtures.py so the whole convert() pipeline runs, stdlib only."""
import argparse
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))  # converter/
sys.path.insert(0, HERE)                    # converter/tests/ (fixtures)
import convert   # noqa: E402
import fixtures  # noqa: E402


def run_convert(results_dir, **overrides):
    out_dir = tempfile.mkdtemp()
    args = argparse.Namespace(
        results_dir=results_dir, run_id=None, run_name=None, run_date=None,
        dataset_kind="synthetic", unit_facts=None, source_gcs=None, source_uri=None,
        notes=None, description=None, out_dir=out_dir)
    for k, v in overrides.items():
        setattr(args, k, v)
    convert._warnings.clear()
    data = convert.convert(args)
    return data, list(convert._warnings), out_dir


def build_and_convert(**kw):
    root = os.path.join(tempfile.mkdtemp(), "b")
    fixtures.build_rps_bundle(root, **kw)
    return run_convert(root)


UNIT = f"{fixtures.RPS_DATASET}-c1"


class ProfileKeyTests(unittest.TestCase):
    def test_dataset_model_name(self):
        self.assertEqual(convert.query_profile("sac-6000-c1"), "sac")
        self.assertEqual(convert.query_profile("custom_token-3600-c1"), "custom_token")
        self.assertEqual(convert.query_profile("soroswap-1500-c2"), "soroswap")
        # only ONE trailing number is stripped, and only after the chunk suffix
        self.assertEqual(convert.query_profile("sac"), "sac")


class RpsLayoutDetectionTests(unittest.TestCase):
    def test_per_qtype_dirs_are_a_campaign(self):
        self.assertEqual(
            convert.detect_layout(["query-cold-sac-6000-c1-txhash-run1"]), "campaign")
        self.assertEqual(
            convert.detect_layout(["query-hot-soroswap-1500-c2-events-run12"]), "campaign")

    def test_qtype_discovery(self):
        tmp = tempfile.mkdtemp()
        root = os.path.join(tmp, "b")
        fixtures.build_rps_bundle(root)
        self.assertEqual(convert.rps_query_types(root, "cold", UNIT),
                         ["events", "ledgers", "txhash", "txpage"])
        # a neighbouring unit must not pick up this one's legs
        self.assertEqual(convert.rps_query_types(root, "cold", "sac-6000-c11"), [])


class RpsCellTests(unittest.TestCase):
    """Default fixture: phase 3 (600 ms), sac profile, every budget met."""
    @classmethod
    def setUpClass(cls):
        cls.data, cls.warnings, _ = build_and_convert()
        cls.cold = cls.data["queries"]["cold"][UNIT]

    def test_clean_and_structurally_valid(self):
        self.assertEqual(self.warnings, [])
        self.assertEqual(convert.validate_run(self.data, reps=2), [])

    def test_all_qtypes_and_setup(self):
        self.assertEqual(set(self.data["queries"]), {"cold", "hot"})
        self.assertEqual(set(self.cold),
                         {"ledgers", "txpage", "txhash", "events", "setup"})

    def test_cells_are_rate_tokens_sorted_ascending(self):
        # sorted by VALUE, not by string: r500 < r1000 < r2000.
        self.assertEqual([k for k in self.cold["txhash"] if k != "verdict_1x"],
                         ["r500", "r1000", "r2000"])
        self.assertEqual([k for k in self.cold["ledgers"] if k != "verdict_1x"],
                         ["r0.835", "r1.67", "r3.34"])

    def test_headline_is_the_scheduled_latency(self):
        cell = self.cold["txhash"]["r1000"]
        s = fixtures.RPS_SCHED_P99_NS
        self.assertEqual(cell["p99"], {"m": s, "lo": s, "hi": s * 2, "r": [s, s * 2]})
        self.assertEqual(cell["target_rps"], 1000.0)
        # service (dispatch->done) is a separate, secondary column
        self.assertEqual(cell["service"]["p99"]["m"], fixtures.RPS_SERVICE_P99_NS)

    def test_achieved_rps_is_millirps_over_1000(self):
        cell = self.cold["txhash"]["r1000"]
        self.assertEqual(cell["achieved_rps"],
                         {"m": 999.998, "lo": 999.998, "hi": 999.998,
                          "r": [999.998, 999.998]})
        self.assertEqual(self.cold["ledgers"]["r0.835"]["achieved_rps"]["m"], 0.833)

    def test_wall_dispatch_lag_and_shed(self):
        cell = self.cold["txhash"]["r1000"]
        self.assertEqual(cell["wall"]["r"], [100_000_000, 200_000_000])
        self.assertEqual(cell["dispatch_lag"]["p50"]["m"], 0)   # zeros are kept
        self.assertEqual(cell["dispatch_lag"]["n"], 100)
        self.assertEqual(cell["shed"]["r"], [0, 0])
        # only the top rung sheds at the in-flight cap
        self.assertEqual(self.cold["txhash"]["r2000"]["shed"]["m"], fixtures.RPS_SHED)

    def test_events_mean_page_and_items(self):
        cell = self.cold["events"]["r10"]
        self.assertEqual(cell["mean_page_ns"]["m"], float(fixtures.RPS_MEAN_PAGE_NS))
        self.assertEqual(cell["items_r"], [1000, 1000])
        self.assertNotIn("mean_page_ns", self.cold["txhash"]["r1000"])

    def test_setup_excludes_every_per_leg_row(self):
        self.assertEqual(list(self.cold["setup"]), ["open", "evict", "peak_rss_bytes"])
        hot = self.data["queries"]["hot"][UNIT]["setup"]
        self.assertEqual(list(hot), ["open", "peak_rss_bytes"])   # only cold evicts

    def test_query_load_embedded_in_campaign(self):
        self.assertEqual(self.data["campaign"]["query_load"], convert.QUERY_LOAD)

    def test_verdicts_pass_at_the_phase3_floors(self):
        for qt, rate in (("ledgers", "r1.67"), ("txpage", "r50"),
                         ("txhash", "r1000"), ("events", "r10")):
            v = self.cold[qt]["verdict_1x"]
            self.assertEqual(v["rate"], rate)
            self.assertTrue(v["pass"])
            self.assertEqual(v["threshold_ns"], convert.QUERY_P99_TARGET_NS)
            self.assertEqual(v["p99_ns"], fixtures.RPS_SCHED_P99_NS)
        self.assertEqual(self.cold["txhash"]["verdict_1x"]["target_rps"], 1000.0)
        self.assertEqual(self.cold["txhash"]["verdict_1x"]["achieved_rps_m"], 999.998)

    def test_txhash_in_rpc_is_a_separate_column(self):
        v = self.cold["txhash"]["verdict_1x"]
        self.assertEqual(v["in_rpc"], {"p99_ns": fixtures.RPS_SERVICE_P99_NS,
                                       "threshold_ns": 10_000_000, "pass": True})
        self.assertNotIn("in_rpc", self.cold["ledgers"]["verdict_1x"])

    def test_events_page_budget_is_a_separate_column(self):
        v = self.cold["events"]["verdict_1x"]
        self.assertEqual(v["page_budget"], {"mean_ns": 80_000_000.0,
                                            "budget_ns": 100_000_000, "pass": True})
        self.assertNotIn("page_budget", self.cold["txpage"]["verdict_1x"])


class RpsVerdictFailureTests(unittest.TestCase):
    def test_scheduled_p99_over_the_sla_fails(self):
        data, _, _ = build_and_convert(sched_p99={"txhash": 700_000_000})
        v = data["queries"]["cold"][UNIT]["txhash"]["verdict_1x"]
        self.assertEqual(v["p99_ns"], 700_000_000)
        self.assertFalse(v["pass"])
        # a sibling type at the same rate ladder is unaffected
        self.assertTrue(data["queries"]["cold"][UNIT]["ledgers"]["verdict_1x"]["pass"])

    def test_in_rpc_budget_fails_without_failing_the_headline(self):
        data, _, _ = build_and_convert(svc_p99={"txhash": 12_000_000})
        v = data["queries"]["cold"][UNIT]["txhash"]["verdict_1x"]
        self.assertTrue(v["pass"])                 # scheduled p99 still inside 500 ms
        self.assertFalse(v["in_rpc"]["pass"])      # but over the 10 ms in-RPC budget
        self.assertEqual(v["in_rpc"]["p99_ns"], 12_000_000)

    def test_page_budget_fails_without_failing_the_headline(self):
        data, _, _ = build_and_convert(mean_page={"events": 150_000_000})
        v = data["queries"]["cold"][UNIT]["events"]["verdict_1x"]
        self.assertTrue(v["pass"])
        self.assertFalse(v["page_budget"]["pass"])
        self.assertEqual(v["page_budget"]["mean_ns"], 150_000_000.0)


class RpsVerdictOmissionTests(unittest.TestCase):
    def test_no_phase_no_verdict(self):
        data, warnings, _ = build_and_convert(close_interval="1500ms")
        cold = data["queries"]["cold"][UNIT]
        self.assertNotIn("phase", data["campaign"])
        self.assertEqual(data["campaign"]["query_load"], convert.QUERY_LOAD)
        for qt in ("ledgers", "txpage", "txhash", "events"):
            self.assertNotIn("verdict_1x", cold[qt])
        self.assertTrue(any("verdicts omitted" in w for w in warnings))
        self.assertEqual(convert.validate_run(data, reps=2), [])

    def test_unknown_profile_warns_once_per_unit(self):
        data, warnings, _ = build_and_convert(dataset="mystery-9000")
        cold = data["queries"]["cold"]["mystery-9000-c1"]
        self.assertNotIn("verdict_1x", cold["txhash"])
        hits = [w for w in warnings if "no query_load profile 'mystery'" in w]
        self.assertEqual(len(hits), 2)             # one per tier, not per qtype
        self.assertEqual(convert.validate_run(data, reps=2), [])

    def test_no_cell_at_the_floor(self):
        # sac phase-3 txpage floor is 50 rps; this ladder never reaches it.
        data, warnings, _ = build_and_convert(rates={"txpage": ["1", "2", "4"]})
        self.assertNotIn("verdict_1x", data["queries"]["cold"][UNIT]["txpage"])
        self.assertTrue(any("no cell at the phase-3 floor of 50 rps" in w
                            for w in warnings))


class RpsRateTokenTests(unittest.TestCase):
    """Fractional rates must survive verbatim as cell keys and parse back."""
    @classmethod
    def setUpClass(cls):
        cls.data, cls.warnings, _ = build_and_convert(
            dataset="soroswap-1500", close_interval="2s",
            rates={"ledgers": ["0.25", "0.5", "1"],
                   "txpage": ["3.75", "7.5", "15"]})
        cls.cold = cls.data["queries"]["cold"]["soroswap-1500-c1"]

    def test_tokens_kept_verbatim(self):
        self.assertEqual([k for k in self.cold["txpage"] if k != "verdict_1x"],
                         ["r3.75", "r7.5", "r15"])
        self.assertEqual(self.cold["txpage"]["r3.75"]["target_rps"], 3.75)
        self.assertEqual(self.cold["ledgers"]["r0.5"]["target_rps"], 0.5)

    def test_phase1_soroswap_floors_matched(self):
        # soroswap phase 1: txpage 3.75 rps, ledgers 0.5 rps — the 1x cell is
        # the floor itself, wherever it sits in the ladder the leg swept.
        self.assertEqual(self.data["campaign"]["phase"], 1)
        self.assertEqual(self.cold["txpage"]["verdict_1x"]["rate"], "r3.75")
        self.assertEqual(self.cold["txpage"]["verdict_1x"]["target_rps"], 3.75)
        self.assertEqual(self.cold["ledgers"]["verdict_1x"]["rate"], "r0.5")
        self.assertEqual(self.cold["ledgers"]["verdict_1x"]["target_rps"], 0.5)

    def test_clean(self):
        self.assertEqual(self.warnings, [])
        self.assertEqual(convert.validate_run(self.data, reps=2), [])


class RpsMissingRepTests(unittest.TestCase):
    def test_missing_leg_dir_warns(self):
        tmp = tempfile.mkdtemp()
        root = os.path.join(tmp, "b")
        fixtures.build_rps_bundle(root)
        shutil.rmtree(os.path.join(root, f"query-cold-{UNIT}-events-run2"))
        _, warnings, _ = run_convert(root)
        self.assertTrue(any(f"missing run dir: query-cold-{UNIT}-events-run2" in w
                            for w in warnings))


class ClosedLoopStillWorksTests(unittest.TestCase):
    """The published generation — one dir per (unit, rep), c<W> cells — must
    convert exactly as before, untouched by the RPS path."""
    @classmethod
    def setUpClass(cls):
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=True,
                                       close_interval="600ms", queries=True)
        cls.data, cls.warnings, _ = run_convert(os.path.join(tmp, "b"))
        cls.cold = cls.data["queries"]["cold"]["sac-6000-c1"]

    def test_concurrency_cells(self):
        for qt in ("ledgers", "txpage", "txhash", "events"):
            self.assertEqual(list(self.cold[qt]), ["c1", "c4"])
            self.assertNotIn("verdict_1x", self.cold[qt])
            self.assertNotIn("target_rps", self.cold[qt]["c1"])
            self.assertIn("ops_s", self.cold[qt]["c1"])

    def test_no_query_load_embed(self):
        self.assertNotIn("query_load", self.data["campaign"])
        self.assertEqual(self.data["campaign"]["phase"], 3)

    def test_setup_unchanged(self):
        self.assertEqual(list(self.cold["setup"]), ["open", "evict", "peak_rss_bytes"])
        self.assertEqual(convert.validate_run(self.data, reps=2), [])


if __name__ == "__main__":
    unittest.main()
