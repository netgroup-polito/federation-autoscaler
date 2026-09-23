#!/usr/bin/env python3
"""
latencyDiagramMaker.py
======================

Direct-comparison latency chart for a comparative-latency run -- the latency
twin of ecoDiagramMaker.py, built the same way so the two thesis figures can be
read side by side without caveats.

Phase A (Random) and Phase B (Latency) are each measured from THEIR OWN start
(elapsed minutes since that phase's first valid event) and plotted on the SAME
X axis, so "N minutes into the phase" compares directly between the two
policies -- e.g. "at minute 5, were Consumers closer to their provider under
Random or under Latency?".

Method, identical to ecoDiagramMaker.py: every Consumer carries its latest
measured RTT to the provider it is peered with, forward-filled until its next
successful reservation event; that per-Consumer state is resampled onto a
regular grid and aggregated across Consumers.

One deliberate difference: the Y axis is the MEAN RTT across active Consumers,
where the eco chart plots a SUM of carbon intensities. A sum of milliseconds has
no physical meaning, and it grows with the number of Consumers, so a 3x7 run and
a 30x70 run would land on scales a factor of ten apart. The mean stays in real
milliseconds on the same scale at every size. (The sum is still written to the
CSV output as raw data.)

Generic by design: the number of Consumers is auto-detected from the CSV, so
the same command works unchanged for every experiment size, e.g.:

    python3 federation-tests/scripts/latencyDiagramMaker.py --input results/3c-7p/reservations.csv
    python3 federation-tests/scripts/latencyDiagramMaker.py --input results/30c-70p/reservations.csv

Outputs (default: an `analysis/` directory next to the input file; override
with --output-dir):

    latency_comparison.png   -- 300 DPI step chart, both policies overlaid
    latency_comparison.pdf   -- vector version of the same chart
    latency_comparison.csv   -- the underlying per-phase timeline data
    latency_summary.md       -- text summary + sanity-check warnings

Only `reservations.csv` is read; no other experiment output file is required.

Requires: Python 3, pandas, matplotlib (standard library otherwise).
"""

from __future__ import annotations

import argparse
import math
import sys
from pathlib import Path

import pandas as pd

import matplotlib

matplotlib.use("Agg")  # headless-safe: no display needed (e.g. on a remote server)
import matplotlib.pyplot as plt  # noqa: E402  (must follow matplotlib.use)

# Columns this script expects to find in the input CSV. A comparative-latency
# reservations.csv always carries all of them (see ReservationRecord in
# federation-tests/testlib/writer.go).
REQUIRED_COLUMNS = [
    "timestamp",
    "consumer_id",
    "phase",
    "policy",
    "provider_id",
    "action",
    "rtt_ms",
    "outcome",
    "final_phase",
]

# The two phase labels this test harness always produces (testlib.PhaseA /
# testlib.PhaseB), which is what lets phase boundaries be read from the phase
# column instead of being passed in.
PHASE_A = "phase-a"
PHASE_B = "phase-b"


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="latencyDiagramMaker.py",
        description=(
            "Overlay Phase A (Random) and Phase B (Latency) on the SAME X axis -- "
            "each measured as elapsed minutes since ITS OWN start -- plotting the mean "
            "RTT from each Consumer to the provider it is peered with. Works unchanged "
            "for any experiment size -- the number of Consumers is auto-detected from "
            "the CSV."
        ),
        epilog=(
            "examples (identical command, only --input changes with scale):\n"
            "  python3 federation-tests/scripts/latencyDiagramMaker.py --input results/3c-7p/reservations.csv\n"
            "  python3 federation-tests/scripts/latencyDiagramMaker.py --input results/30c-70p/reservations.csv\n"
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--input",
        required=True,
        type=Path,
        help="Path to a comparative-latency reservations.csv file.",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        default=None,
        help="Output directory (default: an 'analysis' folder next to --input).",
    )
    parser.add_argument(
        "--grid",
        choices=["regular", "events"],
        default="regular",
        help=(
            "How to build each phase's own timeline. 'regular' (default) resamples "
            "onto an evenly spaced grid (see --grid-minutes); 'events' uses every "
            "distinct valid event timestamp instead, giving an exact step function "
            "at the cost of a less even spacing."
        ),
    )
    parser.add_argument(
        "--grid-minutes",
        type=float,
        default=1.0,
        help="Grid spacing in minutes when --grid=regular (default: 1.0).",
    )
    args = parser.parse_args(argv)
    if args.grid == "regular" and args.grid_minutes <= 0:
        parser.error("--grid-minutes must be a positive number")
    return args


def load_reservations(path: Path) -> pd.DataFrame:
    """Reads and validates the input CSV, exiting with a clear message on any
    problem -- this script is run by hand while writing a thesis, so a
    traceback is the wrong way to say "wrong file"."""
    if not path.is_file():
        sys.exit(f"error: input file not found: {path}")

    try:
        df = pd.read_csv(path, dtype=str)
    except Exception as exc:  # noqa: BLE001 -- surfaced to the user as-is
        sys.exit(f"error: failed to read CSV '{path}': {exc}")

    if df.empty:
        sys.exit(f"error: input CSV '{path}' has no data rows.")

    missing = [c for c in REQUIRED_COLUMNS if c not in df.columns]
    if missing:
        sys.exit(
            "error: input CSV is missing required column(s): "
            + ", ".join(missing)
            + f"\n  found columns: {', '.join(df.columns)}"
            + "\n  this script expects a comparative-latency reservations.csv "
            "(see federation-tests/testlib/writer.go, ReservationRecord)."
        )

    # Go's time.RFC3339Nano trims trailing zero fractional digits, so the
    # precision varies row to row; pd.to_datetime handles that, and
    # errors="coerce" drops a stray bad row instead of aborting the run.
    df["timestamp"] = pd.to_datetime(df["timestamp"], utc=True, errors="coerce")
    n_bad_ts = int(df["timestamp"].isna().sum())
    if n_bad_ts:
        print(
            f"warning: {n_bad_ts} row(s) had an unparseable timestamp and were dropped",
            file=sys.stderr,
        )
        df = df.dropna(subset=["timestamp"])
    if df.empty:
        sys.exit("error: no rows with a parseable timestamp remain in the input.")

    # "+Inf" (unreachable provider) parses to inf here; "0.000" stays 0. Both
    # are rejected by filter_valid, not here, so they can still be counted.
    df["rtt_ms"] = pd.to_numeric(df["rtt_ms"], errors="coerce")
    return df


def has_rtt_reading(rtt: pd.Series) -> pd.Series:
    """True where rtt_ms is an actual measurement.

    The writer formats RTT unconditionally, so a row that never measured one
    (a keep with no growable alternative) comes out as 0.000, and an
    unreachable provider as +Inf. Neither is a latency: a 0 would enter the
    mean as a perfect connection and drag it down, so both count as no reading.
    """
    return rtt.notna() & rtt.map(lambda v: math.isfinite(v) if pd.notna(v) else False) & (rtt > 0)


def is_eco_csv(df: pd.DataFrame) -> bool:
    """A comparative-eco reservations.csv shares the same header -- rtt_ms is
    present but never filled -- so the column check alone cannot tell the two
    apart. What does is that not a single row carries a real RTT."""
    return not has_rtt_reading(df["rtt_ms"]).any()


def filter_valid(df: pd.DataFrame) -> pd.DataFrame:
    """Rows that may update a Consumer's active RTT state: a successful,
    Peered reservation with a real RTT reading and a real consumer_id.
    Everything else never touches the per-Consumer state, so a failed or
    unmeasured event simply leaves the previous reading in place."""
    consumer_id = df["consumer_id"].fillna("")
    mask = (
        (df["outcome"] == "success")
        & (df["final_phase"] == "Peered")
        & has_rtt_reading(df["rtt_ms"])
        & df["consumer_id"].notna()
        & (consumer_id.str.strip() != "")
    )
    return df[mask].copy()


def phase_duration(df: pd.DataFrame, phase_value: str) -> pd.Timedelta | None:
    """Wall-clock span of a phase, from the RAW (unfiltered) data."""
    ts = df.loc[df["phase"] == phase_value, "timestamp"]
    if ts.empty:
        return None
    return ts.max() - ts.min()


def phase_policy_name(df: pd.DataFrame, phase_value: str, default: str) -> str:
    """The policy name recorded for a phase, read from the data so labels stay
    correct if policy names ever change."""
    values = df.loc[df["phase"] == phase_value, "policy"].dropna()
    values = values[values.str.strip() != ""]
    return values.iloc[0] if not values.empty else default


def build_state_matrix(valid: pd.DataFrame) -> pd.DataFrame:
    """Wide matrix: index = distinct valid-event timestamp, one column per
    Consumer, values = that Consumer's latest RTT as of that timestamp.
    Forward-filled per column, and NaN (not zero) before a Consumer's first
    reading, so a Consumer not yet peered is absent rather than instant."""
    v = valid.sort_values("timestamp")
    # pivot() raises on duplicate (index, column) pairs; keep the later event.
    v = v.drop_duplicates(subset=["timestamp", "consumer_id"], keep="last")
    wide = v.pivot(index="timestamp", columns="consumer_id", values="rtt_ms")
    return wide.sort_index().ffill()


def build_timeline_index(wide: pd.DataFrame, grid: str, grid_minutes: float) -> pd.DatetimeIndex:
    if grid == "events":
        return wide.index
    t0, t1 = wide.index.min(), wide.index.max()
    idx = pd.date_range(t0, t1, freq=pd.Timedelta(minutes=grid_minutes))
    if idx.empty or idx[-1] < t1:
        idx = idx.append(pd.DatetimeIndex([t1]))
    return idx


def resample_state(wide: pd.DataFrame, target_index: pd.DatetimeIndex) -> pd.DataFrame:
    """Evaluates the per-Consumer state at target_index. A plain reindex only
    aligns on exact timestamp matches; unioning the event index in first, then
    forward-filling, carries each Consumer's last reading onto every grid point
    while leaving not-yet-active Consumers as NaN."""
    unioned = wide.reindex(wide.index.union(target_index)).sort_index().ffill()
    return unioned.reindex(target_index)


def compute_aggregate(state: pd.DataFrame) -> pd.DataFrame:
    active_consumers = state.notna().sum(axis=1)
    total = state.sum(axis=1, skipna=True)
    # Zero active Consumers (before anyone's first reading) leaves the mean
    # blank rather than inf; plain NaN, not pd.NA, to keep float64 arithmetic.
    denom = active_consumers.astype("float64").replace(0.0, float("nan"))
    mean = total / denom
    return pd.DataFrame(
        {
            "timestamp": state.index,
            "active_consumers": active_consumers.to_numpy(),
            "mean_rtt_ms": mean.to_numpy(),
            "sum_rtt_ms": total.to_numpy(),
        }
    )


def build_phase_local_timeline(
    valid: pd.DataFrame,
    phase_value: str,
    policy_label: str,
    grid: str,
    grid_minutes: float,
) -> pd.DataFrame | None:
    """One phase's own timeline, measured from THAT PHASE's first valid event
    (elapsed_minutes == 0 there), which is what makes the two curves overlay
    instead of sitting end to end. None if the phase has no valid data."""
    phase_valid = valid[valid["phase"] == phase_value]
    if phase_valid.empty:
        return None

    wide = build_state_matrix(phase_valid)
    t0_phase = wide.index.min()
    target_index = build_timeline_index(wide, grid, grid_minutes)
    state = resample_state(wide, target_index)

    timeline = compute_aggregate(state)
    timeline["elapsed_minutes"] = (timeline["timestamp"] - t0_phase).dt.total_seconds() / 60.0
    timeline["phase"] = phase_value
    timeline["policy"] = policy_label
    return timeline[
        [
            "timestamp",
            "elapsed_minutes",
            "phase",
            "policy",
            "active_consumers",
            "mean_rtt_ms",
            "sum_rtt_ms",
        ]
    ]


def make_comparison_chart(
    timeline_a: pd.DataFrame | None,
    timeline_b: pd.DataFrame | None,
    subtitle: str,
    phase_a_label: str,
    phase_b_label: str,
    png_out: Path,
    pdf_out: Path,
) -> None:
    plt.rcParams.update(
        {
            "font.size": 11,
            "axes.grid": True,
            "grid.alpha": 0.35,
            "grid.linestyle": "--",
            "grid.linewidth": 0.6,
            "axes.spines.top": False,
            "axes.spines.right": False,
            "figure.facecolor": "white",
            "axes.facecolor": "white",
            "savefig.facecolor": "white",
        }
    )
    fig, ax = plt.subplots(figsize=(10, 5.5))

    # Same colours as ecoDiagramMaker.py -- Random red, policy under test green
    # -- so both thesis figures read with one legend in mind.
    if timeline_a is not None and not timeline_a.empty:
        ax.step(
            timeline_a["elapsed_minutes"],
            timeline_a["mean_rtt_ms"],
            where="post",
            color="#c0392b",
            linewidth=1.8,
            label=f"{phase_a_label} (Phase A)",
        )
    if timeline_b is not None and not timeline_b.empty:
        ax.step(
            timeline_b["elapsed_minutes"],
            timeline_b["mean_rtt_ms"],
            where="post",
            color="#1e8449",
            linewidth=1.8,
            label=f"{phase_b_label} (Phase B)",
        )

    ax.set_xlabel("Elapsed time since phase start [minutes]")
    ax.set_ylabel("Mean RTT to selected provider [ms]")
    ax.set_xlim(left=0)
    ax.set_ylim(bottom=0)

    fig.suptitle("Latency Indicator by Policy — Direct Comparison", fontsize=13, fontweight="bold", y=0.98)
    ax.set_title(subtitle, fontsize=9.5, color="#555555", pad=10)

    ax.legend(loc="best", frameon=True, framealpha=0.9)
    fig.tight_layout(rect=(0, 0, 1, 0.94))

    fig.savefig(png_out, dpi=300)
    fig.savefig(pdf_out)
    plt.close(fig)


def write_summary(
    path: Path,
    *,
    input_path: Path,
    n_detected: int,
    valid_consumer_ids: list[str],
    missing_consumers: list[str],
    phase_a_label: str,
    phase_b_label: str,
    dur_a: pd.Timedelta | None,
    dur_b: pd.Timedelta | None,
    n_valid: int,
    n_ignored: int,
    n_success_without_rtt: int,
    timeline: pd.DataFrame,
    grid_mode: str,
    grid_minutes: float,
) -> None:
    def fmt_td(td: pd.Timedelta | None) -> str:
        if td is None:
            return "n/a (phase not present in the input)"
        total = td.total_seconds()
        return f"{total / 60:.2f} min ({total:.1f} s)"

    def fmt_ms(v: float) -> str:
        return f"{v:.2f} ms" if pd.notna(v) else "n/a"

    mean_a = timeline.loc[timeline["phase"] == PHASE_A, "mean_rtt_ms"].mean()
    mean_b = timeline.loc[timeline["phase"] == PHASE_B, "mean_rtt_ms"].mean()

    if pd.isna(mean_a) or pd.isna(mean_b):
        diff_line = "- Difference (A -> B): n/a (insufficient data in one phase)\n"
    else:
        abs_diff = mean_b - mean_a
        pct = f"{abs_diff / mean_a * 100.0:+.1f}%" if mean_a != 0 else "n/a"
        # Lower RTT is the improvement a latency-aware policy is meant to bring.
        direction = "lower, i.e. closer providers" if abs_diff < 0 else "higher, i.e. farther providers"
        diff_line = f"- Difference (A -> B): {abs_diff:+.2f} ms ({pct}; Phase B {direction})\n"

    warnings_lines: list[str] = []
    min_active = int(timeline["active_consumers"].min()) if not timeline.empty else 0
    max_active = int(timeline["active_consumers"].max()) if not timeline.empty else 0
    if min_active != max_active:
        warnings_lines.append(
            f"- Active Consumer count varied over the run: between {min_active} and "
            f"{max_active} (of {n_detected} detected in the file). The mean is taken over "
            "active Consumers only, so this changes how many readings each point averages, "
            "not its scale.\n"
        )
    if missing_consumers:
        warnings_lines.append(
            f"- {len(missing_consumers)} Consumer(s) never had a successful Peered "
            f"reservation with an RTT reading and contributed no data: {', '.join(missing_consumers)}.\n"
        )
    if n_success_without_rtt:
        warnings_lines.append(
            f"- {n_success_without_rtt} successful Peered row(s) carried no RTT reading "
            "(rtt_ms 0.000 or +Inf) and were left out, so they neither count as a 0ms "
            "connection nor break the forward-filled state.\n"
        )
    if not warnings_lines:
        warnings_lines.append("- None.\n")

    grid_desc = f"regular ({grid_minutes:g} min spacing)" if grid_mode == "regular" else "events (exact timestamps)"

    lines = [
        "# Latency Analysis Summary (direct policy comparison)\n\n",
        "Phase A and Phase B are each measured from their own start and plotted on the "
        "same X axis. The indicator is the mean RTT from each active Consumer to the "
        "provider it is peered with.\n\n",
        f"Input: `{input_path}`\n\n",
        f"- Detected Consumers: {n_detected}\n",
        f"- Consumers with at least one valid reservation: {len(valid_consumer_ids)}\n",
        f"- Phase A ({phase_a_label}) duration: {fmt_td(dur_a)}\n",
        f"- Phase B ({phase_b_label}) duration: {fmt_td(dur_b)}\n",
        f"- Valid successful Peered reservation records with an RTT: {n_valid}\n",
        f"- Ignored records (failed, incomplete, or without an RTT): {n_ignored}\n",
        f"- Timeline grid: {grid_desc}\n",
        "\n## Mean RTT to the selected provider\n\n",
        f"- Phase A ({phase_a_label}): {fmt_ms(mean_a)}\n",
        f"- Phase B ({phase_b_label}): {fmt_ms(mean_b)}\n",
        diff_line,
        "\n## Warnings\n\n",
        *warnings_lines,
        "\n## Reproduce\n\n",
        "```bash\n",
        f"python3 federation-tests/scripts/latencyDiagramMaker.py --input {input_path}\n",
        "```\n",
    ]
    path.write_text("".join(lines), encoding="utf-8")


def main(argv: list[str] | None = None) -> None:
    args = parse_args(argv)
    input_path = args.input

    df = load_reservations(input_path)
    if is_eco_csv(df):
        sys.exit(
            "error: no row in this file carries an RTT reading -- it looks like a "
            "comparative-eco reservations.csv. Use federation-tests/scripts/ecoDiagramMaker.py for it."
        )

    valid = filter_valid(df)
    n_ignored = len(df) - len(valid)
    success_peered = (df["outcome"] == "success") & (df["final_phase"] == "Peered")
    n_success_without_rtt = int((success_peered & ~has_rtt_reading(df["rtt_ms"])).sum())
    if valid.empty:
        sys.exit(
            "error: no valid rows found (need outcome=='success', "
            "final_phase=='Peered', and a positive finite rtt_ms)."
        )

    output_dir = args.output_dir or (input_path.parent / "analysis")
    output_dir.mkdir(parents=True, exist_ok=True)

    all_consumer_ids = sorted(df["consumer_id"].dropna().unique())
    valid_consumer_ids = sorted(valid["consumer_id"].unique())
    n_detected = len(all_consumer_ids)
    missing_consumers = sorted(set(all_consumer_ids) - set(valid_consumer_ids))

    phase_a_label = phase_policy_name(df, PHASE_A, "Phase A")
    phase_b_label = phase_policy_name(df, PHASE_B, "Phase B")

    timeline_a = build_phase_local_timeline(valid, PHASE_A, phase_a_label, args.grid, args.grid_minutes)
    timeline_b = build_phase_local_timeline(valid, PHASE_B, phase_b_label, args.grid, args.grid_minutes)
    if timeline_a is None and timeline_b is None:
        sys.exit("error: neither Phase A nor Phase B has any valid data to plot.")

    parts = [t for t in (timeline_a, timeline_b) if t is not None]
    timeline = pd.concat(parts, ignore_index=True)

    active_range = timeline["active_consumers"]
    min_active, max_active = int(active_range.min()), int(active_range.max())
    if min_active == max_active:
        subtitle = f"Detected Consumers: {min_active}"
    else:
        subtitle = f"Active Consumers: {min_active}–{max_active} (of {n_detected} detected)"

    png_out = output_dir / "latency_comparison.png"
    pdf_out = output_dir / "latency_comparison.pdf"
    csv_out = output_dir / "latency_comparison.csv"
    summary_out = output_dir / "latency_summary.md"

    make_comparison_chart(timeline_a, timeline_b, subtitle, phase_a_label, phase_b_label, png_out, pdf_out)

    timeline_out = timeline.copy()
    timeline_out["timestamp"] = timeline_out["timestamp"].dt.strftime("%Y-%m-%dT%H:%M:%S.%f%z")
    timeline_out.to_csv(csv_out, index=False)

    write_summary(
        summary_out,
        input_path=input_path,
        n_detected=n_detected,
        valid_consumer_ids=valid_consumer_ids,
        missing_consumers=missing_consumers,
        phase_a_label=phase_a_label,
        phase_b_label=phase_b_label,
        dur_a=phase_duration(df, PHASE_A),
        dur_b=phase_duration(df, PHASE_B),
        n_valid=len(valid),
        n_ignored=n_ignored,
        n_success_without_rtt=n_success_without_rtt,
        timeline=timeline,
        grid_mode=args.grid,
        grid_minutes=args.grid_minutes,
    )

    print("Wrote:")
    for p in (png_out, pdf_out, csv_out, summary_out):
        print(f"  {p}")


if __name__ == "__main__":
    main()
