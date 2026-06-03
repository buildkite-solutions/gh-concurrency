#!/usr/bin/env python3
"""
gha_concurrency.py - Estimate GitHub Actions job concurrency.

GitHub bills Actions by minutes (the AREA under your usage curve). Buildkite
bills by concurrency (the HEIGHT of that curve). GitHub's billing and metrics
views never report concurrency, so to compare the two cost models you have to
reconstruct it yourself from job start/finish timestamps.

This pulls completed jobs over a window and runs a sweep-line over their
start/end times to produce peak and time-weighted percentile concurrency --
the number you'd actually size a Buildkite plan against. It also re-derives
billable minutes so you can sanity-check the result against your real invoice.

Dependencies: none. Standard library only, Python 3.7+.

  export GITHUB_TOKEN=ghp_xxx          # needs actions:read / metadata:read
  ./gha_concurrency.py --repo owner/name --since 2025-05-01

GitHub Enterprise Server (self-hosted GitHub):
  ./gha_concurrency.py --repo owner/name --since 2025-05-01 \\
      --base-url https://ghes.example.com/api/v3
  (or set GITHUB_API_URL)
"""

import argparse
import json
import os
import random
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

VERSION = "1.0.0"

# GitHub-hosted billable-minute multipliers, by runner OS. Self-hosted is free.
# Used only for the invoice sanity-check, never for the concurrency math.
OS_MULTIPLIER = {"linux": 1, "windows": 2, "macos": 10}


# --------------------------------------------------------------------------- #
# HTTP: stdlib only, rate-limit aware, retry with backoff + jitter.
# --------------------------------------------------------------------------- #
class GitHubClient:
    def __init__(self, base_url, token, max_retries=6, timeout=30, verbose=False):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.max_retries = max_retries
        self.timeout = timeout
        self.verbose = verbose

    def _log(self, msg):
        if self.verbose:
            print(f"[gha-concurrency] {msg}", file=sys.stderr)

    def _request(self, url):
        """Single GET with retry/backoff. Returns (json_body, link_header)."""
        req = urllib.request.Request(url, method="GET")
        req.add_header("Authorization", f"Bearer {self.token}")
        req.add_header("Accept", "application/vnd.github+json")
        req.add_header("X-GitHub-Api-Version", "2022-11-28")
        req.add_header("User-Agent", f"gha-concurrency/{VERSION}")

        for attempt in range(self.max_retries):
            try:
                resp = urllib.request.urlopen(req, timeout=self.timeout)
                body = json.loads(resp.read().decode("utf-8"))
                # Pre-empt the primary rate-limit wall instead of hitting a 403.
                remaining = resp.headers.get("X-RateLimit-Remaining")
                reset = resp.headers.get("X-RateLimit-Reset")
                if remaining is not None and int(remaining) <= 1 and reset:
                    self._sleep_until(int(reset), reason="primary rate limit")
                return body, resp.headers.get("Link", "")
            except urllib.error.HTTPError as e:
                retry_after = e.headers.get("Retry-After")
                reset = e.headers.get("X-RateLimit-Reset")
                if e.code in (403, 429) and retry_after:
                    self._log(f"{e.code}: honoring Retry-After={retry_after}s")
                    time.sleep(int(retry_after) + 1)
                elif e.code in (403, 429) and reset:
                    self._sleep_until(int(reset), reason="secondary rate limit")
                elif e.code >= 500 or e.code in (403, 429):
                    self._backoff(attempt)
                elif e.code == 404:
                    raise NotFound(url) from e
                elif e.code == 401:
                    raise AuthError() from e
                else:
                    raise
            except (urllib.error.URLError, TimeoutError) as e:
                self._log(f"network error: {e}; retrying")
                self._backoff(attempt)
        raise RuntimeError(f"exhausted {self.max_retries} retries for {url}")

    def _backoff(self, attempt):
        delay = min(2 ** attempt, 60) + random.random()
        self._log(f"backing off {delay:.1f}s (attempt {attempt + 1})")
        time.sleep(delay)

    def _sleep_until(self, reset_epoch, reason=""):
        wait = max(0, reset_epoch - int(time.time())) + 1
        self._log(f"{reason}: sleeping {wait}s until window resets")
        time.sleep(wait)

    def paginate(self, path, params=None, items_key=None):
        """Yield items across all pages following the Link rel=next header."""
        params = dict(params or {})
        params["per_page"] = 100
        url = f"{self.base_url}{path}?{urllib.parse.urlencode(params)}"
        while url:
            body, link = self._request(url)
            page = body.get(items_key, []) if items_key else body
            for item in page:
                yield item
            url = _next_link(link)


class NotFound(Exception):
    def __init__(self, url):
        super().__init__(f"not found: {url}")


class AuthError(Exception):
    pass


def _next_link(link_header):
    """Parse a GitHub Link header for the rel="next" URL, or return None."""
    for part in link_header.split(","):
        segs = part.split(";")
        if len(segs) < 2:
            continue
        url = segs[0].strip().strip("<>")
        if any('rel="next"' in s.strip() for s in segs[1:]):
            return url
    return None


# --------------------------------------------------------------------------- #
# Data collection
# --------------------------------------------------------------------------- #
def collect_jobs(client, repo, since, until=None):
    """Yield normalized job records for runs created in the window."""
    created = f">={since}" if not until else f"{since}..{until}"
    runs = client.paginate(
        f"/repos/{repo}/actions/runs",
        params={"created": created},
        items_key="workflow_runs",
    )
    for run in runs:
        jobs = client.paginate(
            f"/repos/{repo}/actions/runs/{run['id']}/jobs",
            items_key="jobs",
        )
        for job in jobs:
            rec = _normalize_job(job, repo)
            if rec:
                yield rec


def _normalize_job(job, repo):
    started, completed = job.get("started_at"), job.get("completed_at")
    if not started or not completed:
        return None  # still running, queued, or cancelled before start
    start, end = _parse(started), _parse(completed)
    if end <= start:
        return None
    created = job.get("created_at")
    queue_s = (start - _parse(created)).total_seconds() if created else None
    return {
        "repo": repo,
        "start": start,
        "end": end,
        "queue_s": queue_s if (queue_s is None or queue_s >= 0) else 0.0,
        "os": _infer_os(job.get("labels", [])),
        "self_hosted": any("self-hosted" == str(l).lower()
                           for l in job.get("labels", [])),
    }


def _infer_os(labels):
    joined = " ".join(str(l).lower() for l in labels)
    if "windows" in joined:
        return "windows"
    if "macos" in joined or "mac-" in joined:
        return "macos"
    return "linux"


def _parse(ts):
    # fromisoformat handles a trailing 'Z' natively only on 3.11+; normalize.
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))


# --------------------------------------------------------------------------- #
# Analysis (pure functions: no network, fully unit-testable)
# --------------------------------------------------------------------------- #
def concurrency_profile(intervals):
    """Sweep-line over (start, end) pairs.

    Returns (peak, {level: seconds_spent_at_that_level}).
    At a tie, ends (-1) are processed before starts (+1) so a clean handoff
    (job B starts exactly when job A ends) is never counted as 2-at-once.
    """
    events = []
    for start, end in intervals:
        if end <= start:
            continue
        events.append((start, 1))
        events.append((end, -1))
    events.sort(key=lambda x: (x[0], x[1]))

    running = peak = 0
    prev_t = None
    time_at_level = {}
    for t, delta in events:
        if prev_t is not None and running > 0:
            secs = (t - prev_t).total_seconds()
            time_at_level[running] = time_at_level.get(running, 0.0) + secs
        running += delta
        if running < 0:  # should be impossible; guard against bad data
            running = 0
        peak = max(peak, running)
        prev_t = t
    return peak, time_at_level


def percentiles(time_at_level, ps=(50, 90, 95, 99)):
    """Time-weighted concurrency percentiles over BUSY time (>=1 running)."""
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


def billable_minutes(records):
    """Re-derive billable minutes (GitHub rounds each job up to the minute).

    Estimate only -- used to sanity-check against the real invoice.
    Self-hosted jobs are billed as 0.
    """
    by_os = {}
    for r in records:
        if r["self_hosted"]:
            continue
        raw_minutes = (r["end"] - r["start"]).total_seconds() / 60.0
        rounded = int(raw_minutes) + (1 if raw_minutes % 1 else 0)
        rounded = max(rounded, 1)
        billed = rounded * OS_MULTIPLIER.get(r["os"], 1)
        slot = by_os.setdefault(r["os"], {"jobs": 0, "billable_minutes": 0})
        slot["jobs"] += 1
        slot["billable_minutes"] += billed
    return by_os


def queue_stats(records):
    qs = [r["queue_s"] for r in records if r["queue_s"] is not None]
    if not qs:
        return None
    qs.sort()
    n = len(qs)
    return {
        "count": n,
        "median_s": qs[n // 2],
        "p95_s": qs[min(n - 1, int(n * 0.95))],
        "max_s": qs[-1],
    }


def detect_warnings(peak, pct, qstats):
    """Heuristics for 'the measured concurrency is a floor, not a ceiling'."""
    warnings = []
    if qstats and qstats["p95_s"] > 60:
        warnings.append(
            f"95th-percentile queue time is {qstats['p95_s']:.0f}s. Sustained "
            "queueing means jobs waited instead of running in parallel, so true "
            "demand is likely HIGHER than the concurrency reported here."
        )
    # A peak pinned to a round number can indicate a hit GHA concurrency cap.
    if peak in (5, 10, 20, 40, 60, 180, 300) and pct.get(95, 0) == peak:
        warnings.append(
            f"Peak ({peak}) sits at a round number and equals p95, which can "
            "indicate you were hitting a GitHub concurrency limit. If so, real "
            "demand exceeds this figure."
        )
    return warnings


# --------------------------------------------------------------------------- #
# Reporting
# --------------------------------------------------------------------------- #
def build_report(records, args):
    intervals = [(r["start"], r["end"]) for r in records]
    peak, profile = concurrency_profile(intervals)
    pct = percentiles(profile)
    qstats = queue_stats(records)
    return {
        "tool": "gha-concurrency",
        "version": VERSION,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "parameters": {
            "repos": args.repo,
            "since": args.since,
            "until": args.until,
            "base_url": args.base_url,
        },
        "jobs_analyzed": len(records),
        "busy_hours": round(sum(profile.values()) / 3600.0, 2),
        "peak_concurrency": peak,
        "percentile_concurrency": {f"p{k}": v for k, v in pct.items()},
        "billable_minutes_estimate": billable_minutes(records),
        "queue_seconds": qstats,
        "warnings": detect_warnings(peak, pct, qstats),
    }


def print_text(report):
    p = report["parameters"]
    print(f"\ngha-concurrency v{report['version']}")
    print(f"repos:  {', '.join(p['repos'])}")
    print(f"window: {p['since']} -> {p['until'] or 'now'}   api: {p['base_url']}")
    print(f"\nJobs analyzed:        {report['jobs_analyzed']}")
    print(f"Busy wall-clock time: {report['busy_hours']}h (>=1 job running)")
    print(f"Peak concurrency:     {report['peak_concurrency']}")
    for k, v in report["percentile_concurrency"].items():
        print(f"{k} concurrency:       {v}")

    bm = report["billable_minutes_estimate"]
    if bm:
        print("\nBillable-minutes estimate (sanity-check vs your invoice):")
        total = 0
        for os_name, slot in sorted(bm.items()):
            total += slot["billable_minutes"]
            print(f"  {os_name:<8} {slot['jobs']:>6} jobs  "
                  f"{slot['billable_minutes']:>10,} billable min")
        print(f"  {'TOTAL':<8} {'':>6}       {total:>10,} billable min")

    q = report["queue_seconds"]
    if q:
        print(f"\nQueue time: median {q['median_s']:.0f}s  "
              f"p95 {q['p95_s']:.0f}s  max {q['max_s']:.0f}s")

    print("\nSize Buildkite toward ~p95/p99, not the absolute peak: one 2am")
    print("cron fan-out shouldn't make you pay for that slot all month.")

    for w in report["warnings"]:
        print(f"\n!  WARNING: {w}")


# --------------------------------------------------------------------------- #
# Entry point
# --------------------------------------------------------------------------- #
def parse_args(argv=None):
    ap = argparse.ArgumentParser(
        description="Estimate GitHub Actions job concurrency for Buildkite sizing.")
    ap.add_argument("--repo", action="append", required=True, metavar="OWNER/NAME",
                    help="repository (repeatable; repos pool into one profile)")
    ap.add_argument("--since", required=True, metavar="YYYY-MM-DD",
                    help="lower bound on workflow-run creation date")
    ap.add_argument("--until", metavar="YYYY-MM-DD", default=None,
                    help="optional upper bound on run creation date")
    ap.add_argument("--base-url", default=os.environ.get(
        "GITHUB_API_URL", "https://api.github.com"),
        help="API base URL. GHES: https://HOST/api/v3 "
             "(env: GITHUB_API_URL)")
    ap.add_argument("--token", default=os.environ.get("GITHUB_TOKEN"),
                    help="token (default: env GITHUB_TOKEN)")
    ap.add_argument("--format", choices=("text", "json"), default="text")
    ap.add_argument("--max-retries", type=int, default=6)
    ap.add_argument("--verbose", action="store_true",
                    help="progress + rate-limit logging to stderr")
    return ap.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    if not args.token:
        sys.exit("error: no token. Set GITHUB_TOKEN or pass --token "
                 "(needs actions:read + metadata:read).")

    client = GitHubClient(args.base_url, args.token,
                          max_retries=args.max_retries, verbose=args.verbose)

    records = []
    for repo in args.repo:
        client._log(f"fetching {repo} since {args.since}")
        try:
            records.extend(collect_jobs(client, repo, args.since, args.until))
        except NotFound:
            print(f"warning: {repo} not found or no Actions access; skipping.",
                  file=sys.stderr)
        except AuthError:
            sys.exit("error: 401 unauthorized. Check token scope and validity.")

    if not records:
        sys.exit("error: no completed jobs found in that window.")

    report = build_report(records, args)
    if args.format == "json":
        print(json.dumps(report, indent=2))
    else:
        print_text(report)


if __name__ == "__main__":
    main()
