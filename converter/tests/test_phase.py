"""Tests for phase matching (Phase 1/2/3 performance targets) and the phase
fields emitted into campaign-layout run JSON. Bundles are materialised on disk
by fixtures.py so the whole convert() pipeline runs, stdlib only."""
import argparse
import os
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
        dataset_kind="synthetic", unit_facts=None, source_gcs=None, source_uri=None, notes=None,
        description=None, out_dir=out_dir)
    for k, v in overrides.items():
        setattr(args, k, v)
    convert._warnings.clear()
    data = convert.convert(args)
    return data, list(convert._warnings), out_dir


class MatchPhaseTests(unittest.TestCase):
    def test_exact_matches(self):
        self.assertEqual(convert.match_phase(2_000_000_000)["phase"], 1)
        self.assertEqual(convert.match_phase(1_000_000_000)["phase"], 2)
        self.assertEqual(convert.match_phase(600_000_000)["phase"], 3)

    def test_custom_pace_no_phase(self):
        self.assertIsNone(convert.match_phase(1_500_000_000))
        self.assertIsNone(convert.match_phase(500_000_000))

    def test_unpaced_no_phase(self):
        self.assertIsNone(convert.match_phase(0))
        self.assertIsNone(convert.match_phase(None))


class PhaseTableTests(unittest.TestCase):
    def test_three_phases_exact_numbers(self):
        by = {p["phase"]: p for p in convert.PHASE_TARGETS}
        self.assertEqual(sorted(by), [1, 2, 3])
        self.assertEqual(by[1]["block_time_ns"], 2_000_000_000)
        self.assertEqual(by[2]["block_time_ns"], 1_000_000_000)
        self.assertEqual(by[3]["block_time_ns"], 600_000_000)
        self.assertEqual(by[1]["e2e_budget_ns"], 5_000_000_000)
        self.assertEqual(by[2]["e2e_budget_ns"], 2_500_000_000)
        self.assertEqual(by[3]["e2e_budget_ns"], 2_000_000_000)
        self.assertEqual(by[1]["ingest_p99_target_ns"], 905_000_000)
        self.assertEqual(by[3]["ingest_p99_target_ns"], 105_000_000)
        self.assertEqual(by[1]["orgs"], 10)
        self.assertEqual(by[2]["orgs"], 19)
        self.assertEqual(by[3]["orgs"], 31)
        self.assertEqual(by[1]["retention"], "3 months")
        self.assertEqual(by[2]["retention"], "6 months")
        self.assertEqual(by[3]["retention"], "2 years")
        wl = {p["phase"]: {w["name"]: w for w in p["workloads"]}
              for p in convert.PHASE_TARGETS}
        self.assertEqual(wl[1]["SAC transfers"], {"name": "SAC transfers", "tps": 3000, "tx_per_ledger": 6000})
        self.assertEqual(wl[2]["SAC transfers"], {"name": "SAC transfers", "tps": 5000, "tx_per_ledger": 5000})
        self.assertEqual(wl[3]["SAC transfers"], {"name": "SAC transfers", "tps": 10000, "tx_per_ledger": 6000})
        self.assertEqual(wl[1]["OZ token transfers"], {"name": "OZ token transfers", "tps": 2000, "tx_per_ledger": 4000})
        self.assertEqual(wl[2]["OZ token transfers"], {"name": "OZ token transfers", "tps": 4000, "tx_per_ledger": 4000})
        self.assertEqual(wl[3]["OZ token transfers"], {"name": "OZ token transfers", "tps": 6000, "tx_per_ledger": 3600})
        self.assertEqual(wl[1]["Soroswap swaps"], {"name": "Soroswap swaps", "tps": 750, "tx_per_ledger": 1500})
        self.assertEqual(wl[2]["Soroswap swaps"], {"name": "Soroswap swaps", "tps": 1500, "tx_per_ledger": 1500})
        self.assertEqual(wl[3]["Soroswap swaps"], {"name": "Soroswap swaps", "tps": 3000, "tx_per_ledger": 1800})

    def test_phase2_ingest_target_derived_from_e2e_budget(self):
        # Phase 2 states no explicit ingest-slice target; it is derived from the
        # e2e budget: 2.5s = 25ms + 10ms + 1s*2 + ingest + 50ms + 10ms
        # -> ingest = 405ms.
        by = {p["phase"]: p for p in convert.PHASE_TARGETS}
        self.assertEqual(by[2]["ingest_p99_target_ns"], 405_000_000)

    def test_ingest_p99_target_formula(self):
        self.assertEqual(
            convert.ingest_p99_target(1_000_000_000, 2_500_000_000), 405_000_000)
        # Explicit phase-1 target matches the formula.
        self.assertEqual(
            convert.ingest_p99_target(2_000_000_000, 5_000_000_000), 905_000_000)


class PhaseEmissionTests(unittest.TestCase):
    def test_paced_phase1_run(self):
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=True)  # 2s
        data, _, _ = run_convert(os.path.join(tmp, "b"))
        self.assertEqual(data["campaign"]["close_interval_ns"], 2_000_000_000)
        self.assertEqual(data["campaign"]["phase"], 1)
        self.assertEqual(data["campaign"]["phase_targets"], convert.PHASE_TARGETS)
        self.assertEqual(data["checks"], {
            "kind": "block_keepup", "interval_ns": 2_000_000_000,
            "label": "Phase 1 block model (2 s)", "applies_to": "ingest_hot"})

    def test_paced_phase3_run(self):
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=True,
                                       close_interval="600ms")
        data, _, _ = run_convert(os.path.join(tmp, "b"))
        self.assertEqual(data["campaign"]["phase"], 3)
        self.assertEqual(data["checks"]["interval_ns"], 600_000_000)
        self.assertEqual(data["checks"]["label"], "Phase 3 block model (600 ms)")

    def test_custom_pace_no_phase_but_keepup(self):
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=True,
                                       close_interval="1500ms")
        data, _, _ = run_convert(os.path.join(tmp, "b"))
        self.assertNotIn("phase", data["campaign"])
        self.assertEqual(data["campaign"]["phase_targets"], convert.PHASE_TARGETS)
        self.assertEqual(data["checks"], {
            "kind": "block_keepup", "interval_ns": 1_500_000_000,
            "label": "1.5 s pace", "applies_to": "ingest_hot"})

    def test_unpaced_no_phase_no_keepup(self):
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=False)
        data, _, _ = run_convert(os.path.join(tmp, "b"))
        self.assertNotIn("phase", data["campaign"])
        self.assertEqual(data["campaign"]["phase_targets"], convert.PHASE_TARGETS)
        self.assertNotIn("checks", data)

    def test_paced_run_with_queries_carries_every_check(self):
        # A paced campaign that also swept queries is judged three ways: it must
        # keep up with the block model, meet the read-path SLA, and hold
        # getTransaction inside its slice of the end-to-end budget. Emitting
        # only the keep-up check would leave the query cells unjudged.
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=True,
                                       close_interval="600ms", queries=True)
        data, _, _ = run_convert(os.path.join(tmp, "b"))
        self.assertIn("queries", data["sections"])
        # The primary stays the keep-up check, so readers written before the
        # list see exactly what they saw before.
        self.assertEqual(data["checks"]["kind"], "block_keepup")
        self.assertEqual([c["kind"] for c in data["checks_all"]],
                         ["block_keepup", "query_sla", "query_e2e_probe"])
        self.assertEqual(data["checks_all"][0], data["checks"])
        sla = next(c for c in data["checks_all"] if c["kind"] == "query_sla")
        self.assertEqual(sla["targets_ns"], convert.SLA_P99_NS)
        self.assertEqual(sla["floors_rps"], convert.SLA_FLOORS_RPS)
        self.assertEqual(sla["applies_to"], "queries")
        probe = next(c for c in data["checks_all"] if c["kind"] == "query_e2e_probe")
        self.assertEqual(probe["threshold_ns"], 10_000_000)
        self.assertEqual(probe["label"],
                         "getTransaction in-RPC p99 ≤ 10 ms at the demand-derived rate")
        self.assertEqual(probe["applies_to"], "queries")

    def test_unpaced_run_with_queries_carries_only_the_query_checks(self):
        tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(tmp, "b"), paced=False, queries=True)
        data, _, _ = run_convert(os.path.join(tmp, "b"))
        self.assertEqual([c["kind"] for c in data["checks_all"]],
                         ["query_sla", "query_e2e_probe"])
        self.assertEqual(data["checks"]["kind"], "query_sla")

    def test_query_targets_come_from_targets_json(self):
        # The read-path numbers are design targets, not converter constants:
        # they live in docs/targets.json beside the phase goals. The SLA family
        # states one rate and one p99 per endpoint and storage tier (hot = the
        # SLA's Live window, cold = Recent); the probe states one in-RPC budget.
        self.assertEqual(convert.SLA_FLOORS_RPS,
                         {"txhash": 300, "txpage": 75, "events": 100, "ledgers": 25})
        self.assertEqual(convert.SLA_P99_NS, {
            "txhash": {"hot": 20_000_000, "cold": 30_000_000},
            "txpage": {"hot": 60_000_000, "cold": 80_000_000},
            "events": {"hot": 40_000_000, "cold": 40_000_000},
            "ledgers": {"hot": 150_000_000, "cold": 200_000_000},
        })
        self.assertEqual(convert.E2E_IN_RPC_P99_NS, 10_000_000)
        self.assertEqual(convert.E2E_FLOORS_RPS["sac"], [300, 500, 1000])

    def test_legacy_layouts_unchanged(self):
        tmp = tempfile.mkdtemp()
        fixtures.build_legacy_bundle(os.path.join(tmp, "leg"))
        data, _, _ = run_convert(
            os.path.join(tmp, "leg"), dataset_kind="pubnet",
            run_id="legacy-test", run_name="Legacy", run_date="2026-07-13")
        self.assertNotIn("phase", data["campaign"])
        self.assertNotIn("phase_targets", data["campaign"])
        self.assertNotIn("checks", data)   # no queries in this bundle
        # legacy synthetic layout keeps the constant 600 ms block model
        data, _, _ = run_convert(
            os.path.join(tmp, "leg"), dataset_kind="synthetic",
            run_id="legacy-synth", run_name="Legacy", run_date="2026-07-13")
        self.assertNotIn("phase_targets", data["campaign"])
        self.assertEqual(data["checks"], {
            "kind": "block_keepup", "interval_ns": 600000000,
            "label": "600 ms block model", "applies_to": "ingest_hot"})


class FormatIntervalTests(unittest.TestCase):
    def test_labels(self):
        self.assertEqual(convert.format_interval(2_000_000_000), "2 s")
        self.assertEqual(convert.format_interval(1_500_000_000), "1.5 s")
        self.assertEqual(convert.format_interval(600_000_000), "600 ms")


if __name__ == "__main__":
    unittest.main()
