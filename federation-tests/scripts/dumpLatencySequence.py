#!/usr/bin/env python3
"""One-off diagnostic: print Phase A vs Phase B's per-tick RTT side by side for
one (consumer, provider) pair, so a real replay can be told apart from two
independent draws by eye -- a whole-phase mean/stdev summary can't do that,
since it looks similar either way.

Reach for this when verifyLatencyReplayAlignment.py reports FAIL or a stubborn
INCONCLUSIVE. Raw values settle in seconds what summary statistics argue about:
readings of the same replayed delay agree to a fraction of a millisecond, so if
the two columns line up the replay is working and the score is measuring
something else (usually Phase A's sparse sampling), while genuinely unrelated
numbers mean the replay itself needs looking at.

Usage:
    python dumpLatencySequence.py --input results/.../probes.csv --consumer consumer-1 --provider provider-1
"""

from __future__ import annotations

import argparse
import csv
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path

PHASE_A = "phase-a"
PHASE_B = "phase-b"


def parse_timestamp(raw: str) -> datetime | None:
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


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--input", required=True, type=Path)
    p.add_argument("--consumer", required=True)
    p.add_argument("--provider", required=True)
    p.add_argument("--tick-seconds", type=float, default=120.0)
    args = p.parse_args()

    rows = defaultdict(list)
    starts = {}
    with args.input.open(newline="", encoding="utf-8") as f:
        for row in csv.DictReader(f):
            phase = row.get("phase", "")
            if phase not in (PHASE_A, PHASE_B):
                continue
            if row.get("consumer_id") != args.consumer or row.get("provider_id") != args.provider:
                continue
            ts = parse_timestamp(row.get("timestamp", ""))
            if ts is None:
                continue
            try:
                rtt = float(row["rtt_ms"])
            except ValueError:
                continue
            if rtt in (float("inf"), float("-inf")):
                continue
            starts[phase] = min(starts.get(phase, ts), ts)
            rows[phase].append((ts, rtt))

    if PHASE_A not in rows or PHASE_B not in rows:
        print("no data for this pair in one or both phases")
        return 1

    bins = {PHASE_A: defaultdict(list), PHASE_B: defaultdict(list)}
    for phase in (PHASE_A, PHASE_B):
        for ts, rtt in rows[phase]:
            k = int((ts - starts[phase]).total_seconds() // args.tick_seconds)
            bins[phase][k].append(rtt)

    maxk = max(max(bins[PHASE_A], default=-1), max(bins[PHASE_B], default=-1))
    print(f"{'tick':>4}  {'A (raw ms)':<40} {'B (raw ms)':<40}")
    for k in range(maxk + 1):
        a = bins[PHASE_A].get(k, [])
        b = bins[PHASE_B].get(k, [])
        a_str = ",".join(f"{v:.0f}" for v in a) if a else "-"
        b_str = ",".join(f"{v:.0f}" for v in b) if b else "-"
        print(f"{k:>4}  {a_str:<40} {b_str:<40}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
