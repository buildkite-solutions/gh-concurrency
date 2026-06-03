#!/usr/bin/env python3
"""
gha-concurrency.py - Estimate GitHub Actions job concurrency.

GitHub bills Actions by minutes (the AREA under your usage curve). Buildkite
bills by concurrency (the HEIGHT of that curve). GitHub's billing and metrics
views never report concurrency, so to compare the two cost models you have to
reconstruct it yourself from job start/finish timestamps.

This pulls completed jobs over a window and runs a sweep-line over their
start/end times to produce peak and time-weighted percentile concurrency --
the number you'd actually size a Buildkite plan against.

Usage:
  export GITHUB_TOKEN=ghp_xxx          # needs actions:read / repo scope
  ./gha-concurrency.py --repo owner/name --since 2025-05-01
  ./gha-concurrency.py --repo owner/a --repo owner/b --since 2025-05-01
                                       # multiple repos pool into one profile
"""

import argparse
import os
import sys
from datetime import datetime

import requests

API = "https://api.github.com"


def gh_get(url, token, params=None):
    """GET with link-header pagination; yields each JSON page."""
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    params = dict(params or {})
    params["per_page"] = 100
    while url:
        r = requests.get(url, headers=headers, params=params)
        r.raise_for_status()
        yield r.json()
        url = r.links.get("next", {}).get("url")
        params = None  # the 'next' URL already carries the query string


def _parse(ts):
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))


def collect_jobs(repo, token, since):
    """Return [(start, end)] for every job that actually ran."""
    intervals = []
    runs_url = f"{API}/repos/{repo}/actions/runs"
    for page in gh_get(runs_url, token, {"created": f">={since}"}):
        for run in page.get("workflow_runs", []):
            jobs_url = f"{API}/repos/{repo}/actions/runs/{run['id']}/jobs"
            for jpage in gh_get(jobs_url, token):
                for job in jpage.get("jobs", []):
                    s, e = job.get("started_at"), job.get("completed_at")
                    if s and e:
                        intervals.append((_parse(s), _parse(e)))
    return intervals


def concurrency_profile(intervals):
    """Sweep-line -> (peak, {level: seconds_spent_at_that_level})."""
    events = []
    for start, end in intervals:
        if end <= start:
            continue
        events.append((start, 1))
        events.append((end, -1))
    # at a tie, process ends (-1) before starts (+1) so a handoff isn't
    # double-counted as a moment of higher concurrency
    events.sort(key=lambda x: (x[0], x[1]))

    running = peak = 0
    prev_t = None
    time_at_level = {}
    for t, delta in events:
        if prev_t is not None and running > 0:
            secs = (t - prev_t).total_seconds()
            time_at_level[running] = time_at_level.get(running, 0.0) + secs
        running += delta
        peak = max(peak, running)
        prev_t = t
    return peak, time_at_level


def percentiles(time_at_level, ps=(50, 90, 95, 99)):
    """Time-weighted percentiles over BUSY time (>=1 job running)."""
    total = sum(time_at_level.values())
    if total == 0:
        return {p: 0 for p in ps}
    out = {}
    for p in ps:
        target = total * p / 100.0
        cum = 0.0
        for level in sorted(time_at_level):
            cum += time_at_level[level]
            if cum >= target:
                out[p] = level
                break
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", action="append", required=True,
                    help="owner/name (repeatable; repos pool into one profile)")
    ap.add_argument("--since", required=True,
                    help="YYYY-MM-DD lower bound on run creation date")
    args = ap.parse_args()

    token = os.environ.get("GITHUB_TOKEN")
    if not token:
        sys.exit("Set GITHUB_TOKEN (needs actions:read / repo scope).")

    intervals = []
    for repo in args.repo:
        print(f"Fetching jobs for {repo} since {args.since} ...", file=sys.stderr)
        intervals += collect_jobs(repo, token, args.since)

    if not intervals:
        sys.exit("No completed jobs found in that window.")

    peak, profile = concurrency_profile(intervals)
    pct = percentiles(profile)
    busy_hours = sum(profile.values()) / 3600.0

    print(f"\nJobs analyzed:        {len(intervals)}")
    print(f"Busy wall-clock time: {busy_hours:.1f}h (>=1 job running)")
    print(f"Peak concurrency:     {peak}")
    for p in (50, 90, 95, 99):
        print(f"p{p} concurrency:       {pct[p]}")
    print("\nSize Buildkite toward ~p95/p99, not the absolute peak: one 2am")
    print("cron fan-out shouldn't make you pay for that slot all month.")


if __name__ == "__main__":
    main()
