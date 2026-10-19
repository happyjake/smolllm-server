#!/usr/bin/env python3
"""Tests for scripts/smolllm_exporter.py.

Run: python3 -m unittest scripts/test_smolllm_exporter.py  (or `just test-exporter`).

The behaviour under test is the one that decides whether the graphs are
trustworthy: smolllm-server's counters are in RAM, so every deploy resets them
to zero. Served verbatim, Prometheus reads a deploy as a catastrophic drop.
"""
import importlib.util
import json
import os
import sys
import unittest

_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "smolllm_exporter.py")
_spec = importlib.util.spec_from_file_location("smolllm_exporter", _PATH)
ex = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ex)


class RestartSafeCounterTest(unittest.TestCase):
    def setUp(self):
        ex._baseline.clear()
        ex._last_raw.clear()

    def test_monotonic_within_one_incarnation(self):
        for raw in (0, 5, 9):
            self.assertEqual(ex.restart_safe_counter("k", float(raw)), float(raw))

    def test_reset_is_absorbed_not_reported_as_drop(self):
        # 80 before the restart, 3 after: the exporter must never go backwards.
        self.assertEqual(ex.restart_safe_counter("k", 80.0), 80.0)
        self.assertEqual(ex.restart_safe_counter("k", 83.0), 83.0)
        self.assertEqual(ex.restart_safe_counter("k", 3.0), 86.0,
                         "a deploy must not look like lost traffic")
        self.assertEqual(ex.restart_safe_counter("k", 7.0), 90.0)

    def test_two_restarts_accumulate_both_baselines(self):
        for raw in (100.0, 4.0, 10.0, 2.0, 6.0):
            ex.restart_safe_counter("k", raw)
        # 100 + 10 + 6
        self.assertEqual(ex.restart_safe_counter("k", 6.0), 116.0)

    def test_series_are_independent(self):
        ex.restart_safe_counter("a", 50.0)
        ex.restart_safe_counter("b", 7.0)
        ex.restart_safe_counter("a", 1.0)  # a restarted
        self.assertEqual(ex.restart_safe_counter("a", 2.0), 52.0)  # 50 + 2
        self.assertEqual(ex.restart_safe_counter("b", 8.0), 8.0)

    def test_equal_value_is_not_treated_as_reset(self):
        ex.restart_safe_counter("k", 20.0)
        self.assertEqual(ex.restart_safe_counter("k", 20.0), 20.0)


class TransformTest(unittest.TestCase):
    def setUp(self):
        ex._baseline.clear()
        ex._last_raw.clear()

    def _apply(self, lines):
        return ex.transform("\n".join(lines) + "\n")

    def test_counter_line_is_rewritten(self):
        out = self._apply([
            '# TYPE smolllm_attempts_total counter',
            'smolllm_attempts_total{alias="a"} 5',
        ])
        self.assertIn('smolllm_attempts_total{alias="a"} 5', out)

    def test_histogram_buckets_are_restart_safe(self):
        before = [
            '# TYPE smolllm_attempt_first_output_seconds histogram',
            'smolllm_attempt_first_output_seconds_bucket{alias="a",le="2"} 10',
            'smolllm_attempt_first_output_seconds_count{alias="a"} 10',
            'smolllm_attempt_first_output_seconds_sum{alias="a"} 8',
        ]
        self._apply(before)
        after = [
            '# TYPE smolllm_attempt_first_output_seconds histogram',
            'smolllm_attempt_first_output_seconds_bucket{alias="a",le="2"} 2',
            'smolllm_attempt_first_output_seconds_count{alias="a"} 2',
            'smolllm_attempt_first_output_seconds_sum{alias="a"} 1.5',
        ]
        out = self._apply(after)
        self.assertIn('smolllm_attempt_first_output_seconds_bucket{alias="a",le="2"} 12', out)
        self.assertIn('smolllm_attempt_first_output_seconds_count{alias="a"} 12', out)
        self.assertIn('smolllm_attempt_first_output_seconds_sum{alias="a"} 9.5', out)

    def test_help_lines_are_preserved(self):
        out = self._apply([
            '# HELP smolllm_attempt_first_output_seconds Seconds to first token.',
            '# TYPE smolllm_attempt_first_output_seconds histogram',
        ])
        self.assertIn("# HELP smolllm_attempt_first_output_seconds", out)

    def test_integer_formatting_has_no_decimal_point(self):
        self.assertEqual(ex._fmt(5.0), "5")
        self.assertEqual(ex._fmt(1.5), "1.5")


class PersistenceTest(unittest.TestCase):
    """The baseline must survive this process restarting.

    Without it, restarting the exporter silently discards everything it had
    accumulated -- the counters look fine but the week is gone.
    """

    def setUp(self):
        import tempfile
        ex._baseline.clear()
        ex._last_raw.clear()
        fd, self.path = tempfile.mkstemp(suffix=".state")
        os.close(fd)
        os.unlink(self.path)

    def tearDown(self):
        for p in (self.path, self.path + ".tmp"):
            try:
                os.unlink(p)
            except OSError:
                pass

    def test_baseline_survives_a_restart(self):
        # Incarnation 1: 80 then a deploy resets to 0, then 3.
        for raw in (80.0, 0.0, 3.0):
            ex.restart_safe_counter("k", raw)
        before = ex.restart_safe_counter("k", 3.0)
        ex.save_state(self.path)

        # Process dies. Fresh memory, same file.
        ex._baseline.clear()
        ex._last_raw.clear()
        self.assertEqual(ex.load_state(self.path), 1)

        # Incarnation 2 must continue, not restart.
        self.assertEqual(ex.restart_safe_counter("k", 4.0), before + 1)

    def test_saved_state_contains_no_secret(self):
        ex.restart_safe_counter("k", 7.0)
        ex.save_state(self.path)
        with open(self.path) as fh:
            body = fh.read()
        self.assertIn("k", body)
        self.assertNotIn("access_key", body.lower())
        self.assertNotIn("Bearer", body)

    def test_state_file_is_written_atomically(self):
        # No leftover .tmp after a successful save, and the file is valid JSON.
        ex.restart_safe_counter("k", 3.0)
        ex.save_state(self.path)
        self.assertFalse(os.path.exists(self.path + ".tmp"))
        with open(self.path) as fh:
            self.assertIn("baseline", json.load(fh))

    def test_corrupt_state_is_ignored_not_imported(self):
        # A truncated/garbage file must not crash the exporter, and must not
        # seed a wrong baseline that would then be served as truth.
        with open(self.path, "w") as fh:
            fh.write('{"baseline": {"k": 99')  # truncated
        self.assertEqual(ex.load_state(self.path), 0)
        self.assertEqual(ex._baseline, {})
        self.assertEqual(ex.restart_safe_counter("k", 5.0), 5.0)

    def test_negative_and_non_numeric_entries_are_dropped(self):
        with open(self.path, "w") as fh:
            json.dump({"baseline": {"good": 10.0, "neg": -5, "str": "x"},
                       "last_raw": {"good": 3.0}}, fh)
        self.assertEqual(ex.load_state(self.path), 1)
        self.assertEqual(ex._baseline, {"good": 10.0})

    def test_missing_file_is_not_an_error(self):
        self.assertEqual(ex.load_state(self.path), 0)

    def test_unwritable_path_does_not_raise(self):
        # A monitoring process must not die because its bookkeeping file is
        # unwritable; it degrades to memory-only and says so.
        ex.restart_safe_counter("k", 1.0)
        ex.save_state("/proc/definitely/not/writable/state.json")
        self.assertEqual(ex.restart_safe_counter("k", 2.0), 2.0)

    def test_no_state_file_configured_is_a_noop(self):
        ex.restart_safe_counter("k", 1.0)
        ex.save_state("")          # empty = memory only
        self.assertEqual(ex.load_state(""), 0)


if __name__ == "__main__":
    unittest.main()
