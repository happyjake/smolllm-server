#!/usr/bin/env python3
"""Tests for scripts/smolllm_exporter.py.

Run: python3 -m unittest scripts/test_smolllm_exporter.py  (or `just test-exporter`).

The behaviour under test is the one that decides whether the graphs are
trustworthy: smolllm-server's counters are in RAM, so every deploy resets them
to zero. Served verbatim, Prometheus reads a deploy as a catastrophic drop.
"""
import importlib.util
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


if __name__ == "__main__":
    unittest.main()
