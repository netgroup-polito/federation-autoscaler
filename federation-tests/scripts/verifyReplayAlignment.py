#!/usr/bin/env python3
"""Check that Phase B actually replayed Phase A's environment, as observed.

comparative-eco re-seeds its carbon-refresh goroutine with the same seed at the
start of each phase, so Phase B pushes the identical sequence of per-region
carbon intensities that Phase A pushed. That is the *source*. This script
checks the part that actually matters for the charts: whether the Consumers
OBSERVED the same thing at the same elapsed time in both phases.

The two can disagree. A value written to mock-eco reaches a Consumer only after
the provider's eco cache expires and its next advertisement fires, and each
provider's advertisement ticker has its own fixed phase offset. If that
combined lag is not small compared to the refresh tick, providers land on
different ticks in Phase A than in Phase B and the overlaid A-vs-B chart
compares points that were never taken under the same conditions -- even though
the replay itself is working perfectly.

Method: bin every observation by tick index (elapsed since the phase's first
sample, floor-divided by the tick length), take the modal value per
(phase, provider, tick), and count how often Phase A and Phase B agree. The
chance baseline comes from re-running the same count with Phase B's provider
labels shuffled, which preserves the value distribution while destroying any
real correspondence -- without it a high match rate is unreadable, because
green values are drawn from a narrow range and collide often by luck.

Usage:
    python verifyReplayAlignment.py --input results/<run>/nodegroups.csv
    python verifyReplayAlignment.py --input ... --tick-seconds 120 --windows 6

Reads only the standard library: it runs anywhere the harness runs, with no
pandas/numpy install needed.
"""

from __future__ import annotations

import argparse
import csv
import random
import statistics
import sys
from collections import Counter, defaultdict
from datetime import datetime, timedelta
from pathlib import Path

PHASE_A = "phase-a"
PHASE_B = "phase-b"

# Below this the run should be rejected: the phases were not observed as the
# same environment and the overlaid chart would be comparing unlike points.
PASS_THRESHOLD = 80.0
# Between FAIL_THRESHOLD and PASS_THRESHOLD the result is inconclusive rather
# than clearly broken -- usually too few ticks to say anything.
FAIL_THRESHOLD = 50.0

REQUIRED_COLUMNS = ["timestamp", "phase", "provider_id", "carbon_intensity", "has_carbon"]


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Verify Phase B observed the same carbon sequence as Phase A.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    p.add_argument(
        "--input",
        required=True,
        type=Path,
        help="Path to a run's nodegroups.csv (per-provider carbon observations).",
    )
    p.add_argument(
        "--tick-seconds",
        type=float,
        default=120.0,
        help="carbonRefreshInterval of the run, in seconds (default: 120, i.e. 2m).",
    )
    p.add_argument(
        "--windows",
        type=int,
        default=6,
        help="Split the phase into this many equal windows to spot drift (default: 6).",
    )
    p.add_argument(
        "--shuffles",
        type=int,
        default=200,
        help="Label shuffles used for the chance baseline (default: 200).",
    )
    return p.parse_args(argv)


def parse_timestamp(raw: str) -> datetime | None:
    """Parse the harness's RFC3339Nano timestamps.

    Go trims trailing zeros from the fractional second, so the precision varies
    row to row and fromisoformat needs the fraction normalised to 6 digits.
    """
    s = raw.strip().replace("Z", "+00:00")
    if not s:
        return None
    if "." in s:
        head, rest = s.split(".", 1)
        frac, _, tz = rest.partition("+")
        s = f"{head}.{frac[:6].ljust(6, '0')}+{tz}" if tz else f"{head}.{frac[:6].ljust(6, '0')}"
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        return None


def load_observations(path: Path) -> tuple[list[tuple[str, str, datetime, str]], dict[str, datetime]]:
    """Return (phase, provider, timestamp, value) rows plus each phase's start."""
    if not path.is_file():
        sys.exit(f"error: no such file: {path}")

    rows: list[tuple[str, str, datetime, str]] = []
    starts: dict[str, datetime] = {}

    with path.open(newline="", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        if reader.fieldnames is None:
            sys.exit(f"error: {path} is empty")
        missing = [c for c in REQUIRED_COLUMNS if c not in reader.fieldnames]
        if missing:
            sys.exit(f"error: {path} is missing required column(s): {', '.join(missing)}")

        for row in reader:
            if row.get("has_carbon") != "true":
                continue
            phase = row.get("phase", "")
            if phase not in (PHASE_A, PHASE_B):
                continue
            ts = parse_timestamp(row.get("timestamp", ""))
            if ts is None:
                continue
            value = (row.get("carbon_intensity") or "").strip()
            if not value:
                continue
            starts[phase] = min(starts.get(phase, ts), ts)
            rows.append((phase, row.get("provider_id", ""), ts, value))

    return rows, starts


def bin_by_tick(
    rows: list[tuple[str, str, datetime, str]],
    starts: dict[str, datetime],
    tick: float,
) -> dict[tuple[str, str, int], Counter]:
    bins: dict[tuple[str, str, int], Counter] = defaultdict(Counter)
    for phase, provider, ts, value in rows:
        elapsed = (ts - starts[phase]).total_seconds()
        bins[(phase, provider, int(elapsed // tick))][value] += 1
    return bins


def modal(bins: dict[tuple[str, str, int], Counter], phase: str, provider: str, k: int) -> str | None:
    c = bins.get((phase, provider, k))
    return c.most_common(1)[0][0] if c else None


def compare(
    bins: dict[tuple[str, str, int], Counter],
    providers: list[str],
    ticks: range,
) -> tuple[int, int]:
    same = total = 0
    for provider in providers:
        for k in ticks:
            a = modal(bins, PHASE_A, provider, k)
            b = modal(bins, PHASE_B, provider, k)
            if a is None or b is None:
                continue
            total += 1
            same += a == b
    return same, total


def chance_baseline(
    bins: dict[tuple[str, str, int], Counter],
    providers: list[str],
    ticks: range,
    shuffles: int,
) -> tuple[float, float]:
    """Match rate with Phase B's provider labels shuffled.

    Preserves each phase's value distribution while destroying the real
    provider-to-provider correspondence, so it measures how much agreement
    coincidence alone buys.
    """
    rng = random.Random(0)  # fixed: the baseline should not move between runs
    rates: list[float] = []
    for _ in range(shuffles):
        shuffled = providers[:]
        rng.shuffle(shuffled)
        mapping = dict(zip(providers, shuffled))
        same = total = 0
        for provider in providers:
            for k in ticks:
                a = modal(bins, PHASE_A, provider, k)
                b = modal(bins, PHASE_B, mapping[provider], k)
                if a is None or b is None:
                    continue
                total += 1
                same += a == b
        if total:
            rates.append(same / total * 100)
    if not rates:
        return float("nan"), float("nan")
    return statistics.mean(rates), statistics.pstdev(rates)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    tick = args.tick_seconds
    if tick <= 0:
        sys.exit("error: --tick-seconds must be positive")

    rows, starts = load_observations(args.input)
    if PHASE_A not in starts or PHASE_B not in starts:
        sys.exit("error: the file does not contain both phase-a and phase-b carbon observations")

    bins = bin_by_tick(rows, starts, tick)
    providers = sorted({p for _, p, _, _ in rows}, key=lambda s: (len(s), s))

    def last_tick(phase: str) -> int:
        ks = [k for (ph, _, k) in bins if ph == phase]
        return max(ks) if ks else -1

    shared = min(last_tick(PHASE_A), last_tick(PHASE_B))
    if shared < 0:
        sys.exit("error: no overlapping ticks between the two phases")
    ticks = range(shared + 1)

    same, total = compare(bins, providers, ticks)
    if not total:
        sys.exit("error: no tick had an observation in both phases")
    match = same / total * 100
    baseline, spread = chance_baseline(bins, providers, ticks, args.shuffles)

    span_a = (starts[PHASE_B] - starts[PHASE_A]).total_seconds()
    print(f"Input:            {args.input}")
    print(f"Tick length:      {tick:.0f}s ({timedelta(seconds=tick)})")
    print(f"Providers:        {len(providers)}")
    print(f"Ticks compared:   {shared + 1} (phase A start to phase B start: {span_a / 60:.1f} min)")
    print()
    print(f"Per-tick match:   {match:.1f}%  ({same}/{total})")
    print(f"Chance baseline:  {baseline:.1f}% +/- {spread:.1f}%")
    print()

    # A single number can hide a sequence that starts aligned and drifts apart
    # -- that failure mode means the two phases' tickers diverged rather than
    # that the lag is too long, and it needs a different fix, so report it
    # separately instead of averaging it away.
    if args.windows > 1 and shared + 1 >= args.windows:
        per = (shared + 1) / args.windows
        print("Per-window match (to catch progressive drift):")
        for w in range(args.windows):
            lo, hi = int(w * per), int((w + 1) * per)
            s, t = compare(bins, providers, range(lo, hi))
            label = f"ticks {lo:>3}-{hi - 1:<3}"
            print(f"  {label}  {s / t * 100:5.1f}%  ({s}/{t})" if t else f"  {label}      n/a")
        print()

    if match >= PASS_THRESHOLD:
        print(f"PASS: the phases were observed as the same environment (>= {PASS_THRESHOLD:.0f}%).")
        return 0
    if match >= FAIL_THRESHOLD:
        print(
            f"INCONCLUSIVE: {match:.1f}% is above chance but below the {PASS_THRESHOLD:.0f}% bar.\n"
            "Usually too few ticks to be sure -- run a longer phase before trusting an overlaid chart."
        )
        return 2
    print(
        f"FAIL: {match:.1f}% -- Phase A and Phase B were not observed as the same environment.\n"
        "Do not overlay them. Check that the observation lag (ecoCacheTTL + the provider's 30s\n"
        "advertisement cycle) is small against carbonRefreshInterval; the harness logs a warning\n"
        "at startup when it is not."
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
