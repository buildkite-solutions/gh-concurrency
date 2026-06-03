#!/usr/bin/env python3
"""Tests for gha_concurrency. Pure-function and offline-replay coverage.

Run:  python3 -m unittest -v
No network, no third-party deps.
"""

import unittest
from datetime import datetime, timedelta, timezone

import gha_concurrency as g


def dt(hms):
    """'10:00:05' -> aware datetime on a fixed day."""
    h, m, s = (int(x) for x in hms.split(":"))
    return datetime(2025, 5, 1, h, m, s, tzinfo=timezone.utc)


class SweepLineTests(unittest.TestCase):
    def test_empty(self):
        peak, profile = g.concurrency_profile([])
        self.assertEqual(peak, 0)
        self.assertEqual(profile, {})

    def test_single_job(self):
        peak, _ = g.concurrency_profile([(dt("10:00:00"), dt("10:10:00"))])
        self.assertEqual(peak, 1)

    def test_full_overlap_is_two(self):
        peak, _ = g.concurrency_profile([
            (dt("10:00:00"), dt("10:00:10")),
            (dt("10:00:05"), dt("10:00:15")),
        ])
        self.assertEqual(peak, 2)

    def test_handoff_is_not_double_counted(self):
        # B starts the exact instant A ends -> never 2 at once.
        peak, _ = g.concurrency_profile([
            (dt("10:00:00"), dt("10:00:01")),
            (dt("10:00:01"), dt("10:00:02")),
        ])
        self.assertEqual(peak, 1)

    def test_zero_duration_ignored(self):
        peak, profile = g.concurrency_profile([(dt("10:00:00"), dt("10:00:00"))])
        self.assertEqual(peak, 0)
        self.assertEqual(profile, {})

    def test_nested_intervals(self):
        # Outer fully contains two staggered inner jobs -> peak 3.
        peak, _ = g.concurrency_profile([
            (dt("10:00:00"), dt("10:00:30")),
            (dt("10:00:05"), dt("10:00:20")),
            (dt("10:00:10"), dt("10:00:25")),
        ])
        self.assertEqual(peak, 3)

    def test_time_at_level_accounting(self):
        # Two jobs overlap for 5s in the middle of a 15s span.
        _, profile = g.concurrency_profile([
            (dt("10:00:00"), dt("10:00:10")),
            (dt("10:00:05"), dt("10:00:15")),
        ])
        self.assertEqual(profile[1], 10.0)  # 5s before + 5s after the overlap
        self.assertEqual(profile[2], 5.0)   # the overlap

    def test_matches_bruteforce_grid(self):
        # Property check: sweep peak == naive second-by-second max.
        import random
        random.seed(7)
        intervals = []
        for _ in range(50):
            start = random.randint(0, 600)
            intervals.append((dt("10:00:00") + timedelta(seconds=start),
                              dt("10:00:00") + timedelta(seconds=start +
                                                         random.randint(1, 120))))
        peak, _ = g.concurrency_profile(intervals)
        grid_peak = 0
        for sec in range(0, 800):
            t = dt("10:00:00") + timedelta(seconds=sec)
            count = sum(1 for s, e in intervals if s <= t < e)
            grid_peak = max(grid_peak, count)
        self.assertEqual(peak, grid_peak)


class PercentileTests(unittest.TestCase):
    def test_empty(self):
        self.assertEqual(g.percentiles({}), {50: 0, 90: 0, 95: 0, 99: 0})

    def test_weighted(self):
        # 90s at level 1, 10s at level 5: p50 -> 1, p95 -> 5.
        pct = g.percentiles({1: 90.0, 5: 10.0})
        self.assertEqual(pct[50], 1)
        self.assertEqual(pct[95], 5)


class BillableMinutesTests(unittest.TestCase):
    def _rec(self, secs, os_name="linux", self_hosted=False):
        return {"start": dt("10:00:00"),
                "end": dt("10:00:00") + timedelta(seconds=secs),
                "os": os_name, "self_hosted": self_hosted, "queue_s": 0.0,
                "repo": "x/y"}

    def test_rounds_up_to_minute(self):
        bm = g.billable_minutes([self._rec(61)])  # 1m1s -> 2 min
        self.assertEqual(bm["linux"]["billable_minutes"], 2)

    def test_macos_multiplier(self):
        bm = g.billable_minutes([self._rec(60, "macos")])  # 1 min x10
        self.assertEqual(bm["macos"]["billable_minutes"], 10)

    def test_self_hosted_is_free(self):
        bm = g.billable_minutes([self._rec(600, self_hosted=True)])
        self.assertEqual(bm, {})


class LinkHeaderTests(unittest.TestCase):
    def test_next_present(self):
        h = ('<https://api.github.com/x?page=2>; rel="next", '
             '<https://api.github.com/x?page=9>; rel="last"')
        self.assertEqual(g._next_link(h), "https://api.github.com/x?page=2")

    def test_no_next(self):
        h = '<https://api.github.com/x?page=1>; rel="prev"'
        self.assertIsNone(g._next_link(h))

    def test_empty(self):
        self.assertIsNone(g._next_link(""))


class OfflineReplayTests(unittest.TestCase):
    """Replay recorded API pages through the client without any network."""

    def test_pagination_and_collection(self):
        runs_p1 = {"workflow_runs": [{"id": 1}, {"id": 2}]}
        jobs_1 = {"jobs": [
            {"started_at": "2025-05-01T10:00:00Z",
             "completed_at": "2025-05-01T10:05:00Z",
             "created_at": "2025-05-01T09:59:00Z", "labels": ["ubuntu-latest"]},
        ]}
        jobs_2 = {"jobs": [
            {"started_at": "2025-05-01T10:02:00Z",
             "completed_at": "2025-05-01T10:08:00Z",
             "created_at": "2025-05-01T10:02:00Z", "labels": ["windows-latest"]},
            {"started_at": None, "completed_at": None, "labels": []},  # queued
        ]}
        responses = {
            "/repos/o/r/actions/runs": (runs_p1, ""),
            "/repos/o/r/actions/runs/1/jobs": (jobs_1, ""),
            "/repos/o/r/actions/runs/2/jobs": (jobs_2, ""),
        }

        client = g.GitHubClient("https://api.github.com", "tok")

        def fake_request(url):
            # Most specific (longest) matching path wins, so the runs path
            # doesn't shadow the .../runs/N/jobs paths it's a substring of.
            for path in sorted(responses, key=len, reverse=True):
                if path in url:
                    return responses[path]
            raise AssertionError(f"unexpected url {url}")

        client._request = fake_request
        records = list(g.collect_jobs(client, "o/r", "2025-05-01"))
        # 2 valid jobs; the null-timestamp queued job is dropped.
        self.assertEqual(len(records), 2)
        self.assertEqual({r["os"] for r in records}, {"linux", "windows"})
        peak, _ = g.concurrency_profile([(r["start"], r["end"]) for r in records])
        self.assertEqual(peak, 2)  # 10:02-10:05 they overlap


if __name__ == "__main__":
    unittest.main()
