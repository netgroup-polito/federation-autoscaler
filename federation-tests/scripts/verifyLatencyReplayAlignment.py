#!/usr/bin/env python3
"""Check that Phase B observed the same latency environment Phase A did.

The latency counterpart of verifyReplayAlignment.py. It is a separate script,
and works differently, because the two measurements are not the same kind of
thing and the two tests do not sample at the same density.

WHAT MAKES THE LATENCY CASE HARDER

comparative-eco writes a carbon-intensity number and every Consumer reads that
same number back on every iteration, for every provider -- dense, exact data.
Here:

  * The value is a measured RTT. In practice the injected tc delay dominates it
    so completely that repeated probes in one window return the same number to
    the millisecond (jitter is sub-millisecond on a Kind host), but it is still
    a measurement, so the comparison uses a tolerance rather than equality.

  * Phase A is structurally sparse. Under Random the Broker masks to a single
    provider, so a given (consumer, provider) pair is only measured when the
    roll happens to pick it -- on a 3x7 hour-long run that was 7 of 30 windows,
    one sample each, against 28 of 30 for Phase B, which probes the whole
    latency shortlist. Coverage is therefore reported alongside the match rate:
    a high percentage over very few cells is a weak result, and the output has
    to make that visible instead of hiding it behind one number.

WHY THE GRID IS ESTIMATED RATHER THAN ASSUMED

An earlier version binned by elapsed time since each phase's FIRST OBSERVATION.
That put the bin boundaries wherever sampling happened to start, not where the
environment actually changed, so bins straddled real refresh boundaries and a
Phase A window holding a single sample was assigned whichever value fell on the
near side of the split. It scored a genuinely-aligned run at 70%, and widening
the value tolerance barely moved it -- correctly, because the mismatches were
right values in the wrong bin, and no tolerance on the value fixes an error in
time.

So the refresh grid is estimated from the data instead. refreshLatency redraws
the entire delay matrix on a single ticker, so every pair changes value at the
same instant: each observed change brackets a real boundary, and the brackets
from all pairs estimate one common offset, recovered here with a circular mean.

Usage:
    python verifyLatencyReplayAlignment.py --input results/.../probes.csv
    python verifyLatencyReplayAlignment.py --input ... --tick-seconds 120 --tolerance-ms 2
"""

from __future__ import annotations

import argparse
import csv
import math
import random
import statistics
import sys
from collections import defaultdict
from datetime import datetime, timedelta
from pathlib import Path

PHASE_A = "phase-a"
PHASE_B = "phase-b"

PASS_THRESHOLD = 80.0
FAIL_THRESHOLD = 50.0
# Below this, the estimated refresh grid is not trustworthy and neither is
# anything computed on top of it.
MIN_RESULTANT = 0.5

REQUIRED_COLUMNS = ["timestamp", "consumer_id", "phase", "provider_id", "rtt_ms"]

Pair = tuple[str, str]


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Verify Phase B observed the same latency environment as Phase A.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    p.add_argument("--input", required=True, type=Path, help="Path to a run's probes.csv.")
    p.add_argument(
        "--tick-seconds",
        type=float,
        default=120.0,
        help="latencyRefreshInterval of the run, in seconds (default: 120, i.e. 2m).",
    )
    p.add_argument(
        "--tolerance-ms",
        type=float,
        default=10.0,
        help="How far two RTTs may differ and still count as the same delay (default: 10). "
        "The question this script answers is whether the two phases saw the same conditions, "
        "not whether they agree to the millisecond, so the default is deliberately relaxed. "
        "It costs almost nothing to be generous here: measured differences are bimodal -- "
        "readings of the same delay land within ~0.1ms of each other -- so on real data every "
        "value from 1 to 10 scored within one cell of the others.",
    )
    p.add_argument(
        "--max-bracket-seconds",
        type=float,
        default=45.0,
        help="When estimating the refresh grid, ignore value changes bracketed by a gap wider "
        "than this -- they locate the boundary too imprecisely to be worth averaging (default: 45).",
    )
    p.add_argument("--windows", type=int, default=6, help="Windows to split the phase into (default: 6).")
    p.add_argument("--shuffles", type=int, default=200, help="Chance-baseline shuffles (default: 200).")
    return p.parse_args(argv)


def parse_timestamp(raw: str) -> datetime | None:
    """Parse the harness's RFC3339Nano timestamps (Go trims trailing zeros)."""
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


def load_observations(
    path: Path,
) -> tuple[dict[str, dict[Pair, list[tuple[float, float]]]], dict[str, datetime]]:
    """Return per-phase {(consumer, provider): [(elapsed_s, rtt_ms), ...]} and phase starts."""
    if not path.is_file():
        sys.exit(f"error: no such file: {path}")

    raw: dict[str, dict[Pair, list[tuple[datetime, float]]]] = {PHASE_A: defaultdict(list), PHASE_B: defaultdict(list)}
    starts: dict[str, datetime] = {}

    with path.open(newline="", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        if reader.fieldnames is None:
            sys.exit(f"error: {path} is empty")
        missing = [c for c in REQUIRED_COLUMNS if c not in reader.fieldnames]
        if missing:
            sys.exit(
                f"error: {path} is missing required column(s): {', '.join(missing)}. "
                "This script reads probes.csv; use verifyReplayAlignment.py for the eco test."
            )

        for row in reader:
            phase = row.get("phase", "")
            if phase not in (PHASE_A, PHASE_B):
                continue
            ts = parse_timestamp(row.get("timestamp", ""))
            if ts is None:
                continue
            try:
                rtt = float((row.get("rtt_ms") or "").strip())
            except ValueError:
                continue
            # An unreachable provider is recorded as +Inf: meaningful as "timed
            # out", not as a delay to compare.
            if not math.isfinite(rtt):
                continue
            starts[phase] = min(starts.get(phase, ts), ts)
            raw[phase][(row.get("consumer_id", ""), row.get("provider_id", ""))].append((ts, rtt))

    out: dict[str, dict[Pair, list[tuple[float, float]]]] = {PHASE_A: {}, PHASE_B: {}}
    for phase, pairs in raw.items():
        if phase not in starts:
            continue
        for pair, samples in pairs.items():
            samples.sort()
            out[phase][pair] = [((ts - starts[phase]).total_seconds(), rtt) for ts, rtt in samples]
    return out, starts


def estimate_grid_offset(
    phase_data: dict[Pair, list[tuple[float, float]]],
    tick: float,
    tolerance: float,
    max_bracket: float,
) -> tuple[float, float, int]:
    """Estimate where this phase's refresh boundaries fall.

    Every pair is redrawn by the same ticker, so a value change in any pair
    brackets the same shared boundary. Each usable bracket contributes its
    midpoint; the midpoints are averaged circularly modulo the tick, which is
    the right mean for a phase on a repeating grid (a boundary just after 0 and
    one just before the tick are neighbours, not opposites).

    Returns (offset_seconds, resultant_length, brackets_used). The resultant
    length is 1 when every bracket agrees and near 0 when they are scattered,
    so it doubles as a confidence measure on the offset.
    """
    sin_sum = cos_sum = 0.0
    used = 0
    for samples in phase_data.values():
        for (t_prev, v_prev), (t_next, v_next) in zip(samples, samples[1:]):
            if abs(v_next - v_prev) <= tolerance:
                continue
            if t_next - t_prev > max_bracket:
                continue
            midpoint = (t_prev + t_next) / 2.0
            angle = 2.0 * math.pi * ((midpoint % tick) / tick)
            sin_sum += math.sin(angle)
            cos_sum += math.cos(angle)
            used += 1

    if used == 0:
        return 0.0, 0.0, 0

    mean_sin, mean_cos = sin_sum / used, cos_sum / used
    resultant = math.hypot(mean_sin, mean_cos)
    offset = (math.atan2(mean_sin, mean_cos) / (2.0 * math.pi)) * tick
    return offset % tick, resultant, used


def bin_medians(
    phase_data: dict[Pair, list[tuple[float, float]]],
    tick: float,
    offset: float,
) -> dict[Pair, dict[int, float]]:
    """Collapse each (pair, window) to one representative RTT.

    Median rather than mean or mode: robust to a single sample that landed on
    the far side of a boundary the offset estimate did not place perfectly.
    """
    buckets: dict[Pair, dict[int, list[float]]] = defaultdict(lambda: defaultdict(list))
    for pair, samples in phase_data.items():
        for elapsed, rtt in samples:
            buckets[pair][math.floor((elapsed - offset) / tick)].append(rtt)
    return {pair: {k: statistics.median(v) for k, v in ks.items()} for pair, ks in buckets.items()}


def compare(
    a: dict[Pair, dict[int, float]],
    b: dict[Pair, dict[int, float]],
    pairs: list[Pair],
    ticks: range,
    tolerance: float,
    slack: int = 0,
    remap: dict[Pair, Pair] | None = None,
) -> tuple[int, int]:
    """Count matching windows.

    A window counts as comparable when BOTH phases have a reading for it, and
    slack only widens which of B's readings may satisfy the match -- it never
    widens the comparable set. Keeping the denominator fixed is what makes
    slack=1 a genuine relaxation of slack=0 (its rate can only be higher), so
    the gap between the two isolates how much of the residue is boundary
    placement rather than a different environment. Letting slack pull in extra
    cells instead produced the nonsense of the "looser" rate scoring lower.

    Phase B samples nearly every window, so a grid that is off by one still
    lands inside this set: B has a reading at k, it just carries the
    neighbouring window's value, which slack=1 then recognises.
    """
    same = total = 0
    for pair in pairs:
        a_ticks = a.get(pair)
        b_ticks = b.get(remap[pair] if remap else pair)
        if not a_ticks or not b_ticks:
            continue
        for k in ticks:
            if k not in a_ticks or k not in b_ticks:
                continue
            total += 1
            candidates = [b_ticks[j] for j in range(k - slack, k + slack + 1) if j in b_ticks]
            if any(abs(a_ticks[k] - c) <= tolerance for c in candidates):
                same += 1
    return same, total


def chance_baseline(
    a: dict[Pair, dict[int, float]],
    b: dict[Pair, dict[int, float]],
    pairs: list[Pair],
    ticks: range,
    tolerance: float,
    shuffles: int,
) -> tuple[float, float]:
    """Match rate with Phase B's provider labels shuffled within each consumer.

    Keeps every phase's value distribution intact while destroying the real
    pair-to-pair correspondence, which is what makes the headline percentage
    readable: delays are drawn from a bounded range, so some agreement happens
    by luck and the baseline says how much.
    """
    rng = random.Random(0)  # fixed: the baseline should not move between runs
    by_consumer: dict[str, list[str]] = defaultdict(list)
    for consumer, provider in pairs:
        by_consumer[consumer].append(provider)

    rates: list[float] = []
    for _ in range(shuffles):
        remap: dict[Pair, Pair] = {}
        for consumer, providers in by_consumer.items():
            shuffled = providers[:]
            rng.shuffle(shuffled)
            for provider, swapped in zip(providers, shuffled):
                remap[(consumer, provider)] = (consumer, swapped)
        same, total = compare(a, b, pairs, ticks, tolerance, remap=remap)
        if total:
            rates.append(same / total * 100)
    if not rates:
        return float("nan"), float("nan")
    return statistics.mean(rates), statistics.pstdev(rates)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if args.tick_seconds <= 0:
        sys.exit("error: --tick-seconds must be positive")
    if args.tolerance_ms <= 0:
        sys.exit("error: --tolerance-ms must be positive")

    data, starts = load_observations(args.input)
    if PHASE_A not in starts or PHASE_B not in starts:
        sys.exit("error: the file does not contain both phase-a and phase-b RTT observations")

    tick = args.tick_seconds
    offsets: dict[str, tuple[float, float, int]] = {}
    for phase in (PHASE_A, PHASE_B):
        offsets[phase] = estimate_grid_offset(data[phase], tick, args.tolerance_ms, args.max_bracket_seconds)

    # Phase A usually cannot locate its own boundaries: Random masks to one
    # provider, so consecutive observations OF THE SAME PAIR sit minutes apart
    # and never bracket a change tightly enough to be usable. Borrow the other
    # phase's offset in that case. The two phases are structurally in step --
    # each starts its refresh goroutine, waits the same drain, then samples --
    # so the grids should sit at nearly the same place, and any residue shows
    # up as strict/±1 disagreement rather than being silently absorbed.
    borrowed: list[str] = []
    for phase, other in ((PHASE_A, PHASE_B), (PHASE_B, PHASE_A)):
        if offsets[phase][2] == 0 and offsets[other][2] > 0:
            offsets[phase] = (offsets[other][0], offsets[phase][1], 0)
            borrowed.append("phase A" if phase == PHASE_A else "phase B")

    medians: dict[str, dict[Pair, dict[int, float]]] = {
        phase: bin_medians(data[phase], tick, offsets[phase][0]) for phase in (PHASE_A, PHASE_B)
    }

    pairs = sorted(set(medians[PHASE_A]) | set(medians[PHASE_B]))

    def last_tick(phase: str) -> int:
        ks = [k for ticks in medians[phase].values() for k in ticks]
        return max(ks) if ks else -1

    shared = min(last_tick(PHASE_A), last_tick(PHASE_B))
    if shared < 0:
        sys.exit("error: no overlapping windows between the two phases")
    ticks = range(shared + 1)

    strict_same, strict_total = compare(medians[PHASE_A], medians[PHASE_B], pairs, ticks, args.tolerance_ms)
    if not strict_total:
        sys.exit("error: no window had an observation in both phases for the same (consumer, provider)")
    adj_same, adj_total = compare(medians[PHASE_A], medians[PHASE_B], pairs, ticks, args.tolerance_ms, slack=1)
    strict = strict_same / strict_total * 100
    adjacent = adj_same / adj_total * 100 if adj_total else float("nan")
    baseline, spread = chance_baseline(
        medians[PHASE_A], medians[PHASE_B], pairs, ticks, args.tolerance_ms, args.shuffles
    )

    cells = len(pairs) * len(ticks)
    cov_a = sum(1 for p in pairs for k in ticks if k in medians[PHASE_A].get(p, {}))
    cov_b = sum(1 for p in pairs for k in ticks if k in medians[PHASE_B].get(p, {}))

    span = (starts[PHASE_B] - starts[PHASE_A]).total_seconds()
    print(f"Input:              {args.input}")
    print(f"Tick length:        {tick:.0f}s ({timedelta(seconds=tick)})")
    print(f"RTT tolerance:      +/-{args.tolerance_ms:.1f}ms")
    print(f"(consumer,provider) pairs: {len(pairs)}")
    print(f"Windows compared:   {shared + 1} (phase A start to phase B start: {span / 60:.1f} min)")
    print()
    print("Estimated refresh grid (recovered from observed value changes):")
    for phase, label in ((PHASE_A, "phase A"), (PHASE_B, "phase B")):
        offset, resultant, used = offsets[phase]
        if used == 0:
            print(f"  {label}: offset {offset:6.1f}s   borrowed (too sparse to locate its own boundaries)")
        else:
            print(f"  {label}: offset {offset:6.1f}s   agreement {resultant:.2f}   from {used} boundaries")
    print()
    print(f"Coverage:           {cov_a}/{cells} windows sampled in A, {cov_b}/{cells} in B")
    print(f"                    {strict_total} comparable (both phases)")
    print()
    print(f"Per-window match:   {strict:.1f}%  ({strict_same}/{strict_total})")
    print(f"  allowing +/-1 window: {adjacent:.1f}%  ({adj_same}/{adj_total})")
    print(f"Chance baseline:    {baseline:.1f}% +/- {spread:.1f}%")
    print()

    if args.windows > 1 and shared + 1 >= args.windows:
        per = (shared + 1) / args.windows
        print("Per-window-group match (to catch progressive drift):")
        for w in range(args.windows):
            lo, hi = int(w * per), int((w + 1) * per)
            s, t = compare(medians[PHASE_A], medians[PHASE_B], pairs, range(lo, hi), args.tolerance_ms)
            label = f"windows {lo:>3}-{hi - 1:<3}"
            print(f"  {label}  {s / t * 100:5.1f}%  ({s}/{t})" if t else f"  {label}      n/a")
        print()

    # A borrowed offset carries the lending phase's confidence, not its own
    # (meaningless) resultant of zero, so it is not flagged here.
    weak_grid = [
        lbl
        for ph, lbl in ((PHASE_A, "phase A"), (PHASE_B, "phase B"))
        if offsets[ph][2] > 0 and offsets[ph][1] < MIN_RESULTANT
    ]
    if weak_grid:
        print(
            f"WARNING: the refresh grid could not be pinned down for {', '.join(weak_grid)} "
            f"(agreement below {MIN_RESULTANT}).\nThe windows below may be misaligned, so treat "
            "the match rate as unreliable rather than as evidence either way."
        )
        print()

    if strict >= PASS_THRESHOLD:
        print(f"PASS: the phases were observed as the same latency environment (>= {PASS_THRESHOLD:.0f}%).")
        return 0
    if strict >= FAIL_THRESHOLD:
        print(
            f"INCONCLUSIVE: {strict:.1f}% is above chance but below the {PASS_THRESHOLD:.0f}% bar.\n"
            "Check the coverage line first: a low count of comparable windows makes this figure\n"
            "noisy on its own. If the +/-1 window rate is much higher, the residue is boundary\n"
            "placement rather than a different environment."
        )
        return 2
    print(
        f"FAIL: {strict:.1f}% -- Phase A and Phase B were not observed as the same latency environment.\n"
        "Inspect a single pair with dumpLatencySequence.py before concluding: it prints the raw\n"
        "per-window RTTs side by side, which distinguishes a genuine replay problem from an\n"
        "artefact of how sparse Phase A's sampling is."
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
