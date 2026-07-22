"""Tests for the campaign layout, manifest-derived identity/provenance, pace_lag
aggregation, and Go-duration parsing. Bundles are materialised on disk by
fixtures.py so the whole convert() pipeline runs, stdlib only."""
import argparse
import json
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
        dataset_kind="synthetic", unit_facts=None, source_gcs=None, notes=None,
        description=None, out_dir=out_dir)
    for k, v in overrides.items():
        setattr(args, k, v)
    convert._warnings.clear()
    data = convert.convert(args)
    return data, list(convert._warnings), out_dir


class GoDurationTests(unittest.TestCase):
    def test_units(self):
        self.assertEqual(convert.parse_go_duration("0"), 0)
        self.assertEqual(convert.parse_go_duration("2s"), 2_000_000_000)
        self.assertEqual(convert.parse_go_duration("600ms"), 600_000_000)
        self.assertEqual(convert.parse_go_duration("1500us"), 1_500_000)
        self.assertEqual(convert.parse_go_duration("250µs"), 250_000)
        self.assertEqual(convert.parse_go_duration("100ns"), 100)
        self.assertEqual(convert.parse_go_duration("1m"), 60_000_000_000)
        self.assertEqual(convert.parse_go_duration("1h"), 3_600_000_000_000)

    def test_compound_and_decimal(self):
        self.assertEqual(convert.parse_go_duration("1h30m"), 5_400_000_000_000)
        self.assertEqual(convert.parse_go_duration("1.5s"), 1_500_000_000)
        self.assertEqual(convert.parse_go_duration("2s500ms"), 2_500_000_000)

    def test_bad_raises(self):
        for bad in ("abc", "10", "5sx", "s"):
            with self.assertRaises(ValueError):
                convert.parse_go_duration(bad)


class LayoutDetectionTests(unittest.TestCase):
    def test_campaign_vs_pubnet_vs_synth(self):
        self.assertEqual(convert.detect_layout(["ingest-cold-sac-6000-c1-run1"]), "campaign")
        self.assertEqual(convert.detect_layout(["query-hot-soroswap-1500-c2-run3"]), "campaign")
        self.assertEqual(convert.detect_layout(["ingest-cold-3000-run1"]), "pubnet")
        self.assertEqual(convert.detect_layout(["golden-download-3000"]), "pubnet")
        self.assertEqual(convert.detect_layout(["synth-cold-sac-run1"]), "synthetic")

    def test_campaign_unit_discovery(self):
        names = [f"ingest-cold-sac-6000-c1-run{r}" for r in (1, 2)]
        names += ["golden-sac-6000-c1", "ingest-hot-sac-6000-c1-run1"]
        units, reps = convert.discover_units_reps(names, "campaign")
        self.assertEqual(units, {"sac-6000-c1"})
        self.assertEqual(reps, [1, 2])


class CampaignPacedTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(cls.tmp, "b"), paced=True)
        cls.data, cls.warnings, _ = run_convert(os.path.join(cls.tmp, "b"))

    def test_structural_validity(self):
        self.assertEqual(convert.validate_run(self.data, reps=2), [])

    def test_units_ordered_by_manifest(self):
        self.assertEqual(self.data["dataset"]["unit_order"],
                         ["sac-6000-c1", "soroswap-1500-c1", "soroswap-1500-c2"])

    def test_pace_lag_exact_aggregate(self):
        pl = self.data["ingest_hot"]["sac-6000-c1"]["driver"]["pace_lag"]
        self.assertEqual(pl["p99"], {"m": 800_000_000, "lo": 800_000_000,
                                     "hi": 1_600_000_000, "r": [800_000_000, 1_600_000_000]})
        self.assertEqual(pl["p50"]["m"], 0)          # on schedule >= half the time
        self.assertEqual(pl["max"]["m"], 1_400_000_000)
        self.assertEqual(pl["n"], 100)               # committed ledgers

    def test_close_interval_ns(self):
        self.assertEqual(self.data["campaign"]["close_interval_ns"], 2_000_000_000)

    def test_identity_defaults_from_manifest(self):
        self.assertEqual(self.data["run_id"],
                         "phase1-synthetic-minspec-b32bc9be-20260722T010000Z")
        self.assertEqual(self.data["run_date"], "2026-07-22")   # from started_at
        self.assertEqual(self.data["run_name"], "phase1-synthetic-minspec")

    def test_campaign_config_surfaced(self):
        c = self.data["campaign"]
        self.assertEqual(c["name"], "phase1-synthetic-minspec")
        self.assertEqual(c["config_file"], "phase1-synthetic-minspec.cfg")
        self.assertEqual(c["config"]["hot_num_ledgers"], 100)
        self.assertNotIn("close_interval", c["config"])   # converted to ns instead

    def test_provenance_structured_wins(self):
        self.assertEqual(self.data["hardware"]["instance_type"], "m6id.2xlarge")
        self.assertEqual(self.data["hostname"], "user-dev-063a")
        m = self.data["machine"]
        self.assertEqual(m["instance"], "m6id.2xlarge")     # hardware beat OLD.instance
        self.assertEqual(m["vcpus"], 8)                     # hardware beat CPU(s): 2
        b = self.data["build"]
        self.assertEqual(b["commit"], fixtures.COMMIT)      # invocation beat repo: line
        self.assertEqual(b["branch"], "bench-ci-775")
        self.assertEqual(b["version"], fixtures.VERSION)

    def test_golden_dirs_skipped(self):
        self.assertNotIn("golden", self.data)
        self.assertTrue(any("golden prep dir" in w for w in self.warnings))
        self.assertNotIn("golden-sac-6000-c1", self.data["dataset"]["unit_order"])

    def test_cli_overrides_win(self):
        data, _, _ = run_convert(os.path.join(self.tmp, "b"),
                                 run_id="explicit-id", run_date="2020-01-01")
        self.assertEqual(data["run_id"], "explicit-id")
        self.assertEqual(data["run_date"], "2020-01-01")


class CampaignUnpacedTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        fixtures.build_campaign_bundle(os.path.join(cls.tmp, "u"), paced=False)
        cls.data, cls.warnings, _ = run_convert(os.path.join(cls.tmp, "u"))

    def test_structural_validity(self):
        self.assertEqual(convert.validate_run(self.data, reps=2), [])

    def test_no_pace_lag_anywhere(self):
        for u in self.data["ingest_hot"].values():
            self.assertNotIn("pace_lag", u["driver"])

    def test_close_interval_zero(self):
        self.assertEqual(self.data["campaign"]["close_interval_ns"], 0)


class PaceLagInconsistencyTests(unittest.TestCase):
    def test_zero_fill_and_warn(self):
        tmp = tempfile.mkdtemp()
        root = os.path.join(tmp, "mix")
        # sac-6000-c1 run 2 loses its pace_lag row: run 2 is treated as all-zero.
        fixtures.build_campaign_bundle(root, paced=True, drop_pace={"sac-6000-c1": {2}})
        data, warnings, _ = run_convert(root)
        self.assertEqual(convert.validate_run(data, reps=2), [])
        pl = data["ingest_hot"]["sac-6000-c1"]["driver"]["pace_lag"]
        self.assertEqual(pl["p99"]["r"], [800_000_000, 0])   # run1 present, run2 zero-filled
        self.assertEqual(pl["p99"]["m"], 0)
        self.assertTrue(any("pace_lag present in 1/2" in w for w in warnings))
        # the other units still have their two real pace_lag runs
        self.assertEqual(
            data["ingest_hot"]["soroswap-1500-c1"]["driver"]["pace_lag"]["p99"]["r"],
            [800_000_000, 1_600_000_000])


class BinaryMismatchTests(unittest.TestCase):
    def test_warns_on_commit_mismatch(self):
        tmp = tempfile.mkdtemp()
        root = os.path.join(tmp, "mm")
        fixtures.build_campaign_bundle(root, paced=True)
        # Corrupt one invocation's commit so it disagrees with the rest.
        inv_path = os.path.join(root, "ingest-hot-sac-6000-c1-run1", "invocation.json")
        with open(inv_path) as f:
            inv = json.load(f)
        inv["binary"]["commit_hash"] = "f" * 40
        with open(inv_path, "w") as f:
            json.dump(inv, f)
        _, warnings, _ = run_convert(root)
        self.assertTrue(any("binary commit mismatch" in w for w in warnings))


class LegacyBundleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        fixtures.build_legacy_bundle(os.path.join(cls.tmp, "leg"))
        cls.data, cls.warnings, _ = run_convert(
            os.path.join(cls.tmp, "leg"), dataset_kind="pubnet",
            run_id="legacy-test", run_name="Legacy", run_date="2026-07-13")

    def test_structural_validity(self):
        self.assertEqual(convert.validate_run(self.data, reps=2), [])

    def test_no_manifest_fields(self):
        self.assertNotIn("hardware", self.data)
        self.assertNotIn("hostname", self.data)
        self.assertNotIn("close_interval_ns", self.data["campaign"])
        self.assertNotIn("name", self.data["campaign"])

    def test_old_vocabulary_and_golden(self):
        self.assertEqual(self.data["campaign"]["vocabulary"], "old")
        self.assertIn("golden", self.data)                       # golden-download-3000
        self.assertIn("3000", self.data["golden"])

    def test_build_from_machine_metadata(self):
        self.assertEqual(self.data["build"]["commit"], "a" * 40)  # repo: line
        self.assertEqual(self.data["build"]["branch"], "old-branch")
        self.assertNotIn("version", self.data["build"])           # no invocation.json


if __name__ == "__main__":
    unittest.main()
