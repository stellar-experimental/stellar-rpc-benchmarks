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


def rate_keys(qout):
    """The qtype entry's rate cells, in order — the verdicts are siblings."""
    return [k for k in qout if not k.startswith("verdict_")]


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
        # sorted by VALUE, not by string: r500 < r1000 < r2000. getTransaction
        # sweeps both families in one leg, so its ladder is the union.
        self.assertEqual(rate_keys(self.cold["txhash"]),
                         ["r150", "r300", "r500", "r600", "r1000", "r2000"])
        self.assertEqual(rate_keys(self.cold["ledgers"]),
                         ["r12.5", "r25", "r50"])

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
        self.assertEqual(self.cold["ledgers"]["r12.5"]["achieved_rps"]["m"], 12.498)

    def test_wall_dispatch_lag_and_shed(self):
        cell = self.cold["txhash"]["r1000"]
        self.assertEqual(cell["wall"]["r"], [100_000_000, 200_000_000])
        self.assertEqual(cell["dispatch_lag"]["p50"]["m"], 0)   # zeros are kept
        self.assertEqual(cell["dispatch_lag"]["n"], 100)
        self.assertEqual(cell["shed"]["r"], [0, 0])
        # only the top rung sheds at the in-flight cap
        self.assertEqual(self.cold["txhash"]["r2000"]["shed"]["m"], fixtures.RPS_SHED)

    def test_events_mean_page_and_items(self):
        cell = self.cold["events"]["r100"]
        self.assertEqual(cell["mean_page_ns"]["m"], float(fixtures.RPS_MEAN_PAGE_NS))
        self.assertEqual(cell["items_r"], [1000, 1000])
        self.assertNotIn("mean_page_ns", self.cold["txhash"]["r1000"])

    def test_setup_excludes_every_per_leg_row(self):
        self.assertEqual(list(self.cold["setup"]), ["open", "evict", "peak_rss_bytes"])
        hot = self.data["queries"]["hot"][UNIT]["setup"]
        self.assertEqual(list(hot), ["open", "peak_rss_bytes"])   # only cold evicts

    def test_query_load_embedded_in_campaign(self):
        self.assertEqual(self.data["campaign"]["query_load"], convert.QUERY_LOAD)

    def test_sla_verdict_on_every_endpoint_at_its_sla_floor(self):
        # The SLA floors belong to the endpoint alone, so the same four rates
        # are judged in every phase and every profile — getTransaction included.
        for qt, rate in (("ledgers", "r25"), ("txpage", "r75"),
                         ("txhash", "r300"), ("events", "r100")):
            v = self.cold[qt]["verdict_sla"]
            self.assertEqual(v["rate"], rate)
            self.assertTrue(v["pass"])
            self.assertEqual(v["threshold_ns"], convert.SLA_P99_NS[qt]["cold"])
            self.assertEqual(v["p99_ns"], fixtures.RPS_SCHED_P99_NS)
            self.assertNotIn("in_rpc", v)
        self.assertEqual(self.cold["txhash"]["verdict_sla"]["target_rps"], 300.0)
        self.assertEqual(self.cold["txhash"]["verdict_sla"]["achieved_rps_m"], 299.998)

    def test_e2e_verdict_on_getTransaction_alone_at_the_demand_floor(self):
        # sac phase 3 asks getTransaction for 1000 rps; the cell answers for its
        # time inside the RPC, and for nothing else.
        v = self.cold["txhash"]["verdict_e2e"]
        self.assertEqual(v["rate"], "r1000")
        self.assertEqual(v["target_rps"], 1000.0)
        self.assertEqual(v["in_rpc"], {"p99_ns": fixtures.RPS_SERVICE_P99_NS,
                                       "threshold_ns": 10_000_000, "pass": True})
        self.assertTrue(v["pass"])
        # the scheduled p99 rides along as context, with no threshold beside it
        self.assertEqual(v["p99_ns"], fixtures.RPS_SCHED_P99_NS)
        self.assertNotIn("threshold_ns", v)
        for qt in ("ledgers", "txpage", "events"):
            self.assertNotIn("verdict_e2e", self.cold[qt])

    def test_threshold_follows_the_storage_tier(self):
        # The SLA reads the tiers as data-age windows — hot is Live, cold is
        # Recent — so the same endpoint is judged differently in each.
        hot = self.data["queries"]["hot"][UNIT]
        self.assertEqual(hot["txpage"]["verdict_sla"]["threshold_ns"], 60_000_000)
        self.assertEqual(self.cold["txpage"]["verdict_sla"]["threshold_ns"], 80_000_000)
        self.assertEqual(hot["txhash"]["verdict_sla"]["threshold_ns"], 20_000_000)
        self.assertEqual(self.cold["txhash"]["verdict_sla"]["threshold_ns"], 30_000_000)
        # The E2E budget is one number for every tier, profile and phase.
        for entry in (hot, self.cold):
            self.assertEqual(entry["txhash"]["verdict_e2e"]["in_rpc"]["threshold_ns"],
                             10_000_000)

    def test_the_legacy_single_verdict_is_gone(self):
        for qt in ("ledgers", "txpage", "txhash", "events"):
            self.assertNotIn("verdict_1x", self.cold[qt])

    def test_no_page_budget_verdict(self):
        # getEvents' mean page is reported as a cell column, never judged: the
        # SLA states a p99 per endpoint and nothing else.
        self.assertNotIn("page_budget", self.cold["events"]["verdict_sla"])
        self.assertIn("mean_page_ns", self.cold["events"]["r100"])


class RpsVerdictFailureTests(unittest.TestCase):
    def test_scheduled_p99_over_the_sla_fails(self):
        data, _, _ = build_and_convert(sched_p99={"txhash": 700_000_000})
        v = data["queries"]["cold"][UNIT]["txhash"]["verdict_sla"]
        self.assertEqual(v["p99_ns"], 700_000_000)
        self.assertFalse(v["pass"])
        # a sibling type at the same rate ladder is unaffected
        self.assertTrue(data["queries"]["cold"][UNIT]["ledgers"]["verdict_sla"]["pass"])

    def test_a_p99_between_the_two_tier_targets_fails_hot_only(self):
        # 70 ms clears getTransactions' 80 ms Recent target but breaches its
        # 60 ms Live one, so the same latency passes cold and fails hot.
        data, _, _ = build_and_convert(sched_p99={"txpage": 70_000_000})
        self.assertTrue(data["queries"]["cold"][UNIT]["txpage"]["verdict_sla"]["pass"])
        self.assertFalse(data["queries"]["hot"][UNIT]["txpage"]["verdict_sla"]["pass"])

    def test_the_two_families_fail_independently(self):
        # 40 ms is over getTransaction's SLA p99 in both tiers (20 hot, 30 cold)
        # but the RPC itself answered in 5 ms: the SLA verdict fails, the
        # E2E-budget verdict passes, and neither reads the other's number.
        data, _, _ = build_and_convert(sched_p99={"txhash": 40_000_000})
        cold = data["queries"]["cold"][UNIT]["txhash"]
        self.assertFalse(cold["verdict_sla"]["pass"])
        self.assertTrue(cold["verdict_e2e"]["pass"])
        self.assertEqual(cold["verdict_e2e"]["p99_ns"], 40_000_000)

    def test_in_rpc_budget_fails_the_e2e_verdict_alone(self):
        data, _, _ = build_and_convert(svc_p99={"txhash": 12_000_000})
        cold = data["queries"]["cold"][UNIT]["txhash"]
        self.assertTrue(cold["verdict_sla"]["pass"])   # the tail is still inside 30 ms
        self.assertFalse(cold["verdict_e2e"]["pass"])  # but over the 10 ms in-RPC budget
        self.assertEqual(cold["verdict_e2e"]["in_rpc"]["p99_ns"], 12_000_000)
        self.assertNotIn("in_rpc", cold["verdict_sla"])

    def test_a_slow_mean_page_never_fails_a_verdict(self):
        data, _, _ = build_and_convert(mean_page={"events": 150_000_000})
        v = data["queries"]["cold"][UNIT]["events"]["verdict_sla"]
        self.assertTrue(v["pass"])
        self.assertNotIn("page_budget", v)


class RpsVerdictOmissionTests(unittest.TestCase):
    def test_no_phase_no_verdict(self):
        data, warnings, _ = build_and_convert(close_interval="1500ms")
        cold = data["queries"]["cold"][UNIT]
        self.assertNotIn("phase", data["campaign"])
        self.assertEqual(data["campaign"]["query_load"], convert.QUERY_LOAD)
        for qt in ("ledgers", "txpage", "txhash", "events"):
            self.assertNotIn("verdict_sla", cold[qt])
            self.assertNotIn("verdict_e2e", cold[qt])
        self.assertTrue(any("verdicts omitted" in w for w in warnings))
        self.assertEqual(convert.validate_run(data, reps=2), [])

    def test_unknown_profile_costs_the_probe_verdict_only(self):
        # The SLA floors belong to the endpoint, so an unknown dataset profile
        # still earns every SLA verdict; only the demand-derived probe, which is
        # keyed by profile, has nothing to judge against.
        data, warnings, _ = build_and_convert(dataset="mystery-9000")
        cold = data["queries"]["cold"]["mystery-9000-c1"]
        self.assertIn("verdict_sla", cold["txhash"])
        self.assertNotIn("verdict_e2e", cold["txhash"])
        hits = [w for w in warnings if "no query_load.e2e_probe profile 'mystery'" in w]
        self.assertEqual(len(hits), 2)             # one per tier, not per qtype
        self.assertEqual(convert.validate_run(data, reps=2), [])

    def test_no_cell_at_the_sla_floor(self):
        # the txpage SLA floor is 75 rps; this ladder never reaches it.
        data, warnings, _ = build_and_convert(rates={"txpage": ["1", "2", "4"]})
        self.assertNotIn("verdict_sla", data["queries"]["cold"][UNIT]["txpage"])
        self.assertTrue(any("no cell at the SLA floor of 75 rps" in w
                            for w in warnings))

    def test_no_cell_at_the_probe_floor(self):
        # An SLA-only txhash ladder: the SLA verdict lands, the probe's
        # phase-3 floor of 1000 rps has no cell and says so.
        data, warnings, _ = build_and_convert(
            rates={"txhash": ["150", "300", "600"]})
        cold = data["queries"]["cold"][UNIT]["txhash"]
        self.assertEqual(cold["verdict_sla"]["rate"], "r300")
        self.assertNotIn("verdict_e2e", cold)
        self.assertTrue(any("E2E-probe floor of 1000 rps" in w for w in warnings))


class RpsExplicitPhaseTests(unittest.TestCase):
    """An unpaced campaign has no pace to read a phase from — a cold-only query
    run has nothing to pace — so the manifest states the goal phase outright as
    campaign.query_phase, and the verdicts come from that."""
    # A phase-1 sac ladder. sac's phase-1 demand floor is 300 rps, which IS the
    # SLA floor, so the two txhash ladders coincide exactly and the leg sweeps
    # one list — the case where a single cell carries both verdicts.
    PHASE1_RATES = {"ledgers": ["12.5", "25", "50"], "txpage": ["37.5", "75", "150"],
                    "txhash": ["150", "300", "600"], "events": ["50", "100", "200"]}

    @classmethod
    def setUpClass(cls):
        cls.data, cls.warnings, _ = build_and_convert(
            close_interval="0", query_phase=1, rates=cls.PHASE1_RATES)
        cls.cold = cls.data["queries"]["cold"][UNIT]

    def test_clean_and_structurally_valid(self):
        self.assertEqual(self.warnings, [])
        self.assertEqual(convert.validate_run(self.data, reps=2), [])

    def test_phase_from_the_manifest(self):
        self.assertEqual(self.data["campaign"]["close_interval_ns"], 0)
        self.assertEqual(self.data["campaign"]["phase"], 1)

    def test_verdicts_at_the_phase1_floors(self):
        for qt, rate in (("ledgers", "r25"), ("txpage", "r75"),
                         ("txhash", "r300"), ("events", "r100")):
            self.assertEqual(self.cold[qt]["verdict_sla"]["rate"], rate)
        # the latency target is a property of the endpoint and the tier, not of
        # the phase: phase 1 is judged at the same p99 as phase 3.
        self.assertEqual(self.cold["events"]["verdict_sla"]["threshold_ns"],
                         40_000_000)

    def test_one_cell_carries_both_verdicts_when_the_floors_coincide(self):
        txhash = self.cold["txhash"]
        self.assertEqual(txhash["verdict_sla"]["rate"], "r300")
        self.assertEqual(txhash["verdict_e2e"]["rate"], "r300")
        # Same cell, two independent judgements against two different numbers.
        self.assertEqual(txhash["verdict_sla"]["threshold_ns"], 30_000_000)
        self.assertEqual(txhash["verdict_e2e"]["in_rpc"]["threshold_ns"], 10_000_000)
        self.assertTrue(txhash["verdict_sla"]["pass"])
        self.assertTrue(txhash["verdict_e2e"]["pass"])

    def test_unpaced_still_earns_no_keepup_check(self):
        # The goal phase judges the READ path; it does not invent a block model
        # for a run that paced nothing.
        self.assertEqual([c["kind"] for c in self.data["checks_all"]],
                         ["query_sla", "query_e2e_probe"])

    def test_neither_pace_nor_manifest_phase(self):
        data, warnings, _ = build_and_convert(close_interval="0", query_phase=0)
        self.assertNotIn("phase", data["campaign"])
        self.assertNotIn("verdict_sla", data["queries"]["cold"][UNIT]["txhash"])
        self.assertTrue(any("query_phase" in w and "verdicts omitted" in w
                            for w in warnings))
        self.assertEqual(convert.validate_run(data, reps=2), [])

    def test_the_pace_wins_over_the_manifest(self):
        # The runner refuses a config where the two disagree; if one ever
        # reaches here, the measured pace is the truth.
        data, _, _ = build_and_convert(close_interval="600ms", query_phase=1)
        self.assertEqual(data["campaign"]["phase"], 3)
        # phase 3's probe floor, not phase 1's — the SLA rate is the same either way
        self.assertEqual(
            data["queries"]["cold"][UNIT]["txhash"]["verdict_e2e"]["rate"], "r1000")


class RpsCellScanTests(unittest.TestCase):
    """rps_cells must survive a second pass over already-converted data: both
    verdicts are siblings of the rate cells and carry target_rps too."""
    def test_verdict_is_not_a_cell(self):
        data, _, _ = build_and_convert()
        qout = data["queries"]["cold"][UNIT]["txhash"]
        self.assertIn("verdict_sla", qout)
        self.assertIn("verdict_e2e", qout)
        self.assertEqual(list(convert.rps_cells(qout)),
                         ["r150", "r300", "r500", "r600", "r1000", "r2000"])
        self.assertTrue(convert.has_rps_cells(data["queries"]))

    def test_only_rate_keys_count(self):
        cell = {"target_rps": 1.0}
        self.assertEqual(
            list(convert.rps_cells({"r1": cell, "r0.5": cell, "verdict_sla": cell,
                                    "verdict_e2e": cell, "setup_r1": cell})),
            ["r1", "r0.5"])


class RpsRateTokenTests(unittest.TestCase):
    """Fractional rates must survive verbatim as cell keys and parse back."""
    @classmethod
    def setUpClass(cls):
        cls.data, cls.warnings, _ = build_and_convert(
            dataset="soroswap-1500", close_interval="2s",
            rates={"ledgers": ["12.5", "25", "50"],
                   "txpage": ["37.5", "75", "150"]})
        cls.cold = cls.data["queries"]["cold"]["soroswap-1500-c1"]

    def test_tokens_kept_verbatim(self):
        self.assertEqual(rate_keys(self.cold["txpage"]),
                         ["r37.5", "r75", "r150"])
        self.assertEqual(self.cold["txpage"]["r37.5"]["target_rps"], 37.5)
        self.assertEqual(self.cold["ledgers"]["r12.5"]["target_rps"], 12.5)

    def test_soroswap_sla_floors_matched(self):
        # txpage 75 rps, ledgers 25 rps — the SLA cell is the floor itself,
        # wherever it sits in the ladder the leg swept. The 0.5x rung is the
        # fractional token the matching has to survive.
        self.assertEqual(self.data["campaign"]["phase"], 1)
        self.assertEqual(self.cold["txpage"]["verdict_sla"]["rate"], "r75")
        self.assertEqual(self.cold["txpage"]["verdict_sla"]["target_rps"], 75.0)
        self.assertEqual(self.cold["ledgers"]["verdict_sla"]["rate"], "r25")
        self.assertEqual(self.cold["ledgers"]["verdict_sla"]["target_rps"], 25.0)

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
            self.assertNotIn("verdict_sla", self.cold[qt])
            self.assertNotIn("target_rps", self.cold[qt]["c1"])
            self.assertIn("ops_s", self.cold[qt]["c1"])

    def test_no_query_load_embed(self):
        self.assertNotIn("query_load", self.data["campaign"])
        self.assertEqual(self.data["campaign"]["phase"], 3)

    def test_setup_unchanged(self):
        self.assertEqual(list(self.cold["setup"]), ["open", "evict", "peak_rss_bytes"])
        self.assertEqual(convert.validate_run(self.data, reps=2), [])


class MismatchedGenerationTests(unittest.TestCase):
    """A one-dir-per-rep bundle whose query CSVs carry the paced total_r<rate>
    rows — a bench binary newer than the runner that laid the bundle out. There
    is no concurrency cell to read, so every qtype must be skipped with a loud
    warning rather than converted into an empty entry."""
    QTYPES = list(fixtures.RPS_RATES)

    @classmethod
    def _driver_rows(cls, tier, run):
        """One closed-loop-shaped driver.csv carrying every leg's paced rows."""
        rows = []
        for qt in cls.QTYPES:
            rows += [r for r in fixtures._rps_driver_rows(tier, qt, run,
                                                          fixtures.RPS_RATES)
                     if r[0] not in ("open", "evict", "peak_rss_bytes")]
        head = [("open", 1, 1) + (60_000_000,) * 5]
        if tier == "cold":
            head.append(("evict", 12, 6, 4000, 500, 600, 700, 700))
        return head + rows + [("peak_rss_bytes", 1, 0, 900_000_000, 0, 0, 0, 0)]

    @classmethod
    def setUpClass(cls):
        root = os.path.join(tempfile.mkdtemp(), "b")
        fixtures.build_campaign_bundle(root, paced=True, close_interval="600ms",
                                       queries=True)
        p99 = {q: fixtures.RPS_SCHED_P99_NS for q in cls.QTYPES}
        svc = {q: fixtures.RPS_SERVICE_P99_NS for q in cls.QTYPES}
        page = {q: fixtures.RPS_MEAN_PAGE_NS for q in cls.QTYPES}
        for name in sorted(os.listdir(root)):
            if not name.startswith("query-"):
                continue
            tier, run = name.split("-")[1], int(name.rsplit("run", 1)[1])
            for qt in cls.QTYPES:
                fixtures._write_csv(
                    os.path.join(root, name, f"{qt}.csv"),
                    fixtures._rps_qtype_rows(qt, run, fixtures.RPS_RATES,
                                             p99, svc, page))
            fixtures._write_csv(os.path.join(root, name, "driver.csv"),
                                cls._driver_rows(tier, run))
        cls.data, cls.warnings, _ = run_convert(root)

    def test_every_qtype_is_named_in_a_mismatch_warning(self):
        hits = [w for w in self.warnings if "mismatched runner/bench pair" in w]
        # one per (tier, unit, qtype): 2 tiers x 3 units x 4 types
        self.assertEqual(len(hits), 24)
        self.assertIn("query-cold-sac-6000-c1 txhash: rows are the paced-RPS "
                      "generation but the bundle has the one-dir-per-rep layout "
                      "— mismatched runner/bench pair? skipping txhash", hits)

    def test_no_empty_qtype_entries(self):
        for tier, units in self.data["queries"].items():
            for unit, entry in units.items():
                self.assertEqual(list(entry), ["setup"], f"{tier}/{unit}")
                self.assertTrue(entry["setup"])

    def test_conversion_still_succeeds(self):
        # warn-and-continue: the rest of the bundle converts and validates.
        self.assertEqual(convert.validate_run(self.data, reps=2), [])
        self.assertIn("ingest_cold", self.data)
        self.assertFalse(convert.has_rps_cells(self.data["queries"]))


if __name__ == "__main__":
    unittest.main()
